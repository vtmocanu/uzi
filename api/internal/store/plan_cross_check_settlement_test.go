package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func assertPlanCrossCheckSettled(ctx context.Context, t *testing.T, f *awaitingInputFixture, childID uuid.UUID, minimumCredit int32) int32 {
	t.Helper()
	var verdict, reason, childStatus string
	var credit int32
	var bounded, released bool
	if err := f.pool.QueryRow(ctx, `SELECT cc.verdict, cc.reason_class, child.status,
        lead.budget_paused_seconds, cc.decided_at <= cc.deadline_at,
        child.claim_released_at IS NOT NULL
        FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
        JOIN runs child ON child.id = cc.checker_run_id
        WHERE lead.id = $1 AND child.id = $2`, f.runID, childID).
		Scan(&verdict, &reason, &childStatus, &credit, &bounded, &released); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" || reason != "superseded" || childStatus != "cancelled" ||
		credit < minimumCredit || !bounded || !released {
		t.Fatalf("synchronous exit: verdict=%s reason=%s child=%s credit=%d bounded=%v released=%v",
			verdict, reason, childStatus, credit, bounded, released)
	}
	return credit
}

func TestPlanCrossCheckSynchronousSettlementLiveDB(t *testing.T) {
	exits := []struct {
		name string
		sql  string
	}{
		{"requeue", "UPDATE runs SET status = 'queued' WHERE id = $1"},
		{"approval park", "UPDATE runs SET status = 'awaiting_approval' WHERE id = $1"},
		{"input park", "UPDATE runs SET status = 'awaiting_input' WHERE id = $1"},
		{"followup park", "UPDATE runs SET status = 'awaiting_followup' WHERE id = $1"},
		{"limit park", "UPDATE runs SET status = 'limit_wait' WHERE id = $1"},
		{"recovery park", "UPDATE runs SET status = 'recovery_wait' WHERE id = $1"},
		{"pool park", "UPDATE runs SET status = 'pool_wait' WHERE id = $1"},
		{"pause", "UPDATE runs SET status = 'paused' WHERE id = $1"},
		{"cancel", "UPDATE runs SET status = 'cancelled' WHERE id = $1"},
		{"complete", "UPDATE runs SET status = 'completed' WHERE id = $1"},
		{"fail", "UPDATE runs SET status = 'failed' WHERE id = $1"},
		{"reclaim", "UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1"},
		{"release", "UPDATE runs SET claim_released_at = now() WHERE id = $1"},
	}
	for _, exit := range exits {
		t.Run(exit.name, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			// An expired interval gives an exact, non-clock-sensitive wait credit.
			mustExec(ctx, t, f.pool, `UPDATE cross_checks SET
                created_at = '2020-01-01 00:00:00+00',
                deadline_at = '2020-01-01 00:00:17.25+00' WHERE id = $1`, fx.crossCheckID)
			mustExec(ctx, t, f.pool, `UPDATE runs SET budget_paused_seconds = 7 WHERE id = $1`, f.runID)
			mustExec(ctx, t, f.pool, exit.sql, f.runID)
			credit := assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 25)
			if credit != 25 {
				t.Fatalf("wait credit = %d, want existing 7 + rounded wait 18", credit)
			}
			// Re-delivery and a later reclaim cannot consume the same pending transition.
			mustExec(ctx, t, f.pool, exit.sql, f.runID)
			mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed', claim_released_at = NULL,
                claim_generation = claim_generation + 1 WHERE id = $1`, f.runID)
			mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
			mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
			again := assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 25)
			if again != credit {
				t.Fatalf("duplicate wait credit: first=%d second=%d", credit, again)
			}
		})
	}
}

func TestPlanCrossCheckSettlementPreservationLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed', budget_paused_seconds = 7 WHERE id = $1`, f.runID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, f.runID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = status, claim_generation = claim_generation,
        claim_released_at = claim_released_at, budget_extension_seconds = 10 WHERE id = $1`, f.runID)
	// Checker transitions must not settle their parent's check.
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'claimed', claim_generation = 1 WHERE id = $1`, fx.checkerID)
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running' WHERE id = $1`, fx.checkerID)
	var verdict string
	var credit int32
	if err := f.pool.QueryRow(ctx, `SELECT cc.verdict, r.budget_paused_seconds
        FROM cross_checks cc JOIN runs r ON r.id = cc.lead_run_id WHERE cc.id = $1`, fx.crossCheckID).
		Scan(&verdict, &credit); err != nil {
		t.Fatal(err)
	}
	if verdict != "pending" || credit != 7 {
		t.Fatalf("live/noop updates settled check: verdict=%s credit=%d", verdict, credit)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
	assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 7)
}

func TestPlanCrossCheckSettlementRollbackLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, `UPDATE runs SET budget_paused_seconds = 7 WHERE id = $1`, f.runID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	var verdict, child string
	if err = tx.QueryRow(ctx, `SELECT cc.verdict, child.status FROM cross_checks cc
        JOIN runs child ON child.id = cc.checker_run_id WHERE cc.id = $1`, fx.crossCheckID).
		Scan(&verdict, &child); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" || child != "cancelled" {
		t.Fatalf("trigger not visible inside transaction: verdict=%s child=%s", verdict, child)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var generation int64
	var credit int32
	if err = f.pool.QueryRow(ctx, `SELECT cc.verdict, child.status, lead.claim_generation,
        lead.budget_paused_seconds FROM cross_checks cc
        JOIN runs lead ON lead.id = cc.lead_run_id JOIN runs child ON child.id = cc.checker_run_id
        WHERE cc.id = $1`, fx.crossCheckID).Scan(&verdict, &child, &generation, &credit); err != nil {
		t.Fatal(err)
	}
	if verdict != "pending" || child != "queued" || generation != 1 || credit != 7 {
		t.Fatalf("rollback leaked settlement: verdict=%s child=%s generation=%d credit=%d",
			verdict, child, generation, credit)
	}
	mustExec(ctx, t, f.pool, `UPDATE runs SET claim_released_at = now() WHERE id = $1`, f.runID)
	assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 7)
}

func TestPlanCrossCheckSettlementDecidedAndMissingChildLiveDB(t *testing.T) {
	for _, mode := range []string{"decided", "missing child", "terminal child"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			fx := setupPlanCrossCheckDeletion(ctx, t)
			f := fx.f
			mustExec(ctx, t, f.pool, `UPDATE runs SET budget_paused_seconds = 7 WHERE id = $1`, f.runID)
			switch mode {
			case "decided":
				mustExec(ctx, t, f.pool, `UPDATE cross_checks SET verdict = 'approve',
                    reason_class = 'approve', decided_at = now() WHERE id = $1`, fx.crossCheckID)
			case "missing child":
				mustExec(ctx, t, f.pool, `DELETE FROM runs WHERE id = $1`, fx.checkerID)
			case "terminal child":
				mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'failed' WHERE id = $1`, fx.checkerID)
			}
			mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
			var verdict string
			var credit int32
			if err := f.pool.QueryRow(ctx, `SELECT cc.verdict, lead.budget_paused_seconds
                FROM cross_checks cc JOIN runs lead ON lead.id = cc.lead_run_id
                WHERE cc.id = $1`, fx.crossCheckID).Scan(&verdict, &credit); err != nil {
				t.Fatal(err)
			}
			if mode == "decided" {
				if verdict != "approve" || credit != 7 {
					t.Fatalf("exit changed decided check: verdict=%s credit=%d", verdict, credit)
				}
			} else if verdict != "failed" || credit < 7 {
				t.Fatalf("exit did not settle %s: verdict=%s credit=%d", mode, verdict, credit)
			}
			if mode == "terminal child" {
				var status string
				if err := f.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, fx.checkerID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status != "failed" {
					t.Fatalf("exit overwrote terminal child: %s", status)
				}
			}
		})
	}
}

// Both connections are bounded by the context deadline. A contender failure is
// returned through its channel; it never leaves a goroutine running after the test.
func TestPlanCrossCheckExitVerdictOrderingLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, `UPDATE runs SET status = 'running', worker_id = $2,
        claim_generation = 1 WHERE id = $1`, fx.checkerID, f.workerID)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, f.runID); err != nil {
		t.Fatal(err)
	}
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pid := conn.Conn().PgConn().PID()
	result := make(chan error, 1)
	go func() {
		_, verdictErr := store.New(conn).LockPlanCrossCheckLeadForVerdict(ctx,
			store.LockPlanCrossCheckLeadForVerdictParams{
				ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1,
			})
		result <- verdictErr
	}()
	consumed := false
	defer func() {
		cancel()
		if !consumed {
			<-result
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err = f.pool.QueryRow(ctx, `SELECT COALESCE(
            (SELECT wait_event_type = 'Lock' FROM pg_stat_activity WHERE pid = $1), false)`, pid).
			Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err = <-result:
			consumed = true
			t.Fatalf("verdict did not wait for lead lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-result
	consumed = true
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("verdict admitted after committed lead exit: %v", err)
	}
	assertPlanCrossCheckSettled(ctx, t, f, fx.checkerID, 0)
}
