package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Capture successful server rows, not a reconstructed identity. The parent array
// is emitted on just one row; aggregate its union in SQL before passing it on.
func freezeWorkerSnapshot(ctx context.Context, t *testing.T, tx pgx.Tx, worker uuid.UUID, ids []uuid.UUID) ([]byte, []uuid.UUID) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", worker); err != nil {
		t.Fatal(err)
	}
	rows, err := store.New(tx).LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
		WorkerID: pgU(worker), SnapshotRunIds: ids,
		AttestedRunIds: []uuid.UUID{}, AttestedClaimGenerations: []int64{},
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var parents []uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT p.id::uuid ORDER BY p.id::uuid), ARRAY[]::uuid[])
		FROM jsonb_array_elements($1::jsonb) target
		CROSS JOIN LATERAL jsonb_array_elements_text(target->'locked_parent_ids') p(id)`, frozen).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	return frozen, parents
}

func lockFrozenSnapshot(ctx context.Context, t *testing.T, tx pgx.Tx, worker uuid.UUID, ids []uuid.UUID, frozen []byte, parents []uuid.UUID) []uuid.UUID {
	t.Helper()
	rows, err := store.New(tx).LockFrozenWorkerSnapshotRuns(ctx, store.LockFrozenWorkerSnapshotRunsParams{
		WorkerID: pgU(worker), RunIds: ids, FrozenTargets: frozen, LockedParentIds: parents,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func upsertFrozenSnapshot(ctx context.Context, t *testing.T, tx pgx.Tx, worker, run uuid.UUID, frozen []byte, parents []uuid.UUID, generation int64, pending bool, since pgtype.Timestamptz, epoch int64) int64 {
	t.Helper()
	n, err := store.New(tx).UpsertFrozenWorkerActiveRun(ctx, store.UpsertFrozenWorkerActiveRunParams{
		WorkerID: worker, RunID: run, FrozenTargets: frozen, LockedParentIds: parents,
		ClaimGeneration: generation, Phase: "running", TerminalPending: pending,
		LeaseSeconds: 60, PendingSince: since, SnapshotEpoch: epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFrozenWorkerSnapshotValidAndEmptyLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	orphan, emptyWorker := uuid.New(), uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash)
		VALUES ($1,$2,'empty',$3)`, emptyWorker, f.userID, "hash-"+emptyWorker.String())
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,status,target_run_id,issue_title,issue_description,harness,report_only,budget_wall_seconds)
		VALUES ($1,$2,$3,$4,'cross_check','running',$5,'orphan','orphan','codex',true,300)`,
		orphan, f.userID, f.repoID, f.workerID, f.runID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, tx)
	emptyFrozen, emptyParents := freezeWorkerSnapshot(ctx, t, tx, emptyWorker, []uuid.UUID{})
	if string(emptyFrozen) != "[]" || len(emptyParents) != 0 {
		t.Fatalf("successful empty freeze: targets=%s parents=%v", emptyFrozen, emptyParents)
	}
	if rows := lockFrozenSnapshot(ctx, t, tx, emptyWorker, []uuid.UUID{}, emptyFrozen, emptyParents); len(rows) != 0 {
		t.Fatalf("successful empty freeze acquired %v", rows)
	}
	if n := upsertFrozenSnapshot(ctx, t, tx, emptyWorker, f.runID, emptyFrozen, emptyParents, 1, false, pgtype.Timestamptz{}, 1); n != 0 {
		t.Fatalf("successful empty freeze affected=%d", n)
	}
	ids := []uuid.UUID{orphan, fx.checkerID, f.runID}
	frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, ids)
	if !reflect.DeepEqual(parents, []uuid.UUID{f.runID}) {
		t.Fatalf("actual parent union=%v, want only %s", parents, f.runID)
	}
	got := lockFrozenSnapshot(ctx, t, tx, f.workerID, ids, frozen, parents)
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("locked targets=%v, want ordered %v", got, ids)
	}
	for _, id := range ids {
		if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, id, frozen, parents, 0, false, pgtype.Timestamptz{}, 1); n != 1 {
			t.Fatalf("valid target %s affected=%d", id, n)
		}
	}
	// Server generation is 1. Both stale and future reports must be stored,
	// while conflict updates preserve all original pending/lease/epoch behavior.
	since := pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
	if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, f.runID, frozen, parents, 0, true, since, 2); n != 1 {
		t.Fatalf("stale report affected=%d", n)
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT claim_generation=0 AND phase='running' AND terminal_pending
		AND terminal_pending_since=$3 AND terminal_pending_until=now()+interval '60 seconds'
		AND snapshot_epoch=2 AND reported_at=now()
		FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.workerID, f.runID, since).Scan(&matches); err != nil || !matches {
		t.Fatalf("stale pending report fields: matches=%t err=%v", matches, err)
	}
	if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, f.runID, frozen, parents, 99, false, since, 3); n != 1 {
		t.Fatalf("future report affected=%d", n)
	}
	if err := tx.QueryRow(ctx, `SELECT claim_generation=99 AND phase='running' AND NOT terminal_pending
		AND terminal_pending_since IS NULL AND terminal_pending_until IS NULL
		AND snapshot_epoch=3 AND reported_at=now()
		FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.workerID, f.runID).Scan(&matches); err != nil || !matches {
		t.Fatalf("future live report fields: matches=%t err=%v", matches, err)
	}
	for _, ledger := range [][]byte{nil, []byte("null"), []byte("[]")} {
		if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, ids, ledger, parents); len(rows) != 0 {
			t.Fatalf("missing ledger acquired %v", rows)
		}
		if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, f.runID, ledger, parents, 5, false, pgtype.Timestamptz{}, 4); n != 0 {
			t.Fatalf("missing ledger affected=%d", n)
		}
	}
	if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{}, frozen, parents); len(rows) != 0 {
		t.Fatalf("empty snapshot acquired %v", rows)
	}
	if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{f.runID}, frozen, []uuid.UUID{uuid.New()}); len(rows) != 0 {
		t.Fatalf("bogus parent set acquired %v", rows)
	}
	if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, f.runID, frozen, []uuid.UUID{}, 5, false, pgtype.Timestamptz{}, 4); n != 0 {
		t.Fatalf("missing actual parent affected=%d", n)
	}
	var generation int64
	if err := tx.QueryRow(ctx, "SELECT claim_generation FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", f.workerID, f.runID).Scan(&generation); err != nil || generation != 99 {
		t.Fatalf("rejected writes changed prior snapshot: generation=%d err=%v", generation, err)
	}
}

