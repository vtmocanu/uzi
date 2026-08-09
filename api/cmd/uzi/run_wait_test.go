package main

import (
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #264 M1 — `uzi run wait <id>` blocks until the run reaches a `--until` state.
// These tests drive the poll loop through FakeClient.GetRunHook, which scripts a
// per-call STATUS SEQUENCE (and injects a transient error), so each acceptance the
// PRD names — immediate return, gate-then-terminal, timeout, not-found, and a
// transient-6 that recovers — is exercised against a real cobra invocation.

// waitStep is one scripted GetRun outcome: a run to return, or an error to fail with.
type waitStep struct {
	run apitypes.RunDTO
	err error
}

// scriptHook returns a GetRunHook that yields each step in turn and REPEATS the last
// one forever, so "always running" / "always unreachable" is a single step and a
// terminating sequence is just its last step being a target/terminal.
func scriptHook(steps ...waitStep) func(string) (apitypes.RunDTO, error) {
	i := 0
	return func(string) (apitypes.RunDTO, error) {
		s := steps[i]
		if i < len(steps)-1 {
			i++
		}
		return s.run, s.err
	}
}

func runAt(status string) apitypes.RunDTO { return apitypes.RunDTO{ID: "r1", Status: status} }

func okStep(status string) waitStep { return waitStep{run: runAt(status)} }

// shrinkWaitTimers makes the transient-blip backoff sub-millisecond for the duration
// of a test, so an exit-6 path (which waits runWaitMaxUnreachable-1 times) is instant.
// The default poll interval is overridden per-call with --interval.
func shrinkWaitTimers(t *testing.T) {
	t.Helper()
	origBackoff := runWaitBackoff
	runWaitBackoff = time.Millisecond
	t.Cleanup(func() { runWaitBackoff = origBackoff })
}

func TestRunWaitImmediateTargetExitsZero(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("completed"))}
	stdout, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty without --json, got %q", stdout)
	}
	if !strings.Contains(stderr, "completed") {
		t.Errorf("stderr should show the observed status, got %q", stderr)
	}
}

func TestRunWaitStopsAtGateThenNarrowedTerminal(t *testing.T) {
	// Default --until stops at the plan gate even though the run keeps working after.
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		okStep("running"), okStep("running"), okStep("awaiting_approval"), okStep("completed"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "running → awaiting_approval") {
		t.Errorf("expected a running→awaiting_approval transition, stderr = %q", stderr)
	}
	// It must STOP at the gate, not merely pass through it: awaiting_approval is a
	// DEFAULT stop-state, so the wait must never reach the later `completed` step. This
	// is what pins gate-membership of the default --until set — a weaker "contains
	// awaiting_approval" assertion passes even if the wait runs on to completed.
	if strings.Contains(stderr, "→ completed") {
		t.Errorf("must stop AT the gate, not run past it to completed; stderr = %q", stderr)
	}
}

func TestRunWaitNarrowedUntilSkipsGate(t *testing.T) {
	// D2 corollary: after approving, narrow to terminals so the lingering gate does
	// not return immediately. Here awaiting_approval must be skipped.
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		okStep("awaiting_approval"), okStep("running"), okStep("completed"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1",
		"--until", "completed,failed,cancelled", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "→ completed") {
		t.Errorf("should have run past the gate to completed, stderr = %q", stderr)
	}
}

func TestRunWaitTimeoutExitsSeven(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1",
		"--interval", "2ms", "--timeout", "15ms")
	if code != uzicli.ExitTimeout {
		t.Fatalf("exit = %d, want 7 (stderr: %s)", code, stderr)
	}
}

func TestRunWaitNotFoundExitsFour(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		waitStep{err: uzicli.Exitf(uzicli.ExitNotFound, "run r1 not found")},
	)}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want 4", code)
	}
}

func TestRunWaitTransientUnreachableRecovers(t *testing.T) {
	shrinkWaitTimers(t)
	// One blip, then the gate — D9: a single ExitUnreachable must not end the wait.
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		waitStep{err: uzicli.Exitf(uzicli.ExitUnreachable, "cannot reach uzi")},
		okStep("awaiting_approval"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "retry 1/") {
		t.Errorf("expected a retry notice on the transient blip, stderr = %q", stderr)
	}
}

func TestRunWaitUnreachableCounterIsConsecutiveNotCumulative(t *testing.T) {
	shrinkWaitTimers(t)
	// Five blips TOTAL but never five in a row — a successful poll between them must
	// reset the counter, so the wait survives to the gate. If the counter were
	// cumulative it would hit runWaitMaxUnreachable (5) and wrongly exit 6.
	blip := waitStep{err: uzicli.Exitf(uzicli.ExitUnreachable, "cannot reach uzi")}
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		blip, okStep("running"), blip, okStep("running"), blip, okStep("running"),
		blip, okStep("running"), blip, okStep("awaiting_approval"),
	)}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 — interspersed blips must not accumulate (stderr: %s)", code, stderr)
	}
}

func TestRunWaitUnreachableExhaustsToSix(t *testing.T) {
	shrinkWaitTimers(t)
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(
		waitStep{err: uzicli.Exitf(uzicli.ExitUnreachable, "cannot reach uzi")},
	)}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitUnreachable {
		t.Fatalf("exit = %d, want 6", code)
	}
}

func TestRunWaitJSONPrintsFinalRunObject(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), okStep("completed"))}
	stdout, _, code := runCLI(t, fakeEnv(fc), "--json", "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// Same top-level shape as `run get --json`: a bare run object carrying the id and
	// final status on stdout.
	if !strings.Contains(stdout, `"id": "r1"`) || !strings.Contains(stdout, `"status": "completed"`) {
		t.Errorf("stdout should be the final run object, got %q", stdout)
	}
}

func TestRunWaitUnknownUntilIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("completed"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--until", "done")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
	}
}

func TestRunWaitUnknownStatusSurfacedAndNonTerminal(t *testing.T) {
	// A status outside the nine-value enum must be surfaced and NOT treated as a stop
	// state (it is never in --until), so the wait continues to a real target.
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("teleporting"), okStep("completed"))}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "unrecognized status") {
		t.Errorf("expected an unrecognized-status notice, stderr = %q", stderr)
	}
}
