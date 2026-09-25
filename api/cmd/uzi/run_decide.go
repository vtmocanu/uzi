package main

// run_decide.go holds the owner completion-decision verb — `uzi run decide` (PRD #1226 M5, D7 +
// #1227 M5).

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunDecideCmd builds `uzi run decide` (PRD #1226 M5, D7 + #1227 M5).
//
// It posts the owner's completion decision on a completion-blocked run to
// POST /api/runs/{id}/completion/decision and resumes the run. Exactly ONE of three decisions is
// required (its absence is a clean usage error before any request, not a round-trip 400):
//
//   - --continue [--guidance <text>]  — resume past the completion check, unchanged from #1226.
//   - --partial <ids> --reason <text> — reduce scope to the kept milestone ids; the rest are
//     deferred and the run's PR does NOT close the issue.
//   - --accept <ids> --reason <text>  — waive the named criterion ids as met; a closing PR then
//     names them in a warning block.
//
// It prints a one-line confirmation (the run's new status, plus decision detail) like `run resume`;
// --json emits the resumed RunDTO for the agent contract. Owner-only.
//
// The completion interlock is on by default for unseeded Claude and Codex issue runs (#1626).
// A seeded or kill-switched run is not interlocked, and on a non-interlocked or not-blocked
// run the server answers 409 (ErrCompletionNotBlocked → exit 5); the CLI drives the decision
// honestly regardless, keeping web and CLI on the one endpoint.
func newRunDecideCmd(env Env, gf *globalFlags) *cobra.Command {
	decide := &cobra.Command{
		Use:   "decide <run-id>",
		Short: "Record the owner's completion decision on a completion-blocked run (continue, partial or accept)",
		Long: "Record the owner's decision on a run the completion interlock has blocked (PRD #1226/#1227) " +
			"— either a live completion question (awaiting_input, whose answer is recorded and the current " +
			"worker resumes in place) or a run already parked in the completion hold. Owner-only.\n\n" +
			"Exactly ONE decision is required (its absence, or more than one, is a usage error before any " +
			"request):\n" +
			"  --continue [--guidance <text>]  resume past the check; --guidance optionally steers the run.\n" +
			"  --partial <ids> --reason <text> reduce scope to the kept milestone ids (comma-separated); the " +
			"rest are deferred and the run's PR does NOT close the issue.\n" +
			"  --accept <ids> --reason <text>  waive the named criterion ids (comma-separated) as met; a " +
			"closing PR then names them in a warning block.\n\n" +
			"--guidance is valid only with --continue; --reason is required with --partial/--accept. A " +
			"partial/accept sends the run's CURRENT contract revision, which the server fences: a stale " +
			"revision (someone else already decided) is a 409 (exit 5). A run that is NOT completion-blocked " +
			"is a 409 (exit 5); a foreign or unknown run is a 404 (exit 4). Prints the run's new status; " +
			"--json emits the resumed run object.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			id := args[0]

			cont, _ := cmd.Flags().GetBool("continue")
			partialSet := cmd.Flags().Changed("partial")
			acceptSet := cmd.Flags().Changed("accept")

			// Exactly one of --continue / --partial / --accept, checked before any request.
			chosen := 0
			if cont {
				chosen++
			}
			if partialSet {
				chosen++
			}
			if acceptSet {
				chosen++
			}
			if chosen == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "run decide requires exactly one of --continue, --partial or --accept")
			}
			if chosen > 1 {
				return uzicli.Exitf(uzicli.ExitUsage, "--continue, --partial and --accept are mutually exclusive")
			}

			guidanceSet := cmd.Flags().Changed("guidance")
			reasonSet := cmd.Flags().Changed("reason")
			reason, _ := cmd.Flags().GetString("reason")

			p := env.printer(gf)

			if cont {
				// --guidance rides continue; --reason belongs to partial/accept.
				if reasonSet {
					return uzicli.Exitf(uzicli.ExitUsage, "--reason is only valid with --partial or --accept")
				}
				guidance, _ := cmd.Flags().GetString("guidance")
				run, err := c.ContinueCompletionDecision(cmd.Context(), id, guidance)
				if err != nil {
					return err
				}
				if p.Format == uzicli.FormatJSON {
					return p.JSON(run)
				}
				if !gf.quiet {
					if strings.TrimSpace(guidance) != "" {
						p.Printf("Continue recorded for %s: %s (guidance recorded).\n", id, run.Status)
					} else {
						p.Printf("Continue recorded for %s: %s.\n", id, run.Status)
					}
				}
				return nil
			}

			// partial / accept share the required --reason, the forbidden --guidance, and the
			// revision fence: fetch the run's CURRENT revision (a 404 here surfaces as ExitNotFound)
			// and send it so the server applies rather than 400s on a missing/zero revision.
			if guidanceSet {
				return uzicli.Exitf(uzicli.ExitUsage, "--guidance is only valid with --continue")
			}
			if strings.TrimSpace(reason) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--partial and --accept require --reason <text>")
			}
			reason = strings.TrimSpace(reason)

			if partialSet {
				raw, _ := cmd.Flags().GetString("partial")
				keep := parseIDList(raw)
				if len(keep) == 0 {
					return uzicli.Exitf(uzicli.ExitUsage, "no milestone ids in --partial")
				}
				rev, err := runCompletionRevision(cmd, c, id)
				if err != nil {
					return err
				}
				run, err := c.PartialCompletionDecision(cmd.Context(), id, keep, reason, rev)
				if err != nil {
					return err
				}
				if p.Format == uzicli.FormatJSON {
					return p.JSON(run)
				}
				if !gf.quiet {
					newRev := rev
					if run.CompletionRevision != nil {
						newRev = *run.CompletionRevision
					}
					p.Printf("Scope reduced for %s: %s (revision %d).\n", id, run.Status, newRev)
					if len(run.CompletionDeferred) > 0 {
						deferred := make([]string, 0, len(run.CompletionDeferred))
						for _, d := range run.CompletionDeferred {
							deferred = append(deferred, d.MilestoneID)
						}
						p.Printf("Deferred milestones: %s\n", strings.Join(deferred, ", "))
					}
				}
				return nil
			}

			// accept
			raw, _ := cmd.Flags().GetString("accept")
			criteria := parseIDList(raw)
			if len(criteria) == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "no criterion ids in --accept")
			}
			rev, err := runCompletionRevision(cmd, c, id)
			if err != nil {
				return err
			}
			run, err := c.AcceptCompletionDecision(cmd.Context(), id, criteria, reason, rev)
			if err != nil {
				return err
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			if !gf.quiet {
				newRev := rev
				if run.CompletionRevision != nil {
					newRev = *run.CompletionRevision
				}
				p.Printf("Criteria accepted for %s: %s (revision %d).\n", id, run.Status, newRev)
				if len(run.CompletionAccepted) > 0 {
					accepted := make([]string, 0, len(run.CompletionAccepted))
					for _, a := range run.CompletionAccepted {
						accepted = append(accepted, a.ID)
					}
					p.Printf("Accepted criteria: %s\n", strings.Join(accepted, ", "))
				}
			}
			return nil
		},
	}
	decide.Flags().Bool("continue", false, "record the CONTINUE decision — resume past the completion check")
	decide.Flags().String("guidance", "", "optional guidance to steer the continued run (only with --continue)")
	decide.Flags().String("partial", "", "record the PARTIAL decision: comma-separated milestone ids to KEEP in scope (requires --reason)")
	decide.Flags().String("accept", "", "record the ACCEPT decision: comma-separated criterion ids to waive as met (requires --reason)")
	decide.Flags().String("reason", "", "owner reason for the partial/accept decision (required for those)")
	return decide
}

// parseIDList splits a comma-separated id flag into a clean list: each element is
// TrimSpace'd and empties are dropped, so `m2, ,m3,` yields [m2 m3] and a blank flag
// yields an empty slice (the caller then raises a usage error).
func parseIDList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// runCompletionRevision fetches the run and returns its current completion contract revision for a
// partial/accept decision. A run with no frozen contract (completion_revision == null) is not
// completion-blocked — which partial/accept require — so it is an honest ExitConflict here rather
// than a misleading server 400 ("contract_revision required"). A foreign/unknown run surfaces as
// the GetRun 404 (ExitNotFound) automatically.
func runCompletionRevision(cmd *cobra.Command, c uzicli.Client, id string) (int, error) {
	run, err := c.GetRun(cmd.Context(), id)
	if err != nil {
		return 0, err
	}
	if run.CompletionRevision == nil {
		return 0, uzicli.Exitf(uzicli.ExitConflict, "run %s has no completion contract to revise (it is not completion-blocked)", id)
	}
	return *run.CompletionRevision, nil
}
