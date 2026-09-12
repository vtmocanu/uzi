package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1226 M5, D7/D8 — `uzi run decide <id> --continue [--guidance <text>]` and the honest
// completion-state rows of `uzi run get`.

// TestRunDecideRequiresContinue: `--continue` is required for this child (only decision
// supported). Its absence is a clean usage error (exit 2) BEFORE any request — the client is
// never called.
func TestRunDecideRequiresContinue(t *testing.T) {
	fc := &uzicli.FakeClient{DecideRun: apitypes.RunDTO{ID: "r1", Status: "running"}}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "decide", "r1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage(%d) (stderr: %s)", code, uzicli.ExitUsage, stderr)
	}
	if fc.LastDecideRunID != "" {
		t.Errorf("decide reached the client (id %q) without --continue; it must fail first", fc.LastDecideRunID)
	}
	if !strings.Contains(stderr, "--continue") {
		t.Errorf("usage error should mention --continue, got: %s", stderr)
	}
}

// TestRunDecidePostsContinue: `run decide <id> --continue` calls ContinueCompletionDecision with
// the run id and an empty guidance, and prints the plain continue confirmation carrying the run's
// new status.
//
// MUTATION PROOF: point the verb at any other client method and LastDecideRunID is no longer set.
func TestRunDecidePostsContinue(t *testing.T) {
	fc := &uzicli.FakeClient{DecideRun: apitypes.RunDTO{ID: "r1", Status: "running"}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "decide", "r1", "--continue")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastDecideRunID != "r1" {
		t.Errorf("decide reached ContinueCompletionDecision with id %q, want %q", fc.LastDecideRunID, "r1")
	}
	if fc.LastDecideGuidance != "" {
		t.Errorf("no --guidance was passed, but guidance %q reached the client", fc.LastDecideGuidance)
	}
	if !strings.Contains(stdout, "Continue recorded for r1: running.") {
		t.Errorf("decide confirmation missing from stdout:\n%s", stdout)
	}
	if strings.Contains(stdout, "guidance recorded") {
		t.Errorf("no guidance was given, so the confirmation must not claim one was recorded:\n%s", stdout)
	}
}

// TestRunDecideCarriesGuidance: `--guidance <text>` is carried verbatim to the client, and the
// confirmation notes that guidance was recorded.
func TestRunDecideCarriesGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{DecideRun: apitypes.RunDTO{ID: "r1", Status: "running"}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "decide", "r1", "--continue", "--guidance", "focus on M4 first")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastDecideGuidance != "focus on M4 first" {
		t.Errorf("guidance reached the client as %q, want %q", fc.LastDecideGuidance, "focus on M4 first")
	}
	if !strings.Contains(stdout, "guidance recorded") {
		t.Errorf("confirmation should note guidance was recorded:\n%s", stdout)
	}
}

