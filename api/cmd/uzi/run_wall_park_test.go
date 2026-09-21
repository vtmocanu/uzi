package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1497 M3 — the CLI minimum for a run parked at its wall-clock time limit (paused with
// hold_reason=budget_exhausted). These pin: the `run extend` "…and resumed it." wording an
// Extend-on-the-hold produces (server returns resumed:true), the verbatim 409/exit-5 mapping
// of `run resume` / `run stop` on the hold, the `uzi run get` HOLD row for both hold reasons
// (under a colour profile AND NO_COLOR, with an untrusted hold_reason sanitized), and the
// `run wait` informative line that keeps waiting rather than treating the park as terminal.

// heldRun builds a milestone issue run parked in a hold: 4/7 milestones done, paused, with the
// park time (updated_at) fixed so the "parked HH:MM (dur)" clause is deterministic. reason ""
// leaves hold_reason null (an un-held run).
func heldRun(id, reason string, updatedAt time.Time) apitypes.RunDTO {
	ms := make([]apitypes.Milestone, 7)
	for i := range ms {
		ms[i] = apitypes.Milestone{ID: fmt.Sprintf("m%d", i+1), Title: fmt.Sprintf("milestone %d", i+1)}
	}
	r := apitypes.RunDTO{
		ID: id, Kind: "issue", Status: statusPaused, IssueTitle: "do the thing",
		Milestones:          ms,
		MilestonesCompleted: []string{"m1", "m2", "m3", "m4"},
		UpdatedAt:           updatedAt,
	}
	if reason != "" {
		r.HoldReason = &reason
	}
	return r
}

// TestExtendSuccessLineResumedWallPark pins the exact "…and resumed it." line an Extend on a
// budget_exhausted wall park produces (resumed:true on the response): it names the resume,
// renders the remaining time compactly (2h, not 2h00m), and DROPS the allowance tail the
// ordinary extend carries.
//
// MUTATION PROOF: ignore res.Resumed (always take the plain branch) and the "and resumed it."
// clause vanishes and the allowance tail reappears; keep fmtUntil for the resumed "left" and a
// whole-hour grant reads "2h00m left" instead of "2h left".
func TestExtendSuccessLineResumedWallPark(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(2 * time.Hour)
	res := apitypes.RunInputResponse{Resumed: true, ExtensionSeconds: intPtr(7200), DeadlineAt: &deadline}

	got := extendSuccessLine("r1", 2*60*60, res, 57600, now)
	want := "Extended r1 by 2h and resumed it. 2h left, times out " + deadline.Local().Format("15:04") + "."
	if got != want {
		t.Errorf("extendSuccessLine(resumed) =\n  %q\nwant\n  %q", got, want)
	}
	// The resumed line never carries the "Extensions on this run: …" allowance tail (the owner is
	// un-parking a run, not managing a budget cap), even though ExtensionSeconds and the cap are set.
	if strings.Contains(got, "Extensions on this run") {
		t.Errorf("resumed line must drop the allowance tail:\n%s", got)
	}
}

// TestRunExtendResumedWallParkLineFlowsThroughCLI: the whole `uzi run extend` verb wires the
// response's resumed flag into the success line, so the owner sees the run resumed rather than
// just "time added".
func TestRunExtendResumedWallParkLineFlowsThroughCLI(t *testing.T) {
	deadline := time.Now().Add(2 * time.Hour)
	fc := &uzicli.FakeClient{
		RunByID:   map[string]apitypes.RunDTO{"r1": {ID: "r1", BudgetExtensionCapSeconds: 57600}},
		InputResp: apitypes.RunInputResponse{Resumed: true, ExtensionSeconds: intPtr(7200), DeadlineAt: &deadline},
	}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "extend", "r1", "--by", "2h")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Extended r1 by 2h and resumed it.") {
		t.Errorf("extend on a wall park must say it resumed the run:\n%s", stdout)
	}
	if strings.Contains(stdout, "Extensions on this run") {
		t.Errorf("the resumed extend line must not carry the allowance tail:\n%s", stdout)
	}
}

