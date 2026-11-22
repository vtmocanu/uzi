package main

// admin_run_writers.go is `uzi admin run-writers` (issue #4618): the admin lookup of active
// ci_fix / mr_rework writers across all owners, for callers that must know whether a branch has
// a helper that may still push to it before they merge or rewrite it.

import (
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// runWritersIncompleteNote is the stderr line (via the returned error) when the server reports complete=false: more
// matching writers existed than it serves, so the list proves nothing about absence.
const runWritersIncompleteNote = "incomplete: more than 500 matching writers; absence is unknown"

// runWritersEmptyLine is the human output for a complete, empty result.
const runWritersEmptyLine = "no active ci_fix/mr_rework writers (complete, all owners, at this database snapshot)"

func newAdminRunWritersCmd(env Env, gf *globalFlags) *cobra.Command {
	var repoID, branch string
	var mrIID int64
	cmd := &cobra.Command{
		Use:   "run-writers --repo <uuid> --branch <ref> [--mr <iid>]",
		Short: "List active ci_fix/mr_rework writers for a branch across all owners",
		Long: "List the active ci_fix and mr_rework runs, across all owners, that may push to a branch " +
			"(admin run debug, read-only, audited). ci_fix runs match by exact --branch; mr_rework runs " +
			"match only by the supplied --mr, never by branch alone.\n\n" +
			"It needs the server flag UZI_ADMIN_RUN_DEBUG and an admin_ro (uza_) token. An older server " +
			"answers 404.\n\n" +
			"complete=true proves absence only at that database snapshot: a writer may start after the " +
			"read. When the server reports complete=false (more than 500 matches) the command prints a " +
			"note on stderr and exits non-zero, because absence is then unknown. --json prints the " +
			"decoded {writers, complete} document.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("mr") && mrIID <= 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid --mr %s: a positive merge request number is required", cellText(strconv.FormatInt(mrIID, 10)))
			}
			id, err := uuid.Parse(repoID)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid --repo %q: a repo UUID is required", cellText(repoID))
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			out, err := c.AdminRunWriters(cmd.Context(), id.String(), branch, mrIID)
			if err != nil {
				return hintOnForeignReadRefusal(env, err)
			}
			p := env.printer(gf)
			var renderErr error
			switch {
			case p.Format == uzicli.FormatJSON:
				renderErr = p.JSON(out)
			case len(out.Writers) == 0 && out.Complete:
				_, renderErr = fmt.Fprintln(env.Stdout, runWritersEmptyLine)
			default:
				rows := make([][]string, 0, len(out.Writers))
				for _, w := range out.Writers {
					ref, mr := "-", "-"
					if w.PipelineRef != nil {
						ref = *w.PipelineRef
					}
					if w.MRIID != nil {
						mr = strconv.FormatInt(*w.MRIID, 10)
					}
					rows = append(rows, []string{w.RunID, w.Kind, w.Status, w.OwnerID, w.RepoID, ref, mr})
				}
				renderErr = p.Table([]string{"RUN", "KIND", "STATUS", "OWNER", "REPO", "BRANCH", "MR"}, rows)
			}
			if renderErr != nil {
				return renderErr
			}
			if !out.Complete {
				// Main prints the returned error on stderr, so the note appears exactly once.
				return uzicli.Exitf(uzicli.ExitGeneric, "%s", runWritersIncompleteNote)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoID, "repo", "", "repo UUID (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "branch ref the writers would push to (required)")
	cmd.Flags().Int64Var(&mrIID, "mr", 0, "merge request number, to also match mr_rework writers on that MR")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("branch")
	return cmd
}
