package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// issue #2760: a run mid-"revise" replan keeps status awaiting_approval with is_revising set.
// The detail view, run get and the crew rail must present it as working, never as waiting on
// the owner at the plan gate.

func revisingRun(id string, revising bool) apitypes.RunDTO {
	r := apitypes.RunDTO{ID: id, Kind: "issue", Status: "awaiting_approval", Health: "ok", IsRevising: revising}
	if revising {
		r.Progress = &apitypes.RunProgress{State: "planning"}
	} else {
		r.Progress = &apitypes.RunProgress{State: "waiting"}
	}
	return r
}

func ownerDetail(t *testing.T, run apitypes.RunDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m = applyDetail(m, run, nil)
	next, _ := m.Update(runInputsMsg{runID: run.ID})
	m = next.(tuiModel)
	m.width = 100
	return m
}

// Reddening mutation: pass false to runStateToken in the detail header.
func TestDetailHeaderTokenRevising(t *testing.T) {
	m := ownerDetail(t, revisingRun("rv-1", true))
	tok := m.pal.runStateToken(m.detail.run, true)
	if tok.word != "revising" {
		t.Fatalf("fixture token word = %q, want revising", tok.word)
	}
	hdr := stripANSI(m.View().Content)
	if !strings.Contains(hdr, tok.glyph+" revising") {
		t.Errorf("detail header lacks the revising token %q\n%s", tok.glyph+" revising", hdr)
	}
	if g := m.pal.runStateToken(revisingRun("rv-1", false), false); strings.Contains(hdr, g.glyph+" "+g.word) {
		t.Errorf("detail header shows the plan-gate token %q for a revising run", g.word)
	}
}

// Reddening mutation: drop the IsRevising early return in detailBanner.
func TestDetailBannerRevising(t *testing.T) {
	if b := ownerDetail(t, revisingRun("rv-2", true)).detailBanner(); b != "" {
		t.Errorf("revising run has a banner: %q", b)
	}
	if b := ownerDetail(t, revisingRun("rv-2", false)).detailBanner(); !strings.Contains(b, "PLAN GATE") {
		t.Errorf("genuine plan gate lost its banner: %q", b)
	}
}

// Reddening mutation: drop the !run.IsRevising clause in atPlanGate.
func TestAtPlanGateRevising(t *testing.T) {
	if atPlanGate(revisingRun("x", true)) {
		t.Error("atPlanGate true while revising")
	}
	if !atPlanGate(revisingRun("x", false)) {
		t.Error("atPlanGate false at a genuine plan gate")
	}
	// The y key must not start an approve while revising.
	m := ownerDetail(t, revisingRun("rv-3", true))
	if _, cmd, handled := m.steerKey(keyConfirmY); handled || cmd != nil {
		t.Error("y handled as approve while revising")
	}
	// The review hint stays hidden for both the gate and a revising run.
	for _, rev := range []bool{true, false} {
		if h := stripANSI(ownerDetail(t, revisingRun("rv-4", rev)).detailFooter()); strings.Contains(h, "review") {
			t.Errorf("revising=%v footer offers review: %q", rev, h)
		}
	}
}

// Reddening mutations: (a) revert applyMeta/applyEvents handling is not involved; this pins that
// the flag survives each live-update path. Streaming preserves status but adopts the DTO flag.
func TestDetailRevisingLiveUpdates(t *testing.T) {
	for _, polling := range []bool{false, true} {
		m := ownerDetail(t, revisingRun("rv-5", false))
		d := &m.detail
		d.polling = polling
		if m.detailBanner() == "" {
			t.Fatalf("polling=%v: start state should show PLAN GATE", polling)
		}
		d.applyMeta(revisingRun("rv-5", true))
		if m.detailBanner() != "" || !m.detail.run.IsRevising {
			t.Errorf("polling=%v: revising meta still shows a banner", polling)
		}
		d.applyEvents([]apitypes.RunEventDTO{{Type: uzicli.RunEventTypeState, Status: "awaiting_approval"}})
		if m.detailBanner() != "" || atPlanGate(m.detail.run) {
			t.Errorf("polling=%v: a state event re-armed the gate while revising", polling)
		}
		d.applyMeta(revisingRun("rv-5", false))
		if !strings.Contains(m.detailBanner(), "PLAN GATE") || !atPlanGate(m.detail.run) {
			t.Errorf("polling=%v: re-gated run lost its PLAN GATE", polling)
		}
	}
}

// Reddening mutations: skip the revising clause in crewStateFor / activeLaneKey.
func TestCrewStateRevising(t *testing.T) {
	now := time.Now()
	frames := []laneFrame{lf(1, "planner", "toolu_01", "", time.Second, now)}
	if got := activeLaneKey("awaiting_approval", true, frames); got != "toolu_01" {
		t.Errorf("revising active lane = %q, want toolu_01", got)
	}
	if got := activeLaneKey("awaiting_approval", false, frames); got != "" {
		t.Errorf("plan-gate active lane = %q, want empty", got)
	}
	if got := crewStateFor("awaiting_approval", true, "ok", "toolu_01", "toolu_01", now, now); got != crewWorking {
		t.Errorf("revising speaking lane = %s, want working", got)
	}
	if got := crewStateFor("awaiting_approval", false, "ok", "toolu_01", "", now, now); got != crewWaiting {
		t.Errorf("plan-gate lane = %s, want waiting", got)
	}
}

// A revising run carries planning progress: detail names it, and the board omits
// the repeated flag. Neither renderer claims it waits on the owner, even with stalled health.
func TestRevisingProgressNeverWaitsOnYou(t *testing.T) {
	m := ownerDetail(t, revisingRun("rv-6", true))
	got := stripANSI(m.renderProgress())
	if !strings.Contains(got, "planning") || strings.Contains(got, "on you") || strings.Contains(got, "plan gate") {
		t.Errorf("detail progress for a revising run = %q", got)
	}
	for _, health := range []string{"ok", "stalled"} {
		r := apitypes.RunListItemDTO{RunDTO: revisingRun("rv-6", true)}
		r.Health = health
		cell := stripANSI(m.boardProgressCell(r, false, nil, m.pal.runStateToken(r.RunDTO, r.IsRevising).word))
		if strings.Contains(cell, "on you") || strings.Contains(cell, "plan gate") {
			t.Errorf("board progress with health %s = %q", health, cell)
		}
	}
}

// Reddening mutation: pass false to displayRunStatus in run_render.go.
func TestRunGetRevisingStatusAndProgress(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"rv": revisingRun("rv", true),
		"pg": revisingRun("pg", false),
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "rv")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if st := lineWith(t, out, "STATUS"); !strings.Contains(st, "revising") {
		t.Errorf("STATUS = %q, want revising", st)
	}
	if pr := lineWith(t, out, "PROGRESS"); !strings.Contains(pr, "planning") || strings.Contains(out, "waits on you") {
		t.Errorf("PROGRESS = %q / output mentions waits on you", pr)
	}
	out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "pg")
	if st := lineWith(t, out, "STATUS"); strings.Contains(st, "revising") {
		t.Errorf("plan-gate STATUS = %q", st)
	}
}
