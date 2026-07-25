package main

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func reviewFixture() *apitypes.ReviewDTO {
	return &apitypes.ReviewDTO{
		ID: "rv1", TargetRunID: "r-own", Verdict: "issues", Status: "complete",
		SummaryMd: "The run **mostly** worked.",
		Recommendations: []apitypes.RecommendationDTO{
			{ID: "aaaaaaaa-1111-2222-3333-444444444444", Category: "improve_uzi",
				Target: "api/internal/hub", RationaleMd: "the drop path is under-documented", Confidence: "high"},
			{ID: "bbbbbbbb-1111-2222-3333-444444444444", Category: "fix_bug",
				Target: "api/cmd/uzi", RationaleMd: "an off-by-one", Confidence: "medium"},
		},
	}
}

func reviewModel(t *testing.T, c uzicli.Client) tuiModel {
	t.Helper()
	m := ownerModel(t, c, "r-own", ownedRun("r-own"))
	next, _ := m.Update(reviewLoadedMsg{runID: "r-own", review: reviewFixture()})
	m = next.(tuiModel)
	m.detail.review.open = true
	return m
}

func TestReviewOverlayOpensAndRenders(t *testing.T) {
	fake := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}}
	m := ownerModel(t, fake, "r-own", ownedRun("r-own"))

	nm, cmd := m.handleKey("v")
	m = nm.(tuiModel)
	if !m.detail.review.open {
		t.Fatal("v did not open the review overlay")
	}
	if cmd == nil {
		t.Fatal("opening the overlay did not fetch the review")
	}
	next, _ := m.Update(cmd())
	m = next.(tuiModel)

	out := m.View().Content
	for _, want := range []string{"issues", "improve_uzi", "api/internal/hub", "high"} {
		if !strings.Contains(out, want) {
			t.Errorf("the overlay does not render %q\n%s", want, out)
		}
	}
	// The triage tally comes from the SHARED renderer, not a second one here.
	if tl := triageLine(reviewFixture().Triage); tl != "" && !strings.Contains(out, cellText(tl)) {
		t.Errorf("the overlay does not use the shared triageLine output %q", tl)
	}
}

// An unjudged run says so, and does not render as an empty or broken overlay.
func TestReviewOverlayHandlesUnjudgedAndErrors(t *testing.T) {
	m := ownerModel(t, &uzicli.FakeClient{}, "r-own", ownedRun("r-own"))
	m.detail.review.open = true
	next, _ := m.Update(reviewLoadedMsg{runID: "r-own", review: nil})
	m = next.(tuiModel)
	if !strings.Contains(m.View().Content, "not been judged") {
		t.Errorf("an unjudged run does not say so\n%s", m.View().Content)
	}

	m2 := ownerModel(t, &uzicli.FakeClient{}, "r-own", ownedRun("r-own"))
	m2.detail.review.open = true
	next, _ = m2.Update(reviewLoadedMsg{runID: "r-own", err: uzicli.Exitf(uzicli.ExitNotFound, "run not found")})
	m2 = next.(tuiModel)
	if !strings.Contains(m2.View().Content, "could not load the review") {
		t.Error("a failed review load is not explained")
	}
}

// Resolve / dismiss / undo drive the real disposition calls, and a dismissal must
// carry one of the server's closed reasons.
func TestReviewDispositionKeys(t *testing.T) {
	fake := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}}
	m := reviewModel(t, fake)

	// resolve
	_, cmd := m.handleKey("r")
	if cmd == nil {
		t.Fatal("r did not record a resolve")
	}
	cmd()
	if fake.LastDispositionStatus != "done" {
		t.Errorf("resolve sent status %q, want done", fake.LastDispositionStatus)
	}
	if fake.LastDispositionRecID != reviewFixture().Recommendations[0].ID {
		t.Errorf("resolve targeted %q, want the selected recommendation", fake.LastDispositionRecID)
	}

	// dismiss asks for a REASON first — the set is the server's closed enum.
	nm, cmd := m.handleKey("d")
	m2 := nm.(tuiModel)
	if cmd != nil {
		t.Fatal("d dismissed without asking for a reason; the server requires one")
	}
	if m2.detail.review.pendingDismiss == "" {
		t.Fatal("d did not open the reason prompt")
	}
	if !strings.Contains(m2.View().Content, "won't do") {
		t.Error("the reason prompt does not offer the server's reasons")
	}
	fake2 := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}}
	m3 := reviewModel(t, fake2)
	nm, _ = m3.handleKey("d")
	m3 = nm.(tuiModel)
	_, cmd = m3.handleKey("w")
	if cmd == nil {
		t.Fatal("the reason key did not submit the dismissal")
	}
	cmd()
	if fake2.LastDispositionStatus != "dismissed" || fake2.LastDispositionReason != "wont_do" {
		t.Errorf("dismissal sent (%q,%q), want (dismissed,wont_do)", fake2.LastDispositionStatus, fake2.LastDispositionReason)
	}

	// An unrecognised key CANCELS the dismissal rather than defaulting to a reason.
	m4 := reviewModel(t, &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}})
	nm, _ = m4.handleKey("d")
	m4 = nm.(tuiModel)
	nm, cmd = m4.handleKey("z")
	m4 = nm.(tuiModel)
	if cmd != nil {
		t.Error("an unrecognised key submitted a dismissal; a reason must be chosen, never defaulted")
	}
	if m4.detail.review.pendingDismiss != "" {
		t.Error("an unrecognised key left the reason prompt open")
	}

	// undo
	fake3 := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}}
	m5 := reviewModel(t, fake3)
	_, cmd = m5.handleKey("u")
	if cmd == nil {
		t.Fatal("u did not undo")
	}
	cmd()
	if fake3.LastDispositionRecID == "" {
		t.Error("undo did not reach DeleteDisposition")
	}
}

