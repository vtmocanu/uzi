package workersvc

// Issue #4628 live-DB proofs for the fresh resume sweep arm and the auto-stop terminal allowance. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// pendingTransfer starts a transfer (a pending row) on f's held run.
func (f *freshFix) pendingTransfer() uuid.UUID {
	f.t.Helper()
	f.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	res, err := f.start(f.preview().SelectionToken, uuid.New())
	if err != nil {
		f.t.Fatalf("start: %v", err)
	}
	return res.Row.ID
}

// healthyClaim claims the pending row on W2 at generation 3 and makes W2 look healthy: a fresh heartbeat and
// accepted snapshot, the run listed at G', and the claim older than the 60 second snapshot grace.
func (f *freshFix) healthyClaim(frid uuid.UUID) {
	f.t.Helper()
	f.claimTo(frid, 3)
	f.env.exec(`UPDATE workers SET snapshot_register_nonce='n1', snapshot_epoch=1, last_heartbeat_at=now(), roll_heartbeat_at=now(),
	    pending_overflow=false, pending_overflow_until=NULL WHERE id=$1`, f.w2)
	f.env.exec(`UPDATE runs SET status_since=now() - interval '5 minutes' WHERE id=$1`, f.run)
	f.env.exec(`INSERT INTO worker_active_runs(worker_id, run_id, claim_generation, phase, snapshot_epoch, reported_at)
	    VALUES ($1, $2, 3, 'running', 1, now())`, f.w2, f.run)
}

func (f *freshFix) sweep() SweepResult {
	f.t.Helper()
	res, err := f.svc.Sweep(f.env.ctx)
	if err != nil {
		f.t.Fatalf("Sweep: %v", err)
	}
	return res
}

func (f *freshFix) wantTransfer(frid uuid.UUID, state, reason string) {
	f.t.Helper()
	row := f.transfer(frid)
	if row.State != state || row.RefusalReason.String != reason {
		f.t.Fatalf("transfer = %s / %q, want %s / %q", row.State, row.RefusalReason.String, state, reason)
	}
}

