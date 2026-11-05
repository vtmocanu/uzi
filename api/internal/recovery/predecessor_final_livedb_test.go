package recovery

import (
	"testing"

	"github.com/google/uuid"
)

// Issue #2969: characterization of the server side of a live-flight predecessor FINAL. A run
// whose current claim generation N is still running keeps an older guarded open hold at N-1
// (taken by the same worker). The worker may release that older hold with a self-contained
// archive FINAL while generation N keeps flying. These tests pin today's server behaviour
// (no schema, guard or admission-formula change); they are expected green on the base.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// predecessorFx is a running run at claim generation 2 owned by the hold's original worker,
// with an older open guarded hold at generation 1 (inventoryEnv's own hold) and the current
// generation's own open guarded hold.
type predecessorFx struct {
	*inventoryEnv
	cur uuid.UUID // the generation 2 hold; e.hold is the generation 1 predecessor
}

func newPredecessorFx(t *testing.T) *predecessorFx {
	t.Helper()
	e := newInventoryEnv(t)
	fx := &predecessorFx{inventoryEnv: e, cur: uuid.New()}
	e.exec("UPDATE workers SET last_heartbeat_at=now() WHERE id=$1", e.w.ID)
	e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", e.run)
	e.exec(`INSERT INTO recovery_custody_holds
		(id,user_id,run_id,generation,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded,state,repo_id)
		VALUES($1,$2,$3,2,$4,$5,$4,$3,true,'open',(SELECT repo_id FROM runs WHERE id=$3))`,
		fx.cur, e.w.UserID, e.run, e.w.ID, e.w.Name)
	return fx
}

// admissionCount is the canonical owner aggregate (ADR-2445) with a heartbeat cutoff far
// enough back that the fixture worker is always live.
func (fx *predecessorFx) admissionCount() int64 {
	fx.t.Helper()
	var n int64
	if err := fx.pool.QueryRow(fx.ctx, "SELECT fn_custody_admission_count($1, now()-interval '1 hour')", fx.w.UserID).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

type admissionMember struct {
	counted bool
	reason  string
}

// members is the per-hold explanation of the same aggregate, keyed by hold id.
func (fx *predecessorFx) members() map[uuid.UUID]admissionMember {
	fx.t.Helper()
	rows, err := fx.pool.Query(fx.ctx, "SELECT hold_id, counted, reason FROM fn_custody_admission_members($1, now()-interval '1 hour')", fx.w.UserID)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]admissionMember{}
	for rows.Next() {
		var id uuid.UUID
		var m admissionMember
		if err := rows.Scan(&id, &m.counted, &m.reason); err != nil {
			fx.t.Fatal(err)
		}
		out[id] = m
	}
	if err := rows.Err(); err != nil {
		fx.t.Fatal(err)
	}
	return out
}

