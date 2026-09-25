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

// TestRunRecoveryJSONCaptures (issue #1417) proves --json attaches each hold's captures, in
// archive order, joined on hold_id: a capture reserved under another hold, or with no hold_id
// (an older server), is attached nowhere, and the hold's own keys stay flat.
func TestRunRecoveryJSONCaptures(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: recoveryHoldsFixture(),
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"run1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				{ID: "cap-1", HoldID: "hold-run1-gen1", State: "available", SourceSha: "aaaa", ByteSize: i64(8)},
				{ID: "cap-other", HoldID: "hold-run1-gen0", State: "expired", SourceSha: "bbbb"},
				{ID: "cap-2", HoldID: "hold-run1-gen1", State: "needs_action", SourceSha: "cccc"},
				{ID: "cap-legacy", State: "available", SourceSha: "dddd"},
			}},
		},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb)
	}
	var holds []struct {
		ID        string `json:"id"`
		Attention string `json:"attention"`
		Captures  []struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			SourceSha string `json:"source_sha"`
			ByteSize  *int64 `json:"byte_size"`
		} `json:"captures"`
	}
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 1 || holds[0].ID != "hold-run1-gen1" || holds[0].Attention != "source_only" {
		t.Fatalf("run recovery --json = %+v, want run1's single hold with its flat keys", holds)
	}
	caps := holds[0].Captures
	if len(caps) != 2 || caps[0].ID != "cap-1" || caps[1].ID != "cap-2" {
		t.Fatalf("captures = %+v, want exactly cap-1 then cap-2", caps)
	}
	if caps[0].State != "available" || caps[0].SourceSha != "aaaa" || caps[0].ByteSize == nil || *caps[0].ByteSize != 8 {
		t.Errorf("cap-1 metadata wrong: %+v", caps[0])
	}
	if caps[1].ByteSize != nil {
		t.Errorf("cap-2 has no byte size, want byte_size omitted; got %d", *caps[1].ByteSize)
	}
}

// TestRunRecoveryJSONNoCaptures proves a hold with no captures carries "captures": [] (never
// null), so a consuming agent iterates it unconditionally.
func TestRunRecoveryJSONNoCaptures(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"captures": []`) {
		t.Errorf("a capture-less hold should emit \"captures\": []; got:\n%s", out)
	}
}

// TestRunRecoveryJSONArchivesError proves a failed archives read fails --json rather than
// emitting holds with silently empty captures.
func TestRunRecoveryJSONArchivesError(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: recoveryHoldsFixture(),
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives read failed"),
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code == uzicli.ExitOK {
		t.Fatalf("exit = 0 on an archives read error, want non-zero; stdout=%q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("a failed archives read must print no holds; stdout=%q", out)
	}
}

// TestRunRecoveryNoArchivesReadWithoutNeed proves the archives are read only when they are
// joined: the human table and a --json run with no holds both succeed with the read failing.
func TestRunRecoveryNoArchivesReadWithoutNeed(t *testing.T) {
	for _, args := range [][]string{
		{"run", "recovery", "run1"},
		{"run", "recovery", "run-none", "--json"},
	} {
		fc := &uzicli.FakeClient{
			RecoveryHoldsResult: recoveryHoldsFixture(),
			RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives read failed"),
		}
		if _, errb, code := runCLI(t, fakeEnv(fc), args...); code != uzicli.ExitOK {
			t.Errorf("%v: exit = %d, want 0 (no archives read needed); stderr=%q", args, code, errb)
		}
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