func TestFrozenWorkerSnapshotIdentityDriftLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	otherWorker, otherLead := uuid.New(), uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash)
		VALUES ($1,$2,'other',$3)`, otherWorker, f.userID, "hash-"+otherWorker.String())
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,status,issue_title,issue_description)
		VALUES ($1,$2,$3,$4,'prompt','running','other','other')`, otherLead, f.userID, f.repoID, f.workerID)
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	cases := []struct {
		name    string
		query   string
		args    []any
		unowned bool
	}{
		{"server generation", "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", []any{fx.checkerID}, false},
		{"ownership", "UPDATE runs SET worker_id=$2 WHERE id=$1", []any{fx.checkerID, otherWorker}, false},
		{"kind", "UPDATE runs SET kind='prompt' WHERE id=$1", []any{fx.checkerID}, false},
		{"target", "UPDATE runs SET target_run_id=$2 WHERE id=$1", []any{fx.checkerID, otherLead}, false},
		{"association identity", "UPDATE cross_checks SET id=$2 WHERE id=$1", []any{fx.crossCheckID, uuid.New()}, false},
		{"association disappears", "DELETE FROM cross_checks WHERE id=$1", []any{fx.crossCheckID}, false},
		{"already locked different parent", "UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", []any{fx.crossCheckID, otherLead}, false},
		{"unowned at freeze", "UPDATE runs SET worker_id=$2 WHERE id=$1", []any{fx.checkerID, f.workerID}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			if tc.unowned {
				if _, err := tx.Exec(ctx, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, otherWorker); err != nil {
					t.Fatal(err)
				}
			}
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID})
			if _, err := tx.Exec(ctx, tc.query, tc.args...); err != nil {
				t.Fatal(err)
			}
			if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID}, frozen, parents); len(rows) != 0 {
				t.Fatalf("changed identity acquired %v", rows)
			}
			if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, fx.checkerID, frozen, parents, 0, false, pgtype.Timestamptz{}, 1); n != 0 {
				t.Fatalf("changed identity affected=%d", n)
			}
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2", f.workerID, fx.checkerID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("rejected entry exists: count=%d err=%v", count, err)
			}
		})
	}
}