// TestRunResumeWallParkPrints409Verbatim: `uzi run resume` on an out-of-time hold with no budget
// surfaces the server's 409 message verbatim and exits 5 — the CLI's standard "server refused"
// exit — rather than inventing its own wording.
func TestRunResumeWallParkPrints409Verbatim(t *testing.T) {
	const msg = "this run is out of time; extend it to resume: uzi run extend r1 --by 2h"
	fc := &uzicli.FakeClient{ResumeRunNowErr: uzicli.Exitf(uzicli.ExitConflict, "%s", msg)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "resume", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (409)", code, uzicli.ExitConflict)
	}
	if !strings.Contains(stderr, msg) {
		t.Errorf("server 409 must be printed verbatim, stderr = %q", stderr)
	}
	if fc.LastResumeRunID != "r1" {
		t.Errorf("resume must reach the server for r1, got %q", fc.LastResumeRunID)
	}
}

// TestRunStopWallParkPrints409Verbatim: `uzi run stop` on a hold that cannot be stopped (no
// completed milestone / allowance used / wrong kind) surfaces the server's 409 verbatim and exits
// 5. The stop rides the same submitInput 409-verbatim path scope/reject use, so no bespoke wiring
// is needed — this pins that it stays that way.
func TestRunStopWallParkPrints409Verbatim(t *testing.T) {
	const msg = "this run already used its finalize allowance; extend or cancel it"
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "%s", msg)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "stop", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (409)", code, uzicli.ExitConflict)
	}
	if !strings.Contains(stderr, msg) {
		t.Errorf("server 409 must be printed verbatim, stderr = %q", stderr)
	}
	if fc.LastInputKind != kindStop {
		t.Errorf("stop must reach the server as kind=%q, got %q", kindStop, fc.LastInputKind)
	}
}

// TestHoldRow pins the exact HOLD-row value for both hold reasons, and the emit-only/clause-shed
// contract. A fixed now makes the "parked HH:MM (dur)" clock deterministic (14:02 UTC + 1h10m).
//
// MUTATION PROOF: gate holdRow on status instead of hold_reason and the un-held run stops
// returning nil; drop the budget_exhausted branch and the extend clause vanishes; render the
// completion hold with the extend clause and the completion_blocked case reddens.
func TestHoldRow(t *testing.T) {
	now := time.Date(2026, 9, 12, 15, 12, 0, 0, time.UTC)
	parkedAt := now.Add(-70 * time.Minute) // 14:02 UTC

	wall := holdRow(heldRun("r1", holdBudgetExhausted, parkedAt), now)
	wantWall := "time limit reached · parked 14:02 (1h10m) · 4/7 milestones · extend: uzi run extend r1 --by 2h"
	if wall == nil || wall[0] != "HOLD" || wall[1] != wantWall {
		t.Errorf("holdRow(budget_exhausted) = %v, want [HOLD %q]", wall, wantWall)
	}

	blocked := holdRow(heldRun("r1", holdCompletionBlocked, parkedAt), now)
	wantBlocked := "completion blocked · parked 14:02 (1h10m) · 4/7 milestones"
	if blocked == nil || blocked[1] != wantBlocked {
		t.Errorf("holdRow(completion_blocked) = %v, want [HOLD %q]", blocked, wantBlocked)
	}
	// The completion hold resumes through its own decision, not by extending the clock, so its HOLD
	// row must NOT carry the extend command.
	if strings.Contains(wantBlocked, "extend:") {
		t.Errorf("the completion-hold HOLD row must not carry an extend command: %q", wantBlocked)
	}

	// A run with a null hold_reason is not held: no HOLD row at all.
	if row := holdRow(heldRun("r1", "", parkedAt), now); row != nil {
		t.Errorf("holdRow(no hold) = %v, want nil", row)
	}

	// A prompt-kind park with no frozen milestones drops the milestone clause rather than reading "0/0".
	noMs := heldRun("r1", holdBudgetExhausted, parkedAt)
	noMs.Milestones = nil
	noMs.MilestonesCompleted = nil
	if row := holdRow(noMs, now); row == nil || strings.Contains(row[1], "milestones") {
		t.Errorf("holdRow(no milestones) = %v, want the milestone clause dropped", row)
	}
}