// TestRunDecideJSON: `--json` emits the resumed run object (agent contract), not the human line.
func TestRunDecideJSON(t *testing.T) {
	fc := &uzicli.FakeClient{DecideRun: apitypes.RunDTO{ID: "r1", Status: "running"}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "decide", "r1", "--continue", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"status": "running"`) && !strings.Contains(stdout, `"status":"running"`) {
		t.Errorf("--json output should carry the run object, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "Continue recorded") {
		t.Errorf("--json output must not carry the human confirmation line:\n%s", stdout)
	}
}

// TestRunDecideExitCodes: the server's status→exit mapping flows through with no per-command
// logic — a 409 (not completion-blocked) maps to exit 5, a 404 (foreign/unknown) to exit 4. The
// write is still reached with the run id before the error surfaces.
func TestRunDecideExitCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not blocked 409", uzicli.Exitf(uzicli.ExitConflict, "run is not completion-blocked"), uzicli.ExitConflict},
		{"unknown run 404", uzicli.Exitf(uzicli.ExitNotFound, "run not found"), uzicli.ExitNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{ContinueCompletionDecisionErr: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "run", "decide", "r1", "--continue")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
			if fc.LastDecideRunID != "r1" {
				t.Errorf("decide reached ContinueCompletionDecision with id %q, want %q", fc.LastDecideRunID, "r1")
			}
		})
	}
}

// TestCompletionPhaseLabel pins the completion_phase → human label map (the SAME three labels the
// web renders), and that an unrecognised/empty phase maps to "" (the caller then draws no row).
func TestCompletionPhaseLabel(t *testing.T) {
	cases := map[string]string{
		"checking":  "Checking completion",
		"reworking": "Reworking unmet milestones",
		"blocked":   "Completion blocked",
		"":          "",
		"future":    "",
	}
	for phase, want := range cases {
		if got := completionPhaseLabel(phase); got != want {
			t.Errorf("completionPhaseLabel(%q) = %q, want %q", phase, got, want)
		}
	}
}

// TestCompletionRowsEmitWhenSet: an interlocked run in a reworking+held state emits every row —
// the phase label, the unmet ids joined, the attempt count and the same-worker-only hold context.
func TestCompletionRowsEmitWhenSet(t *testing.T) {
	r := apitypes.RunDTO{
		CompletionPhase:    "reworking",
		CompletionUnmet:    []string{"m4", "m5"},
		CompletionAttempts: 2,
		HoldContext:        sp("unavailable(same_worker_only)"),
	}
	rows := completionRows(r)
	got := map[string]string{}
	for _, row := range rows {
		got[row[0]] = row[1]
	}
	want := map[string]string{
		"COMPLETION":          "Reworking unmet milestones",
		"COMPLETION_UNMET":    "m4,m5",
		"COMPLETION_ATTEMPTS": "2",
		"HOLD_CONTEXT":        "unavailable(same_worker_only)",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("completionRows[%s] = %q, want %q (rows=%v)", k, got[k], v, rows)
		}
	}
}

// TestCompletionRowsOmitWhenInert: a NON-interlocked run (rollout OFF) carries every completion
// field at its inert default, so completionRows returns nothing — the detail renders as today.
func TestCompletionRowsOmitWhenInert(t *testing.T) {
	r := apitypes.RunDTO{
		CompletionPhase:    "",
		CompletionUnmet:    []string{},
		CompletionAttempts: 0,
		HoldContext:        nil,
	}
	if rows := completionRows(r); rows != nil {
		t.Errorf("a non-interlocked run must emit no completion rows, got: %v", rows)
	}
}

// TestRunGetRendersCompletionRows: end to end through `uzi run get`, a blocked+held run's detail
// table carries the COMPLETION label, the unmet ids, the attempt count and the HOLD_CONTEXT
// durability limitation (D8 — visible in the CLI).
func TestRunGetRendersCompletionRows(t *testing.T) {
	blocked := apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: statusPaused, Health: "ok",
		CompletionInterlock: true,
		CompletionPhase:     "blocked",
		CompletionUnmet:     []string{"m4"},
		CompletionAttempts:  3,
		HoldReason:          sp("completion_blocked"),
		HoldContext:         sp("unavailable(same_worker_only)"),
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": blocked}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"COMPLETION", "Completion blocked", "COMPLETION_UNMET", "m4", "COMPLETION_ATTEMPTS", "3", "HOLD_CONTEXT", "unavailable(same_worker_only)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`run get` completion block missing %q:\n%s", want, stdout)
		}
	}
}

// TestRunGetOmitsCompletionRows: a non-interlocked run's detail carries NONE of the completion
// rows (the rollout-OFF / legacy case renders exactly as before).
func TestRunGetOmitsCompletionRows(t *testing.T) {
	plain := apitypes.RunDTO{
		ID: "r1", Kind: "issue", Status: "running", Health: "ok",
		CompletionUnmet: []string{},
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": plain}}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, absent := range []string{"COMPLETION", "COMPLETION_UNMET", "COMPLETION_ATTEMPTS", "HOLD_CONTEXT"} {
		if strings.Contains(stdout, absent) {
			t.Errorf("a non-interlocked run must not render %q:\n%s", absent, stdout)
		}
	}
}