func TestFrozenWorkerSnapshotAssociationFencesLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	orphan, foreignUser, foreignLead := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,status,target_run_id,issue_title,issue_description,harness,report_only,budget_wall_seconds)
		VALUES ($1,$2,$3,$4,'cross_check','running',$5,'orphan','orphan','codex',true,300)`,
		orphan, f.userID, f.repoID, f.workerID, f.runID)
	mustExec(ctx, t, f.pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", foreignUser, foreignUser.String()+"@e2e")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := f.pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", foreignUser); err != nil {
			t.Errorf("foreign parent cleanup: %v", err)
		}
	})
	mustExec(ctx, t, f.pool, `INSERT INTO runs (id,user_id,kind,status,issue_title,issue_description)
		VALUES ($1,$2,'chat','running','foreign','foreign')`, foreignLead, foreignUser)

	t.Run("orphan gains association to held parent", func(t *testing.T) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rollbackWorkerRecoveryTx(t, tx)
		frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{orphan})
		if _, err := tx.Exec(ctx, "UPDATE cross_checks SET checker_run_id=$2 WHERE id=$1", fx.crossCheckID, orphan); err != nil {
			t.Fatal(err)
		}
		if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{orphan}, frozen, parents); len(rows) != 0 {
			t.Fatalf("new association acquired %v", rows)
		}
		if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, orphan, frozen, parents, 0, false, pgtype.Timestamptz{}, 1); n != 0 {
			t.Fatalf("new association affected=%d", n)
		}
	})
	t.Run("non-null association without valid same-tenant parent", func(t *testing.T) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer rollbackWorkerRecoveryTx(t, tx)
		if _, err := tx.Exec(ctx, "UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", fx.crossCheckID, foreignLead); err != nil {
			t.Fatal(err)
		}
		frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID})
		if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID}, frozen, parents); len(rows) != 0 {
			t.Fatalf("foreign parent acquired %v", rows)
		}
		if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, fx.checkerID, frozen, parents, 0, false, pgtype.Timestamptz{}, 1); n != 0 {
			t.Fatalf("foreign parent affected=%d", n)
		}
	})
}

func TestFrozenWorkerSnapshotRejectedTupleDoesNotAcquireLocksLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, tx)
	frozen, parents := freezeWorkerSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID})
	// The checker was not locked by the freeze. Change its identity in another
	// transaction and hold its row, so any target acquisition would time out.
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=9 WHERE id=$1", fx.checkerID)
	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, holder)
	if _, err := holder.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", fx.checkerID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout='500ms'"); err != nil {
		t.Fatal(err)
	}
	if rows := lockFrozenSnapshot(ctx, t, tx, f.workerID, []uuid.UUID{fx.checkerID}, frozen, parents); len(rows) != 0 {
		t.Fatalf("changed held checker acquired %v", rows)
	}
	if n := upsertFrozenSnapshot(ctx, t, tx, f.workerID, fx.checkerID, frozen, parents, 9, false, pgtype.Timestamptz{}, 1); n != 0 {
		t.Fatalf("changed held checker affected=%d", n)
	}
	// Release the captured parent locks. Holding that parent in a third session
	// then makes an accidental FK insert outside the actual set observable.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	parentHolder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, parentHolder)
	if _, err := parentHolder.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", f.runID); err != nil {
		t.Fatal(err)
	}
	rejected, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, rejected)
	if _, err := rejected.Exec(ctx, "SET LOCAL lock_timeout='500ms'"); err != nil {
		t.Fatal(err)
	}
	if n := upsertFrozenSnapshot(ctx, t, rejected, f.workerID, f.runID, frozen, []uuid.UUID{}, 1, false, pgtype.Timestamptz{}, 1); n != 0 {
		t.Fatalf("FK write outside parent set affected=%d", n)
	}
}