// TestRenderRunDetailHoldRowUnderColourAndNoColor renders the whole `uzi run get` detail for a wall
// park under BOTH a colour profile and NO_COLOR, and confirms the HOLD row and its clauses survive
// each. The parked-since clock is live here (renderRunDetail uses time.Now()), so this asserts the
// stable clauses by substring; the exact wording is pinned by TestHoldRow above.
func TestRenderRunDetailHoldRowUnderColourAndNoColor(t *testing.T) {
	r := heldRun("r1", holdBudgetExhausted, time.Now().Add(-70*time.Minute))
	for _, tc := range []struct {
		name string
		p    func(t *testing.T, b *bytes.Buffer) *uzicli.Printer
	}{
		{"NO_COLOR", func(_ *testing.T, b *bytes.Buffer) *uzicli.Printer {
			return uzicli.NewPrinter(b, false, false, true, false) // non-tty, non-json, no colour
		}},
		{"colour", func(t *testing.T, b *bytes.Buffer) *uzicli.Printer {
			t.Setenv("NO_COLOR", "") // let colour turn on for a TTY printer
			return uzicli.NewPrinter(b, true, false, false, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := tc.p(t, &buf)
			if err := renderRunDetail(p, r); err != nil {
				t.Fatalf("renderRunDetail: %v", err)
			}
			out := buf.String()
			for _, want := range []string{"HOLD", "time limit reached", "parked", "4/7 milestones", "extend: uzi run extend r1 --by 2h"} {
				if !strings.Contains(out, want) {
					t.Errorf("run get detail missing %q under %s profile:\n%s", want, tc.name, out)
				}
			}
		})
	}
}

// TestRenderRunDetailHoldRowSanitizesHostileReason: hold_reason is unconstrained text on the wire,
// so an UNRECOGNISED value carrying terminal-control bytes must be scrubbed before it reaches the
// HOLD row — the same terminal-safety obligation the HOLD_CONTEXT row carries. The printable part
// still survives (an empty cell would hide the hold rather than sanitize it).
func TestRenderRunDetailHoldRowSanitizesHostileReason(t *testing.T) {
	hostile := "evil\u202ednetsop\x1b[2J\x1b]0;pwned\x07\nrow-forge"
	r := heldRun("r1", hostile, time.Now().Add(-time.Minute))

	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderRunDetail(p, r); err != nil {
		t.Fatalf("renderRunDetail: %v", err)
	}
	out := buf.String()
	for _, bad := range []rune{0x1b, 0x07, 0x202e} {
		if strings.ContainsRune(out, bad) {
			t.Errorf("hostile hold_reason drove a terminal control byte %U to the output:\n%q", bad, out)
		}
	}
	// The printable stem still reached the HOLD row (the reason was sanitized, not dropped).
	if !strings.Contains(out, "HOLD") || !strings.Contains(out, "evil") {
		t.Errorf("the sanitized reason must still render on the HOLD row:\n%s", out)
	}
}

// TestRunWaitPrintsWallParkLineAndKeepsWaiting: a bare `run wait` on a run that goes
// running → paused(budget_exhausted) → completed prints the ONE informative line naming the wall
// park and the extend command, keeps waiting THROUGH the park (paused is non-terminal, never a
// default target), and stops at completed.
//
// MUTATION PROOF: gate the line on status alone (drop the hold_reason check) and it fires for an
// owner pause too; add paused to defaultWaitStates and the wait stops at the park, so the
// "paused → completed" assertion reddens.
func TestRunWaitPrintsWallParkLineAndKeepsWaiting(t *testing.T) {
	reason := holdBudgetExhausted
	parked := apitypes.RunDTO{ID: "r1", Status: statusPaused, HoldReason: &reason}
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: parked}, okStep("completed"))}

	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "running → paused") {
		t.Errorf("expected the running→paused transition line, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "parked at its time limit; waiting for the owner (uzi run extend r1 --by 2h)") {
		t.Errorf("expected the wall-park informative line, stderr = %q", stderr)
	}
	if strings.Contains(stderr, "unrecognized status") {
		t.Errorf("paused is a recognised status — the older-than-server warning must not fire, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "paused → completed") {
		t.Errorf("wait must keep going THROUGH the wall park to completed, stderr = %q", stderr)
	}
}

// TestRunWaitNoWallParkLineForOwnerPause: the informative line is gated on the budget_exhausted
// hold, so an ordinary owner pause (paused, no hold_reason) gets the plain transition line and
// nothing about a time limit.
func TestRunWaitNoWallParkLineForOwnerPause(t *testing.T) {
	ownerPause := apitypes.RunDTO{ID: "r1", Status: statusPaused} // hold_reason nil
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: ownerPause}, okStep("completed"))}

	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.Contains(stderr, "parked at its time limit") {
		t.Errorf("an owner pause must not print the wall-park line, stderr = %q", stderr)
	}
}
