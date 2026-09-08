package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1190 M4 — `uzi run wait` treats `paused` as a recognised NON-TERMINAL status: it
// keeps waiting through it (an owner park is not the end of the run) and prints one clean
// `paused` transition line, NOT the "unrecognized status" warning an older-than-server CLI
// would print.

// TestRunWaitKeepsWaitingOnPaused: a run that goes running → paused → completed under a bare
// wait must print the paused transition, keep waiting through the park, and stop at completed.
// MUTATION PROOF: drop `paused` from allRunStatusesOrder and the "unrecognized status" warning
// fires (this test's assertion that it is ABSENT reddens); add paused to defaultWaitStates and
// the wait stops at paused (the "→ completed" assertion reddens).
func TestRunWaitKeepsWaitingOnPaused(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		okStep("running"), okStep("paused"), okStep("completed"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "running → paused") {
		t.Errorf("expected a running→paused transition line, stderr = %q", stderr)
	}
	if strings.Contains(stderr, "unrecognized status") {
		t.Errorf("paused is a recognised status — the older-than-server warning must not fire, stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "→ completed") {
		t.Errorf("wait must keep going THROUGH the paused park to completed, stderr = %q", stderr)
	}
}

// TestRunWaitUntilPausedIsAValidTarget: `--until paused` is accepted (paused is a known
// status) and the wait stops the instant the run is paused.
func TestRunWaitUntilPausedIsAValidTarget(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		okStep("running"), okStep("paused"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--until", "paused", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s); --until paused must be a valid target", code, stderr)
	}
	if !strings.Contains(stderr, "running → paused") {
		t.Errorf("expected a running→paused transition, stderr = %q", stderr)
	}
}
