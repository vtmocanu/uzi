package main

import "github.com/spf13/cobra"

// newSkillCmd — `uzi skill`. Manages the bundled, self-upgrading Claude Code
// skill (go:embed → ~/.claude/skills/uzi-cli/). Stubs in M3 (M9 implements).
func newSkillCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the bundled uzi-cli agent skill",
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the installed skill is current",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("skill status"),
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh the bundled skill",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("skill install"),
	}
	install.Flags().Bool("force", false, "overwrite an edited skill without prompting")

	cmd.AddCommand(status, install)
	return cmd
}
