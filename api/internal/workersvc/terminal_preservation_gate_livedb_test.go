package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The pre-body gate admits only a run the worker has a stake in that needs no generation: its own
// claimed or running flight, or a run with an open guarded hold of this worker. Every other shape is
// the same ErrRunNotOwned, and the gate writes nothing.
func TestTerminalPreservationGateLiveDB(t *testing.T) {
	gate := func(f *termFix) error {
		return f.svc.TerminalPreservationGate(f.e.ctx, f.worker, f.run)
	}
	t.Run("a run claimed by this worker passes", func(t *testing.T) {
		f := newTermFix(t, "flight")
		if err := gate(f); err != nil {
			t.Fatalf("gate = %v", err)
		}
		f.assertNothing("gate pass")
	})
	t.Run("a hold owned by this worker passes", func(t *testing.T) {
		f := newTermFix(t, "hold")
		if err := gate(f); err != nil {
			t.Fatalf("gate = %v", err)
		}
		f.assertNothing("gate pass")
	})
	refused := []struct {
		name   string
		mode   string
		mutate func(f *termFix)
	}{
		{"another user's run", "flight", func(f *termFix) { f.worker.UserID = uuid.New() }},
		{"a lane mismatch", "flight", func(f *termFix) { f.worker.IsolatedLane = true }},
		{"a same-user run claimed by another worker, no hold of this worker", "flight", func(f *termFix) {
			other := f.e.seedWorker(t, f.worker.ProtocolCapabilities)
			f.e.exec(t, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.run, other)
		}},
		{"a terminal run whose hold belongs to another worker", "hold", func(f *termFix) {
			other := f.e.seedWorker(t, f.worker.ProtocolCapabilities)
			f.worker = store.Worker{ID: other, UserID: f.e.userID, ProtocolCapabilities: f.worker.ProtocolCapabilities}
		}},
		{"a terminal run with no hold", "hold", func(f *termFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL,
				released_at=now() WHERE id=$1`)
		}},
		{"an unknown run", "flight", func(f *termFix) { f.run = uuid.New() }},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			f := newTermFix(t, tc.mode)
			tc.mutate(f)
			if err := gate(f); !errors.Is(err, ErrRunNotOwned) {
				t.Fatalf("gate = %v, want ErrRunNotOwned", err)
			}
			f.assertNothing(tc.name)
		})
	}
}

// The class chosen by the unlocked pre-check is decided again under the locks. A run that moved from
// a live flight to completed (with an open guarded hold) in between is refused with the retryable
// authority_changed, not stale_claim (which tells the worker to stop), with no send and no row; the
// retry is then classified afresh and publishes through the hold.
func TestTerminalAuthorityChangedUnderLocksLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	hold := uuid.New()
	f.e.exec(t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,1,'open',$5,'term-ident',$5,$4,true)`, hold, f.e.userID, f.e.repoID, f.run, f.worker.ID)
	f.svc.terminal.beforeBind = func() {
		f.e.exec(t, `UPDATE runs SET status='completed', claim_released_at=now() WHERE id=$1`, f.run)
	}
	_, err := f.publish(f.req(1, termTip1, ""))
	if errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v: a class change must not be stale_claim", err)
	}
	if r := termReason(t, err); r != apitypes.TerminalReasonAuthorityChanged {
		t.Fatalf("reason = %s, want %s", r, apitypes.TerminalReasonAuthorityChanged)
	}
	// The pack was prepared before the bind (no write), but tx1 wrote no row and nothing was sent.
	if _, ok := f.pres(); ok {
		t.Fatal("a refused bind left a preservation row")
	}
	if _, ok := f.con(1); ok {
		t.Fatal("a refused bind left a contribution row")
	}
	if _, sends, _ := f.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d, want 0", sends)
	}

	f.svc.terminal.beforeBind = nil
	res, err := f.publish(f.req(1, termTip1, ""))
	if err != nil || res.State != "published" {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if c, _ := f.con(1); c.Authority != "open_guarded_hold" || uuid.UUID(c.HoldID.Bytes) != hold {
		t.Fatalf("retry contribution = %+v", c)
	}
}
