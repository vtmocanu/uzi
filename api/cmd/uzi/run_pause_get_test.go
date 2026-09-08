package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1190 M4 — the pause block of `uzi run get` and its shared clause builders.

// sixMilestones is a small frozen milestone list for the pause-progress tests.
func sixMilestones() []apitypes.Milestone {
	return []apitypes.Milestone{
		{ID: "m1", Title: "one"}, {ID: "m2", Title: "two"}, {ID: "m3", Title: "three"},
		{ID: "m4", Title: "four"}, {ID: "m5", Title: "five"}, {ID: "m6", Title: "six"},
	}
}

// pausedFixture is a run parked by an owner pause: 3 of 6 milestones reported complete, a
// checkpoint pushed, and updated_at (the park stamp) at 11:02 UTC.
func pausedFixture() apitypes.RunDTO {
	updated := time.Date(2026, 9, 8, 11, 2, 0, 0, time.UTC)
	checkpoint := time.Date(2026, 9, 8, 11, 1, 0, 0, time.UTC)
	return apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: statusPaused, Health: "ok",
		Milestones:          sixMilestones(),
		MilestonesCompleted: []string{"m1", "m2", "m3"},
		CheckpointTipAt:     &checkpoint,
		UpdatedAt:           updated,
	}
}

func intPtr(n int) *int { return &n }

// TestPausedSummary pins the full paused summary: the since clock (UTC 15:04) + duration, the
// milestone count over the frozen list, and the checkpoint age.
func TestPausedSummary(t *testing.T) {
	r := pausedFixture()
	// 2h40m after the 11:02 park, 2h41m after the 11:01 checkpoint.
	now := time.Date(2026, 9, 8, 13, 42, 0, 0, time.UTC)
	got := pausedSummary(r, now)
	want := "since 11:02 (2h40m) · 3/6 milestones · checkpoint 2h41m ago"
	if got != want {
		t.Errorf("pausedSummary = %q, want %q", got, want)
	}
}

// TestPausedSummaryNoMilestonesNoCheckpoint: a prompt-kind pause (no frozen milestones) with
// no checkpoint yet sheds both optional clauses rather than rendering "0/0" / "checkpoint -".
func TestPausedSummaryNoMilestonesNoCheckpoint(t *testing.T) {
	updated := time.Date(2026, 9, 8, 11, 2, 0, 0, time.UTC)
	r := apitypes.RunDTO{ID: "r1", Kind: "prompt", Status: statusPaused, UpdatedAt: updated}
	now := time.Date(2026, 9, 8, 11, 7, 0, 0, time.UTC)
	got := pausedSummary(r, now)
	want := "since 11:02 (5m)"
	if got != want {
		t.Errorf("pausedSummary (no milestones/checkpoint) = %q, want %q", got, want)
	}
}

// TestPauseBoundaryClause: the pending-boundary wording per mode. Milestone mode names
// M(pause_after_count+1); `now` names "now"; a run with no frozen milestones in milestone mode
// names "after the current step".
func TestPauseBoundaryClause(t *testing.T) {
	cases := []struct {
		name string
		run  apitypes.RunDTO
		want string
	}{
		{
			name: "milestone mode after M4",
			run:  apitypes.RunDTO{Milestones: sixMilestones(), PauseMode: sp("milestone"), PauseAfterCount: intPtr(3)},
			want: "after M4",
		},
		{
			name: "now mode",
			run:  apitypes.RunDTO{Milestones: sixMilestones(), PauseMode: sp("now"), PauseAfterCount: intPtr(3)},
			want: "now",
		},
		{
			name: "no frozen milestones",
			run:  apitypes.RunDTO{PauseMode: sp("milestone"), PauseAfterCount: intPtr(0)},
			want: "after the current step",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pauseBoundaryClause(tc.run); got != tc.want {
				t.Errorf("pauseBoundaryClause = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPauseDetailRows: exactly one row for a paused run (PAUSED), exactly one for a pending
// pause on a running run (PAUSE_REQUESTED), and none for a run that is neither.
func TestPauseDetailRows(t *testing.T) {
	now := time.Date(2026, 9, 8, 13, 42, 0, 0, time.UTC)

	// Paused → a PAUSED row.
	rows := pauseDetailRows(pausedFixture(), now)
	if len(rows) != 1 || rows[0][0] != "PAUSED" {
		t.Fatalf("paused run: rows = %v, want one PAUSED row", rows)
	}

	// Running with a pending request → a PAUSE_REQUESTED row.
	req := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	pending := apitypes.RunDTO{
		ID: "r1", Status: "running", Milestones: sixMilestones(),
		PauseRequestedAt: &req, PauseMode: sp("milestone"), PauseAfterCount: intPtr(3),
	}
	rows = pauseDetailRows(pending, now)
	if len(rows) != 1 || rows[0][0] != "PAUSE_REQUESTED" || rows[0][1] != "after M4" {
		t.Fatalf("pending pause: rows = %v, want one PAUSE_REQUESTED 'after M4' row", rows)
	}

	// A plain running run with no request → no rows.
	if rows := pauseDetailRows(apitypes.RunDTO{ID: "r1", Status: "running"}, now); rows != nil {
		t.Errorf("running run with no pause: rows = %v, want none", rows)
	}
}

// TestRunGetRendersPausedRow: end to end through `uzi run get`, the human table carries a
// PAUSED row with the since clock, the milestone count and the checkpoint clause.
func TestRunGetRendersPausedRow(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": pausedFixture()}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"PAUSED", "since 11:02", "3/6 milestones", "checkpoint"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`run get` PAUSED row missing %q:\n%s", want, stdout)
		}
	}
}

// TestRunGetRendersPendingRow: a running run with a pending pause shows a PAUSE_REQUESTED row.
func TestRunGetRendersPendingRow(t *testing.T) {
	req := time.Date(2026, 9, 8, 11, 0, 0, 0, time.UTC)
	pending := apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: "running", Health: "ok", Milestones: sixMilestones(),
		PauseRequestedAt: &req, PauseMode: sp("milestone"), PauseAfterCount: intPtr(3),
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": pending}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "PAUSE_REQUESTED") || !strings.Contains(stdout, "after M4") {
		t.Errorf("`run get` PAUSE_REQUESTED row missing (want 'after M4'):\n%s", stdout)
	}
	// A pending pause must NOT also draw a PAUSED row (the run is still running).
	if strings.Contains(stdout, "\nPAUSED ") || strings.Contains(stdout, "PAUSED  ") {
		t.Errorf("a pending pause on a running run must not draw a PAUSED row:\n%s", stdout)
	}
}
