package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func rejectionExhaustionWriter(ctx context.Context, e *settleEnv, q *store.Queries, writer string) error {
	generic := pgconv.TextOrNull("generic worker lost")
	var err error
	switch writer {
	case "stale":
		_, err = q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{FailureReason: generic, MaxRequeues: 2, FailCutoff: pgconv.Time(time.Now().Add(-time.Hour))})
	case "register":
		_, err = q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 2})
	case "attested":
		_, err = q.FailAttestedFinalizeRunsOverCap(ctx, store.FailAttestedFinalizeRunsOverCapParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 0, RunIds: []uuid.UUID{e.run}, ClaimGenerations: []int64{1}})
	case "snapshot":
		_, err = q.FailRunsMissingFromSnapshot(ctx, store.FailRunsMissingFromSnapshotParams{FailureReason: generic, WorkerID: pgconv.UUID(e.workerA), MaxRequeues: 2, MissingCutoff: pgconv.Time(time.Now()), Now: pgconv.Time(time.Now()), GlobalTimeoutSeconds: 7200})
	default:
		return fmt.Errorf("unknown writer %q", writer)
	}
	return err
}

func rejectionWriterFixture(t *testing.T) *settleEnv {
	t.Helper()
	e := newSettleEnv(t)
	e.wsvc.SetTxBeginner(e.pool)
	e.exec("UPDATE runs SET status='running', claim_generation=1, requeue_count=2, claim_released_at=NULL, status_since=now()-interval '1 hour', started_at=now() WHERE id=$1", e.run)
	e.exec("UPDATE workers SET last_heartbeat_at=now()-interval '2 hours' WHERE id=$1", e.workerA)
	return e
}

func rejectionWriterReport(e *settleEnv) *httptest.ResponseRecorder {
	return rejectionHTTP(e, http.MethodPost, "/api/worker/terminal-rejections", e.tokenA,
		fmt.Sprintf(`{"rejections":[{"run_id":"%s","claim_generation":1,"reason":"mac_failure"}]}`, e.run))
}

// Open owner/run custody is evidence for an exhaustion hold, independent of MAC scope.
func rejectionAssertPark(t *testing.T, e *settleEnv) {
	t.Helper()
	var parked bool
	if err := e.pool.QueryRow(e.ctx, `SELECT status='recovery_wait'
		AND recovery_wait_cause='worker_requeue_exhausted' AND fail_origin IS NULL
		AND failure_reason IS NULL AND finished_at IS NULL AND claim_released_at IS NOT NULL
		AND (worker_recovery_evidence->>'custody_uncertain')::boolean
		FROM runs WHERE id=$1`, e.run).Scan(&parked); err != nil || !parked {
		t.Fatalf("expected nonterminal custody exhaustion hold: parked=%t err=%v", parked, err)
	}
}

func rejectionWriterAssert(t *testing.T, e *settleEnv) {
	t.Helper()
	var annotation, holdState string
	rejectionAssertPark(t, e)
	var generation int64
	var requeues int
	if err := e.pool.QueryRow(e.ctx, "SELECT claim_generation,requeue_count FROM runs WHERE id=$1", e.run).Scan(&generation, &requeues); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || requeues != 2 {
		t.Fatalf("run generation=%d requeues=%d", generation, requeues)
	}
	if err := e.pool.QueryRow(e.ctx, "SELECT terminal_record_rejection,state FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&annotation, &holdState); err != nil {
		t.Fatal(err)
	}
	if annotation != "mac_failure" || holdState != "open" {
		t.Fatalf("hold = %s %s", annotation, holdState)
	}
}

