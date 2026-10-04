package store_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerRecoveryLockIdentitiesLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	otherWorker, otherLead := uuid.New(), uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash)
		VALUES ($1,$2,'other',$3)`, otherWorker, f.userID, "hash-"+otherWorker.String())
	// Keep the lost owned lead out of the checker-only snapshot. Its live lease
	// must not exclude it: replacement could remove that protection.
	mustExec(ctx, t, f.pool, `UPDATE runs SET claim_generation=7 WHERE id=$1`, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO worker_active_runs
		(worker_id,run_id,claim_generation,phase,terminal_pending,terminal_pending_until,snapshot_epoch,reported_at)
		VALUES ($1,$2,7,'running',true,now()+interval '5 minutes',1,now())`, f.workerID, f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,status,issue_title,issue_description)
		VALUES ($1,$2,$3,$4,'prompt','running','other','other')`, otherLead, f.userID, f.repoID, otherWorker)
	// The authoritative association deliberately disagrees with target_run_id.
	mustExec(ctx, t, f.pool, `UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1`, fx.crossCheckID, otherLead)
	mustExec(ctx, t, f.pool, `UPDATE runs SET worker_id=$2,claim_generation=9 WHERE id=$1`, fx.checkerID, f.workerID)

	orphan, snapshotOnly, attestedOnly, staleAttested := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{snapshotOnly, attestedOnly, staleAttested} {
		mustExec(ctx, t, f.pool, `INSERT INTO runs
			(id,user_id,repo_id,worker_id,kind,status,claim_generation,issue_title,issue_description)
			VALUES ($1,$2,$3,$4,'prompt','queued',11,'queued','queued')`, id, f.userID, f.repoID, f.workerID)
	}
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,worker_id,kind,status,target_run_id,issue_title,issue_description,harness,report_only,budget_wall_seconds)
		VALUES ($1,$2,$3,$4,'cross_check','running',$5,'orphan','orphan','codex',true,300)`, orphan, f.userID, f.repoID, f.workerID, f.runID)
	// Even an inconsistent cross-tenant worker assignment must not authorize a target.
	foreignUser, foreignRun := uuid.New(), uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, foreignUser, foreignUser.String()+"@e2e")
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := f.pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", foreignUser); err != nil {
			t.Errorf("foreign fixture cleanup: %v", err)
		}
	})
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,worker_id,kind,status,issue_title,issue_description)
		VALUES ($1,$2,$3,'chat','running','foreign','foreign')`, foreignRun, foreignUser, f.workerID)

	rows, err := f.q.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
		WorkerID:                 pgU(f.workerID),
		SnapshotRunIds:           []uuid.UUID{fx.checkerID, snapshotOnly, otherLead, foreignRun},
		AttestedRunIds:           []uuid.UUID{attestedOnly, staleAttested, otherLead, foreignRun},
		AttestedClaimGenerations: []int64{11, 10, 0, 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantParents := []uuid.UUID{f.runID, otherLead, snapshotOnly, attestedOnly}
	sort.Slice(wantParents, func(i, j int) bool { return wantParents[i].String() < wantParents[j].String() })
	wantTargets := map[uuid.UUID]int64{f.runID: 7, fx.checkerID: 9, orphan: 0, snapshotOnly: 11, attestedOnly: 11}
	if len(rows) != len(wantTargets) {
		t.Fatalf("targets=%v, want IDs=%v", rows, wantTargets)
	}
	parentSetRows := 0
	for _, row := range rows {
		generation, ok := wantTargets[row.ID]
		if !ok || row.ClaimGeneration != generation || row.WorkerID != pgU(f.workerID) {
			t.Fatalf("unexpected frozen identity: %+v", row)
		}
		delete(wantTargets, row.ID)
		if len(row.LockedParentIds) > 0 {
			parentSetRows++
			if !reflect.DeepEqual(row.LockedParentIds, wantParents) {
				t.Fatalf("parents=%v, want=%v", row.LockedParentIds, wantParents)
			}
		}
		switch row.ID {
		case fx.checkerID:
			if row.Kind != "cross_check" || row.TargetRunID != pgU(f.runID) ||
				row.CrossCheckID != pgU(fx.crossCheckID) || row.ParentLeadID != pgU(otherLead) {
				t.Fatalf("authoritative mapping lost: %+v", row)
			}
		case orphan:
			if row.Kind != "cross_check" || row.TargetRunID != pgU(f.runID) ||
				row.CrossCheckID.Valid || row.ParentLeadID.Valid {
				t.Fatalf("orphan acquired an invented association: %+v", row)
			}
		default:
			wantKind := "prompt"
			if row.ID == f.runID {
				wantKind = "issue"
			}
			if row.Kind != wantKind || row.TargetRunID.Valid || row.CrossCheckID.Valid || row.ParentLeadID != pgU(row.ID) {
				t.Fatalf("ordinary target is not its own parent: %+v", row)
			}
		}
	}
	if parentSetRows != 1 {
		t.Fatalf("parent set returned %d times, want once", parentSetRows)
	}
	empty, err := f.q.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
		WorkerID: pgU(otherWorker), SnapshotRunIds: []uuid.UUID{f.runID},
		AttestedRunIds: []uuid.UUID{f.runID}, AttestedClaimGenerations: []int64{7},
	})
	// Foreign supplied IDs cannot expand another worker's own relevant set.
	if err != nil || len(empty) != 1 || empty[0].ID != otherLead {
		t.Fatalf("other worker scope: rows=%v err=%v", empty, err)
	}
	empty, err = f.q.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
		WorkerID: pgU(uuid.New()), SnapshotRunIds: []uuid.UUID{f.runID},
		AttestedRunIds: []uuid.UUID{f.runID}, AttestedClaimGenerations: []int64{7},
	})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("successful empty evaluation: rows=%v err=%v", empty, err)
	}
}

func TestWorkerRecoveryParentLockOrderLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	secondParent := uuid.New()
	mustExec(ctx, t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,kind,status,issue_title,issue_description)
		VALUES ($1,$2,$3,'prompt','running','second','second')`, secondParent, f.userID, f.repoID)
	mustExec(ctx, t, f.pool, "UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", fx.crossCheckID, secondParent)
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	parents := []uuid.UUID{f.runID, secondParent}
	sort.Slice(parents, func(i, j int) bool { return parents[i].String() < parents[j].String() })

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, holder)
	if _, err := holder.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", parents[1]); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	contender, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, contender)
	var rows []store.LockWorkerRecoveryParentsRow
	result := make(chan error, 1)
	joined := false
	// Cancel and join before rolling back either transaction or releasing its connection.
	defer func() {
		cancel()
		if !joined {
			<-result
		}
	}()
	go func() {
		var queryErr error
		rows, queryErr = store.New(contender).LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
			WorkerID: pgU(f.workerID), SnapshotRunIds: []uuid.UUID{fx.checkerID},
		})
		result <- queryErr
	}()
	observePlanCrossCheckLock(ctx, t, f, conn.Conn().PgConn().PID(), result)
	// The lower parent must already be held while the upper parent blocks.
	// Each independent probe is attempted once and rolled back, so a NOWAIT error
	// cannot abort a sibling checker/cross-check probe.
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", parents[0], true)
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", fx.checkerID, false)
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM cross_checks WHERE id=$1 FOR UPDATE NOWAIT", fx.crossCheckID, false)
	// A checker can change while the prelock waits, but the returned server
	// generation remains the identity frozen before the parent-lock attempt.
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=12 WHERE id=$1", fx.checkerID)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-result
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("frozen targets=%v", rows)
	}
	parentSetRows := 0
	for _, row := range rows {
		if row.ID == fx.checkerID && row.ClaimGeneration != 0 {
			t.Fatalf("checker identity changed during parent wait: %+v", row)
		}
		if len(row.LockedParentIds) > 0 {
			parentSetRows++
			if !reflect.DeepEqual(row.LockedParentIds, parents) {
				t.Fatalf("actual parent set=%v, want=%v", row.LockedParentIds, parents)
			}
		}
	}
	if parentSetRows != 1 {
		t.Fatalf("parent set returned %d times, want once", parentSetRows)
	}
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", parents[1], true)
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", fx.checkerID, false)
	probeWorkerRecoveryLock(ctx, t, f, "SELECT id FROM cross_checks WHERE id=$1 FOR UPDATE NOWAIT", fx.crossCheckID, false)
}

func rollbackWorkerRecoveryTx(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("rollback: %v", err)
	}
}

func probeWorkerRecoveryLock(ctx context.Context, t *testing.T, f *awaitingInputFixture, query string, id uuid.UUID, wantBlocked bool) {
	t.Helper()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, tx)
	_, err = tx.Exec(ctx, query, id)
	var pgErr *pgconn.PgError
	blocked := errors.As(err, &pgErr) && pgErr.Code == "55P03"
	if wantBlocked && !blocked || !wantBlocked && err != nil {
		t.Fatalf("lock probe id=%s: err=%v, wantBlocked=%t", id, err, wantBlocked)
	}
}