// The overlay SWALLOWS keys while open, so j/k move the recommendation cursor rather
// than scrolling the transcript underneath.
func TestReviewOverlaySwallowsKeysWhileOpen(t *testing.T) {
	m := reviewModel(t, &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r-own": reviewFixture()}})
	before := m.detail.scroll

	m = press(t, m, "j")
	if m.detail.review.cursor != 1 {
		t.Errorf("j moved the review cursor to %d, want 1", m.detail.review.cursor)
	}
	if m.detail.scroll != before {
		t.Error("j scrolled the transcript underneath the open overlay")
	}
	m = press(t, m, keyEsc)
	if m.detail.review.open {
		t.Error("esc did not close the overlay")
	}
}

// D7, the requirement M3 did not need: judge free text is untrusted, and structural
// spoofing SURVIVES sanitization because it is valid markdown. The defence is chrome
// the UI owns.
func TestReviewOverlayFencesUntrustedJudgeText(t *testing.T) {
	spoofed := reviewFixture()
	// A summary pretending to be the UI's own verdict line, plus control bytes and a
	// bidi override in the free-text fields.
	spoofed.SummaryMd = "# VERDICT: APPROVED\n\nnothing to see\x1b[2J\u202Ehere"
	spoofed.Recommendations[0].Target = "safe\u202Ednammoc\x07"
	spoofed.Recommendations[0].RationaleMd = "## looks official\x1b[31m"

	m := ownerModel(t, &uzicli.FakeClient{}, "r-own", ownedRun("r-own"))
	next, _ := m.Update(reviewLoadedMsg{runID: "r-own", review: spoofed})
	m = next.(tuiModel)
	m.detail.review.open = true

	out := m.View().Content
	assertNoRawControls(t, "review overlay", out)

	// The spoofed heading still RENDERS — sanitizing markdown structure away would
	// defeat using Glamour at all — so what protects the reader is the labelled box.
	if !strings.Contains(out, "judge summary") {
		t.Error("the judge summary is not fenced in a labelled box; a heading in model-authored text is indistinguishable from the UI's own without one")
	}
	titleAt := strings.Index(out, "judge summary")
	spoofAt := strings.Index(out, "VERDICT")
	if titleAt < 0 || spoofAt < 0 || titleAt > spoofAt {
		t.Errorf("the provenance label (%d) does not precede the untrusted summary (%d); untrusted text must never render above its own frame label", titleAt, spoofAt)
	}
	// The rationale of the SELECTED recommendation is fenced too.
	if !strings.Contains(out, "rationale") {
		t.Error("the recommendation rationale is not fenced with its provenance")
	}
}

// The overlay branches ONLY on closed enums: an unrecognised value renders as DATA,
// never replaced by a placeholder and never hidden. It may be TRUNCATED — these are
// fixed-width cells — which is why the assertion is on a recognisable prefix rather
// than the whole string. An earlier version of this test asserted the full value and
// failed on the display cap, which measured the cap rather than the branching.
func TestReviewOverlayDoesNotBranchOnOpenText(t *testing.T) {
	odd := reviewFixture()
	odd.Verdict = "newverdict"
	odd.Recommendations[0].Category = "newcategory"
	odd.Recommendations[0].Confidence = "certainish"

	m := ownerModel(t, &uzicli.FakeClient{}, "r-own", ownedRun("r-own"))
	next, _ := m.Update(reviewLoadedMsg{runID: "r-own", review: odd})
	m = next.(tuiModel)
	m.detail.review.open = true

	out := m.View().Content
	for _, want := range []string{"newverdict", "newcategory", "certainish"} {
		if !strings.Contains(out, want) {
			t.Errorf("an unrecognised enum value %q was dropped or replaced; a value this binary does not recognise must still render as data\n%s", want, out)
		}
	}
	// And nothing became a placeholder.
	if strings.Contains(out, "unknown") {
		t.Errorf("the overlay substituted a placeholder for an unrecognised enum; that hides what the judge actually said\n%s", out)
	}
}
