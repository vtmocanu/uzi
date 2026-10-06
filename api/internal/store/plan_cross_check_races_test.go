package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func planCrossCheckClaimParams(f *awaitingInputFixture) store.ClaimRunParams {
	return store.ClaimRunParams{WorkerID: pgU(f.workerID), UserID: f.userID,
		HeartbeatCutoff: planCrossCheckTime(time.Now().Add(-time.Minute)), AffinityCutoff: planCrossCheckTime(time.Now().Add(-time.Hour)),
		SpreadCutoff:       planCrossCheckTime(time.Now().Add(-time.Minute)),
		WorkerProtocolCaps: []string{"codex_harness_v1", "codex_runtime_v2", "cross_check_v1", "codex_custom_model_v1"}}
}

// Polling has a context deadline and returns early if the contender completes.
// Callers cancel and join the contender even when this observation fails.
func observePlanCrossCheckLock(ctx context.Context, t *testing.T, f *awaitingInputFixture, pid uint32, result chan error) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE((SELECT wait_event_type='Lock'
   FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-result:
			result <- err
			t.Fatalf("contender completed before observed lock wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

// Verdict/expiry follow their public transaction protocol: lead lock, decision,
// then wait banking. ClaimRun itself takes the parent lock with SKIP LOCKED.
func runPlanCrossCheckRaceActor(ctx context.Context, q *store.Queries, fx *planCrossCheckDeletionFixture, actor string) error {
	f := fx.f
	switch actor {
	case "verdict":
		if _, err := q.LockPlanCrossCheckLeadForVerdict(ctx, store.LockPlanCrossCheckLeadForVerdictParams{ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1}); err != nil {
			return err
		}
		cc, err := q.DecidePlanCrossCheck(ctx, store.DecidePlanCrossCheckParams{ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1, Verdict: "approve", ReasonClass: pgT("approve"), Findings: []byte("[]")})
		if err != nil {
			return err
		}
		_, err = q.BankPlanCrossCheckWait(ctx, cc.ID)
		return err
	case "expiry":
		lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: f.runID, WorkerID: pgU(f.workerID)})
		if err != nil {
			return err
		}
		_, err = q.ExpirePlanCrossCheck(ctx, store.ExpirePlanCrossCheckParams{LeadRunID: lead.ID, WorkerID: pgU(f.workerID), ClaimGeneration: 1})
		return err
	case "claim":
		child, err := q.ClaimRun(ctx, planCrossCheckClaimParams(f))
		if err == nil && child.ID != fx.checkerID {
			return fmt.Errorf("claimed %s instead of child", child.ID)
		}
		return err
	default:
		_, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: fx.checkerID, UserID: f.userID})
		return err
	}
}

func runPlanCrossCheckRaceExit(ctx context.Context, q *store.Queries, tx pgx.Tx, f *awaitingInputFixture, exit string) error {
	if exit == "worker" {
		return requeuePlanCrossCheckBatch(ctx, q, f, exit)
	}
	_, err := tx.Exec(ctx, "UPDATE runs SET status='queued' WHERE id=$1", f.runID)
	return err
}

