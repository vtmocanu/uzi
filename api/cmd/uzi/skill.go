package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
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

	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the installed skill is current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := skillInstaller(env)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
			}
			st := inst.Status()
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(st)
			}
			return p.Table(nil, [][]string{
				{"PATH", st.Path},
				{"INSTALLED", boolStr(st.Installed)},
				{"UP_TO_DATE", boolStr(st.UpToDate)},
				{"USER_EDITED", boolStr(st.UserEdited)},
			})
		},
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh the bundled skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			force, _ := cmd.Flags().GetBool("force")
			inst, err := skillInstaller(env)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no home directory available: %v", err)
			}
			// Explicit install: a failure here IS a real error (exit non-zero) — the
			// user asked for it. The best-effort auto path (root) swallows errors.
			res, err := inst.Install(force)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "installing skill: %v", err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(res)
			}
			if res.BackedUp {
				fmt.Fprintf(env.Stderr, "your edited %s was preserved as %s\n", res.Path, res.BackupPath)
			}
			if !gf.quiet {
				switch {
				case res.AlreadyCurrent:
					p.Printf("skill already up to date at %s\n", res.Path)
				case res.Wrote:
					p.Printf("skill installed at %s\n", res.Path)
				default:
					p.Printf("skill state refreshed at %s\n", res.Path)
				}
			}
			return nil
		},
	}
	install.Flags().Bool("force", false, "overwrite an edited skill without prompting")

	cmd.AddCommand(status, install)
	return cmd
}

// skillInstaller builds the installer for the current Env. SkillHome is a
// test-only injection (empty means the real ~/.config-adjacent home via
// os.UserHomeDir); the version stamped into the binary is recorded in the sidecar.
func skillInstaller(env Env) (*uzicli.SkillInstaller, error) {
	if env.SkillHome != "" {
		return uzicli.NewSkillInstallerAt(env.SkillHome, version), nil
	}
	return uzicli.NewSkillInstaller(version)
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

// maybeAutoUpgradeSkill installs the bundled skill best-effort. It NEVER fails a
// command: a disabled env var or a missing home dir is a silent skip; any write
// error warns on stderr and returns. When a user-edited SKILL.md was rescued to
// .bak, it says so once (naturally once — the next run finds the file current).
func maybeAutoUpgradeSkill(env Env) {
	if os.Getenv("UZI_SKILL_AUTO_UPGRADE") == "0" {
		return
	}
	inst, err := skillInstaller(env)
	if err != nil {
		return // no home dir → nowhere to install; env/flags still drive the CLI
	}
	res, err := inst.Install(false)
	if err != nil {
		fmt.Fprintf(env.Stderr, "uzi: skill auto-upgrade skipped: %v\n", err)
		return
	}
	if res.BackedUp {
		fmt.Fprintf(env.Stderr, "uzi: your edited %s was preserved as %s and the bundled skill was reinstalled\n", res.Path, res.BackupPath)
	}
}