// wantHeldRestored asserts a refused claimed transfer put the run back at its held recovery_wait on the
// predecessor, with the claim generation left at G'.
func (f *freshFix) wantHeldRestored() {
	f.t.Helper()
	var status string
	var worker *uuid.UUID
	var gen int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, worker_id, claim_generation FROM runs WHERE id=$1`, f.run).Scan(&status, &worker, &gen); err != nil {
		f.t.Fatal(err)
	}
	if status != "recovery_wait" || worker == nil || *worker != f.w1 || gen != 3 {
		f.t.Fatalf("run = %s worker %v gen %d, want recovery_wait on %s at generation 3", status, worker, gen, f.w1)
	}
}

func TestFreshResumeSweepPendingLiveDB(t *testing.T) {
	t.Run("gate off drains a pending row in one tick", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		if res := f.sweep(); res.FreshResumeRefused != 0 {
			t.Fatalf("a gate-on sweep refused %d healthy pending rows", res.FreshResumeRefused)
		}
		f.wantTransfer(frid, "pending", "")

		p := testParams()
		p.FreshResume = false
		off := snapshotSvc(f.env, p)
		res, err := off.Sweep(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.FreshResumeRefused < 1 {
			t.Fatalf("gate-off sweep refused %d rows, want at least 1", res.FreshResumeRefused)
		}
		f.wantTransfer(frid, "refused", FreshRefusalGateOff)
		var status string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&status); err != nil || status != "recovery_wait" {
			t.Fatalf("run = %q (%v), want the held recovery_wait", status, err)
		}
	})
	t.Run("admission deadline", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.env.exec(`UPDATE run_fresh_resumes SET admission_deadline = now() - interval '1 second' WHERE id=$1`, frid)
		f.sweep()
		f.wantTransfer(frid, "refused", FreshRefusalAdmissionDeadline)
	})
}

func TestFreshResumeSweepClaimedLiveDB(t *testing.T) {
	t.Run("a healthy claim is not refused", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.sweep()
		f.wantTransfer(frid, "claimed", "")
	})
	t.Run("a snapshot that omits the run inside the grace is not loss evidence", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id=$1`, f.w2)
		f.env.exec(`UPDATE runs SET status_since=now() - interval '20 seconds' WHERE id=$1`, f.run)
		f.sweep()
		f.wantTransfer(frid, "claimed", "")
	})
	t.Run("a pending-outcome overflow closure is not loss evidence", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id=$1`, f.w2)
		f.env.exec(`UPDATE workers SET pending_overflow=true, pending_overflow_until=now() + interval '10 minutes' WHERE id=$1`, f.w2)
		f.sweep()
		f.wantTransfer(frid, "claimed", "")
	})
	t.Run("a snapshot past the grace that omits the run refuses and restores the tuple", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id=$1`, f.w2)
		if res := f.sweep(); res.FreshResumeRefused < 1 {
			t.Fatalf("sweep refused %d rows", res.FreshResumeRefused)
		}
		f.wantTransfer(frid, "refused", "snapshot_omitted")
		f.wantHeldRestored()
	})
	t.Run("stale heartbeat", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id=$1`, f.w2)
		f.sweep()
		f.wantTransfer(frid, "refused", "destination_stale")
		f.wantHeldRestored()
	})
	t.Run("transfer deadline", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`UPDATE run_fresh_resumes SET transfer_deadline = now() - interval '1 second' WHERE id=$1`, frid)
		f.sweep()
		f.wantTransfer(frid, "refused", "transfer_deadline")
		f.wantHeldRestored()
	})
	t.Run("destination worker deleted", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`DELETE FROM workers WHERE id=$1`, f.w2)
		f.sweep()
		f.wantTransfer(frid, "refused", "destination_deleted")
		f.wantHeldRestored()
	})
	t.Run("destination re-registered", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		f.env.exec(`UPDATE runs SET registration_evidence_generation=3, registration_evidence_nonce='old', registration_evidence_epoch=1 WHERE id=$1`, f.run)
		f.sweep()
		f.wantTransfer(frid, "refused", "destination_reregistered")
		f.wantHeldRestored()
	})
	t.Run("snapshot departure stamp", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		// The destination's snapshot no longer lists the run, the stamp's meaning.
		f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id=$1`, f.w2)
		f.env.exec(`UPDATE runs SET snapshot_departed_generation=3, snapshot_departed_nonce='n1', snapshot_departed_epoch=1 WHERE id=$1`, f.run)
		f.sweep()
		f.wantTransfer(frid, "refused", "snapshot_departed")
		f.wantHeldRestored()
	})
	t.Run("a departure stamp is stale once the destination lists the run again", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		// The stamp is never cleared; the run is listed at G' in the destination's snapshot (healthyClaim).
		f.env.exec(`UPDATE runs SET snapshot_departed_generation=3, snapshot_departed_nonce='n1', snapshot_departed_epoch=1 WHERE id=$1`, f.run)
		f.sweep()
		f.wantTransfer(frid, "claimed", "")
	})
	t.Run("a candidate that stopped holding under the locks is left alone", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		// The unlocked read saw a stale heartbeat; by the time the locks are held the worker heartbeat again.
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '10 minutes' WHERE id=$1`, f.w2)
		base := store.ListFreshResumeSweepCandidatesParams{GateOn: true, ClaimedEnabled: true, StaleCutoff: pgconv.Time(time.Now().Add(-45 * time.Second)), OmittedGraceSeconds: 60}
		cands, err := f.env.q.ListFreshResumeSweepCandidates(f.env.ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		var cand *store.ListFreshResumeSweepCandidatesRow
		for i := range cands {
			if cands[i].ID == frid {
				cand = &cands[i]
			}
		}
		if cand == nil || cand.Reason != "destination_stale" {
			t.Fatalf("candidates = %+v, want %s as destination_stale", cands, frid)
		}
		f.env.exec(`UPDATE workers SET last_heartbeat_at = now() WHERE id=$1`, f.w2)
		out, err := f.svc.refuseFreshResumeForSweep(f.env.ctx, base, *cand)
		if err != nil || out != FreshRefuseNoop {
			t.Fatalf("refuse after the worker recovered = %s, %v, want noop", out, err)
		}
		f.wantTransfer(frid, "claimed", "")
	})
}

func TestFreshResumeAutoStopLiveDB(t *testing.T) {
	// Only a claimed transfer can meet auto-stop: a pending transfer's run is held at recovery_wait,
	// which FailRunAutoStop never fails.
	t.Run("claimed transfer", func(t *testing.T) {
		f := newFreshFix(t, true, 2)
		frid := f.pendingTransfer()
		f.healthyClaim(frid)
		// Without the terminal allowance the guard refuses the failure.
		if _, err := f.env.q.FailRunAutoStop(f.env.ctx, store.FailRunAutoStopParams{ID: f.run}); !store.IsFreshResumeInProgress(err) {
			t.Fatalf("autocommit FailRunAutoStop = %v, want the fresh resume guard", err)
		}
		// The production path: evaluateAutoStop locks the run in a transaction and fails it through the
		// bound service over that transaction's querier (boundClaimControlService, no transaction beginner).
		tx, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.env.ctx) }()
		q := store.New(tx)
		if _, err := q.GetRunByIDForUpdate(f.env.ctx, f.run); err != nil {
			t.Fatal(err)
		}
		locked := f.svc.boundClaimControlService(q)
		rows, err := locked.failRunAutoStopRows(f.env.ctx, f.run)
		if err != nil || rows != 1 {
			t.Fatalf("failRunAutoStopRows over the bound service = %d, %v, want 1 row", rows, err)
		}
		if err := tx.Commit(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		var status, stopKind string
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, COALESCE(stop_kind,'') FROM runs WHERE id=$1`, f.run).Scan(&status, &stopKind); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || stopKind != "auto_stopped" {
			t.Fatalf("run = %s / %s, want failed / auto_stopped", status, stopKind)
		}
		f.wantTransfer(frid, "cancelled", "run_terminal")
	})
}
