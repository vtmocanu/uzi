package main

// run_decide.go holds the owner completion-decision verb — `uzi run decide` (PRD #1226 M5, D7).

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunDecideCmd builds `uzi run decide` (PRD #1226 M5, D7).
//
// It posts the owner's CONTINUE decision on a completion-blocked run to
// POST /api/runs/{id}/completion/decision and resumes the run. Only the continue decision is
// supported today (the endpoint 400s any other value), so `--continue` is REQUIRED for this
// child: its absence is a clean usage error before any request rather than a round-trip 400. It
// prints a one-line confirmation (the run's new status, plus a note when guidance was recorded)
// like `run resume`; --json emits the resumed RunDTO for the agent contract.
//
// The completion interlock is rollout-OFF (inert) at HEAD, so on a non-interlocked run the
// server answers 409 (ErrCompletionNotBlocked → exit 5); the CLI drives the decision honestly
// regardless, keeping web and CLI on the one endpoint.
func newRunDecideCmd(env Env, gf *globalFlags) *cobra.Command {
	decide := &cobra.Command{
		Use:   "decide <run-id>",
		Short: "Record the owner CONTINUE decision on a completion-blocked run (resumes it, with optional guidance)",
		Long: "Record the owner's CONTINUE decision on a run parked in a completion hold (PRD #1226): the " +
			"run resumes and keeps working past the completion check. Owner-only.\n\n" +
			"Only the continue decision is supported today, so `--continue` is REQUIRED (its absence is a " +
			"usage error). Pass `--guidance <text>` to steer the continued run; it is optional (an empty " +
			"continue simply resumes with no note).\n\n" +
			"A run that is NOT completion-blocked is a 409 (exit 5); a foreign or unknown run is a 404 " +
			"(exit 4). Prints the run's new status; --json emits the resumed run object.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			cont, _ := cmd.Flags().GetBool("continue")
			if !cont {
				return uzicli.Exitf(uzicli.ExitUsage, "run decide requires --continue (the only supported decision today)")
			}
			guidance, _ := cmd.Flags().GetString("guidance")
			run, err := c.ContinueCompletionDecision(cmd.Context(), args[0], guidance)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			if !gf.quiet {
				if strings.TrimSpace(guidance) != "" {
					p.Printf("Continue recorded for %s: %s (guidance recorded).\n", args[0], run.Status)
				} else {
					p.Printf("Continue recorded for %s: %s.\n", args[0], run.Status)
				}
			}
			return nil
		},
	}
	decide.Flags().Bool("continue", false, "record the CONTINUE decision (required — the only supported decision today)")
	decide.Flags().String("guidance", "", "optional guidance to steer the continued run")
	return decide
}
