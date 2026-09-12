package main

// ci.go holds `uzi ci` — the CLI twins of the read-only forge CI-run views (PRD
// #1255 M3, D10/D12): `ci list` (a repo's workflow/pipeline runs), `ci jobs <run-id>`
// (one run's jobs + steps) and `ci fix <ref>` (queue a ci_fix run for a failed ref,
// the D12-moved route). All follow the `--json`/table idiom and reuse resolveRepo.

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// ciListDefaultLimit mirrors the api's default page (forgeViewCIRunsDefault): a
// zero/omitted --limit sends no query param, so the server applies its own default.
const ciListDefaultLimit = 0

// newCICmd builds `uzi ci` with the list, jobs and fix subcommands.
func newCICmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "List CI runs, inspect one run's jobs, and trigger a CI-fix",
		Long: "Read the forge CI views the api serves from the connection's stored PAT " +
			"(PRD #1255): a repo's recent workflow/pipeline runs, one run's jobs and steps, " +
			"and (D12) queue a ci_fix run for a failed ref. `--repo` defaults to your single " +
			"enabled repo; with several, pass `--repo <id>` (see `uzi repo list`).",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List a repo's recent CI runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repoID, _, err := resolveRepo(cmd.Context(), c, repoFlag)
			if err != nil {
				return err
			}
			limit, _ := cmd.Flags().GetInt("limit")
			runs, unsupported, err := c.ListCIRuns(cmd.Context(), repoID, limit)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(runs)
			}
			// A forge version without the runs endpoint answers an empty list + a
			// sentence: print the notice to stderr and exit 0, don't fail (R2).
			if unsupported != "" {
				_, _ = fmt.Fprintln(env.Stderr, cellText(unsupported))
				return nil
			}
			return renderCIRuns(p, runs)
		},
	}
	list.Flags().String("repo", "", "repo id (defaults to your single enabled repo)")
	list.Flags().Int("limit", ciListDefaultLimit, "max runs to fetch (server default 30, capped at 100)")

	jobs := &cobra.Command{
		Use:   "jobs <run-id>",
		Short: "Show one CI run's jobs and steps",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, err := parseIID(args[0])
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repoID, _, err := resolveRepo(cmd.Context(), c, repoFlag)
			if err != nil {
				return err
			}
			detail, err := c.GetCIRun(cmd.Context(), repoID, runID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(detail)
			}
			if detail.Unsupported != "" {
				_, _ = fmt.Fprintln(env.Stderr, cellText(detail.Unsupported))
				return nil
			}
			return renderCIJobs(p, detail)
		},
	}
	jobs.Flags().String("repo", "", "repo id (defaults to your single enabled repo)")

	fix := &cobra.Command{
		Use:   "fix <ref>",
		Short: "Queue a CI-fix run for a failed pipeline on a ref",
		Long: "Queue a ci_fix run for a ref whose latest cached pipeline is failed (PRD " +
			"#1255 D12). The server re-validates the precondition, so a ref that is not " +
			"failed (or has no cached pipeline) is refused with a 409 and a reason.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := args[0]
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoFlag, _ := cmd.Flags().GetString("repo")
			repoID, _, err := resolveRepo(cmd.Context(), c, repoFlag)
			if err != nil {
				return err
			}
			run, err := c.CreateCIFixRun(cmd.Context(), repoID, ref)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			p.Printf("Queued CI-fix run %s (%s) for %s\n", run.ID, run.Status, ref)
			return nil
		},
	}
	fix.Flags().String("repo", "", "repo id (defaults to your single enabled repo)")

	cmd.AddCommand(list, jobs, fix)
	return cmd
}

// renderCIRuns prints the CI-run list as a table (PRD #1255 D3). Every forge string
// (workflow name, event, branch, title) is untrusted and sanitised by the Table
// boundary.
func renderCIRuns(p *uzicli.Printer, runs []apitypes.CIRunDTO) error {
	rows := make([][]string, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, []string{
			capCell(fmt.Sprintf("%s #%d", r.Name, r.Number), 32),
			capCell(r.Event, 14),
			capCell(r.Branch, 28),
			forgeState(r.Status, r.Conclusion),
			ciRunElapsed(r),
			capCell(r.Title, 44),
		})
	}
	return p.Table([]string{"RUN", "EVENT", "BRANCH", "STATUS", "ELAPSED", "TITLE"}, rows)
}

// renderCIJobs prints one run's jobs, with each job's steps indented beneath it as
// their own rows in the same table (PRD #1255 D3). Job/step names are untrusted and
// sanitised by the Table boundary.
func renderCIJobs(p *uzicli.Printer, d apitypes.CIRunDetailDTO) error {
	if len(d.Jobs) == 0 {
		p.Printf("(no jobs reported)\n")
		return nil
	}
	var rows [][]string
	for _, j := range d.Jobs {
		rows = append(rows, []string{
			capCell(j.Name, 44),
			forgeState(j.Status, j.Conclusion),
			elapsedBetween(j.StartedAt, j.FinishedAt, j.Status == "completed"),
		})
		for _, s := range j.Steps {
			rows = append(rows, []string{
				"  ↳ " + capCell(s.Name, 40),
				forgeState(s.Status, s.Conclusion),
				elapsedBetween(s.StartedAt, s.CompletedAt, s.Status == "completed"),
			})
		}
	}
	return p.Table([]string{"JOB / STEP", "STATE", "ELAPSED"}, rows)
}

// ciRunElapsed renders a CI run's wall-clock elapsed: to UpdatedAt once completed,
// to now while still running, and a dash when there is no start.
func ciRunElapsed(r apitypes.CIRunDTO) string {
	start := r.StartedAt
	if start.IsZero() {
		start = r.CreatedAt
	}
	return elapsedBetween(start, r.UpdatedAt, r.Status == "completed")
}