// The hold barrier stops the actual report after it locks the run. A lock-graph
// probe proves the public writer is waiting before the report can annotate/commit.
func TestTerminalRejectionExhaustionWriterReportFirstLiveDB(t *testing.T) {
	for _, writer := range []string{"stale", "register", "attested", "snapshot"} {
		for _, mode := range []string{"pool", "savepoint"} {
			t.Run(writer+"/"+mode, func(t *testing.T) {
				e := rejectionWriterFixture(t)
				ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
				defer cancel()
				barrier, err := e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = barrier.Rollback(ctx) }()
				if _, err := barrier.Exec(ctx, "SELECT id FROM recovery_custody_holds WHERE id=$1 FOR UPDATE", e.pred); err != nil {
					t.Fatal(err)
				}
				started := make(chan uint32, 1)
				e.wsvc.SetTxBeginner(rejectionObservedBeginner{e, started})
				report := make(chan *httptest.ResponseRecorder, 1)
				go func() { report <- rejectionWriterReport(e) }()
				var reportPID uint32
				select {
				case reportPID = <-started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				rejectionWaitBlocked(t, e, barrier.Conn().PgConn().PID())
				writerDone := make(chan error, 1)
				go func() {
					if mode == "pool" {
						writerDone <- rejectionExhaustionWriter(ctx, e, store.New(e.pool), writer)
						return
					}
					tx, err := e.pool.Begin(ctx)
					if err != nil {
						writerDone <- err
						return
					}
					defer func() { _ = tx.Rollback(ctx) }()
					// Match Register/Heartbeat's existing worker-first transaction.
					q := store.New(tx)
					if _, err = q.GetWorkerForUpdate(ctx, e.workerA); err == nil {
						err = rejectionExhaustionWriter(ctx, e, q, writer)
					}
					if err == nil {
						err = tx.Commit(ctx)
					}
					writerDone <- err
				}()
				rejectionWaitBlocked(t, e, reportPID)
				// Reporting while running has not written a status or a failure reason.
				var status string
				var reason *string
				if err := e.pool.QueryRow(ctx, "SELECT status,failure_reason FROM runs WHERE id=$1", e.run).Scan(&status, &reason); err != nil {
					t.Fatal(err)
				}
				if status != "running" || reason != nil {
					t.Fatalf("report changed running state: %s %v", status, reason)
				}
				if err := barrier.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case rec := <-report:
					assertRejectionDisposition(t, rec, e.run, 1, "recorded")
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case err := <-writerDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				rejectionWriterAssert(t, e)
			})
		}
	}
}

// The public writer's savepoint must retain its locks until the caller commits.
// A late MAC report preserves the historical exhaustion snapshot and park stamps.
func TestTerminalRejectionExhaustionWriterParkFirstLiveDB(t *testing.T) {
	for _, writer := range []string{"stale", "register", "attested", "snapshot"} {
		t.Run(writer, func(t *testing.T) {
			e := rejectionWriterFixture(t)
			ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
			defer cancel()
			tx, err := e.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := rejectionExhaustionWriter(ctx, e, store.New(tx), writer); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := tx.QueryRow(ctx, `SELECT jsonb_build_array(status,status_since,finished_at,
				fail_origin,failure_reason,recovery_wait_cause,worker_recovery_evidence,
				claim_released_at,released_worker_id,released_worker_nonce)::text FROM runs WHERE id=$1`, e.run).Scan(&before); err != nil {
				t.Fatal(err)
			}
			report := make(chan *httptest.ResponseRecorder, 1)
			go func() { report <- rejectionWriterReport(e) }()
			rejectionWaitBlocked(t, e, tx.Conn().PgConn().PID())
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-report:
				assertRejectionDisposition(t, rec, e.run, 1, "recorded")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			rejectionWriterAssert(t, e)
			var after string
			if err := e.pool.QueryRow(ctx, `SELECT jsonb_build_array(status,status_since,finished_at,
				fail_origin,failure_reason,recovery_wait_cause,worker_recovery_evidence,
				claim_released_at,released_worker_id,released_worker_nonce)::text FROM runs WHERE id=$1`, e.run).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("late report rewrote historical park: before=%s after=%s", before, after)
			}
		})
	}
}

// A historical terminal worker-lost run can receive a late diagnostic without
// changing its terminal stamps or releasing still-open source custody.
func TestTerminalRejectionHistoricalTerminalRetrofitLiveDB(t *testing.T) {
	e := rejectionWriterFixture(t)
	e.exec("UPDATE runs SET status='failed',fail_origin='worker_lost',failure_reason='generic worker lost',finished_at=now(),status_since=now() WHERE id=$1", e.run)
	var before string
	if err := e.pool.QueryRow(e.ctx, "SELECT jsonb_build_array(status,status_since,finished_at,fail_origin)::text FROM runs WHERE id=$1", e.run).Scan(&before); err != nil {
		t.Fatal(err)
	}
	assertRejectionDisposition(t, rejectionWriterReport(e), e.run, 1, "recorded")
	var after, reason, annotation string
	if err := e.pool.QueryRow(e.ctx, "SELECT jsonb_build_array(status,status_since,finished_at,fail_origin)::text,failure_reason FROM runs WHERE id=$1", e.run).Scan(&after, &reason); err != nil {
		t.Fatal(err)
	}
	if before != after || reason != rejectionExplanation {
		t.Fatalf("terminal retrofit changed stamps or lost MAC: before=%s after=%s reason=%q", before, after, reason)
	}
	e.assertOpen(e.pred, e.sibGen, e.sibWork)
	if err := e.pool.QueryRow(e.ctx, "SELECT terminal_record_rejection FROM recovery_custody_holds WHERE id=$1", e.pred).Scan(&annotation); err != nil || annotation != "mac_failure" {
		t.Fatalf("annotation=%q err=%v", annotation, err)
	}
}
