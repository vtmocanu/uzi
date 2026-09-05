package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newSkillCmd — `uzi skill`. Manages the bundled, self-upgrading Claude Code
// skill (go:embed → ~/.claude/skills/uzi-cli/). The CLI also installs it
// best-effort on every command (see root's PersistentPreRun); these verbs are
// the explicit escape hatches.
func newSkillCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the bundled uzi-cli agent skill",
	}
	// --target selects the harness(es) each verb operates on: claude, codex, or all.
	// Empty means "every auto-detected target" (Claude always; Codex when its config
	// home exists). A persistent flag on the parent so all four verbs share it.
	cmd.PersistentFlags().String("target", "", "harness to act on: claude, codex, or all (default: all detected)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the installed skill is current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := selectSkillTargets(env, targetFlag(cmd))
			if err != nil {
				return err
			}
			// Attempt EVERY selected target — never stop after the first failure — and
			// capture each into a per-target envelope item (PRD #1143 D6 / SC3).
			results := make([]skillTargetResult, 0, len(targets))
			for _, t := range targets {
				r := skillTargetResult{Target: t.Name, Result: skillStatusJSON{}}
				st, hst, serr := skillAndHookStatus(env, t)
				r.Result = skillStatusJSON{Skill: st, Hook: hst}
				if serr != nil {
					r.Error = serr.Error()
					r.ExitCode = uzicli.ExitCodeFor(serr)
				}
				results = append(results, r)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// --target set ⇒ envelope only; omitted ⇒ legacy top-level (first/claude
				// target's fields) PLUS the targets[] array, byte-identical for a
				// successful no-target run (SC3).
				var payload any
				if cmd.Flags().Changed("target") {
					payload = skillTargetsEnvelope{Targets: results}
				} else {
					first, _ := results[0].Result.(skillStatusJSON)
					payload = legacyStatusJSON{skillStatusJSON: first, Targets: results}
				}
				if err := p.JSON(payload); err != nil {
					return err
				}
				return firstTargetFailure(results)
			}
			for _, r := range results {
				st, _ := statusOf(r)
				rows := [][]string{
					{"TARGET", r.Target},
					{"PATH", st.Skill.Path},
					{"INSTALLED", boolStr(st.Skill.Installed)},
					{"UP_TO_DATE", boolStr(st.Skill.UpToDate)},
					{"USER_EDITED", boolStr(st.Skill.UserEdited)},
					{"HOOK_INSTALLED", boolStr(st.Hook.Installed)},
					{"HOOK_CURRENT", boolStr(st.Hook.Current)},
				}
				// Codex's mixed inline-[hooks]/hooks.json representation is surfaced only
				// when detected (the row is absent for Claude and for a clean Codex tree).
				if st.Hook.HookConfigConflict {
					rows = append(rows, []string{"HOOK_CONFIG_CONFLICT", boolStr(true)})
				}
				if err := p.Table(nil, rows); err != nil {
					return err
				}
				if r.Error != "" {
					_, _ = fmt.Fprintf(env.Stderr, "reading %s skill status: %s\n", r.Target, r.Error)
				}
			}
			return firstTargetFailure(results)
		},
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh the bundled skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			targets, err := selectSkillTargets(env, targetFlag(cmd))
			if err != nil {
				return err
			}
			// Attempt EVERY selected target for its filesystem effect, capturing each
			// (success or failure) into an envelope item; explicit install failures ARE
			// real errors, so the first one drives a non-zero exit AFTER the envelope
			// prints (PRD #1143 D6). The best-effort auto path (root) still swallows.
			results := make([]skillTargetResult, 0, len(targets))
			for _, t := range targets {
				r := skillTargetResult{Target: t.Name, Result: uzicli.SkillInstallResult{}}
				inst, err := installerForTarget(env, t)
				if err != nil {
					setTargetErr(&r, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err))
					results = append(results, r)
					continue
				}
				res, err := inst.Install(force)
				r.Result = res
				if err != nil {
					setTargetErr(&r, uzicli.Exitf(uzicli.ExitGeneric, "installing %s skill: %v", t.Name, err))
				}
				results = append(results, r)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				var payload any
				if cmd.Flags().Changed("target") {
					payload = skillTargetsEnvelope{Targets: results}
				} else {
					first, _ := results[0].Result.(uzicli.SkillInstallResult)
					payload = legacyInstallJSON{SkillInstallResult: first, Targets: results}
				}
				if err := p.JSON(payload); err != nil {
					return err
				}
				return firstTargetFailure(results)
			}
			for _, r := range results {
				res, _ := r.Result.(uzicli.SkillInstallResult)
				if res.BackedUp {
					_, _ = fmt.Fprintf(env.Stderr, "your edited %s was preserved as %s\n", res.Path, res.BackupPath)
				}
				if r.Error != "" {
					_, _ = fmt.Fprintf(env.Stderr, "%s: %s\n", r.Target, r.Error)
					continue
				}
				if !gf.quiet {
					switch {
					case res.AlreadyCurrent:
						p.Printf("%s skill already up to date at %s\n", r.Target, res.Path)
					case res.Wrote:
						p.Printf("%s skill installed at %s\n", r.Target, res.Path)
					default:
						p.Printf("%s skill state refreshed at %s\n", r.Target, res.Path)
					}
				}
			}
			return firstTargetFailure(results)
		},
	}
	install.Flags().Bool("force", false, "overwrite an edited skill without prompting")

	// install-hook / uninstall-hook manage the opt-in session-start hook (PRD #86,
	// #1143) for every selected harness (Claude settings.json, Codex hooks.json).
	// They are VISIBLE and documented in SKILL.md — an unadvertised opt-in closes
	// nothing, so discoverability is the point.
	installHook := &cobra.Command{
		Use:   "install-hook",
		Short: "Install the opt-in session-start hook (runs `uzi skill install`) for the selected harness(es)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := selectSkillTargets(env, targetFlag(cmd))
			if err != nil {
				return err
			}
			// Attempt EVERY selected target for its filesystem effect, capturing each
			// into an envelope item; the first failure drives the exit AFTER the
			// envelope prints (PRD #1143 D6).
			results := make([]skillTargetResult, 0, len(targets))
			for _, t := range targets {
				r := skillTargetResult{Target: t.Name, Result: uzicli.HookInstallResult{}}
				hm, err := hookManagerForTarget(env, t)
				if err != nil {
					setTargetErr(&r, err)
					results = append(results, r)
					continue
				}
				res, err := hm.InstallHook()
				r.Result = res
				if err != nil {
					setTargetErr(&r, err)
				}
				results = append(results, r)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				var payload any
				if cmd.Flags().Changed("target") {
					payload = skillTargetsEnvelope{Targets: results}
				} else {
					first, _ := results[0].Result.(uzicli.HookInstallResult)
					payload = legacyHookInstallJSON{HookInstallResult: first, Targets: results}
				}
				if err := p.JSON(payload); err != nil {
					return err
				}
				return firstTargetFailure(results)
			}
			for _, r := range results {
				res, _ := r.Result.(uzicli.HookInstallResult)
				if res.BackedUp {
					_, _ = fmt.Fprintf(env.Stderr, "your %s was backed up to %s\n", res.Path, res.BackupPath)
				}
				if r.Error != "" {
					_, _ = fmt.Fprintf(env.Stderr, "%s: %s\n", r.Target, r.Error)
					continue
				}
				if !gf.quiet {
					switch {
					case res.AlreadyPresent:
						p.Printf("%s session-start hook already present in %s\n", r.Target, res.Path)
					case res.Changed:
						p.Printf("%s session-start hook installed in %s\n", r.Target, res.Path)
					default:
						p.Printf("%s session-start hook unchanged in %s\n", r.Target, res.Path)
					}
				}
				// Codex never writes trust: after a change, point the user at /hooks to
				// review and trust the freshly written hook.
				if r.Target == "codex" && res.Changed {
					_, _ = fmt.Fprintf(env.Stderr,
						"run /hooks in Codex to review and trust this hook (uzi never writes Codex trust state)\n")
				}
			}
			return firstTargetFailure(results)
		},
	}

	uninstallHook := &cobra.Command{
		Use:   "uninstall-hook",
		Short: "Remove the session-start hook this CLI manages from the selected harness(es)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := selectSkillTargets(env, targetFlag(cmd))
			if err != nil {
				return err
			}
			// Attempt EVERY selected target for its filesystem effect, capturing each
			// into an envelope item; the first failure drives the exit AFTER the
			// envelope prints (PRD #1143 D6).
			results := make([]skillTargetResult, 0, len(targets))
			for _, t := range targets {
				r := skillTargetResult{Target: t.Name, Result: uzicli.HookUninstallResult{}}
				hm, err := hookManagerForTarget(env, t)
				if err != nil {
					setTargetErr(&r, err)
					results = append(results, r)
					continue
				}
				res, err := hm.UninstallHook()
				r.Result = res
				if err != nil {
					setTargetErr(&r, err)
				}
				results = append(results, r)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				var payload any
				if cmd.Flags().Changed("target") {
					payload = skillTargetsEnvelope{Targets: results}
				} else {
					first, _ := results[0].Result.(uzicli.HookUninstallResult)
					payload = legacyHookUninstallJSON{HookUninstallResult: first, Targets: results}
				}
				if err := p.JSON(payload); err != nil {
					return err
				}
				return firstTargetFailure(results)
			}
			for _, r := range results {
				res, _ := r.Result.(uzicli.HookUninstallResult)
				if res.BackedUp {
					_, _ = fmt.Fprintf(env.Stderr, "your %s was backed up to %s\n", res.Path, res.BackupPath)
				}
				if r.Error != "" {
					_, _ = fmt.Fprintf(env.Stderr, "%s: %s\n", r.Target, r.Error)
					continue
				}
				if !gf.quiet {
					if res.Changed {
						p.Printf("removed %d %s session-start hook entr(y/ies) from %s\n", res.Removed, r.Target, res.Path)
					} else {
						p.Printf("no uzi session-start hook found in %s\n", res.Path)
					}
				}
				// A Codex removal that shifted a later entry's trust index warrants a
				// re-review via /hooks (uzi never writes trust).
				if r.Target == "codex" && res.Changed && res.NonTerminalRemoval {
					_, _ = fmt.Fprintf(env.Stderr,
						"a later hook entry followed the one removed from %s; its Codex trust may need re-review via /hooks\n", res.Path)
				}
			}
			return firstTargetFailure(results)
		},
	}

	cmd.AddCommand(status, install, installHook, uninstallHook)
	return cmd
}

