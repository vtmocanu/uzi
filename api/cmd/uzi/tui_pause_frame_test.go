package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// pauseFrameRuns builds the pause-slot fixtures. Times are fixed or relative to a single
// captured now, and every assertion is on stable text, never on a clock-derived value.
func pauseFrameRuns() []struct {
	name string
	run  apitypes.RunDTO
	want []string
} {
	now := time.Now()
	req := now.Add(-5 * time.Minute)
	retry := now.Add(2 * time.Hour)
	hold := holdCredentialDisabled

	paused := apitypes.RunDTO{ID: "r-paused", Kind: "issue", Status: statusPaused, Health: "ok", IssueTitle: "t"}
	pending := apitypes.RunDTO{ID: "r-pending", Kind: "issue", Status: "running", Health: "ok", IssueTitle: "t", PauseRequestedAt: &req}
	limit := apitypes.RunDTO{
		ID: "r-limit", Kind: "issue", Status: statusLimitWait, Health: "ok", IssueTitle: "t",
		PauseRequestedAt: &req, RetryNotBefore: &retry, LimitWaitCount: 1,
	}
	vault := vaultParkRun("r-vault")
	vault.PauseRequestedAt = &req
	cred := apitypes.RunDTO{ID: "r-cred", Kind: "issue", Status: statusPaused, Health: "ok", IssueTitle: "t", HoldReason: &hold}

	return []struct {
		name string
		run  apitypes.RunDTO
		want []string
	}{
		{"paused", paused, []string{"paused by you"}},
		{"running with pending pause", pending, []string{"pause requested"}},
		{"limit_wait with pending pause", limit, []string{"waiting: usage limit", "pause requested"}},
		{"vault park with pending pause", vault, []string{"waiting for vault unlock", "pause requested"}},
		{"credential_disabled hold", cred, []string{"waiting: credential disabled"}},
	}
}

// TestPauseSlotDetailFrameFitsTerminal: the run detail draws the pause row (paused, pending
// pause request, credential_disabled hold) as its own row, independent of the park line, and
// the whole frame is exactly m.height rows at every terminal height, including a parked run
// that also carries a pending pause (two rows). Reddening mutation: drop the pause-slot term
// from transcriptViewport (the frame comes to m.height+1 rows), or drop the renderDetail
// pause arm (the expected line text is missing).
func TestPauseSlotDetailFrameFitsTerminal(t *testing.T) {
	for _, tc := range pauseFrameRuns() {
		for _, height := range []int{20, 30, 50} {
			m := tuiTestModel(t, &uzicli.FakeClient{}, tc.run.ID)
			m = applyDetail(m, tc.run, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", time.Now())})
			m = step(m, tea.WindowSizeMsg{Width: 100, Height: height})
			out := stripANSI(m.View().Content)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("%s @h%d: missing %q:\n%s", tc.name, height, want, out)
				}
			}
			if rows := len(strings.Split(out, "\n")); rows != m.height {
				t.Errorf("%s @h%d: detail frame is %d rows, want exactly m.height %d\n%s", tc.name, height, rows, m.height, out)
			}
		}
	}
}

// TestNoPauseSlotDetailFrameUnchanged: a plain running run (no park, no pause) still fills the
// terminal exactly, so the pause charge is not applied unconditionally. Reddening mutation:
// charge the pause slot with no condition (the frame comes to m.height-1 rows).
func TestNoPauseSlotDetailFrameUnchanged(t *testing.T) {
	run := apitypes.RunDTO{ID: "r-plain", Kind: "issue", Status: "running", Health: "ok", IssueTitle: "t"}
	for _, height := range []int{20, 30, 50} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m = applyDetail(m, run, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", time.Now())})
		m = step(m, tea.WindowSizeMsg{Width: 100, Height: height})
		out := stripANSI(m.View().Content)
		if rows := len(strings.Split(out, "\n")); rows != m.height {
			t.Errorf("h%d: plain frame is %d rows, want exactly m.height %d\n%s", height, rows, m.height, out)
		}
	}
}

// TestPauseSlotViewportReservesRow: transcriptViewport charges the pause row one row, separate
// from the park row, so a paused run is one row shorter than the same run running, and a
// limit_wait run with a pending pause is one row shorter than the same limit_wait run without.
// Reddening mutation: drop the pause-slot term from transcriptViewport.
func TestPauseSlotViewportReservesRow(t *testing.T) {
	runs := pauseFrameRuns()
	byName := map[string]apitypes.RunDTO{}
	for _, tc := range runs {
		byName[tc.name] = tc.run
	}
	vp := func(r apitypes.RunDTO) int {
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m.width, m.height = 100, 30
		return applyDetail(m, r, nil).transcriptViewport()
	}

	paused := byName["paused"]
	running := paused
	running.Status = "running"
	if p, u := vp(paused), vp(running); p != u-1 {
		t.Errorf("transcriptViewport(paused) = %d, want %d (one fewer than running's %d)", p, u-1, u)
	}

	pending := byName["limit_wait with pending pause"]
	noPending := pending
	noPending.PauseRequestedAt = nil
	if p, u := vp(pending), vp(noPending); p != u-1 {
		t.Errorf("transcriptViewport(limit_wait+pending) = %d, want %d (one fewer than limit_wait alone, %d)", p, u-1, u)
	}
	plain := noPending
	plain.Status = "running"
	if p, u := vp(pending), vp(plain); p != u-2 {
		t.Errorf("transcriptViewport(limit_wait+pending) = %d, want %d (two fewer than plain running's %d)", p, u-2, u)
	}
}
