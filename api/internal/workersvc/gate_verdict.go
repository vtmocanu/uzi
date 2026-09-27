package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1795 M2: plan-gate verdicts are stamped with the gate revision they were sent against,
// and a client may name the revision it displayed (expected_gate_revision, D5).
//
// Every insertion seam of an approve_plan / reject_plan / revise_plan row computes the binding
// inside the insert statement from the locked run row (decision 4):
//
//   - CreateGateVerdictInput        selection-less approve_plan (SELECT ... FOR UPDATE CTE)
//   - CreateApprovePlanInput        selection-bearing approve_plan (UPDATE CTE)
//   - CreateStopVerdictInput        reject_plan with a live poller (UPDATE CTE; cancel/stop stay NULL)
//   - CreateRunReviseInputIfUnderCap revise_plan (UPDATE CTE)
//
// RejectRunServerSide (a reject with no live poller) writes no row but carries the same
// expected-revision predicate, so a stale reject cannot fail the run.

// ErrExpectedGateRevisionNotApplicable answers an expected_gate_revision on a kind that is not
// a plan-gate verdict (400): only approve_plan, reject_plan and revise_plan act on a gate.
var ErrExpectedGateRevisionNotApplicable = errors.New("expected_gate_revision is only valid with approve_plan, reject_plan or revise_plan")

// GateRevisionMismatchError answers a verdict whose expected_gate_revision is not the run's
// current plan gate: the run is not awaiting_approval, or it is at another revision. Nothing
// was written. Current is the run's gate revision at the re-read (0 when it never published a
// gate under an allocating api). The handler maps it to 409 reason gate_revision_mismatch.
type GateRevisionMismatchError struct {
	Expected int64
	Current  int64
}

func (e *GateRevisionMismatchError) Error() string {
	return fmt.Sprintf("the plan gate changed: this verdict was sent for revision %d, the run is at revision %d", e.Expected, e.Current)
}

// gateVerdictKind reports whether kind is a plan-gate verdict (the kinds stamped with a gate
// binding and the only ones an expected revision applies to).
func gateVerdictKind(kind string) bool {
	switch kind {
	case "approve_plan", "reject_plan", "revise_plan":
		return true
	default:
		return false
	}
}

// checkExpectedGateRevision returns a *GateRevisionMismatchError unless run sits at an
// awaiting_approval gate of exactly the expected revision.
func checkExpectedGateRevision(run store.Run, expected int64) error {
	if run.Status == "awaiting_approval" && run.GateRevision == expected {
		return nil
	}
	return &GateRevisionMismatchError{Expected: expected, Current: run.GateRevision}
}

// gateRevisionMismatch resolves a revise_plan write that affected no row, where a second cause
// (the revision cap) can refuse a verdict whose expected revision still matches. With no expected
// revision it returns fallback (the cap). With one it re-reads the run: a mismatch is answered as
// *GateRevisionMismatchError; a run that does match keeps fallback, because the cap refused it.
func (s *Service) gateRevisionMismatch(ctx context.Context, userID, runID uuid.UUID, expected *int64, fallback error) error {
	if expected == nil {
		return fallback
	}
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	if err := checkExpectedGateRevision(run, *expected); err != nil {
		return err
	}
	return fallback
}

// verdictNotWritten resolves a verdict write that affected no row on a seam whose only refusal
// besides the expected-revision predicate is the run having vanished or finished (approve,
// reject with or without a live poller). It re-reads the run, so a vanished run answers
// ErrRunNotFound. With an expected revision the answer is ALWAYS a *GateRevisionMismatchError
// carrying the re-read's revision, even when that revision matches again (a publication and a
// verdict can interleave around the re-read): the verdict was not written, and the client must
// refetch rather than read success. Without one it returns fallback, the seam's own answer.
func (s *Service) verdictNotWritten(ctx context.Context, userID, runID uuid.UUID, expected *int64, fallback error) error {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	if expected == nil {
		return fallback
	}
	return &GateRevisionMismatchError{Expected: *expected, Current: run.GateRevision}
}

// approveClearRevision is the gate revision an approve's capability-override clear is scoped to:
// the bound revision, or 0 for a legacy (NULL-binding) approve. An unbound approve was sent while
// no gate was visible and can never act on one, so it clears nothing (ok=false).
func approveClearRevision(row store.RunUserInput) (int64, bool) {
	switch {
	case row.GateBinding.Valid && row.GateBinding.String == "unbound":
		return 0, false
	case row.GateBinding.Valid && row.GateBinding.String == "bound":
		// The CHECK guarantees a positive revision; fail closed if it were ever absent.
		return row.GateRevision.Int64, row.GateRevision.Valid
	case row.GateBinding.Valid:
		// An unknown binding value (the CHECK forbids it): fail closed.
		return 0, false
	default:
		return 0, true
	}
}
