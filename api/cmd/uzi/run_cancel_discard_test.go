package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRunCancelDiscardPendingOutcomeSendsBit: PRD #1391 Run B M3d (D13) — `uzi run cancel <id>
// --discard-pending-outcome` threads the discard bit onto the /inputs request so the server takes
// the atomic no-live-poller branch that discards a terminal outcome held on the worker.
func TestRunCancelDiscardPendingOutcomeSendsBit(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1", "--discard-pending-outcome")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputKind != "cancel" {
		t.Errorf("submit-input kind = %q, want %q", fc.LastInputKind, "cancel")
	}
	if !fc.LastInputDiscardPendingOutcome {
		t.Error("--discard-pending-outcome did not thread the discard bit onto the request")
	}
}

// TestRunCancelWithoutDiscardFlagSendsFalse: the control — a plain `uzi run cancel <id>` carries
// discard_pending_outcome=false, so the default cancel is backward compatible.
func TestRunCancelWithoutDiscardFlagSendsFalse(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if fc.LastInputDiscardPendingOutcome {
		t.Error("a plain cancel must not set the discard bit")
	}
}

// TestRunCancelPendingOutcomeGuidance: a cancel refused with a typed 409 whose reason is
// outcome_pending_confirmation_required (the server's confirmation gate) prints a clear guidance
// line naming --discard-pending-outcome and exits non-zero (ExitConflict), rather than surfacing
// the raw server prose.
func TestRunCancelPendingOutcomeGuidance(t *testing.T) {
	fc := &uzicli.FakeClient{Err: &uzicli.ExitError{
		Code:   uzicli.ExitConflict,
		Err:    errors.New("cancel refused: this run has a pending outcome held on its worker"),
		Reason: uzicli.ReasonOutcomePendingConfirmationRequired,
	}}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "cancel", "r1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict %d (stderr: %s)", code, uzicli.ExitConflict, stderr)
	}
	if !strings.Contains(stderr, "--discard-pending-outcome") {
		t.Errorf("guidance did not name the flag; stderr = %q", stderr)
	}
}
