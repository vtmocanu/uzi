package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1795 M4 (D5): `uzi run approve|reject|revise` bind the verdict to a plan-gate revision.
// With --expected-gate-revision the caller names it; without the flag the run is read once at
// invocation and its revision is sent ONLY while it is awaiting approval. A 409
// gate_revision_mismatch exits ExitConflict (5) naming the run's current revision.

func gateFake(status string, rev int64) *uzicli.FakeClient {
	return &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Status: status, GateRevision: rev},
	}}
}

func gateMismatchErr(current int64) error {
	return &uzicli.ExitError{
		Code:                uzicli.ExitConflict,
		Err:                 errors.New("the plan gate changed"),
		Reason:              uzicli.ReasonGateRevisionMismatch,
		CurrentGateRevision: current,
	}
}

func TestGateVerdictDefaultsToTheRevisionAtInvocation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		kind string
	}{
		{"approve", []string{"run", "approve", "r1"}, kindApprovePlan},
		{"reject", []string{"run", "reject", "r1", "-m", "no"}, kindRejectPlan},
		{"revise", []string{"run", "revise", "r1", "-m", "split M2"}, kindRevisePlan},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := gateFake("awaiting_approval", 2)
			_, stderr, code := runCLI(t, fakeEnv(fc), tc.args...)
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
			}
			if fc.LastInputKind != tc.kind {
				t.Fatalf("kind = %q, want %q", fc.LastInputKind, tc.kind)
			}
			if fc.LastInputExpectedGateRevision == nil || *fc.LastInputExpectedGateRevision != 2 {
				t.Errorf("expected_gate_revision = %v, want 2", fc.LastInputExpectedGateRevision)
			}
		})
	}
}

// A run that is not at the gate sends no revision, so a reject of a queued or running run keeps
// the legacy server-side handling (stops it at its first gate) instead of a 409.
func TestGateVerdictOmitsRevisionOffTheGate(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		fc := gateFake(status, 2)
		_, stderr, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1", "-m", "stop")
		if code != uzicli.ExitOK {
			t.Fatalf("%s: exit = %d, want 0 (stderr: %s)", status, code, stderr)
		}
		// Positive control that the reject was sent at all, so the nil below is meaningful.
		if fc.LastInputKind != kindRejectPlan {
			t.Fatalf("%s: kind = %q, want %q", status, fc.LastInputKind, kindRejectPlan)
		}
		if fc.LastInputExpectedGateRevision != nil {
			t.Errorf("%s: expected_gate_revision = %d, want omitted", status, *fc.LastInputExpectedGateRevision)
		}
	}
}

// A legacy gate (no revision allocated) sends none either.
func TestGateVerdictOmitsRevisionForALegacyGate(t *testing.T) {
	fc := gateFake("awaiting_approval", 0)
	if _, stderr, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindApprovePlan {
		t.Fatalf("kind = %q, want %q", fc.LastInputKind, kindApprovePlan)
	}
	if fc.LastInputExpectedGateRevision != nil {
		t.Errorf("expected_gate_revision = %d, want omitted", *fc.LastInputExpectedGateRevision)
	}
}

// The flag wins over the run's current revision, including on a run the read would not bind.
func TestGateVerdictExplicitFlagWins(t *testing.T) {
	cases := [][]string{
		{"run", "approve", "r1", "--expected-gate-revision", "7"},
		{"run", "reject", "r1", "-m", "no", "--expected-gate-revision", "7"},
		{"run", "revise", "r1", "-m", "more", "--expected-gate-revision", "7"},
	}
	for _, args := range cases {
		fc := gateFake("running", 2)
		if _, stderr, code := runCLI(t, fakeEnv(fc), args...); code != uzicli.ExitOK {
			t.Fatalf("%v: exit = %d, want 0 (stderr: %s)", args, code, stderr)
		}
		if fc.LastInputExpectedGateRevision == nil || *fc.LastInputExpectedGateRevision != 7 {
			t.Errorf("%v: expected_gate_revision = %v, want 7", args, fc.LastInputExpectedGateRevision)
		}
	}
}

func TestGateVerdictNegativeFlagIsUsageError(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	_, _, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--expected-gate-revision", "-1")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastInputKind != "" {
		t.Errorf("a usage error still submitted kind %q", fc.LastInputKind)
	}
}

// The mismatch exits 5 and names the revision to review.
func TestGateVerdictMismatchExitsConflictNamingCurrentRevision(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	fc.SubmitRunInputErr = gateMismatchErr(3)
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if !strings.Contains(stderr, "now shows plan revision 3") {
		t.Errorf("stderr does not name the current revision: %q", stderr)
	}
}