func TestPlanCrossCheckExitRacesLiveDB(t *testing.T) {
	for _, exit := range []string{"single", "worker"} {
		for _, actor := range []string{"verdict", "expiry", "claim", "cancel"} {
			for _, exitFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/exitFirst=%t", exit, actor, exitFirst), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					fx := setupPlanCrossCheckDeletion(ctx, t)
					f := fx.f
					if actor == "verdict" {
						mustExec(ctx, t, f.pool, "UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", fx.checkerID, f.workerID)
					}
					if actor == "expiry" {
						mustExec(ctx, t, f.pool, `UPDATE cross_checks SET created_at='2020-01-01 00:00:00+00',
      deadline_at='2020-01-01 00:00:17.25+00' WHERE id=$1`, fx.crossCheckID)
					} else {
						mustExec(ctx, t, f.pool, "UPDATE cross_checks SET created_at=now()-interval '17 seconds' WHERE id=$1", fx.crossCheckID)
					}
					tx, err := f.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					// In the exit-first order hold the lead lock without changing its visible
					// state until the contender's lock wait is observed.
					if exitFirst {
						if _, err = tx.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", f.runID); err != nil {
							t.Fatal(err)
						}
					} else if err = runPlanCrossCheckRaceActor(ctx, store.New(tx), fx, actor); err != nil {
						t.Fatal(err)
					}
					conn, err := f.pool.Acquire(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Release()
					result := make(chan error, 1)
					joined := false
					defer func() {
						cancel()
						if !joined {
							<-result
						}
					}()
					go func() {
						e := func() error {
							contenderTx, beginErr := conn.Begin(ctx)
							if beginErr != nil {
								return beginErr
							}
							defer func() {
								cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
								defer cleanupCancel()
								_ = contenderTx.Rollback(cleanupCtx)
							}()
							var operationErr error
							if exitFirst {
								operationErr = runPlanCrossCheckRaceActor(ctx, store.New(contenderTx), fx, actor)
							} else {
								operationErr = runPlanCrossCheckRaceExit(ctx, store.New(contenderTx), contenderTx, f, exit)
							}
							if operationErr != nil {
								return operationErr
							}
							return contenderTx.Commit(ctx)
						}()
						// Publish only after commit or rollback has finished, so joining
						// the contender also completes its transaction cleanup.
						result <- e
					}()
					var actorErr error
					if exitFirst && actor == "claim" {
						// SKIP LOCKED is deliberate: the claim must refuse immediately while
						// custody exit owns the parent. There is no wait to observe in this order.
						actorErr = <-result
						joined = true
						if !errors.Is(actorErr, pgx.ErrNoRows) {
							t.Fatalf("claim under exit lock: %v", actorErr)
						}
					} else {
						// The observer preserves early results for the deferred join.
						observePlanCrossCheckLock(ctx, t, f, conn.Conn().PgConn().PID(), result)
					}
					if exitFirst {
						if err = runPlanCrossCheckRaceExit(ctx, store.New(tx), tx, f, exit); err != nil {
							t.Fatal(err)
						}
					}
					if err = tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if !joined {
						actorErr = <-result
						joined = true
					}
					if exitFirst && actor != "cancel" {
						if !errors.Is(actorErr, pgx.ErrNoRows) {
							t.Fatalf("actor admitted after exit: %v", actorErr)
						}
					} else if actorErr != nil {
						t.Fatal(actorErr)
					}
					lead, err := f.q.GetRunByID(ctx, f.runID)
					if err != nil {
						t.Fatal(err)
					}
					if lead.Status != "queued" {
						t.Fatalf("lead=%s want queued", lead.Status)
					}
					var verdict, reason, child string
					var credit int32
					if err = f.pool.QueryRow(ctx, `SELECT cc.verdict,cc.reason_class,child.status,lead.budget_paused_seconds
     FROM cross_checks cc JOIN runs child ON child.id=cc.checker_run_id JOIN runs lead ON lead.id=cc.lead_run_id
     WHERE cc.id=$1`, fx.crossCheckID).Scan(&verdict, &reason, &child, &credit); err != nil {
						t.Fatal(err)
					}
					wantVerdict, wantReason := "failed", "superseded"
					if !exitFirst && actor == "verdict" {
						wantVerdict, wantReason = "approve", "approve"
					}
					if !exitFirst && actor == "expiry" {
						wantReason = "timed_out"
					}
					if verdict != wantVerdict || reason != wantReason || credit < 17 {
						t.Fatalf("verdict=%s reason=%s child=%s credit=%d", verdict, reason, child, credit)
					}
					if actor != "verdict" || exitFirst {
						if child != "cancelled" {
							t.Fatalf("child=%s want cancelled", child)
						}
					}
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='queued' WHERE id=$1", f.runID)
					if _, err = f.q.SupersedeExitedPlanCrossChecks(ctx); err != nil {
						t.Fatal(err)
					}
					again, err := f.q.GetRunByID(ctx, f.runID)
					if err != nil {
						t.Fatal(err)
					}
					if again.BudgetPausedSeconds != credit {
						t.Fatalf("double credit %d -> %d", credit, again.BudgetPausedSeconds)
					}
				})
			}
		}
	}
}
