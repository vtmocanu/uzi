package main

// review_render.go holds the `uzi review show`/backlog rendering helpers moved out of
// run.go (PRD #1009 M1): the judge-review detail block and its triage-line formatting.

import (
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// pendingJudgePhrase renders a pending judge's normalized state as this CLI's display
// phrase: "scheduled" (enqueued, unclaimed) or "in progress" (a worker has it).
//
// state is NOT untrusted text and deliberately skips sanitizeTTY, unlike every judge
// free-text field beside it: it is produced by the server's total mapper
// (handler.pendingJudgeState) out of a fixed vocabulary, never by the judge model. It is
// also never printed verbatim — only compared — so no server string reaches the terminal
// from here at all.
//
// The default arm is defensive, not reachable today. The mapper answers only
// "scheduled" or "running", but an older CLI against a newer server (the deployment
// order this repo actually ships in) could meet a third value, and the failure mode to
// avoid is "run r1: judge " with a blank phrase. Anything that is not "scheduled" —
// including "" — degrades to the in-progress wording, which is true of every member of
// the active-judge set by construction.
func pendingJudgePhrase(state string) string {
	if state == "scheduled" {
		return "scheduled"
	}
	return "in progress"
}

// renderReview prints the judge's review: a verdict line, an incomplete caveat
// when the judge run did not finish, the summary, and one block per
// recommendation (category, target, confidence) with its rationale beneath.
//
// pj is the ACTIVE judge run for this target (PRD #119) and is independent of rv: all
// four combinations are real states, and the point of the parameter is that two of them
// used to print the same line.
//
//   - rv nil, pj nil → "not judged": nobody has ever judged this run. UNCHANGED, and
//     the one case #119 does not touch.
//   - rv nil, pj set → a judge is already coming; saying "not judged" here told the
//     user a review was missing at the moment one was being written.
//   - rv set, pj set → a re-judge in flight over an existing verdict: the review still
//     renders in full, with a note that it is about to be replaced.
//   - rv set, pj nil → unchanged.
//
// target, rationale_md and summary_md are UNTRUSTED judge free text (Risk 13):
// repo/issue/CI content the judge LLM read can shape them, and ingest cannot
// strip instruction-shaped prose. They are printed as DATA here, never
// interpreted — the same standing the SPA gives them.
func renderReview(p *uzicli.Printer, id string, rv *apitypes.ReviewDTO, pj *apitypes.PendingJudgeDTO) error {
	if rv == nil {
		// 200 {"review": null}: visible but unjudged. Not an error (exit 0) — the
		// API deliberately does not raise one, so the CLI must not invent a 404.
		if pj != nil {
			p.Printf("run %s: judge %s\n", id, pendingJudgePhrase(pj.State))
			return nil
		}
		p.Printf("run %s: not judged\n", id)
		return nil
	}
	p.Printf("verdict: %s", rv.Verdict)
	if rv.JudgeModel != "" {
		p.Printf("    model: %s", rv.JudgeModel)
	}
	p.Println()
	if rv.Status == "failed" {
		// The judge run did not complete: the recommendation set is a fallback and
		// may be partial. Wire value is "failed" (workersvc/judge_review.go enum),
		// NOT "incomplete" (that is only badge copy) — a --json consumer keying on
		// "incomplete" would silently treat every fallback review as complete.
		p.Println("note: judge incomplete — this review is a fallback and may be partial")
	}
	if pj != nil {
		// A re-judge is in flight over the verdict just printed, so what is on screen
		// is the OLD review (the ingest upserts in place). Same "note:" shape as the
		// incomplete caveat above, and both print when both apply — they are separate
		// claims about the same review, not two spellings of one.
		p.Printf("note: judge %s — a re-judge is in flight and will replace this review\n", pendingJudgePhrase(pj.State))
	}
	if s := sanitizeTTY(strings.TrimSpace(rv.SummaryMd)); s != "" {
		p.Println()
		p.Println(s)
	}
	if len(rv.Recommendations) > 0 {
		p.Println()
		p.Println(triageLine(rv.Triage))
		p.Printf("recommendations (%d):\n", len(rv.Recommendations))
		disp := dispositionsByCoord(rv.Dispositions)
		for _, rec := range rv.Recommendations {
			// The short id (git-style, first 8 hex of the rec UUID) is what the
			// mutation verbs accept — printing it makes `uzi review resolve/dismiss`
			// usable straight from this output (Decision 10). Its disposition, if
			// any, is matched on the (category, target) coordinate.
			p.Printf("- %s [%s] %s → %s%s\n",
				shortRecID(rec.ID), rec.Confidence, rec.Category, sanitizeTTY(rec.Target),
				dispositionSuffix(disp[coordKey(rec.Category, rec.Target)]))
			if r := sanitizeTTY(strings.TrimSpace(rec.RationaleMd)); r != "" {
				for _, line := range strings.Split(r, "\n") {
					p.Printf("    %s\n", line)
				}
			}
		}
	}
	return nil
}

// coordKey is the (category, target) coordinate a disposition is keyed on — the
// same coordinate the filed-issue link uses. NUL-joined so no category/target
// pair can collide with another.
func coordKey(category, target string) string {
	return category + "\x00" + target
}

// dispositionsByCoord indexes a review's dispositions by their coordinate so each
// recommendation row can find its own verdict in one lookup (mirrors the panel's
// dispByCoord map).
func dispositionsByCoord(ds []apitypes.DispositionDTO) map[string]apitypes.DispositionDTO {
	out := make(map[string]apitypes.DispositionDTO, len(ds))
	for _, d := range ds {
		out[coordKey(d.Category, d.Target)] = d
	}
	return out
}

// dispositionSuffix renders a recommendation's disposition as a trailing chip,
// e.g. "  (done)" / "  (dismissed: not_an_issue, stale)". An empty (zero-value)
// disposition — the common "to do" case — renders nothing.
func dispositionSuffix(d apitypes.DispositionDTO) string {
	if d.Status == "" {
		return ""
	}
	label := d.Status
	if d.Reason != "" {
		label += ": " + d.Reason
	}
	if d.Stale {
		label += ", stale"
	}
	return "  (" + label + ")"
}

// triageLine is the one-line per-review tally the panel's triage bar mirrors
// (Decision 7): rendered straight from the server-computed TriageDTO so the CLI
// and web never disagree.
func triageLine(t apitypes.TriageDTO) string {
	return fmt.Sprintf("triage: %d total · %d to do · %d filed · %d done · %d dismissed (%d false positive)",
		t.Total, t.Todo, t.Filed, t.Done, t.Dismissed, t.FalsePositives)
}
