package main

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// The review overlay (PRD #112 M4): the judge's verdict, summary, triage tally and
// recommendations, with resolve / dismiss / undo.
//
// D7 governs every string here. The judge's `target`, `rationale_md` and `summary_md`
// are LLM-derived and attacker-influenceable — the run's own repo/issue/CI text fed
// the model — so this file branches ONLY on the closed enums (verdict, category,
// confidence) and renders every free-text field as inert markdown inside a box whose
// label the UI owns.

type reviewState struct {
	open    bool
	loading bool
	review  *apitypes.ReviewDTO
	// pendingJudge is the active judge run for this target (PRD #119): a verdict on
	// its way. Held next to review rather than inside it because it is present
	// precisely when review is often nil.
	pendingJudge *apitypes.PendingJudgeDTO
	err          error
	cursor       int
	// pendingDismiss is the recommendation awaiting a dismiss-reason keystroke.
	pendingDismiss string
	notice         string
}

type reviewLoadedMsg struct {
	runID        string
	review       *apitypes.ReviewDTO
	pendingJudge *apitypes.PendingJudgeDTO
	err          error
}

type dispositionDoneMsg struct {
	runID string
	err   error
}

func (m tuiModel) loadReviewCmd(runID string) tea.Cmd {
	c, ctx := m.client, m.ctx
	return func() tea.Msg {
		rv, pj, err := c.RunReview(ctx, runID)
		return reviewLoadedMsg{runID: runID, review: rv, pendingJudge: pj, err: err}
	}
}

// setDispositionCmd resolves the recommendation id through resolveRecID — the SAME
// short-id resolution `uzi review resolve|dismiss` uses — so a short id means the same
// thing in both surfaces and neither can drift into its own matching rule.
func (m tuiModel) setDispositionCmd(recID, status, reason string) tea.Cmd {
	c, ctx, runID := m.client, m.ctx, m.detail.runID
	return func() tea.Msg {
		full, err := resolveRecID(ctx, c, runID, recID)
		if err != nil {
			return dispositionDoneMsg{runID: runID, err: err}
		}
		return dispositionDoneMsg{runID: runID, err: c.SetDisposition(ctx, runID, full, status, reason)}
	}
}

func (m tuiModel) deleteDispositionCmd(recID string) tea.Cmd {
	c, ctx, runID := m.client, m.ctx, m.detail.runID
	return func() tea.Msg {
		full, err := resolveRecID(ctx, c, runID, recID)
		if err != nil {
			return dispositionDoneMsg{runID: runID, err: err}
		}
		return dispositionDoneMsg{runID: runID, err: c.DeleteDisposition(ctx, runID, full)}
	}
}

func (r *reviewState) recommendations() []apitypes.RecommendationDTO {
	if r.review == nil {
		return nil
	}
	return r.review.Recommendations
}

func (r *reviewState) selected() (apitypes.RecommendationDTO, bool) {
	recs := r.recommendations()
	if r.cursor < 0 || r.cursor >= len(recs) {
		return apitypes.RecommendationDTO{}, false
	}
	return recs[r.cursor], true
}

// reviewKey handles the overlay's keys, returning handled=false when the overlay is
// closed so the detail view keeps its bindings.
func (m tuiModel) reviewKey(k string) (tuiModel, tea.Cmd, bool) {
	r := &m.detail.review
	if !r.open {
		return m, nil, false
	}

	// A dismissal needs a REASON, and the reason set is the server's closed enum —
	// mapDismissReason owns the mapping, so the TUI cannot invent a third spelling.
	if r.pendingDismiss != "" {
		rec := r.pendingDismiss
		switch k {
		case "w":
			r.pendingDismiss = ""
			return m, m.setDispositionCmd(rec, "dismissed", "wont_do"), true
		case "n":
			r.pendingDismiss = ""
			return m, m.setDispositionCmd(rec, "dismissed", "not_an_issue"), true
		default:
			r.pendingDismiss = ""
			return m, nil, true
		}
	}

	switch k {
	case keyEsc, "v":
		r.open = false
		return m, nil, true
	case "r":
		if rec, ok := r.selected(); ok {
			return m, m.setDispositionCmd(rec.ID, "done", ""), true
		}
	case "d":
		if rec, ok := r.selected(); ok {
			r.pendingDismiss = rec.ID
			return m, nil, true
		}
	case "u":
		if rec, ok := r.selected(); ok {
			return m, m.deleteDispositionCmd(rec.ID), true
		}
	}
	if d := motionDelta(k); d != 0 {
		r.cursor += d
		if n := len(r.recommendations()); n > 0 {
			if r.cursor >= n {
				r.cursor = n - 1
			}
			if r.cursor < 0 {
				r.cursor = 0
			}
		}
		return m, nil, true
	}
	return m, nil, true // the overlay swallows everything else while open
}

