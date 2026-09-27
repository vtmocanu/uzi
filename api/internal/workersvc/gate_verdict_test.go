package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestApproveClearRevision(t *testing.T) {
	for _, c := range []struct {
		name    string
		row     store.RunUserInput
		wantRev int64
		wantOK  bool
	}{
		{"legacy approve clears at revision 0", store.RunUserInput{}, 0, true},
		{"bound approve clears at its revision", store.RunUserInput{
			GateBinding: pgtype.Text{String: "bound", Valid: true}, GateRevision: pgtype.Int8{Int64: 4, Valid: true},
		}, 4, true},
		{"unbound approve clears nothing", store.RunUserInput{GateBinding: pgtype.Text{String: "unbound", Valid: true}}, 0, false},
		{"bound without a revision fails closed", store.RunUserInput{GateBinding: pgtype.Text{String: "bound", Valid: true}}, 0, false},
		{"unknown binding fails closed", store.RunUserInput{GateBinding: pgtype.Text{String: "maybe", Valid: true}}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			rev, ok := approveClearRevision(c.row)
			if rev != c.wantRev || ok != c.wantOK {
				t.Fatalf("approveClearRevision = (%d, %v), want (%d, %v)", rev, ok, c.wantRev, c.wantOK)
			}
		})
	}
}

// An unbound approve (sent while the run showed no gate) with the capability override enqueues
// the approve but never clears the run's requirement.
func TestOverrideApproveUnboundClearsNothing(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.Status = "running"
	fs.gateVerdictRow = store.RunUserInput{GateBinding: pgtype.Text{String: "unbound", Valid: true}}
	if _, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "approve_plan", "", nil, SubmitInputOptions{OverrideCapabilities: true}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdGateVerdict == nil {
		t.Fatal("the approve must still be enqueued")
	}
	if fs.clearedCaps != nil {
		t.Fatalf("an unbound approve must not clear required capabilities: %+v", fs.clearedCaps)
	}
}

// A bound approve's override clear is scoped to the revision the insert returned.
func TestOverrideApproveBoundClearsAtItsRevision(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.gateVerdictRow = store.RunUserInput{GateBinding: pgtype.Text{String: "bound", Valid: true}, GateRevision: pgtype.Int8{Int64: 7, Valid: true}}
	if _, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "approve_plan", "", nil, SubmitInputOptions{OverrideCapabilities: true}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.clearedCaps == nil || fs.clearedCaps.GateRevision != 7 {
		t.Fatalf("clear = %+v, want scoped to revision 7", fs.clearedCaps)
	}
}

func TestExpectedGateRevisionPrecheck(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.GateRevision = 2
	rev := int64(1)
	_, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: &rev})
	var mm *GateRevisionMismatchError
	if !errors.As(err, &mm) || mm.Current != 2 || mm.Expected != 1 {
		t.Fatalf("err = %v, want *GateRevisionMismatchError{Expected 1, Current 2}", err)
	}
	if fs.createdGateVerdict != nil || fs.createdApproval != nil {
		t.Fatal("a mismatched approve must write nothing")
	}
	if _, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "follow_up", "x", nil, SubmitInputOptions{ExpectedGateRevision: &rev}); !errors.Is(err, ErrExpectedGateRevisionNotApplicable) {
		t.Fatalf("follow_up err = %v, want ErrExpectedGateRevisionNotApplicable", err)
	}
}
