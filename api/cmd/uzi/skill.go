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
			p := env.printer(gf)
			// M1 keeps the legacy single-object JSON ({skill,hook}) for the first
			// selected target so existing consumers/tests are unchanged; M3 replaces it
			// with the full targets[] envelope.
			if p.Format == uzicli.FormatJSON {
				st, hst, err := skillAndHookStatus(env, targets[0])
				if err != nil {
					return err
				}
				return p.JSON(skillStatusJSON{Skill: st, Hook: hst})
			}
			for _, t := range targets {
				st, hst, err := skillAndHookStatus(env, t)
				if err != nil {
					return err
				}
				rows := [][]string{
					{"TARGET", t.Name},
					{"PATH", st.Path},
					{"INSTALLED", boolStr(st.Installed)},
					{"UP_TO_DATE", boolStr(st.UpToDate)},
					{"USER_EDITED", boolStr(st.UserEdited)},
				}
				// The hook manager is Claude-only until M2, so only the Claude group
				// carries hook rows.
				if t.Name == "claude" {
					rows = append(rows,
						[]string{"HOOK_INSTALLED", boolStr(hst.Installed)},
						[]string{"HOOK_CURRENT", boolStr(hst.Current)},
					)
				}
				if err := p.Table(nil, rows); err != nil {
					return err
				}
			}
			return nil
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
			// Attempt every selected target for its filesystem effect, then render.
			results := make([]uzicli.SkillInstallResult, 0, len(targets))
			for _, t := range targets {
				inst, err := installerForTarget(env, t)
				if err != nil {
					return uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
				}
				// Explicit install: a failure here IS a real error (exit non-zero) — the
				// user asked for it. The best-effort auto path (root) swallows errors.
				res, err := inst.Install(force)
				if err != nil {
					return uzicli.Exitf(uzicli.ExitGeneric, "installing %s skill: %v", t.Name, err)
				}
				results = append(results, res)
			}
			p := env.printer(gf)
			// M1 keeps the legacy single-object result for the first selected target so
			// existing consumers/tests are unchanged; M3 replaces it with targets[].
			if p.Format == uzicli.FormatJSON {
				return p.JSON(results[0])
			}
			for i, res := range results {
				t := targets[i]
				if res.BackedUp {
					_, _ = fmt.Fprintf(env.Stderr, "your edited %s was preserved as %s\n", res.Path, res.BackupPath)
				}
				if !gf.quiet {
					switch {
					case res.AlreadyCurrent:
						p.Printf("%s skill already up to date at %s\n", t.Name, res.Path)
					case res.Wrote:
						p.Printf("%s skill installed at %s\n", t.Name, res.Path)
					default:
						p.Printf("%s skill state refreshed at %s\n", t.Name, res.Path)
					}
				}
			}
			return nil
		},
	}
	install.Flags().Bool("force", false, "overwrite an edited skill without prompting")

	// install-hook / uninstall-hook manage the opt-in Claude Code SessionStart
	// hook (PRD #86). They are VISIBLE and documented in SKILL.md — an
	// unadvertised opt-in closes nothing, so discoverability is the point.
	installHook := &cobra.Command{
		Use:   "install-hook",
		Short: "Install the opt-in Claude Code SessionStart hook (runs `uzi skill install`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := guardHookTarget(targetFlag(cmd)); err != nil {
				return err
			}
			hm, err := hookManager(env)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
			}
			res, err := hm.InstallHook()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "installing hook: %v", err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if res.BackedUp {
				_, _ = fmt.Fprintf(env.Stderr, "your %s was backed up to %s\n", res.Path, res.BackupPath)
			}
			if !gf.quiet {
				switch {
				case res.AlreadyPresent:
					p.Printf("SessionStart hook already present in %s\n", res.Path)
				case res.Changed:
					p.Printf("SessionStart hook installed in %s\n", res.Path)
				default:
					p.Printf("SessionStart hook unchanged in %s\n", res.Path)
				}
			}
			return nil
		},
	}

	uninstallHook := &cobra.Command{
		Use:   "uninstall-hook",
		Short: "Remove the Claude Code SessionStart hook this CLI manages",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := guardHookTarget(targetFlag(cmd)); err != nil {
				return err
			}
			hm, err := hookManager(env)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
			}
			res, err := hm.UninstallHook()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "removing hook: %v", err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if res.BackedUp {
				_, _ = fmt.Fprintf(env.Stderr, "your %s was backed up to %s\n", res.Path, res.BackupPath)
			}
			if !gf.quiet {
				if res.Changed {
					p.Printf("removed %d SessionStart hook entr(y/ies) from %s\n", res.Removed, res.Path)
				} else {
					p.Printf("no uzi SessionStart hook found in %s\n", res.Path)
				}
			}
			return nil
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

// skillAndHookStatus reports the skill status for a target plus, for Claude only
// (the hook manager is Claude-only until M2), its SessionStart-hook status.
func skillAndHookStatus(env Env, t uzicli.SkillTarget) (uzicli.SkillStatusResult, uzicli.HookStatusResult, error) {
	inst, err := installerForTarget(env, t)
	if err != nil {
		return uzicli.SkillStatusResult{}, uzicli.HookStatusResult{}, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
	}
	st := inst.Status()
	if t.Name != "claude" {
		return st, uzicli.HookStatusResult{}, nil
	}
	hm, err := hookManager(env)
	if err != nil {
		return st, uzicli.HookStatusResult{}, uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
	}
	return st, hm.HookStatus(), nil
}

// guardHookTarget rejects the hook verbs' Codex/all selection: the Codex hook
// manager lands in M2. Omitted or explicit claude operate on Claude exactly as
// before; any other value is the standard invalid-target usage error.
func guardHookTarget(target string) error {
	switch target {
	case "", "claude":
		return nil
	case "codex", "all":
		return uzicli.Exitf(uzicli.ExitUsage, "Codex hook target is not yet available")
	default:
		return uzicli.Exitf(uzicli.ExitUsage, "invalid --target %q (want claude, codex, or all)", target)
	}
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
