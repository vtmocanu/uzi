package main

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #3522: the CLI names the weekly usage reserve wherever it parks or asks a run to park.

func TestHoldRowWeeklyReserve(t *testing.T) {
	now := time.Date(2026, 10, 27, 15, 12, 0, 0, time.UTC)
	run := heldRun("r1", holdWeeklyReserve, now.Add(-70*time.Minute))
	want := "weekly usage reserve · parked 14:02 (1h10m) · 4/7 milestones · " +
		"resumes when usage drops below the threshold; override: uzi run resume r1"
	if row := holdRow(run, now); row == nil || row[0] != "HOLD" || row[1] != want {
		t.Errorf("holdRow = %v, want [HOLD %q]", row, want)
	}
	notice := pausedHoldNotice(run)
	if !strings.Contains(notice, "weekly usage reserve") || !strings.Contains(notice, "uzi run resume r1") {
		t.Errorf("pausedHoldNotice = %q, want the reserve cause and the override verb", notice)
	}
	if strings.Contains(notice, "owner") {
		t.Errorf("pausedHoldNotice = %q must not read as an owner pause", notice)
	}
}

func TestRunGetWeeklyReserveHold(t *testing.T) {
	run := heldRun("r1", holdWeeklyReserve, time.Now().Add(-5*time.Minute))
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("run get exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HOLD") || !strings.Contains(out, "weekly usage reserve") || !strings.Contains(out, "uzi run resume r1") {
		t.Errorf("run get lacks the weekly_reserve HOLD row:\n%s", out)
	}
	// A hold, not an owner pause: the PAUSED row carries no pause-origin label, it starts at "since".
	if !regexp.MustCompile(`PAUSED\s+since `).MatchString(out) {
		t.Errorf("the PAUSED row of a reserve park carries an owner-pause origin label:\n%s", out)
	}
}

// TestPauseBoundaryClauseWeeklyReserve: the reserve's pending request waits at the same milestone
// boundary an owner milestone pause does, labelled with its cause.
func TestPauseBoundaryClauseWeeklyReserve(t *testing.T) {
	cases := []struct {
		name string
		run  apitypes.RunDTO
		want string
	}{
		{"milestone boundary", apitypes.RunDTO{Milestones: sixMilestones(), PauseMode: sp("reserve"), PauseAfterCount: intPtr(3)}, "after M4 (weekly usage reserve)"},
		{"no frozen milestones", apitypes.RunDTO{PauseMode: sp("reserve"), PauseAfterCount: intPtr(0)}, "after the current step (weekly usage reserve)"},
		{"an owner milestone request stays unlabelled", apitypes.RunDTO{Milestones: sixMilestones(), PauseMode: sp("milestone"), PauseAfterCount: intPtr(3)}, "after M4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pauseBoundaryClause(tc.run); got != tc.want {
				t.Errorf("pauseBoundaryClause = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPauseDetailRowsWeeklyReservePending(t *testing.T) {
	at := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	r := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Milestones: sixMilestones(),
		PauseRequestedAt: &at, PauseMode: sp("reserve"), PauseAfterCount: intPtr(1)}
	rows := pauseDetailRows(r, at.Add(time.Minute))
	if len(rows) != 1 || rows[0][0] != "PAUSE_REQUESTED" || rows[0][1] != "after M2 (weekly usage reserve)" {
		t.Errorf("pauseDetailRows = %v, want PAUSE_REQUESTED after M2 (weekly usage reserve)", rows)
	}
}

// TestRunResumeNamesTheReserveOverride: resuming a weekly_reserve park returns the run with its
// recorded exemption, and the confirmation says so; an ordinary resume prints the plain line.
func TestRunResumeNamesTheReserveOverride(t *testing.T) {
	until := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	hold := "weekly_reserve"
	fc := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "paused", HoldReason: &hold}},
		ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued", ReserveExemptUntil: &until},
	}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	want := "Resumed r1: queued (overrides the weekly usage reserve until the next weekly reset).\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want it to contain %q", stdout, want)
	}

	plain := &uzicli.FakeClient{ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
	stdout, _, code = runCLI(t, fakeEnv(plain), "run", "resume", "r1")
	if code != uzicli.ExitOK || strings.Contains(stdout, "weekly usage reserve") || !strings.Contains(stdout, "Resumed r1: queued.\n") {
		t.Errorf("an ordinary resume printed %q (exit %d), want the plain queued line", stdout, code)
	}

	// An ordinary (NULL-hold) pause resumed while an exemption recorded by an EARLIER reserve resume is
	// still in force: this resume overrode nothing, so the plain line prints.
	carried := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "paused"}},
		ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued", ReserveExemptUntil: &until},
	}
	stdout, _, code = runCLI(t, fakeEnv(carried), "run", "resume", "r1")
	if code != uzicli.ExitOK || strings.Contains(stdout, "weekly usage reserve") || !strings.Contains(stdout, "Resumed r1: queued.\n") {
		t.Errorf("resuming an ordinary pause with a carried exemption printed %q (exit %d), want the plain queued line", stdout, code)
	}
}
