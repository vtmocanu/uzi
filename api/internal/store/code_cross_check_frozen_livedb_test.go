package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// prepareFrozenRecoveryChild supplies the real child/snapshot rows. Code checks
// require the waiting lead on that same worker, unlike the plan fixture.
func frozenWaitingCodeFixture(ctx context.Context, t *testing.T, phase, shape, outcome string) (*planCrossCheckDeletionFixture, uuid.UUID) {
	t.Helper()
	fx := codeCrossCheckFixture(t)
	f := fx.f
	worker := prepareFrozenRecoveryChild(ctx, t, fx, phase)
	mustExec(ctx, t, f.pool, "UPDATE workers SET protocol_capabilities=$2,max_cross_check_slots=1 WHERE id=$1", worker,
		[]string{"cross_check_v1", "cross_check_lane_v1", "cross_check_code_v1", "codex_harness_v1", "codex_runtime_v2"})
	mustExec(ctx, t, f.pool, `UPDATE runs SET kind='task',interactive=true,status='awaiting_followup',
 issue_iid=NULL,branch='uzi/interactive-test',worker_id=$2,claim_generation=1,claim_released_at=NULL
 WHERE id=$1`, f.runID, worker)
	// Invalid parent shapes precede insertion, so lead-exit settlement cannot
	// cancel the child and mask the recovery predicate being exercised.
	switch shape {
	case "wrong waiting kind":
		mustExec(ctx, t, f.pool, "UPDATE runs SET kind='prompt' WHERE id=$1", f.runID)
	case "noninteractive":
		mustExec(ctx, t, f.pool, "UPDATE runs SET interactive=false WHERE id=$1", f.runID)
	case "foreign lead worker":
		mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", f.runID, f.workerID)
	case "parent generation":
		mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.runID)
	case "released":
		mustExec(ctx, t, f.pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
	}
	if phase == "worker requeue" {
		// This method targets every eligible owned run. Keep the waiting parent
		// outside its under-cap set to exercise independent child recovery.
		mustExec(ctx, t, f.pool, "UPDATE runs SET requeue_count=5 WHERE id=$1", f.runID)
	}
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, outcome, nil); err != nil {
		t.Fatal(err)
	}
	return fx, worker
}

func TestCodeCrossCheckFrozenWaitingTaskRecoveryLiveDB(t *testing.T) {
	// Seven independent phases, each with fresh rows and a 20-second deadline.
	// Each subtest failure leaves siblings free to run; transactions roll back.
	for _, phase := range []string{"readopt", "missing fail", "missing requeue", "worker fail", "worker requeue", "attested fail", "attested requeue"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fx, worker := frozenWaitingCodeFixture(ctx, t, phase, "healthy", "pending")
			tx, err := fx.f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			ids := []uuid.UUID{fx.checkerID}
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, ids)
			if !reflect.DeepEqual(parents, []uuid.UUID{fx.f.runID}) {
				t.Fatalf("parent union=%v", parents)
			}
			rows, err := frozenRecoveryPhase(ctx, store.New(tx), phase, worker, ids, frozen, parents)
			if err != nil || !reflect.DeepEqual(rows, ids) {
				t.Fatalf("%s rows=%v err=%v", phase, rows, err)
			}
			want := "queued"
			switch phase {
			case "readopt":
				want = "running"
			case "missing fail", "worker fail", "attested fail":
				want = "failed"
			}
			var childStatus, parentStatus, outcome string
			var childGeneration, parentGeneration int64
			var released, interrupted bool
			if err := tx.QueryRow(ctx, `SELECT child.status,child.claim_generation,lead.status,lead.claim_generation,
    lead.claim_released_at IS NOT NULL,cc.outcome,cc.interrupted_at IS NOT NULL
    FROM cross_checks cc JOIN runs child ON child.id=cc.checker_run_id
    JOIN runs lead ON lead.id=cc.lead_run_id WHERE cc.id=$1`, fx.crossCheckID).
				Scan(&childStatus, &childGeneration, &parentStatus, &parentGeneration, &released, &outcome, &interrupted); err != nil {
				t.Fatal(err)
			}
			if childStatus != want || childGeneration != 1 || parentStatus != "awaiting_followup" || parentGeneration != 1 || released || outcome != "pending" || interrupted {
				t.Fatalf("child=%s/%d parent=%s/%d released=%t check=%s interrupted=%t", childStatus, childGeneration, parentStatus, parentGeneration, released, outcome, interrupted)
			}
		})
	}
}

