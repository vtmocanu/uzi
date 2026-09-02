package main

// run_limits.go holds the run queue/limit verbs — expedite/resume-now/mr-rework
// (PRD #1009 M4).

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunExpediteCmd builds `uzi run expedite`.
func newRunExpediteCmd(env Env, gf *globalFlags) *cobra.Command {
	expedite := &cobra.Command{
		Use:   "expedite <run-id>",
		Short: "Bump a queued run to the front of the claim queue (or --clear to undo)",
		Long: "Bump ONE queued run to the front of the claim queue so a worker picks it up before " +
			"the rest (PRD #320). It matters only before a run is claimed: ordering is fixed once a " +
			"worker takes the run, so a non-queued run is a 409 (exit 5). A foreign or unknown run is " +
			"a 404 (exit 4).\n\n" +
			"`--clear` undoes it — it removes the manual override and returns the run to its kind " +
			"default priority (it does NOT demote the run below normal).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// --clear removes the manual override (expedite=false); its absence expedites.
			clear, _ := cmd.Flags().GetBool("clear")
			run, err := c.SetRunPriority(cmd.Context(), args[0], !clear)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	expedite.Flags().Bool("clear", false, "clear the manual expedite (undo), returning the run to its kind default priority")
	return expedite
}

// newRunResumeNowCmd builds `uzi run resume-now`.
func newRunResumeNowCmd(env Env, gf *globalFlags) *cobra.Command {
	resumeNow := &cobra.Command{
		Use:   "resume-now <run-id>",
		Short: "Resume a run held waiting for a pooled Anthropic token, without waiting for the sweeper",
		Long: "Resume ONE run held in `pool_wait` — an `auto` run parked because its owner's Anthropic " +
			"token pool was empty when it claimed (PRD #754). It flips the hold straight to `queued` " +
			"instead of waiting up to a sweeper tick for the reactive pass to notice a token was pooled.\n\n" +
			"A run that is NOT held is a 409 (exit 5); a foreign or unknown run is a 404 (exit 4). No " +
			"token is spent and nothing is written to the forge — it only releases the hold.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.ResumeRunNow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	return resumeNow
}

// newRunMrReworkCmd builds `uzi run mr-rework`.
func newRunMrReworkCmd(env Env, gf *globalFlags) *cobra.Command {
	mrRework := &cobra.Command{
		Use:   "mr-rework <run-id>",
		Short: "Set whether this run's MR review comments are auto-reworked (--enabled[=false], or --clear to inherit)",
		Long: "Set the per-run override for the MR review-rework watcher (PRD #841): whether new review " +
			"comments on this run's open MR are auto-reworked. Tri-state, editable on a COMPLETED run for as " +
			"long as its MR is still open (the watcher acts after the run finishes):\n\n" +
			"  --enabled            turn auto-rework ON for this run\n" +
			"  --enabled=false      turn it OFF (its MR is never auto-reworked)\n" +
			"  --clear              clear the override back to inherit (follow the account default)\n\n" +
			"A foreign or unknown run is a 404 (exit 4). The write is inert once the MR is merged or closed. " +
			"Prints the run's resulting MR_REWORK state (inherit/on/off).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Three-way: --clear sends null (inherit); otherwise --enabled's value (default
			// true for a bare `mr-rework <id>`) is sent explicitly. --clear and --enabled
			// together is a usage error — they express opposite intents.
			clear, _ := cmd.Flags().GetBool("clear")
			var enabled *bool
			if clear {
				if cmd.Flags().Changed("enabled") {
					return uzicli.Exitf(uzicli.ExitUsage, "--clear and --enabled are mutually exclusive")
				}
				enabled = nil
			} else {
				v, _ := cmd.Flags().GetBool("enabled")
				enabled = &v
			}
			run, err := c.SetRunMrRework(cmd.Context(), args[0], enabled)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	mrRework.Flags().Bool("enabled", true, "whether this run's MR review comments are auto-reworked; pass --enabled=false to turn it off")
	mrRework.Flags().Bool("clear", false, "clear the per-run override back to inherit (follow the account default)")
	return mrRework
}