// assertUntouchedOpen asserts a hold is open, live on the worker and carries no FINAL receipt.
func (fx *predecessorFx) assertUntouchedOpen(hold uuid.UUID, label string) {
	fx.t.Helper()
	var ok bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state='open' AND live_worker_id=$2 AND live_run_id=$3
		AND final_disposition IS NULL AND final_capture_id IS NULL AND release_evidence IS NULL
		FROM recovery_custody_holds WHERE id=$1`, hold, fx.w.ID, fx.run).Scan(&ok); err != nil || !ok {
		fx.t.Fatalf("%s hold is not untouched-open: %v %v", label, ok, err)
	}
}

func (fx *predecessorFx) assertRunStillRunning() {
	fx.t.Helper()
	var ok bool
	if err := fx.pool.QueryRow(fx.ctx, "SELECT status='running' AND claim_generation=2 AND worker_id=$2 AND claim_released_at IS NULL FROM runs WHERE id=$1",
		fx.run, fx.w.ID).Scan(&ok); err != nil || !ok {
		fx.t.Fatalf("the run is no longer running at generation 2 on the worker: %v %v", ok, err)
	}
}

func TestRecoveryPredecessorFinalWhileSuccessorRunsLiveDB(t *testing.T) {
	// Baseline facts before any FINAL: the predecessor is counted, the current generation is
	// the one discounted live claim.
	t.Run("before FINAL the live claim is discounted and the predecessor is counted", func(t *testing.T) {
		fx := newPredecessorFx(t)
		if n := fx.admissionCount(); n != 1 {
			t.Fatalf("fn_custody_admission_count = %d, want 1 (two open holds minus the one live claim)", n)
		}
		m := fx.members()
		if len(m) != 2 {
			t.Fatalf("members = %+v, want two open holds", m)
		}
		if got := m[fx.cur]; got.counted || got.reason != "live_claim" {
			t.Fatalf("current generation member = %+v, want counted=false reason=live_claim", got)
		}
		if got := m[fx.hold]; !got.counted || got.reason != "earlier_generation" {
			t.Fatalf("predecessor member = %+v, want counted=true reason=earlier_generation", got)
		}
	})

	// Subtests 1 to 3: the older generation's archive FINAL is accepted and releases exactly the
	// predecessor hold, decrements the admission aggregate exactly once, and leaves the current
	// generation's hold open and discounted.
	t.Run("older generation archive FINAL releases only the predecessor", func(t *testing.T) {
		fx := newPredecessorFx(t)
		id := fx.reserve("predecessor-final") // generation 1: e.gen
		body := fx.upload(id)
		before := fx.admissionCount()
		if before != 1 {
			t.Fatalf("admission count before FINAL = %d, want 1", before)
		}

		ack, err := fx.svc.Release(fx.ctx, fx.w, fx.run, fx.request(id))
		if err != nil || !ack.Released || ack.HoldsReleased != 1 || ack.Generation == nil || *ack.Generation != 1 || ack.Retained {
			t.Fatalf("release: %+v %v", ack, err)
		}

		// 1. The predecessor hold is a released archive receipt for this capture.
		var released bool
		if err := fx.pool.QueryRow(fx.ctx, `SELECT state='released' AND generation=1 AND final_disposition='archive'
			AND release_evidence='archive' AND final_capture_id=$2 AND live_worker_id IS NULL AND live_run_id IS NULL
			FROM recovery_custody_holds WHERE id=$1`, fx.hold, id).Scan(&released); err != nil || !released {
			t.Fatalf("predecessor hold is not an archive release of the capture: %v %v", released, err)
		}
		var kept bool
		if err := fx.pool.QueryRow(fx.ctx, `SELECT state='available' AND local_replica_worker_id=$2 AND ready_retention_seconds=3600
			FROM recovery_captures WHERE id=$1`, id, fx.w.ID).Scan(&kept); err != nil || !kept {
			t.Fatalf("capture is not protected on the worker: %v %v", kept, err)
		}
		fx.bytes(id, body)

		// 3. The successor is untouched: its hold is open and the run still flies.
		fx.assertUntouchedOpen(fx.cur, "current generation")
		fx.assertRunStillRunning()
		m := fx.members()
		if len(m) != 1 {
			t.Fatalf("members after release = %+v, want only the current generation hold", m)
		}
		if got := m[fx.cur]; got.counted || got.reason != "live_claim" {
			t.Fatalf("current generation member after release = %+v, want counted=false reason=live_claim", got)
		}

		// 2. The aggregate decrements exactly once, and an exact replay does not decrement again.
		if after := fx.admissionCount(); after != before-1 {
			t.Fatalf("admission count %d -> %d, want exactly one decrement", before, after)
		}
		again, err := fx.svc.Release(fx.ctx, fx.w, fx.run, fx.request(id))
		if err != nil || again != ack {
			t.Fatalf("replay: %+v %v, want %+v", again, err, ack)
		}
		if replayed := fx.admissionCount(); replayed != before-1 {
			t.Fatalf("replay moved the admission count to %d, want %d", replayed, before-1)
		}
		fx.assertUntouchedOpen(fx.cur, "current generation after replay")
	})

	// 4a. The generation still flying is not eligible: only an ended generation may hand off.
	t.Run("current generation FINAL while running is refused", func(t *testing.T) {
		fx := newPredecessorFx(t)
		fx.gen = 2
		id := fx.reserve("current-final")
		body := fx.upload(id)
		before := fx.admissionCount()

		fx.reject(fx.w, fx.request(id), ErrNotAuthorized)

		fx.assertUntouchedOpen(fx.cur, "current generation")
		fx.assertUntouchedOpen(fx.hold, "predecessor")
		fx.assertRunStillRunning()
		fx.bytes(id, body)
		if after := fx.admissionCount(); after != before {
			t.Fatalf("a refused FINAL moved the admission count %d -> %d", before, after)
		}
	})

	// 4b. Owner-only parks keep custody an owner decision even for an older generation.
	for _, cause := range []string{"worker_requeue_exhausted", "worker_memory_pressure", "retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run("park "+cause+" refuses an older generation FINAL", func(t *testing.T) {
			fx := newPredecessorFx(t)
			id := fx.reserve("parked-final") // generation 1, uploaded while the run still flies
			body := fx.upload(id)
			fx.exec("UPDATE runs SET worker_id=NULL,status='recovery_wait',recovery_wait_cause=$2 WHERE id=$1", fx.run, cause)
			before := fx.admissionCount()

			fx.reject(fx.w, fx.request(id), ErrNotAuthorized)

			fx.assertUntouchedOpen(fx.hold, "predecessor")
			fx.assertUntouchedOpen(fx.cur, "current generation")
			fx.bytes(id, body)
			if after := fx.admissionCount(); after != before {
				t.Fatalf("a refused FINAL moved the admission count %d -> %d", before, after)
			}
		})
	}
}
