package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// Disposition wire strings (PRD #94 Decision 1/4), named so a typo is a compile
// error rather than a silent 400. status ∈ {done, dismissed}; reason ∈ {wont_do,
// not_an_issue} and rides only a dismissal.
const (
	dispStatusDone       = "done"
	dispStatusDismissed  = "dismissed"
	dispReasonWontDo     = "wont_do"
	dispReasonNotAnIssue = "not_an_issue"
)

// newReviewCmd builds `uzi review` and its verbs (PRD #94 Decision 10): a
// top-level group that absorbs the judge read (`show`, formerly `uzi run review`)
// and adds the triage mutations (`resolve`, `dismiss`, `undo`) plus the global
// `stats`. The mutation verbs accept the short rec id `show` prints and resolve it
// against the run's CURRENT review before hitting the wire.
func newReviewCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Read and triage the judge's recommendations",
	}

	show := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show the judge's review, recommendations, and triage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runReviewShow(env, gf, c, cmd, args[0])
		},
	}

	resolve := &cobra.Command{
		Use:   "resolve <run-id> <rec-id>",
		Short: "Mark a recommendation done",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			recID, err := resolveRecID(cmd.Context(), c, args[0], args[1])
			if err != nil {
				return err
			}
			if err := c.SetDisposition(cmd.Context(), args[0], recID, dispStatusDone, ""); err != nil {
				return err
			}
			return reportDisposition(env, gf, args[0], recID, "resolved (done)")
		},
	}

	dismiss := &cobra.Command{
		Use:   "dismiss <run-id> <rec-id> --reason wont-do|not-an-issue",
		Short: "Dismiss a recommendation with a reason",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate --reason BEFORE any network call: a bad reason is a usage
			// error (exit 2), and the CLI must not resolve the rec or write anything
			// when the invocation itself is wrong.
			reason, err := mapDismissReason(cmd)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			recID, err := resolveRecID(cmd.Context(), c, args[0], args[1])
			if err != nil {
				return err
			}
			if err := c.SetDisposition(cmd.Context(), args[0], recID, dispStatusDismissed, reason); err != nil {
				return err
			}
			return reportDisposition(env, gf, args[0], recID, "dismissed ("+reason+")")
		},
	}
	dismiss.Flags().String("reason", "", "why it is dismissed: wont-do (valid, not worth it) | not-an-issue (false positive)")

	undo := &cobra.Command{
		Use:   "undo <run-id> <rec-id>",
		Short: "Remove a recommendation's disposition",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			recID, err := resolveRecID(cmd.Context(), c, args[0], args[1])
			if err != nil {
				return err
			}
			err = c.DeleteDisposition(cmd.Context(), args[0], recID)
			if errors.Is(err, uzicli.ErrNoDisposition) {
				// Nothing to undo — treat as already-undone (Decision 6): friendly
				// line, exit 0, never a crash.
				return reportNoDisposition(env, gf, args[0], recID)
			}
			if err != nil {
				return err
			}
			return reportDisposition(env, gf, args[0], recID, "disposition removed")
		},
	}

	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show your judge triage totals across all runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			t, err := c.JudgeStats(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(t)
			}
			rows := [][]string{
				{"TOTAL", strconv.Itoa(t.Total)},
				{"TO DO", strconv.Itoa(t.Todo)},
				{"FILED", strconv.Itoa(t.Filed)},
				{"DONE", strconv.Itoa(t.Done)},
				{"DISMISSED", strconv.Itoa(t.Dismissed)},
				{"FALSE POSITIVES", strconv.Itoa(t.FalsePositives)},
			}
			return p.Table(nil, rows)
		},
	}

	cmd.AddCommand(show, resolve, dismiss, undo, stats)
	return cmd
}

// runReviewShow is the shared body of `uzi review show` and the deprecated
// `uzi run review` alias: fetch the run's review and render it (verdict +
// recommendations + triage), or pass the {"review": …} envelope through with
// --json. A nil review is a visible-but-unjudged run (exit 0), never a 404.
func runReviewShow(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	rv, err := c.RunReview(cmd.Context(), id)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// Pass the envelope through: {"review": <dto>|null}, carrying the FULL rec
		// ids (the short id is a human-render affordance only).
		return p.JSON(map[string]any{"review": rv})
	}
	return renderReview(p, id, rv)
}

// mapDismissReason maps the user-facing --reason flag (hyphenated) to its wire
// enum (underscored). A missing or unrecognised value is a usage error (exit 2),
// raised before any network call so a bad invocation writes nothing.
func mapDismissReason(cmd *cobra.Command) (string, error) {
	flag, _ := cmd.Flags().GetString("reason")
	switch strings.TrimSpace(flag) {
	case "wont-do":
		return dispReasonWontDo, nil
	case "not-an-issue":
		return dispReasonNotAnIssue, nil
	case "":
		return "", uzicli.Exitf(uzicli.ExitUsage, "dismiss requires --reason wont-do|not-an-issue")
	default:
		return "", uzicli.Exitf(uzicli.ExitUsage, "invalid --reason %q: want wont-do or not-an-issue", flag)
	}
}

// resolveRecID resolves a user-supplied rec id — the git-style short id from
// `uzi review show`, or a full UUID — to the FULL recommendation id in the run's
// CURRENT review (PRD #94 Decision 10). It re-fetches the review so the id maps
// against exactly what the user last saw; matching is case-insensitive.
//
//   - an exact full-id match wins outright;
//   - a single unambiguous prefix match resolves;
//   - an ambiguous prefix is a usage error ("use a longer id");
//   - no match is a not-found with a refresh hint (the review may have changed
//     under a re-judge).
func resolveRecID(ctx context.Context, c uzicli.Client, runID, want string) (string, error) {
	rv, err := c.RunReview(ctx, runID)
	if err != nil {
		return "", err
	}
	if rv == nil {
		return "", uzicli.Exitf(uzicli.ExitNotFound, "run %s is not judged — nothing to triage", runID)
	}
	want = strings.ToLower(strings.TrimSpace(want))
	var matches []string
	for _, rec := range rv.Recommendations {
		full := strings.ToLower(rec.ID)
		if full == want {
			return rec.ID, nil // an exact full-id match is unambiguous by definition
		}
		if want != "" && strings.HasPrefix(full, want) {
			matches = append(matches, rec.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", uzicli.Exitf(uzicli.ExitNotFound,
			"no recommendation matches %q in run %s — refresh with `uzi review show %s`; the review may have changed", want, runID, runID)
	default:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"ambiguous recommendation id %q matches %d recommendations — use a longer id", want, len(matches))
	}
}

// reportDisposition prints the outcome of a triage mutation. --json emits the
// FULL rec id (the agent contract); the human line uses the short id it shows.
func reportDisposition(env Env, gf *globalFlags, runID, recID, outcome string) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"run": runID, "recommendation": recID, "status": outcome})
	}
	if !gf.quiet {
		p.Printf("%s: %s\n", shortRecID(recID), outcome)
	}
	return nil
}

// reportNoDisposition is the friendly already-undone report for `uzi review undo`
// when no disposition existed (Decision 6): exit 0, not a not-found failure.
func reportNoDisposition(env Env, gf *globalFlags, runID, recID string) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(map[string]any{"run": runID, "recommendation": recID, "status": "no disposition to undo", "undone": false})
	}
	if !gf.quiet {
		p.Printf("%s: no disposition to undo\n", shortRecID(recID))
	}
	return nil
}
