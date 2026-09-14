package main

// run_recovery_test.go covers `uzi run recovery` and `uzi run discard` (PRD #1349 M5, D7/D9):
// the owner custody-hold LIST render and run filter, and the exact hold DISCARD's confirmation
// contract — interactive prompt, cancellation performing NO mutation, non-TTY hard-refusal
// without --yes, and --yes mapping straight to the discard call.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func recoveryHoldsFixture() apitypes.RecoveryCustodyHoldsDTO {
	return apitypes.RecoveryCustodyHoldsDTO{
		Aggregate: apitypes.RecoveryCustodyAggregateDTO{OpenHolds: 2, CustodyHoldLimit: 8, DecisionNeeded: 1, BlockedRuns: 0},
		Holds: []apitypes.RecoveryCustodyHoldDTO{
			{ID: "hold-run1-gen1", RunID: "run1", Generation: 1, State: "open", Attention: "source_only",
				WorkerID: "w1", WorkerName: "alpha", CaptureState: ""},
			{ID: "hold-run2-gen1", RunID: "run2", Generation: 1, State: "open", Attention: "active",
				WorkerID: "w2", WorkerName: "beta"},
		},
	}
}

// TestRunRecoveryRenders proves `uzi run recovery <run-id>` renders the run's holds — the exact
// hold id, generation and disposition — and filters OUT holds belonging to other runs.
func TestRunRecoveryRenders(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "hold-run1-gen1") || !strings.Contains(out, "source_only") {
		t.Errorf("run recovery output missing the run's hold id/disposition: %q", out)
	}
	// The other run's hold must NOT appear — the endpoint is owner-wide, the command narrows.
	if strings.Contains(out, "hold-run2-gen1") {
		t.Errorf("run recovery leaked another run's hold: %q", out)
	}
}

// TestRunRecoveryJSON proves --json emits the run-filtered raw hold DTOs (never null), and only
// the requested run's holds.
func TestRunRecoveryJSON(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var holds []apitypes.RecoveryCustodyHoldDTO
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 1 || holds[0].ID != "hold-run1-gen1" {
		t.Errorf("run recovery --json = %+v, want only run1's single hold", holds)
	}
}

// TestRunRecoveryEmptyJSON proves --json emits [] (never null) for a run with no holds.
func TestRunRecoveryEmptyJSON(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run-none", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("run recovery --json for a run with no holds = %q, want []", strings.TrimSpace(out))
	}
}

// TestRunDiscardRequiresHold proves `uzi run discard <run-id>` with no --hold is a usage error
// that mutates nothing.
func TestRunDiscardRequiresHold(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "discard", "run1", "--yes")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Errorf("missing --hold must not attempt a discard; got %+v", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardYesMapsToDiscard proves --yes discards exactly the named (run, hold) with no
// prompt — the unattended path.
func TestRunDiscardYesMapsToDiscard(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "discard", "run1", "--hold", "hold-x", "--yes")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 1 || fc.DiscardHoldCalls[0] != (uzicli.DiscardHoldCall{RunID: "run1", HoldID: "hold-x"}) {
		t.Fatalf("discard calls = %+v, want exactly one (run1, hold-x)", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardNonTTYRefusesWithoutYes proves that without --yes and without a TTY the command
// HARD-REFUSES (usage error) and mutates NOTHING — a possible only copy is never destroyed
// unattended (D9).
func TestRunDiscardNonTTYRefusesWithoutYes(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = false // no terminal
	_, errb, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage refusal)", code, uzicli.ExitUsage)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Errorf("a non-TTY refusal must mutate nothing; got %+v", fc.DiscardHoldCalls)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("non-TTY refusal should tell the user to pass --yes; stderr=%q", errb)
	}
}

// TestRunDiscardConfirmCancelled proves that a declined interactive prompt (a bare "n") performs
// NO mutation and exits 0.
func TestRunDiscardConfirmCancelled(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("n\n")
	out, errb, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 on a declined prompt\nstderr=%q", code, errb)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Fatalf("a cancelled prompt must perform NO mutation; got %+v", fc.DiscardHoldCalls)
	}
	if !strings.Contains(errb, "aborted") {
		t.Errorf("a declined discard should say it aborted; stderr=%q stdout=%q", errb, out)
	}
}

// TestRunDiscardConfirmAccepted proves that an accepted interactive prompt ("y") discards the
// exact hold.
func TestRunDiscardConfirmAccepted(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("y\n")
	_, _, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 1 || fc.DiscardHoldCalls[0].HoldID != "hold-x" {
		t.Fatalf("accepted prompt should discard hold-x once; got %+v", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardConfirmEOFDeclines proves that EOF on stdin (an empty piped confirmation under a
// TTY-claimed env) declines rather than proceeds — the safe default for a destructive action.
func TestRunDiscardConfirmEOFDeclines(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("") // immediate EOF
	_, _, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Fatalf("EOF must decline and mutate nothing; got %+v", fc.DiscardHoldCalls)
	}
}