// expected == current: the write lost a race at the same revision (the gate was re-presented or
// left while the verdict was in flight). The run may still be at that gate, so the message tells
// the owner to re-check and retry; it must not claim the plan changed or that the run left.
func TestGateVerdictMismatchAtTheSameRevisionSaysRetry(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	fc.SubmitRunInputErr = gateMismatchErr(2)
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1", "-m", "no")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if !strings.Contains(stderr, "plan revision 2 was not applied") || !strings.Contains(stderr, "retry") {
		t.Errorf("stderr does not tell the owner to re-check and retry: %q", stderr)
	}
	for _, claim := range []string{"plan changed", "no longer waiting", "review revision"} {
		if strings.Contains(stderr, claim) {
			t.Errorf("stderr claims %q, which a same-revision race does not support: %q", claim, stderr)
		}
	}
}

// An explicit flag against a run that shows no revision (current 0) must not ask the owner to
// "review revision 0": there is no plan gate to review.
func TestGateVerdictMismatchAtRevisionZeroSaysNoGate(t *testing.T) {
	fc := gateFake("running", 0)
	fc.SubmitRunInputErr = gateMismatchErr(0)
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--expected-gate-revision", "4")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if strings.Contains(stderr, "revision 0") {
		t.Errorf("stderr names revision 0: %q", stderr)
	}
	if !strings.Contains(stderr, "shows no plan gate") {
		t.Errorf("stderr does not say no plan gate is shown: %q", stderr)
	}
}

// `run approve --token` reads the revision FIRST, then switches the token, then approves: the
// revision must name the plan that was at the gate when the owner decided, before a switch that
// could take a while. A 409 after the switch landed must say the switch was applied (it is not
// rolled back), so the owner re-runs without repeating it.
func TestGateApproveWithTokenOrderAndMismatchSaysSwitchApplied(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	fc.SubmitRunInputErr = gateMismatchErr(3)
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "approve", "r1", "--token", "auto")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	want := []string{"get_run", "set_run_credential", "submit_run_input"}
	if strings.Join(fc.RunVerbCalls, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", fc.RunVerbCalls, want)
	}
	if fc.LastInputExpectedGateRevision == nil || *fc.LastInputExpectedGateRevision != 2 {
		t.Errorf("expected_gate_revision = %v, want 2", fc.LastInputExpectedGateRevision)
	}
	if !strings.Contains(stderr, "review it before deciding") {
		t.Errorf("stderr lost the mismatch guidance: %q", stderr)
	}
	if !strings.Contains(stderr, "--token switch WAS applied") || !strings.Contains(stderr, "without --token") {
		t.Errorf("stderr does not say the token switch was applied: %q", stderr)
	}

	// Control: the same mismatch without --token carries no token note.
	fc = gateFake("awaiting_approval", 2)
	fc.SubmitRunInputErr = gateMismatchErr(3)
	_, stderr, _ = runCLI(t, fakeEnv(fc), "run", "approve", "r1")
	if strings.Contains(stderr, "--token") {
		t.Errorf("a plain approve's mismatch mentions --token: %q", stderr)
	}
}

// A run the invocation-time read cannot find sends the verdict without an expected revision; the
// submit answers the authoritative 404 (here the fake accepts it, proving it was sent).
func TestGateVerdictGetRunNotFoundSendsWithoutRevision(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "approve", "r-missing")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != kindApprovePlan || fc.LastInputRunID != "r-missing" {
		t.Fatalf("the verdict was not sent (kind %q, run %q)", fc.LastInputKind, fc.LastInputRunID)
	}
	if fc.LastInputExpectedGateRevision != nil {
		t.Errorf("expected_gate_revision = %d, want omitted", *fc.LastInputExpectedGateRevision)
	}
}

// Any other read failure aborts before sending: a verdict must not go out unbound because the
// read that would have bound it failed.
func TestGateVerdictGetRunErrorAborts(t *testing.T) {
	fc := &uzicli.FakeClient{GetRunHook: func(string) (apitypes.RunDTO, error) {
		return apitypes.RunDTO{}, uzicli.Exitf(uzicli.ExitUnreachable, "server unreachable")
	}}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "revise", "r1", "-m", "more")
	if code != uzicli.ExitUnreachable {
		t.Fatalf("exit = %d, want ExitUnreachable %d (stderr: %s)", code, uzicli.ExitUnreachable, stderr)
	}
	if fc.LastInputKind != "" {
		t.Errorf("a failed read still submitted kind %q", fc.LastInputKind)
	}
}

// `uzi run get --field gate_revision` reads the revision through the generic field projection,
// including the zero value the DTO omits (omitempty) on a run with no gate yet.
func TestRunGetFieldGateRevision(t *testing.T) {
	stdout, stderr, code := runCLI(t, fakeEnv(gateFake("awaiting_approval", 3)), "run", "get", "r1", "--field", "gate_revision")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stdout != "3\n" {
		t.Errorf("stdout = %q, want %q", stdout, "3\n")
	}

	stdout, stderr, code = runCLI(t, fakeEnv(gateFake("queued", 0)), "run", "get", "r1", "--field", "gate_revision")
	if code != uzicli.ExitOK {
		t.Fatalf("zero revision: exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stdout != "0\n" {
		t.Errorf("zero revision: stdout = %q, want %q", stdout, "0\n")
	}
}