func TestCodeCrossCheckFrozenWaitingTaskReadoptFencesLiveDB(t *testing.T) {
	for _, shape := range []string{"wrong waiting kind", "noninteractive", "foreign lead worker", "parent generation", "released", "child generation drift", "child owner drift", "snapshot generation", "frozen association identity"} {
		t.Run(shape, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			fx, worker := frozenWaitingCodeFixture(ctx, t, "readopt", shape, "pending")
			tx, err := fx.f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackWorkerRecoveryTx(t, tx)
			ids := []uuid.UUID{fx.checkerID}
			frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, ids)
			switch shape {
			case "child generation drift":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET claim_generation=2 WHERE id=$1", fx.checkerID)
			case "child owner drift":
				execFrozenPhase(ctx, t, tx, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, fx.f.workerID)
			case "snapshot generation":
				execFrozenPhase(ctx, t, tx, "UPDATE worker_active_runs SET claim_generation=2 WHERE worker_id=$1 AND run_id=$2", worker, fx.checkerID)
			case "frozen association identity":
				execFrozenPhase(ctx, t, tx, "UPDATE cross_checks SET id=$2 WHERE id=$1", fx.crossCheckID, uuid.New())
			}
			rows, err := frozenRecoveryPhase(ctx, store.New(tx), "readopt", worker, ids, frozen, parents)
			if err != nil || len(rows) != 0 {
				t.Fatalf("refused %s: rows=%v err=%v", shape, rows, err)
			}
			var status string
			if err := tx.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", fx.checkerID).Scan(&status); err != nil || status != "queued" {
				t.Fatalf("child status=%s err=%v", status, err)
			}
		})
	}
}

func TestCodeCrossCheckDirectWaitingTaskReadoptForeignWorkerLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fx, worker := frozenWaitingCodeFixture(ctx, t, "readopt", "foreign lead worker", "pending")
	tx, err := fx.f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackWorkerRecoveryTx(t, tx)
	rows, err := store.New(tx).ReadoptRunsFromSnapshot(ctx, worker)
	if err != nil || len(rows) != 0 {
		t.Fatalf("foreign lead worker: rows=%v err=%v", rows, err)
	}
	var status string
	if err := tx.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", fx.checkerID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("child status=%s err=%v", status, err)
	}
}

// Exercise waiting-parent custody loss through both public failure methods,
// including their settlement path.
func TestCodeCrossCheckWaitingTaskCustodyLossLiveDB(t *testing.T) {
	for _, path := range []string{"direct", "frozen"} {
		for _, outcome := range []string{"pending", "completed"} {
			t.Run(path+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				fx, worker := frozenWaitingCodeFixture(ctx, t, "worker fail", "healthy", outcome)
				mustExec(ctx, t, fx.f.pool, "UPDATE runs SET requeue_count=5,checkpoint_tip=$2 WHERE id=$1", fx.f.runID, codeFixtureHead)
				tx, err := fx.f.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackWorkerRecoveryTx(t, tx)
				q := store.New(tx)
				if path == "direct" {
					_, err = q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgU(worker), MaxRequeues: 5, FailureReason: pgT("lost")})
				} else {
					ids := []uuid.UUID{fx.f.runID, fx.checkerID}
					frozen, parents := freezeWorkerSnapshot(ctx, t, tx, worker, ids)
					_, err = frozenRecoveryPhase(ctx, q, "worker fail", worker, ids, frozen, parents)
				}
				if err != nil {
					t.Fatal(err)
				}
				var parent, child, gotOutcome string
				var interrupted, released bool
				if err := tx.QueryRow(ctx, `SELECT lead.status,child.status,cc.outcome,
     cc.interrupted_at IS NOT NULL,lead.claim_released_at IS NOT NULL
     FROM cross_checks cc JOIN runs lead ON lead.id=cc.lead_run_id
     JOIN runs child ON child.id=cc.checker_run_id WHERE cc.id=$1`, fx.crossCheckID).
					Scan(&parent, &child, &gotOutcome, &interrupted, &released); err != nil {
					t.Fatal(err)
				}
				if parent != "recovery_wait" || child != "cancelled" || gotOutcome != "failed" || !interrupted || !released {
					t.Fatalf("parent=%s child=%s outcome=%s interrupted=%t released=%t", parent, child, gotOutcome, interrupted, released)
				}
			})
		}
	}
}
