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
	if !strings.Contains(stderr, "review revision 3") {
		t.Errorf("stderr does not name the current revision: %q", stderr)
	}
}

// A run that left the gate at the same revision (approved elsewhere, or finished) is a mismatch
// too; the message says the gate is gone rather than claiming a newer plan exists.
func TestGateVerdictMismatchOffTheGateSaysSo(t *testing.T) {
	fc := gateFake("awaiting_approval", 2)
	fc.SubmitRunInputErr = gateMismatchErr(2)
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "reject", "r1", "-m", "no")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if !strings.Contains(stderr, "no longer waiting for a verdict on plan revision 2") {
		t.Errorf("stderr = %q", stderr)
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
