package workersvc

import (
	"errors"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMilestoneReconciliationCredentialAndMessageGuardsLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	assertRejected := func() {
		t.Helper()
		before := reconciliationRow(t, f, id)
		for _, state := range []string{"credential_switch", "credential_switch_failed"} {
			for _, gen := range []*int64{i64(1), nil, i64(0)} {
				worker := f.wkr
				if gen == nil {
					worker.ProtocolCapabilities = nil
				}
				_, applied, err := f.svc.SetState(f.ctx, worker, id, StateRequest{State: state, ClaimGeneration: gen})
				if applied || (!errors.Is(err, ErrMissingClaimGeneration) && !errors.Is(err, ErrStaleClaim)) {
					t.Fatalf("%s generation=%v disposal/applied=%v err=%v", state, gen, applied, err)
				}
			}
		}
		for _, gen := range []*int64{i64(1), nil, i64(0)} {
			worker := f.wkr
			if gen == nil {
				worker.ProtocolCapabilities = nil
			}
			err := f.svc.AppendMessagesForClaim(f.ctx, worker, id, []IncomingMessage{{Seq: before.LastSeq + 1, Kind: "text", Payload: []byte(`{"text":"late old flight"}`)}}, gen)
			if !errors.Is(err, ErrMissingClaimGeneration) && !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("message generation=%v err=%v", gen, err)
			}
		}
		after := reconciliationRow(t, f, id)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("rejected writers changed the run")
		}
		assertReconciliationProgressEmpty(t, after)
	}
	assertRejected()
	claim, err := f.svc.Claim(f.ctx, f.wkr, nil)
	if err != nil || claim == nil {
		t.Fatalf("reclaim=%+v err=%v", claim, err)
	}
	assertRejected()
	gen := claim.ClaimGeneration
	if err := f.svc.AppendMessagesForClaim(f.ctx, f.wkr, id, []IncomingMessage{{Seq: reconciliationRow(t, f, id).LastSeq + 1, Kind: "text", Payload: []byte(`{"text":"current flight"}`)}}, &gen); err != nil {
		t.Fatalf("current message: %v", err)
	}
	f.exec("UPDATE runs SET credential_switch_requested_at=now(), credential_switch_generation=$2 WHERE id=$1", id, gen)
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("current give-up applied=%v err=%v", applied, err)
	}
	f.exec("UPDATE runs SET status='awaiting_input', credential_switch_requested_at=now(), credential_switch_generation=$2 WHERE id=$1", id, gen)
	if _, applied, err := f.svc.SetState(f.ctx, f.wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("current release applied=%v err=%v", applied, err)
	}
}

func TestMilestoneReconciliationLiveConfirmationGuardLiveDB(t *testing.T) {
	f, id, opts := newMilestoneReconciliationFix(t)
	old := reconciliationRow(t, f, id)
	if _, err := f.svc.SubmitInputWithOptions(f.ctx, f.userID, id, "approve_plan", "", nil, opts); err != nil {
		t.Fatal(err)
	}
	for _, reclaimed := range []bool{false, true} {
		if reclaimed {
			if claim, err := f.svc.Claim(f.ctx, f.wkr, nil); err != nil || claim == nil {
				t.Fatalf("reclaim=%+v err=%v", claim, err)
			}
		}
		before := reconciliationRow(t, f, id)
		applied, err := f.svc.confirmLivePublishAttempt(f.ctx, store.CheckpointPublishAttempt{RunID: id}, old, store.CheckpointRetention{}, false, nil)
		if applied || !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("old confirmation reclaimed=%v applied=%v err=%v", reclaimed, applied, err)
		}
		if reclaimed {
			applied, err = f.svc.confirmLivePublishAttempt(f.ctx, store.CheckpointPublishAttempt{RunID: id, AttemptedAt: old.CheckpointTipAt}, before, store.CheckpointRetention{}, false, nil)
			if applied || !errors.Is(err, ErrStaleClaim) {
				t.Fatalf("pre-reconciliation attempt with refreshed observer applied=%v err=%v", applied, err)
			}
		}
		if !reflect.DeepEqual(before, reconciliationRow(t, f, id)) {
			t.Fatal("stale confirmation changed checkpoint authority")
		}
	}
}