// skillStatusJSON is the single JSON object `uzi skill status --json` emits:
// the SKILL.md state and the SessionStart-hook state together.
type skillStatusJSON struct {
	Skill uzicli.SkillStatusResult `json:"skill"`
	Hook  uzicli.HookStatusResult  `json:"hook"`
}

// skillTargetResult is one element of the per-verb `targets` envelope (PRD #1143
// D6 / SC3): the harness name, its per-verb DTO (skillStatusJSON for status; the
// uzicli install/hook result for the others), and — on a target failure — the
// error message and the exit code it maps to. On success Error is empty and
// ExitCode is 0.
type skillTargetResult struct {
	Target   string `json:"target"`
	Result   any    `json:"result"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// skillTargetsEnvelope is the `--target`-set output shape: the targets[] array
// alone, with NO promoted legacy top-level fields.
type skillTargetsEnvelope struct {
	Targets []skillTargetResult `json:"targets"`
}

// The legacy*JSON shapes are the `--target`-omitted output: the FIRST (always
// claude) target's DTO fields promoted to the top level via anonymous embedding —
// byte-identical to the pre-M3 single-object JSON for a successful run — PLUS the
// additive targets[] array. A failed first target embeds the zero DTO, which is
// the intended D6 behaviour (SC3 pins only the SUCCESSFUL legacy shape).
type legacyStatusJSON struct {
	skillStatusJSON
	Targets []skillTargetResult `json:"targets"`
}

type legacyInstallJSON struct {
	uzicli.SkillInstallResult
	Targets []skillTargetResult `json:"targets"`
}

type legacyHookInstallJSON struct {
	uzicli.HookInstallResult
	Targets []skillTargetResult `json:"targets"`
}

type legacyHookUninstallJSON struct {
	uzicli.HookUninstallResult
	Targets []skillTargetResult `json:"targets"`
}

// setTargetErr records a target failure on its envelope item: the message and the
// exit code the error maps to. The per-target Result is left as whatever the call
// returned (a partial or zero DTO).
func setTargetErr(r *skillTargetResult, err error) {
	r.Error = err.Error()
	r.ExitCode = uzicli.ExitCodeFor(err)
}

// firstTargetFailure returns an *ExitError carrying the FIRST failing target's
// exit code and message, so the process exits non-zero AFTER the full envelope has
// already been emitted. It returns nil when every target succeeded.
func firstTargetFailure(results []skillTargetResult) error {
	for _, r := range results {
		if r.Error != "" {
			return uzicli.Exitf(r.ExitCode, "%s", r.Error)
		}
	}
	return nil
}

// statusOf extracts the skillStatusJSON a status target carries, so the text
// renderer can read the typed fields back out of the envelope item.
func statusOf(r skillTargetResult) (skillStatusJSON, bool) {
	st, ok := r.Result.(skillStatusJSON)
	return st, ok
}

// skillHome resolves the base home dir the bundled skill installs under: the
// test-only SkillHome injection when set, else the real os.UserHomeDir().
func skillHome(env Env) (string, error) {
	if env.SkillHome != "" {
		return env.SkillHome, nil
	}
	return os.UserHomeDir()
}

// selectSkillTargets resolves the home dir and the env-lookup seam, then returns —
// in the deterministic claude-before-codex order — the SkillTargets the --target
// value selects. It matches the PRD #1143 selection table:
//
//   - "" (omitted): Claude always, plus Codex only when auto-detected.
//   - "claude":     Claude only.
//   - "codex":      Codex only, even when its config home is absent (its selectErr
//     for a relative $CODEX_HOME is surfaced).
//   - "all":        Claude and Codex, even when Codex is absent (same selectErr).
//   - anything else: a usage error.
func selectSkillTargets(env Env, target string) ([]uzicli.SkillTarget, error) {
	home, err := skillHome(env)
	if err != nil {
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
	}
	var claude, codex uzicli.ResolvedTarget
	for _, rt := range uzicli.ResolveSkillTargets(home, env.getenv) {
		switch rt.Target.Name {
		case "claude":
			claude = rt
		case "codex":
			codex = rt
		}
	}

	switch target {
	case "":
		out := []uzicli.SkillTarget{claude.Target}
		if codex.Detected {
			out = append(out, codex.Target)
		}
		return out, nil
	case "claude":
		return []uzicli.SkillTarget{claude.Target}, nil
	case "codex":
		if codex.SelectErr != nil {
			return nil, codex.SelectErr
		}
		return []uzicli.SkillTarget{codex.Target}, nil
	case "all":
		if codex.SelectErr != nil {
			return nil, codex.SelectErr
		}
		return []uzicli.SkillTarget{claude.Target, codex.Target}, nil
	default:
		return nil, uzicli.Exitf(uzicli.ExitUsage, "invalid --target %q (want claude, codex, or all)", target)
	}
}

// installerForTarget builds the SkillInstaller for a selected target. The Claude
// target routes through skillInstaller so its real-home/test-home seam is unchanged;
// every other target comes straight from the resolved absolute SkillDir.
func installerForTarget(env Env, t uzicli.SkillTarget) (*uzicli.SkillInstaller, error) {
	if t.Name == "claude" {
		return skillInstaller(env)
	}
	return uzicli.NewSkillInstallerForTarget(t, version), nil
}

// skillAndHookStatus reports the skill status for a target plus its session-start
// hook status, using the per-target hook manager (Claude settings.json / Codex
// hooks.json).
func skillAndHookStatus(env Env, t uzicli.SkillTarget) (uzicli.SkillStatusResult, uzicli.HookStatusResult, error) {
	inst, err := installerForTarget(env, t)
	if err != nil {
		return uzicli.SkillStatusResult{}, uzicli.HookStatusResult{}, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
	}
	st := inst.Status()
	hm, err := hookManagerForTarget(env, t)
	if err != nil {
		return st, uzicli.HookStatusResult{}, err
	}
	return st, hm.HookStatus(), nil
}

// hookManagerForTarget builds the session-start hook manager for a selected target.
// Claude routes through the existing hookManager seam (unchanged real-home/test-home
// behaviour); Codex builds one straight from the resolved absolute hooks.json path.
func hookManagerForTarget(env Env, t uzicli.SkillTarget) (*uzicli.HookManager, error) {
	if t.Name == "claude" {
		hm, err := hookManager(env)
		if err != nil {
			return nil, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
		}
		return hm, nil
	}
	return uzicli.NewCodexHookManager(t.HookPath), nil
}

// targetFlag reads the shared --target value (registered as a persistent flag on
// the `skill` parent, so every verb sees it).
func targetFlag(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("target")
	return v
}

// skillInstaller builds the Claude installer for the current Env. SkillHome is a
// test-only injection (empty means the real home via os.UserHomeDir); the version
// stamped into the binary is recorded in the sidecar.
func skillInstaller(env Env) (*uzicli.SkillInstaller, error) {
	if env.SkillHome != "" {
		return uzicli.NewSkillInstallerAt(env.SkillHome, version), nil
	}
	return uzicli.NewSkillInstaller(version)
}

// hookManager builds the SessionStart-hook manager for the current Env,
// mirroring skillInstaller. SkillHome is the test-only injection (empty means
// the real ~/.claude via os.UserHomeDir).
func hookManager(env Env) (*uzicli.HookManager, error) {
	if env.SkillHome != "" {
		return uzicli.NewHookManagerAt(env.SkillHome), nil
	}
	return uzicli.NewHookManager()
}

// underSkillCmd reports whether cmd (or an ancestor) is `uzi skill`, so the
// auto-upgrade hook can skip the explicit skill-management verbs.
func underSkillCmd(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "skill" {
			return true
		}
	}
	return false
}

// maybeAutoUpgradeSkill installs the bundled skill best-effort for every
// auto-detected target (Claude always; Codex only when its config home already
// exists — the automatic path never litters a machine without Codex). It NEVER
// fails a command: a disabled env var or a missing home dir is a silent skip.
//
// Per-target behaviour follows PRD #1143 D7: a Claude write error warns on stderr
// and a rescued Claude edit says so once (naturally once — the next run finds the
// file current); Codex is SILENT on this path so a read-only Codex tree adds no
// noise to every uzi command. Each target has its own dir, sidecar and .bak, so the
// rescue bookkeeping falls out per-target automatically.
func maybeAutoUpgradeSkill(env Env) {
	if os.Getenv("UZI_SKILL_AUTO_UPGRADE") == "0" {
		return
	}
	home, err := skillHome(env)
	if err != nil {
		return // no home dir → nowhere to install; env/flags still drive the CLI
	}
	for _, rt := range uzicli.ResolveSkillTargets(home, env.getenv) {
		if !rt.Detected {
			continue
		}
		inst, err := installerForTarget(env, rt.Target)
		if err != nil {
			continue
		}
		res, err := inst.Install(false)
		if err != nil {
			if rt.Target.Name == "claude" {
				_, _ = fmt.Fprintf(env.Stderr, "uzi: skill auto-upgrade skipped: %v\n", err)
			}
			continue // Codex failures are silent (D7).
		}
		if res.BackedUp && rt.Target.Name == "claude" {
			_, _ = fmt.Fprintf(env.Stderr, "uzi: your edited %s was preserved as %s and the bundled skill was reinstalled\n", res.Path, res.BackupPath)
		}
	}
}
