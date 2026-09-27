package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// staleGatedRun is gatedRun with no live poller (the worker never heartbeated) and a scope
// directive, so a server-side reject that fell through to the failed fan-out would be visible as
// a SettleScopeInputDisposition call.
func staleGatedRun(t *testing.T) (*fakeStore, *Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	fs, svc, user, runID := gatedRun(t)
	fs.workerByID.LastHeartbeatAt = pgtype.Timestamptz{}
	fs.runByID.ScopeCeiling = pgtype.Int4{Int32: 1, Valid: true}
	fs.runByID.GateRevision = 1
	return fs, svc, user, runID
}

// A server-side reject whose RejectRunServerSide matched 0 rows failed nothing: it must never
// fall through to the failed fan-out or report ServerSide success. With an expected revision the
// answer is a mismatch carrying the re-read's revision EVEN WHEN that re-read matches (the store
// wrapper forces 0 rows while the run still shows the expected gate); without one it is the
// terminal 409.
func TestServerSideRejectZeroRowsNeverFansOut(t *testing.T) {
	zero := int64(0)
	t.Run("with an expected revision whose re-read matches", func(t *testing.T) {
		fs, svc, user, runID := staleGatedRun(t)
		fs.rejectRows = &zero
		rev := int64(1)
		res, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "reject_plan", "no", nil, SubmitInputOptions{ExpectedGateRevision: &rev})
		var mm *GateRevisionMismatchError
		if !errors.As(err, &mm) || mm.Current != 1 || mm.Expected != 1 {
			t.Fatalf("err = %v (res %+v), want *GateRevisionMismatchError{Expected 1, Current 1}", err, res)
		}
		if fs.rejected == nil {
			t.Fatal("precondition: the server-side reject must have been attempted")
		}
		if res.ServerSide || fs.settledScope != nil {
			t.Fatalf("a 0-row reject fanned out as failed: res %+v settled %+v", res, fs.settledScope)
		}
	})
	t.Run("without an expected revision", func(t *testing.T) {
		fs, svc, user, runID := staleGatedRun(t)
		fs.rejectRows = &zero
		res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "no", nil)
		if !errors.Is(err, ErrRunTerminal) {
			t.Fatalf("err = %v (res %+v), want ErrRunTerminal", err, res)
		}
		if res.ServerSide || fs.settledScope != nil {
			t.Fatalf("a 0-row reject fanned out as failed: res %+v settled %+v", res, fs.settledScope)
		}
	})
	t.Run("control: an applied reject still fans out", func(t *testing.T) {
		fs, svc, user, runID := staleGatedRun(t)
		res, err := svc.SubmitInput(context.Background(), user, runID, "reject_plan", "no", nil)
		if err != nil || !res.ServerSide || fs.settledScope == nil {
			t.Fatalf("applied reject: err %v res %+v settled %+v, want ServerSide and a settled scope", err, res, fs.settledScope)
		}
	})
}

// An approve or live-poller reject whose insert matched 0 rows with an expected revision is a
// mismatch (409), never a 404 for a run that exists, even when the re-read matches again.
func TestVerdictZeroRowsWithMatchingRereadIsMismatch(t *testing.T) {
	own := &AgentSelection{Source: AgentSourceOwn}
	for _, c := range []struct {
		name string
		kind string
		sel  *AgentSelection
		arm  func(*fakeStore)
	}{
		{"approve (no selection)", "approve_plan", nil, func(f *fakeStore) { f.gateVerdictErr = pgx.ErrNoRows }},
		{"approve (selection)", "approve_plan", own, func(f *fakeStore) { f.approvalErr = pgx.ErrNoRows }},
		{"reject (live poller)", "reject_plan", nil, func(f *fakeStore) { f.stopVerdictErr = pgx.ErrNoRows }},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs, svc, user, runID := gatedRun(t)
			fs.runByID.GateRevision = 3
			c.arm(fs)
			rev := int64(3)
			_, err := svc.SubmitInputWithOptions(context.Background(), user, runID, c.kind, "", c.sel, SubmitInputOptions{ExpectedGateRevision: &rev})
			var mm *GateRevisionMismatchError
			if !errors.As(err, &mm) || mm.Current != 3 {
				t.Fatalf("err = %v, want *GateRevisionMismatchError{Current 3}", err)
			}
			if errors.Is(err, ErrRunNotFound) {
				t.Fatal("an existing run answered not found")
			}
		})
	}
}

// Plan decision 10: with expected_gate_revision present on a verdict kind, a finished run answers
// the typed mismatch (current revision included), not the untyped terminal 409.
func TestExpectedGateRevisionOnTerminalRunIsMismatch(t *testing.T) {
	for _, status := range []string{"failed", "completed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			fs, svc, user, runID := gatedRun(t)
			fs.runByID.Status = status
			fs.runByID.GateRevision = 2
			rev := int64(2)
			_, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: &rev})
			var mm *GateRevisionMismatchError
			if !errors.As(err, &mm) || mm.Current != 2 {
				t.Fatalf("err = %v, want *GateRevisionMismatchError{Current 2}", err)
			}
			if _, err := svc.SubmitInput(context.Background(), user, runID, "approve_plan", "", nil); !errors.Is(err, ErrRunTerminal) {
				t.Fatalf("without an expected revision err = %v, want ErrRunTerminal", err)
			}
		})
	}
}

// The capability-override clear on the SELECTION-bearing approve path is scoped to the revision
// CreateApprovePlanInput bound the row to. On a revision-1 gate a clear at revision 0 (a lost
// binding) is the real query's silent no-op, which the fake mirrors.
func TestOverrideApproveWithSelectionClearsAtBoundRevision(t *testing.T) {
	fs, svc, user, runID := gatedRun(t)
	fs.runByID.GateRevision = 1
	fs.runByID.RequiredCapabilities = []string{"docker"}
	fs.clearCapsRows = 1
	fs.approvalRow = store.RunUserInput{GateBinding: pgtype.Text{String: "bound", Valid: true}, GateRevision: pgtype.Int8{Int64: 1, Valid: true}}
	if _, err := svc.SubmitInputWithOptions(context.Background(), user, runID, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}, SubmitInputOptions{OverrideCapabilities: true}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if fs.createdApproval == nil {
		t.Fatal("the selection-bearing approve must go through CreateApprovePlanInput")
	}
	if fs.clearedCaps == nil || fs.clearedCaps.GateRevision != 1 || len(fs.runByID.RequiredCapabilities) != 0 {
		t.Fatalf("clear = %+v, caps %v: want scoped to revision 1 and emptied", fs.clearedCaps, fs.runByID.RequiredCapabilities)
	}
}
