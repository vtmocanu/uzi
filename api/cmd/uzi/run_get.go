package main

// run_get.go holds the read-only `uzi run` verbs — list/get/logs and the hidden
// `review` alias (PRD #1009 M4).

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunListCmd builds `uzi run list`.
func newRunListCmd(env Env, gf *globalFlags) *cobra.Command {
	list := &cobra.Command{
		Use:   "list",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(runs)
			}
			rows := make([][]string, 0, len(runs))
			now := time.Now()
			for _, r := range runs {
				rows = append(rows, []string{r.ID, r.Kind, effectiveRunStatus(r.Status, r.IsPlanning, r.IsRevising), runAgeCell(r.RunDTO, now), runTitle(r.RunDTO)})
			}
			return p.Table([]string{"ID", "KIND", "STATUS", "AGE", "TITLE"}, rows)
		},
	}
	return list
}

// newRunGetCmd builds `uzi run get`.
func newRunGetCmd(env Env, gf *globalFlags) *cobra.Command {
	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's status and details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			fields, _ := cmd.Flags().GetStringSlice("field")
			fields = nonEmpty(fields)
			p := env.printer(gf)
			// --field and --json are two output modes; refusing the combination up front
			// keeps each single-purpose (a scalar-per-line stream vs a JSON document).
			if len(fields) > 0 && p.Format == uzicli.FormatJSON {
				return uzicli.Exitf(uzicli.ExitUsage, "--field cannot be combined with --json")
			}
			run, err := c.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(fields) > 0 {
				return printRunFields(env, run, fields)
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return renderRunDetail(p, run)
		},
	}
	get.Flags().StringSlice("field", nil,
		"print only these top-level scalar field(s), raw and one per line (repeatable; mutually exclusive with --json)")
	return get
}

// newRunLogsCmd builds `uzi run logs`.
func newRunLogsCmd(env Env, gf *globalFlags) *cobra.Command {
	logs := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Print a run's message history (REST polling)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			follow, _ := cmd.Flags().GetBool("follow")
			after, _ := cmd.Flags().GetInt32("after")
			p := env.printer(gf)
			seq := after
			drain := func() error {
				msgs, err := c.RunLogs(cmd.Context(), args[0], seq)
				if err != nil {
					return err
				}
				for _, m := range msgs {
					if err := renderMessage(p, m); err != nil {
						return err
					}
					if m.Seq > seq {
						seq = m.Seq
					}
				}
				return nil
			}
			// parked tracks whether the LAST poll saw the run parked on a usage limit,
			// so the notice below fires on the EDGE into the park rather than every
			// 2 seconds for the hours one lasts.
			parked := false
			for {
				if err := drain(); err != nil {
					return err
				}
				if !follow {
					return nil
				}
				// Stop once the run reaches a terminal state: no further messages can
				// arrive, so an agent running `--follow` on a finished run must exit
				// (exit 0), not poll forever. Check AFTER draining this round; on a
				// terminal run drain once more (messages are persisted before the run
				// flips terminal — a gapless-seq guarantee) so nothing is dropped.
				run, err := c.GetRun(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if terminalRunStatuses[run.Status] {
					return drain()
				}
				// A park is the only status this loop rides out that produces NOTHING
				// for hours, and it is indistinguishable from a hang or a wedged agent
				// from the outside — the failure mode the milestone exists to fix.
				//
				// STDERR, not the Printer: the Printer is stdout, and `--json` streams
				// NDJSON there for an agent to parse line by line (renderMessage). A
				// human-readable notice on that stream would corrupt the contract. This
				// is the same split cobra's deprecation notice already uses here.
				if run.Status == statusLimitWait || run.Status == statusPoolWait {
					if !parked {
						parked = true
						// pool_wait is the sibling silence limit_wait is (both are long,
						// output-less holds that look like a hang from the outside), so it
						// earns the same one-shot notice — but a DIFFERENT one, because it
						// resumes on a different trigger: a pooled token, not a clock. A
						// direct limit_wait⇄pool_wait transition would be missed by the bare
						// `parked` bool, but it cannot happen — a held run is promoted to
						// `queued` (a non-held status that clears `parked` via the else-if
						// below) before it could hold again, so re-arming here is exact.
						if run.Status == statusPoolWait {
							_, _ = fmt.Fprintf(env.Stderr,
								"run %s held — its token pool is empty; still following, it resumes when a token is pooled\n",
								args[0])
						} else {
							_, _ = fmt.Fprintf(env.Stderr, "run %s %s — still following; it resumes on its own\n",
								args[0], limitWaitLine(run, time.Now()))
						}
					}
				} else if parked {
					parked = false
					// cellText, NOT sanitizeTTY, and the difference is the whole point:
					// sanitizeTTY spares "\n", so a status carrying one would inject a
					// line onto stderr. Unreachable today because runs_status_check
					// constrains status to eleven values (migration 00165) — which is precisely the argument
					// limitWaitLine's own comment REJECTS for rate_limit_type ("server-
					// controlled today" is exactly the assumption that rots). Holding one
					// line of this file to a weaker standard than the line beside it, on a
					// premise that line disowns, is not a defensible split.
					_, _ = fmt.Fprintf(env.Stderr, "run %s resumed (%s)\n", args[0], cellText(run.Status))
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(logsPollInterval):
				}
			}
		},
	}
	logs.Flags().Bool("follow", false, "keep polling for new messages")
	logs.Flags().Int32("after", 0, "only show messages after this sequence number")
	return logs
}

// newRunReviewCmd builds the hidden, deprecated `uzi run review` alias.
//
// review is now a HIDDEN, DEPRECATED alias of `uzi review show` (PRD #94
// Decision 10). It shares runReviewShow so the two stay byte-identical.
// SetOut(env.Stderr) forces cobra's deprecation notice — printed via
// OutOrStderr, which the root's SetOut(env.Stdout) would otherwise route to
// STDOUT — onto stderr, keeping --json output pure (TestRunReviewJSON*).
func newRunReviewCmd(env Env, gf *globalFlags) *cobra.Command {
	review := &cobra.Command{
		Use:        "review <run-id>",
		Short:      "Show the judge's review for a run (read-only)",
		Args:       cobra.ExactArgs(1),
		Hidden:     true,
		Deprecated: "use \"uzi review show\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runReviewShow(env, gf, c, cmd, args[0])
		},
	}
	review.SetOut(env.Stderr)
	return review
}
