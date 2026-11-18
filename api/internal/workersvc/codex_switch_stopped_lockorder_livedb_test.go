package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A late control written (uncommitted) before the report starts must fence it.
// pause_run_only writes the run row without the worker lock, as single-statement
// owner writers do, so only the report's run row locks can fence it. The control
// commits only once a backend is blocked behind its transaction, so the outcome
// does not depend on goroutine scheduling; a report that returns first fails.
func TestCodexSwitchStoppedLateControlLockOrderLiveDB(t *testing.T) {
	for _, control := range []string{"policy", "registration", "pause", "pause_run_only"} {
		t.Run(control, func(t *testing.T) {
			f, id, _, req := stoppedFixture(t)
			withdrawStoppedSwitch(t, f, id)
			worker := f.worker(t)
			ctx, cancel := context.WithTimeout(f.env.ctx, 10*time.Second)
			defer cancel()
			tx, err := f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if control != "pause_run_only" {
				if _, err := store.New(tx).GetWorkerForUpdate(ctx, worker.ID); err != nil {
					t.Fatal(err)
				}
			}
			switch control {
			case "policy":
				_, err = tx.Exec(ctx, "UPDATE runs SET policy_outcome_id=$2, policy_preserve_required=true, policy_continuation_mode='same_conversation', policy_applying_worker_id=worker_id, policy_applying_generation=claim_generation, policy_source_generation=claim_generation WHERE id=$1", id, uuid.New())
			case "registration":
				_, err = tx.Exec(ctx, "UPDATE workers SET snapshot_register_nonce='late-registration' WHERE id=$1", worker.ID)
			case "pause", "pause_run_only":
				_, err = tx.Exec(ctx, "UPDATE runs SET pause_requested_at=now(),pause_mode='wall' WHERE id=$1", id)
			}
			if err != nil {
				t.Fatal(err)
			}
			var txPID int32
			if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&txPID); err != nil {
				t.Fatal(err)
			}
			type report struct {
				accepted bool
				err      error
			}
			done := make(chan report, 1)
			go func() {
				_, accepted, err := f.svc.SetState(ctx, worker, id, req)
				done <- report{accepted, err}
			}()
			var result *report
			blocked := false
			for deadline := time.Now().Add(2 * time.Second); result == nil; {
				select {
				case r := <-done:
					result = &r
					continue
				default:
				}
				if err := f.env.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))", txPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("report neither blocked behind the control nor finished")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if result == nil {
				r := <-done
				result = &r
			}
			if result.err != nil || result.accepted {
				t.Fatalf("late control did not fence the report: accepted=%t err=%v", result.accepted, result.err)
			}
			if !blocked {
				t.Fatal("report was refused without waiting behind the control")
			}
		})
	}
}

// codex_policy_hold is representable only on paused or terminal rows, so a
// running run cannot carry it; if it could, the stop must still refuse it.
func TestCodexSwitchStoppedRunningPolicyHoldLiveDB(t *testing.T) {
	f, id, _, req := stoppedFixture(t)
	withdrawStoppedSwitch(t, f, id)
	_, err := f.env.pool.Exec(f.env.ctx, "UPDATE runs SET policy_outcome_id=$2, policy_preserve_required=true, policy_continuation_mode='same_conversation', policy_applying_worker_id=worker_id, policy_applying_generation=claim_generation, policy_source_generation=claim_generation, policy_resolved_at=now(), hold_reason='codex_policy_hold' WHERE id=$1 AND status='running'", id, uuid.New())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == "runs_policy_hold_shape" {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	f.noWrite(t, id, func() {
		if _, accepted, err := f.svc.SetState(f.env.ctx, f.worker(t), id, req); err != nil || accepted {
			t.Fatal("running policy hold accepted", err)
		}
	})
}
