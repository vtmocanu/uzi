package recovery

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The run holder must still be able to lock custody while v1 release waits.
// Deadlines and the operation helpers bound every wait and drain the release.
func TestMemoryCustodyV1ReleaseRunBeforeHoldLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	e.exec("UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", e.hold)
	ctx, cancel := context.WithTimeout(e.ctx, 8*time.Second)
	defer cancel()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var id uuid.UUID
	if err := tx.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", e.run).Scan(&id); err != nil {
		t.Fatal(err)
	}
	release := startReconcileOperation(t, func(ctx context.Context) (apitypes.RecoveryReleaseResponse, error) {
		return e.svc.Release(ctx, e.w, e.run, apitypes.RecoveryReleaseRequest{})
	})
	// Observe the actual wait behind our run lock, even on the unfixed path.
	waitReconcileLock(t, e, int32(tx.Conn().PgConn().PID()), "FOR UPDATE") //nolint:gosec // G115: PostgreSQL backend PID fits int32.
	if err := tx.QueryRow(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE NOWAIT", e.hold).Scan(&id); err != nil {
		t.Fatalf("v1 release locked hold before run: %v", err)
	}
	if _, err := tx.Exec(ctx, "UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_memory_pressure', claim_released_at=now() WHERE id=$1", e.run); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := awaitReconcileOperation(t, release)
	if got.err != nil || got.value.Released || got.value.HoldsReleased != 0 {
		t.Fatalf("v1 release after memory park: %+v err=%v", got.value, got.err)
	}
	var retained bool
	if err := e.pool.QueryRow(e.ctx, "SELECT state='open' AND live_worker_id=$2 AND live_run_id=$3 AND released_at IS NULL FROM recovery_custody_holds WHERE id=$1", e.hold, e.w.ID, e.run).Scan(&retained); err != nil || !retained {
		t.Fatalf("memory custody retained=%v err=%v", retained, err)
	}
}

func TestMemoryCustodyWorkerFinalReleaseRefusedLiveDB(t *testing.T) {
	for _, kind := range []string{"archive", "settled"} {
		t.Run(kind, func(t *testing.T) {
			e := newInventoryEnv(t)
			id := e.reserve("memory-final")
			body := e.upload(id)
			e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_memory_pressure', claim_released_at=now() WHERE id=$1", e.run)
			req := e.request(id)
			if kind == "settled" {
				evidence := "forge_no_output"
				req = apitypes.RecoveryReleaseRequest{Generation: &e.gen, ReleaseEvidence: &evidence,
					FinalDisposition: &apitypes.RecoveryFinalDisposition{Kind: "settled", CoverageDigest: emptyInventoryDigest}}
			}
			e.reject(e.w, req, ErrNotAuthorized)
			var state string
			var retained bool
			if err := e.pool.QueryRow(e.ctx, "SELECT state, live_worker_id=$2 AND live_run_id=$3 AND released_at IS NULL AND release_evidence IS NULL FROM recovery_custody_holds WHERE id=$1", e.hold, e.w.ID, e.run).Scan(&state, &retained); err != nil {
				t.Fatal(err)
			}
			if state != "open" || !retained {
				t.Fatalf("worker %s released memory custody: state=%s retained=%v", kind, state, retained)
			}
			e.bytes(id, body)
			// Once the owner ends the hold, ordinary final release remains available.
			e.exec("UPDATE runs SET status='cancelled', recovery_wait_cause=NULL WHERE id=$1", e.run)
			e.release(req)
		})
	}
}

func TestMemoryCustodyLegacyWorkerReleaseRefusedLiveDB(t *testing.T) {
	e := newInventoryEnv(t)
	e.exec("UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1", e.hold)
	id := e.reserve("legacy-memory")
	body := e.upload(id)
	e.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='worker_memory_pressure', claim_released_at=now() WHERE id=$1", e.run)
	for _, generation := range []*int64{&e.gen, nil} {
		ack, err := e.svc.Release(e.ctx, e.w, e.run, apitypes.RecoveryReleaseRequest{Generation: generation})
		if err != nil || ack.Released || ack.HoldsReleased != 0 {
			t.Fatalf("legacy worker released memory custody: ack=%+v err=%v", ack, err)
		}
	}
	var state string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatalf("legacy custody state=%s", state)
	}
	e.bytes(id, body)
}