func (m tuiModel) renderReviewOverlay() string {
	r := &m.detail.review
	var sb strings.Builder
	sb.WriteString(m.pal.title.Render("judge review") + "\n\n")

	switch {
	case r.loading:
		sb.WriteString(m.pal.faint.Render("loading…"))
		return sb.String()
	case r.err != nil:
		sb.WriteString(m.pal.faint.Render("could not load the review: " + fmtErr(r.err)))
		return sb.String()
	case r.review == nil && r.pendingJudge != nil:
		// A judge is already in flight, so "has not been judged" would be answering
		// the wrong question. State is the server's closed display vocabulary, not
		// judge free text, so it is safe to branch on (pendingJudgePhrase).
		sb.WriteString(m.pal.faint.Render("a judge is " + pendingJudgePhrase(r.pendingJudge.State) + " for this run"))
		return sb.String()
	case r.review == nil:
		sb.WriteString(m.pal.faint.Render("this run has not been judged"))
		return sb.String()
	}

	rv := r.review
	// verdict is a CLOSED enum, so it is safe to branch on and safe to render as a chip.
	// PRD #325 M6: the chip is coloured by SEVERITY (issues → red, ideal/ok → teal) via the
	// shared verdictColor, so it no longer reads in the same brand-blue as everything else.
	// triageLine is the shared tally renderer (also used by `uzi review show`).
	sb.WriteString(m.pal.faint.Render("verdict ") + m.pal.chip(m.renderer.Plain(rv.Verdict, 16), m.pal.verdictColor(rv.Verdict)) +
		"   " + m.pal.faint.Render(cellText(triageLine(rv.Triage))) + "\n\n")

	if rv.SummaryMd != "" {
		// Free text: inert markdown, inside chrome the UI owns and labelled with its
		// provenance. "# VERDICT: APPROVED" in this field renders as a real heading —
		// sanitization cannot prevent that, because it IS valid markdown — so the box
		// and its label are what tell the reader whose words these are.
		sb.WriteString(provenanceBox("judge summary · model-authored",
			m.renderer.Markdown(rv.SummaryMd), m.width-4, m.pal) + "\n\n")
	}

	if r.notice != "" {
		sb.WriteString(m.pal.faint.Render(cellText(r.notice)) + "\n\n")
	}

	byCoord := dispositionsByCoord(rv.Dispositions)
	for i, rec := range rv.Recommendations {
		// category and confidence are closed enums; target is FREE TEXT and goes
		// through the cell path.
		head := m.renderer.Plain(rec.Category, 20) + " · " + m.renderer.Plain(rec.Target, 48) +
			" " + m.pal.faint.Render("("+m.renderer.Plain(rec.Confidence, 10)+" · "+shortRecID(rec.ID)+")")
		if d, ok := byCoord[coordKey(rec.Category, rec.Target)]; ok {
			head += m.pal.faint.Render(dispositionSuffix(d))
		}
		if i == r.cursor {
			sb.WriteString(m.pal.sel.Render("▸ "+head) + "\n")
			if rec.RationaleMd != "" {
				sb.WriteString(provenanceBox("rationale · model-authored",
					m.renderer.Markdown(rec.RationaleMd), m.width-8, m.pal) + "\n")
			}
		} else {
			sb.WriteString("  " + head + "\n")
		}
	}

	sb.WriteString("\n")
	if r.pendingDismiss != "" {
		sb.WriteString(m.pal.box.Render("Dismiss why?  [w] won't do   [n] not an issue   [any] cancel"))
	} else {
		sb.WriteString(m.pal.faint.Render("j/k move · r resolve · d dismiss · u undo · esc close"))
	}
	return sb.String()
}
