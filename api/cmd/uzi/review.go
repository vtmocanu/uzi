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
	// --run is the ONLY parameter that can narrow a pull below the server's row cap, because
	// the anchor is a coordinate semi-join pushed into SQL BEFORE the LIMIT while --bucket
	// filters the already-cut rows in Go. That makes it the only answer to a `truncated`
	// backlog, which is why it exists here despite Decision 10's flag list not naming it —
	// the list described the surface being specified, and #98's own Decision 1 has carried
	// ?run= since M1. A well-formed but unknown/foreign run id is an EMPTY list, never a 404
	// (no existence oracle), so a typo cannot be told from a run with nothing in it.
	backlog.Flags().String("run", "", "narrow to the coordinates that also occur in this run id — the one filter applied BEFORE the server's row cap")

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
	runAnchor, _ := cmd.Flags().GetString("run")
	b, err := c.JudgeBacklog(cmd.Context(), strings.TrimSpace(bucket), strings.TrimSpace(runAnchor))
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
		p.Println("         narrow with `--run <run-id>`: the anchor is applied BEFORE the cap, so it is the only filter that changes what gets cut (--bucket filters after)")
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
		//
		// g.Category is printed RAW, and that is a considered exception rather than an
		// oversight: category is CHECK-constrained to six literals on review_recommendations
		// (00059_run_reviews.sql), which is the table the grouped read selects it from, so it
		// cannot carry a control byte. (00071/00073 drop that CHECK on their coordinate
		// copies — deliberately, see their comments — but neither is the source here.)
		// Bucket is likewise a closed server-side enum. If a future read ever sourced
		// category from a table without the CHECK, this line would need sanitising too.
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
	// --quiet suppresses the SUCCESS line and nothing else, matching reportDisposition on
	// the per-run path. It used to return here, which silently swallowed both reports below
	// — so `uzi review dismiss --quiet --category X --target <typo>` produced empty stdout,
	// empty stderr and exit 0: exactly the silent-success shape the zero-updated report
	// exists to prevent, reachable through a documented global flag.
	//
	// The flag is documented as "suppress non-essential output". A warning that the data may
	// be wrong, and the only signal that nothing happened, are not non-essential — they are
	// the two things a script CANNOT recover any other way, since neither has a distinct
	// exit code.
	if !gf.quiet {
		// "coordinates", never "recommendations". Updated counts (review_id, category,
		// target) TRIPLES, and one review can carry the same coordinate on two
		// recommendations, which share ONE disposition row and contribute ONE to this
		// count. So Updated can be lower than the number of recommendations the group
		// visibly spans, and reporting it as a recommendation count would be a number the
		// user could disprove from `backlog`.
		p.Printf("%s → %s: %s, %d member coordinate(s) updated\n",
			coord.Category, sanitizeTTY(coord.Target), dispositionOutcome(status, reason), res.Updated)
	}
	if res.Updated == 0 {
		// A 200 with nothing written, which must never read as success.
		//
		// BE PRECISE ABOUT WHICH PAIR IS INDISTINGUISHABLE, because the earlier comment here
		// named the wrong one and that error cost the user information. Measured against a
		// live database, three causes:
		//
		//	A already settled    updated=0  groups=1
		//	B misspelt           updated=0  groups=0
		//	C another user's     updated=0  groups=0
		//
		// The server DOES separate A from the rest — the re-read returns the caller's own
		// group. What #94 Decision 5 refuses to distinguish is B from C, which are
		// byte-identical, and that pair is what the "of yours" wording below carries.
		//
		// So reporting A definitely leaks nothing: it is the caller's own data, and the
		// server already hands it to `--json` in res.Groups today. Withholding it protected
		// nothing and cost a user the one cause that is actionable.
		//
		// The !res.Truncated gate is load-bearing, not caution: past the row cap a settled
		// coordinate can fall outside the read window and return NO group, so `groups` being
		// empty would otherwise be read as "not settled" when the DTO's own contract says a
		// missing group is UNKNOWN.
		switch {
		case len(res.Groups) > 0 && !res.Truncated:
			p.Println("nothing was written: that coordinate is already settled")
		default:
			p.Println("nothing was written: no open member of yours matched — it may be misspelt, it may belong to another user, or it may be settled but outside a truncated read window")
		}
	}
	if res.Truncated {
		// BOTH halves of the contract, and a remedy that WORKS.
		//
		// This used to end "re-check with `uzi review backlog --bucket all`", which is the
		// second false printed instruction found on this path. Measured: every bucket
		// truncates identically, because `truncated` is computed and the rows sliced BEFORE
		// filterGroups runs — so no bucket value can reach what the cap cut. It handed the
		// user the one knob that provably cannot help.
		//
		// --run can: the anchor is a coordinate semi-join pushed into SQL before the LIMIT.
		// And --json ON THE ORIGINAL CALL is the authoritative record of what this write
		// actually did, which no later read reconstructs. Note the re-read itself is
		// unanchored by construction (BulkSetDispositions passes uuid.Nil), so --run narrows
		// a FOLLOW-UP look, it does not retroactively widen this response.
		//
		// THE REMEDY IS PRINTED AS RUNNABLE LINES, ONE PER RUN THIS CALL SETTLED — it used
		// to be a single line carrying the LITERAL text `uzi review backlog --run <run-id>`,
		// with `<run-id>` a placeholder in the emitted output rather than a value
		// substituted at emit time. That is not an instruction a user (or a test) can run
		// verbatim: it is a template, and the difference is exactly what the printed-
		// instruction backstop exists to catch. The e2e helper's character allowlist
		// rejects it on the `<`, which is the mechanical statement of the same thing.
		//
		// Naming every settled run also makes the advice strictly better: a write that
		// spanned three runs used to tell the user to guess one of them. Mirrors the
		// `uzi review undo %s %s` emission below, which already substitutes.
		p.Println("warning: the post-write re-read hit the server's row cap — counts may be understated, and a coordinate it did not return is UNKNOWN, not settled")
		if runs := distinctSettledRuns(res.Settled); len(runs) > 0 {
			p.Println("         --json on THIS call is the only complete record of what was written; --bucket cannot narrow a re-check (it filters after the cut), but --run can — one per run this call settled:")
			for _, id := range runs {
				p.Printf("         uzi review backlog --run %s\n", id)
			}
			// AN EMPTY RESULT IS THE ANSWER, AND SAYING SO IS NOT REASSURANCE — IT CLOSES AN
			// INFERENCE THE USER WAS BEING LEFT TO MAKE UNAIDED.
			//
			// The warning above frames the risk as "a coordinate it did not return is UNKNOWN,
			// not settled" — i.e. DID MY WRITE LAND. The remedy answers a different question:
			// what is still un-triaged on that run. They connect only through a step the reader
			// has to take alone (todo empty ⟹ nothing un-settled ⟹ the dismiss covered
			// everything). Sound, and never stated. That is the same class as the two false
			// printed instructions this file exists because of, one notch milder: advice that
			// parses, runs, and answers a question adjacent to the one just posed.
			//
			// WHAT MAKES AN EMPTY TRUSTWORTHY is a property of renderBacklog, not of this
			// advice: it prints the truncation warning BEFORE the empty line, unconditionally
			// on b.Truncated. So "no recommendations in this bucket" with NO warning above it
			// is a COMPLETE empty rather than a possibly-cut one. Give the reason, not just the
			// assertion — a reader who has it does not need to trust this sentence.
			//
			// Deliberately no --bucket here, not even --bucket all. The line directly above
			// says --bucket cannot narrow a re-check; naming a --bucket flag one line later
			// invites the next reader to re-derive that it narrows something. `--bucket all`
			// was the SECOND false printed instruction this path already paid to remove.
			p.Println("         (an empty result from one of these is the answer, not a dead end: nothing on that run is")
			p.Println("          still un-triaged. A cut re-read prints its own truncation warning above the listing, so")
			p.Println("          an empty result with no warning is complete.)")
		} else {
			// Nothing was settled, so there is no run to anchor on. Deliberately prints no
			// command at all rather than a placeholder: an unrunnable line here is the
			// defect this branch was split out to avoid.
			p.Println("         --json on THIS call is the only complete record of what was written. Nothing was settled, so there is no run to anchor a re-check on (--bucket cannot help either: it filters after the cut)")
		}
	}
	// `settled` names the exact (run, rec) coordinates this call wrote — the ONLY correct
	// basis for a revert. Be precise about what is and is not recoverable, because the loose
	// version of this sentence is what let a false claim sit here for two passes:
	//
	//   * the PAIRS are recoverable — JudgeOccurrenceDTO carries run_id and rec_id, so
	//     `uzi review backlog --bucket all --json` lists every one of them;
	//   * WHICH SUBSET THIS CALL SETTLED is not, ever, after the fact;
	//   * and only the second supports a correct revert. Undoing the visible ones is
	//     precisely the destructive delete BLK-UNDO removed — with scope=open the server
	//     decides membership at WRITE time, so the visible set includes members this action
	//     never touched, and clearing them destroys an M6 auto-done irreversibly.
	//
	// The pairs are PRINTED, as runnable commands. This line used to say "re-run with --json
	// for the N pairs", and that was wrong twice over:
	//
	//  1. It recommended a WRITE as a way to read. A re-run re-executes
	//     PUT /disposition — a fan-out across every member coordinate. That it is a no-op
	//     today is a coincidence of current state, not a property of the command: scope=open
	//     finds nothing left open only because the first call settled it. If the M6 sync
	//     re-opened a coordinate in between, the re-run would settle members the first call
	//     never touched, with nothing recording that it had.
	//  2. It did not work anyway. Measured end to end against a real database:
	//
	//	FIRST  call: updated=2 settled=2
	//	SECOND call: updated=0 settled=0
	//
	// So the honest form is PROSPECTIVE, and printing the pairs is the strongest version of
	// it: the caller needs no second invocation at all, and the line points at neither a
	// write nor `uzi review backlog` (whose pairs are the wrong ones — see above).
	//
	// Everything is listed, never a capped sample: an omitted pair is a member the user
	// cannot revert, so truncating would recreate the same unrecoverability in smaller print.
	//
	// --quiet still suppresses it, and the reasoning is NOT the one the old comment gave.
	// These are not "obtainable another way" — nothing obtains them afterwards. They are
	// suppressed because --quiet is a scripting flag and a caller who wants the pairs
	// controls the invocation: --json carries `settled` in full ON THIS CALL. That differs
	// from the zero-updated and truncation reports, which survive --quiet because they say
	// something did not happen or may be wrong and NO exit code carries either.
	if n := len(res.Settled); n > 0 && !gf.quiet && p.Format != uzicli.FormatJSON {
		p.Printf("to revert (%d member coordinate(s)):\n", n)
		for _, m := range res.Settled {
			p.Printf("  uzi review undo %s %s\n", m.RunID, m.RecID)
		}
	}
	return nil
}

