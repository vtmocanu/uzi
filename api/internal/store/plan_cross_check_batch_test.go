package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func planCrossCheckTime(v time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: v, Valid: true}
}

func requeuePlanCrossCheckBatch(ctx context.Context, q *store.Queries, f *awaitingInputFixture, path string) error {
	switch path {
	case "worker":
		_, err := q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{WorkerID: pgU(f.workerID), MaxRequeues: 5})
		return err
	case "stale":
		_, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{MaxRequeues: 5, Cutoff: planCrossCheckTime(time.Now().Add(-time.Minute))})
		return err
	default:
		_, err := q.RequeueRunsMissingFromSnapshot(ctx, store.RequeueRunsMissingFromSnapshotParams{WorkerID: pgU(f.workerID), MaxRequeues: 5, MissingCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)), Now: planCrossCheckTime(time.Now()), GlobalTimeoutSeconds: 3600})
		return err
	}
}

func TestPlanCrossCheckChildBeforeParentBatchLiveDB(t *testing.T) {
	for _, path := range []string{"worker", "stale", "snapshot"} {
		t.Run(path, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
			mustExec(ctx, t, f.pool, "DELETE FROM runs WHERE id=$1", fx.checkerID)
			old := f.runID
			f.runID = uuid.New()
			f.runID[0] = 255
			fx.checkerID = uuid.New()
			fx.checkerID[0] = 0
			mustExec(ctx, t, f.pool, "UPDATE runs SET id=$2 WHERE id=$1", old, f.runID)
			insertPlanCrossCheckDeletionRows(ctx, t, fx)
			mustExec(ctx, t, f.pool, `UPDATE runs SET status='running',worker_id=$3,claim_generation=1,
    status_since=now()-interval '2 minutes',started_at=now(),budget_wall_seconds=3600
    WHERE id IN ($1,$2)`, f.runID, fx.checkerID, f.workerID)
			mustExec(ctx, t, f.pool, `UPDATE cross_checks SET created_at=now()-interval '17 seconds'
    WHERE id=$1`, fx.crossCheckID)
			mustExec(ctx, t, f.pool, `UPDATE workers SET last_heartbeat_at=now()-interval '10 minutes' WHERE id=$1`, f.workerID)
			if err := requeuePlanCrossCheckBatch(ctx, f.q, f, path); err != nil {
				t.Fatal(err)
			}
			lead, err := f.q.GetRunByID(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if lead.Status != "queued" {
				t.Fatalf("lead=%s want queued", lead.Status)
			}
			credit := assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 17)
			if err := requeuePlanCrossCheckBatch(ctx, f.q, f, path); err != nil {
				t.Fatal(err)
			}
			if _, err := f.q.SupersedeExitedPlanCrossChecks(ctx); err != nil {
				t.Fatal(err)
			}
			if again := assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 17); again != credit {
				t.Fatalf("double credit: %d -> %d", credit, again)
			}
		})
	}
}

func TestPlanCrossCheckChildOnlyRequeueLiveDB(t *testing.T) {
	for _, path := range []string{"worker", "stale", "snapshot"} {
		for _, state := range []string{"pending", "decided", "expired", "stale generation"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				fx := setupPlanCrossCheckDeletion(ctx, t)
				f := fx.f
				childWorker := uuid.New()
				mustExec(ctx, t, f.pool, `INSERT INTO workers (id,user_id,name,token_hash,max_concurrent_runs,last_heartbeat_at)
     VALUES ($1,$2,'child worker',$3,4,now()-interval '10 minutes')`, childWorker, f.userID, "hash-"+childWorker.String())
				// Only the child belongs to the worker selected by the recovery statement.
				mustExec(ctx, t, f.pool, `UPDATE workers SET last_heartbeat_at=now() WHERE id=$1`, f.workerID)
				mustExec(ctx, t, f.pool, `UPDATE runs SET status='running',worker_id=$2,claim_generation=1,
     status_since=now()-interval '2 minutes',started_at=now(),budget_wall_seconds=3600 WHERE id=$1`, fx.checkerID, childWorker)
				switch state {
				case "decided":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict='approve',reason_class='approve',decided_at=now() WHERE id=$1", fx.crossCheckID)
				case "expired":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", fx.crossCheckID)
				case "stale generation":
					mustExec(ctx, t, f.pool, "UPDATE cross_checks SET lead_claim_generation=0 WHERE id=$1", fx.crossCheckID)
				}
				selected := *f
				selected.workerID = childWorker
				if err := requeuePlanCrossCheckBatch(ctx, f.q, &selected, path); err != nil {
					t.Fatal(err)
				}
				child, err := f.q.GetRunByID(ctx, fx.checkerID)
				if err != nil {
					t.Fatal(err)
				}
				want := "running"
				if state == "pending" {
					want = "queued"
				}
				if child.Status != want {
					t.Fatalf("child=%s want=%s", child.Status, want)
				}
				lead, err := f.q.GetRunByID(ctx, f.runID)
				if err != nil {
					t.Fatal(err)
				}
				if lead.Status != "running" || lead.BudgetPausedSeconds != 0 {
					t.Fatalf("child-only requeue changed lead: status=%s credit=%d", lead.Status, lead.BudgetPausedSeconds)
				}
			})
		}
	}
}
