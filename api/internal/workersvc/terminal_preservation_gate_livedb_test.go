package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The pre-body gate admits only a run the worker has a stake in that needs no generation: its own
// claimed or running flight, or a run with an open guarded hold of this worker. The worker's own
// non-flight run gets a typed refusal (TestTerminalPreservationGateOwnRunRefusalsLiveDB); every
// other shape is the same ErrRunNotOwned, and the gate writes nothing.
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

// The worker's OWN run that is terminal, or whose claim is released, and that has no hold of this
// worker gets the typed refusal (unsupported / stale_claim), not a 404, with no body read and no
// write. A run the worker does not own stays ErrRunNotOwned (above).
func TestTerminalPreservationGateOwnRunRefusalsLiveDB(t *testing.T) {
	releasedHold := `UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL,
		released_at=now() WHERE id=$1`
	cases := []struct {
		name   string
		mode   string
		mutate func(f *termFix)
		check  func(err error) bool
	}{
		{"a completed run still naming the worker, no hold", "flight", func(f *termFix) {
			f.e.exec(t, `UPDATE runs SET status='completed' WHERE id=$1`, f.run)
		}, func(err error) bool { return termReasonOf(err) == apitypes.TerminalReasonUnsupported }},
		{"a failed run with its claim released and no hold", "hold", func(f *termFix) { f.rawHold(releasedHold) },
			func(err error) bool { return errors.Is(err, ErrStaleClaim) }},
		{"a running run whose claim is released, no hold", "flight", func(f *termFix) {
			f.e.exec(t, `UPDATE runs SET claim_released_at=now() WHERE id=$1`, f.run)
		}, func(err error) bool { return errors.Is(err, ErrStaleClaim) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTermFix(t, tc.mode)
			tc.mutate(f)
			if err := f.svc.TerminalPreservationGate(f.e.ctx, f.worker, f.run); !tc.check(err) {
				t.Fatalf("gate = %v", err)
			}
			f.assertNothing(tc.name)
		})
	}
}

func termReasonOf(err error) string {
	var r *TerminalRefusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// The reconciliation fence is the other way the flight lock path can refuse after a current_flight
// pre-check: a run_milestone_reconciliations row makes beginClaimMutation return ErrStaleClaim. That
// too is the retryable authority_changed, so the retry can be classified afresh.
func TestTerminalAuthorityChangedOnReconciliationFenceLiveDB(t *testing.T) {
	f := newTermFix(t, "flight")
	var input int64
	if err := f.e.pool.QueryRow(f.e.ctx, `INSERT INTO run_user_inputs(run_id, kind, body) VALUES ($1, 'approve_plan', '') RETURNING id`, f.run).Scan(&input); err != nil {
		t.Fatal(err)
	}
	f.e.exec(t, `INSERT INTO run_milestone_reconciliations(id, run_id, owner_id, gate_revision, presentation_id,
		gate_digest, reset_state_digest, worker_id, claim_generation, reason, consent, before_state, after_state,
		applied_input_id, stop_cutoff)
		VALUES ($1, $2, $3, 1, $4, $5, 'fixture', $6, 1, 'fixture', '{}', '{}', '{}', $7, $7)`,
		uuid.New(), f.run, f.e.userID, uuid.New(), []byte{1}, f.worker.ID, input)
	// The fence only fires when the claim moved between the unlocked pre-check (which classified
	// this request current_flight) and the lock: release the claim in that window, so
	// beginClaimMutation's reconciled-generation check returns ErrStaleClaim.
	f.svc.terminal.beforeBind = func() {
		f.e.exec(t, `UPDATE runs SET claim_released_at=now() WHERE id=$1`, f.run)
	}
	_, err := f.publish(f.req(1, termTip1, ""))
	f.svc.terminal.beforeBind = nil
	if r := termReason(t, err); r != apitypes.TerminalReasonAuthorityChanged {
		t.Fatalf("err = %v, want authority_changed (a stale_claim would tell the worker to stop)", err)
	}
	if _, ok := f.pres(); ok {
		t.Fatal("a refused bind left a preservation row")
	}
	if _, ok := f.con(1); ok {
		t.Fatal("a refused bind left a contribution row")
	}
	if _, sends, _ := f.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d, want 0", sends)
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

// The lifetime is observed on the unlocked run read. A run whose issue or merge request moved before
// tx1 locked it is a different association: the bind is refused with the retryable authority_changed,
// writing nothing and sending nothing, rather than recording an abandonment for an issue the run no
// longer has.
func TestTerminalBindRefusesChangedAssociationLiveDB(t *testing.T) {
	cases := []struct{ name, change string }{
		{"issue replaced", `UPDATE runs SET issue_iid = issue_iid + 1000 WHERE id=$1`},
		{"issue cleared and merge request set", `UPDATE runs SET kind='ci_fix', issue_iid=NULL, mr_iid=77, pipeline_id=1, pipeline_ref='refs/heads/x' WHERE id=$1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTermFix(t, "flight")
			f.forges.issue = forge.Issue{State: "closed"} // the pre-tx lifetime is ClosedProven
			f.svc.terminal.beforeBind = func() { f.e.exec(t, tc.change, f.run) }
			_, err := f.publish(f.req(1, termTip1, ""))
			f.svc.terminal.beforeBind = nil
			if r := termReason(t, err); r != apitypes.TerminalReasonAuthorityChanged {
				t.Fatalf("err = %v, want authority_changed", err)
			}
			if _, ok := f.pres(); ok {
				t.Fatal("a refused bind left a preservation row")
			}
			if _, ok := f.con(1); ok {
				t.Fatal("a refused bind left a contribution row")
			}
			if _, sends, _ := f.fake.counts(); sends != 0 {
				t.Fatalf("sends = %d, want 0", sends)
			}
		})
	}
}