// distinctSettledRuns returns the run ids a bulk disposition wrote to, first-appearance
// order, without duplicates.
//
// Deduping is not tidiness: Settled carries one member per settled COORDINATE, and a single
// run commonly carries several, so a naive loop would print the same remedy line two or
// three times. The e2e row asserts the printed COUNT before executing anything, so a
// duplicate would either break that assertion or — worse, if the count were relaxed to
// match — make it satisfiable by output nobody checked.
//
// Order is first-appearance rather than sorted so the lines follow the order the server
// reported, which is the same order the `to revert` block below prints.
func distinctSettledRuns(settled []apitypes.JudgeSettledMemberDTO) []string {
	seen := make(map[string]bool, len(settled))
	out := make([]string, 0, len(settled))
	for _, m := range settled {
		if m.RunID == "" || seen[m.RunID] {
			continue
		}
		seen[m.RunID] = true
		out = append(out, m.RunID)
	}
	return out
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
// recommendations + triage), or emit the {"review": …, "pending_judge": …} envelope
// with --json. A nil review is a visible-but-unjudged run (exit 0), never a 404 —
// and since PRD #119 it is two states, told apart by pending_judge.
func runReviewShow(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, id string) error {
	rv, pj, err := c.RunReview(cmd.Context(), id)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// This map IS the --json wire envelope. It is RE-MINTED here, not proxied:
		// the client decodes the server body into typed DTOs and this line
		// re-serializes them, so the response shape --json consumers see is the set
		// of keys written on THIS line and nowhere else. A key the server adds and
		// this map omits is invisible to every --json consumer even after the DTO
		// gains the field — which is exactly what happened to pending_judge until it
		// was added here. Carries the FULL rec ids (the short id is a human-render
		// affordance only).
		return p.JSON(map[string]any{"review": rv, "pending_judge": pj})
	}
	return renderReview(p, id, rv, pj)
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
	// The pending judge is irrelevant to id RESOLUTION — a rec id resolves against the
	// review that exists now, and one being written concurrently has no ids yet.
	rv, _, err := c.RunReview(ctx, runID)
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
