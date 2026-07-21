package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
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

	backlog := &cobra.Command{
		Use:   "backlog",
		Short: "List your judge recommendations, deduped across runs",
		Long: "List every recommendation across all your runs, deduped by (category, target)\n" +
			"so a recommendation that recurs in five runs is ONE row carrying \"seen in 5 runs\".\n" +
			"This is the read behind `uzi review resolve|dismiss --category/--target`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runReviewBacklog(env, gf, c, cmd)
		},
	}
	backlog.Flags().String("bucket", "", backlogBucketFlagUsage)

	resolve := &cobra.Command{
		Use:   "resolve <run-id> <rec-id> | --category C --target T",
		Short: "Mark a recommendation done — one in a run, or a whole backlog group",
		// Two forms, one verb (Decision 10). Args is MaximumNArgs, not ExactArgs, so the
		// group form takes none; reviewCoord does the real arity check and rejects a mix.
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			coord, group, err := reviewCoord(cmd, args)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if group {
				return runGroupDisposition(env, gf, c, cmd, coord, dispStatusDone, "")
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
	addCoordFlags(resolve)

	dismiss := &cobra.Command{
		Use:   "dismiss <run-id> <rec-id> | --category C --target T --reason wont-do|not-an-issue",
		Short: "Dismiss a recommendation with a reason — one in a run, or a whole backlog group",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the invocation BEFORE any network call: a bad reason or a mixed
			// form is a usage error (exit 2), and the CLI must not resolve the rec or
			// write anything when the invocation itself is wrong.
			reason, err := mapDismissReason(cmd)
			if err != nil {
				return err
			}
			coord, group, err := reviewCoord(cmd, args)
			if err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if group {
				return runGroupDisposition(env, gf, c, cmd, coord, dispStatusDismissed, reason)
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
	addCoordFlags(dismiss)

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

	cmd.AddCommand(show, backlog, resolve, dismiss, undo, stats)
	return cmd
}

// backlogBucketFlagUsage is the --bucket help text. It is the ONE place the CLI writes the
// bucket set down, and it is documentation, not enforcement: the flag's value is forwarded
// verbatim to the server, which owns the validator, so a stale line here misinforms but
// cannot misbehave (an unknown bucket is the server's 400 → exit 2, never a silently empty
// list). TestBacklogBucketUsageMatchesServerEnum pins it to workersvc's set so it cannot go
// stale unnoticed — a help string nothing asserts is a help string anyone can let rot.
const backlogBucketFlagUsage = "filter by triage bucket: todo (default) | filed | done | dismissed | all"

// addCoordFlags adds the group-form flags shared by `resolve` and `dismiss`. The pair is
// the (category, target) coordinate the Judge backlog groups by — the same grain the web
// menu's per-group action uses, and deliberately not a recommendation id, since one group
// spans many runs and therefore many ids.
func addCoordFlags(cmd *cobra.Command) {
	cmd.Flags().String("category", "", "group form: the recommendation category, as `uzi review backlog` prints it")
	cmd.Flags().String("target", "", "group form: the recommendation target, as `uzi review backlog` prints it")
}

// reviewCoord classifies a resolve/dismiss invocation as the per-run form (two positional
// ids) or the group form (--category + --target), rejecting anything else as a usage error
// (exit 2) BEFORE a client is built or a byte crosses the wire.
//
// Every rejection below is a case that would otherwise do something plausible and wrong:
//
//   - MIXING the forms is refused rather than silently preferring one. Both are writes with
//     different blast radii — one coordinate in one run versus every occurrence of it across
//     every run — so guessing which the user meant is not a recoverable mistake.
//   - a HALF-SPECIFIED coordinate (one flag without the other) is refused rather than sent
//     with an empty half. An empty target is a legal string, not a wildcard: the server
//     would match coordinates whose target is literally "", find none, and answer 200
//     updated=0 — indistinguishable from "already settled". This is the AK shape heuristic
//     in the client: the omission would be consumed by a match, and a match fails silently.
func reviewCoord(cmd *cobra.Command, args []string) (apitypes.JudgeDispositionCoordDTO, bool, error) {
	category, _ := cmd.Flags().GetString("category")
	target, _ := cmd.Flags().GetString("target")
	category = strings.TrimSpace(category)
	target = strings.TrimSpace(target)
	group := category != "" || target != ""

	switch {
	case group && len(args) > 0:
		return apitypes.JudgeDispositionCoordDTO{}, false, uzicli.Exitf(uzicli.ExitUsage,
			"use either the per-run form (<run-id> <rec-id>) or the group form (--category/--target), not both")
	case group && (category == "" || target == ""):
		return apitypes.JudgeDispositionCoordDTO{}, false, uzicli.Exitf(uzicli.ExitUsage,
			"the group form needs BOTH --category and --target — half a coordinate matches nothing and would report success")
	case group:
		return apitypes.JudgeDispositionCoordDTO{Category: category, Target: target}, true, nil
	case len(args) == 2:
		return apitypes.JudgeDispositionCoordDTO{}, false, nil
	default:
		return apitypes.JudgeDispositionCoordDTO{}, false, uzicli.Exitf(uzicli.ExitUsage,
			"want either <run-id> <rec-id> or --category C --target T")
	}
}

// runReviewBacklog fetches and renders the deduped backlog. --bucket is forwarded verbatim
// (empty omits the parameter, so the server's default applies); --json passes the server's
// envelope through unchanged, so an agent sees `truncated` and the canonical `triage`
// alongside the groups.
func runReviewBacklog(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command) error {
	bucket, _ := cmd.Flags().GetString("bucket")
	b, err := c.JudgeBacklog(cmd.Context(), strings.TrimSpace(bucket))
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(b)
	}
	return renderBacklog(p, b)
}

// renderBacklog is the human view: the canonical triage line, then one block per group.
//
// The tally comes from the response's Triage, which the server sources from the /me/judge/stats
// query rather than tallying off these rows — so it stays correct under both the bucket
// filter and truncation, and equals the web nav badge and `uzi review stats` to the digit.
// Do NOT recompute it from the groups on screen.
func renderBacklog(p *uzicli.Printer, b apitypes.JudgeBacklogDTO) error {
	p.Println(triageLine(b.Triage))
	if b.Truncated {
		// Not a cosmetic warning. The cap bounds ROWS and applies BEFORE grouping, so a
		// group that survives can have LOST occurrences: its "seen in N runs" and open
		// count may be understated and its bucket rollup wrong, and a group whose only
		// open occurrence fell outside the cut is missing from a todo view entirely. A
		// MISSING group is therefore UNKNOWN, not settled.
		p.Println("warning: backlog truncated at the server's row cap — counts below may be understated and groups may be missing; a missing group is UNKNOWN, not settled")
	}
	if len(b.Groups) == 0 {
		p.Println("no recommendations in this bucket")
		return nil
	}
	p.Printf("groups (%d):\n", len(b.Groups))
	for _, g := range b.Groups {
		// Target and the rationale preview are attacker-influencable free text rendered
		// into a terminal, so both go through sanitizeTTY — the same treatment
		// renderReview gives the per-run panel. rationale_preview in particular ships
		// deliberately UNESCAPED (the no-raw-render guarantee is the client's job, and for
		// a terminal client that job is stripping the control bytes, not escaping HTML).
		p.Printf("- [%s] %s → %s · seen in %s · %d open\n",
			g.Bucket, g.Category, sanitizeTTY(g.Target), runsPhrase(g.RunCount), g.OpenCount)
		if r := sanitizeTTY(strings.TrimSpace(g.RationalePreview)); r != "" {
			for _, line := range strings.Split(r, "\n") {
				p.Printf("    %s\n", line)
			}
		}
	}
	return nil
}

// runsPhrase renders the "seen in N runs" evidence chip, singular at 1.
func runsPhrase(n int) string {
	if n == 1 {
		return "1 run"
	}
	return strconv.Itoa(n) + " runs"
}

// runGroupDisposition drives the bulk fan-out for one group coordinate and reports what the
// server actually did.
func runGroupDisposition(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, coord apitypes.JudgeDispositionCoordDTO, status, reason string) error {
	res, err := c.BulkSetDispositions(cmd.Context(), []apitypes.JudgeDispositionCoordDTO{coord}, status, reason)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res)
	}
	if gf.quiet {
		return nil
	}
	// "coordinates", never "recommendations". Updated counts (review_id, category, target)
	// TRIPLES, and one review can carry the same coordinate on two recommendations, which
	// share ONE disposition row and contribute ONE to this count. So Updated can be lower
	// than the number of recommendations the group visibly spans, and reporting it as a
	// recommendation count would be a number the user could disprove from `backlog`.
	p.Printf("%s → %s: %s, %d member coordinate(s) updated\n",
		coord.Category, sanitizeTTY(coord.Target), dispositionOutcome(status, reason), res.Updated)
	if res.Updated == 0 {
		// A 200 with nothing written. There is no 404 on this route by design — a
		// coordinate that does not exist and one belonging to another user are the same
		// answer (#94 Decision 5's no-existence-oracle rule) — so the CLI must not let a
		// silent no-op read as success. It also must not claim WHY: "already settled" and
		// "no such coordinate" are exactly what the server refuses to distinguish.
		p.Println("nothing was written: no open member of yours matched that coordinate (it may already be settled, or the category/target may be misspelt)")
	}
	if res.Truncated {
		p.Println("warning: the post-write re-read hit the server's row cap — a group missing from --json output is UNKNOWN, not settled")
	}
	return nil
}

// dispositionOutcome is the human label for a triage verdict, shared by the group verbs so
// "done"/"dismissed (wont_do)" reads identically to the per-run reportDisposition line.
func dispositionOutcome(status, reason string) string {
	if reason != "" {
		return status + " (" + reason + ")"
	}
	return status
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
