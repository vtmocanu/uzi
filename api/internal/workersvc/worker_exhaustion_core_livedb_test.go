package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The hook propagates through nested savepoints, including the evidence reader.
// It executes only a fixed SQL fault, never a supplied command or SQL payload.
type exhaustionTestBeginner struct {
	pool    *pgxpool.Pool
	mode    string
	started chan uint32
}

func (b *exhaustionTestBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &exhaustionTestTx{Tx: tx, hook: b}, nil
}

type exhaustionTestTx struct {
	pgx.Tx
	hook *exhaustionTestBeginner
}

func (tx *exhaustionTestTx) Begin(ctx context.Context) (pgx.Tx, error) {
	nested, err := tx.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &exhaustionTestTx{Tx: nested, hook: tx.hook}, nil
}
func (tx *exhaustionTestTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.HasPrefix(sql, "-- name: LockWorkerRecoveryParents ") || strings.HasPrefix(sql, "-- name: LockFailWorkerRunsOverCap ") {
		if tx.hook.started != nil {
			select {
			case tx.hook.started <- tx.Conn().PgConn().PID():
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if strings.HasPrefix(sql, "-- name: ReadWorkerExhaustionEvidence ") {
		switch tx.hook.mode {
		case "read":
			return nil, errors.New("injected exhaustion evidence reader failure")
		case "sql":
			_, err := tx.Exec(ctx, "SELECT 1/0")
			return nil, err
		case "connection":
			if err := tx.Conn().Close(ctx); err != nil {
				return nil, err
			}
			return nil, errors.New("evidence connection closed")
		}
	}
	return tx.Tx.Query(ctx, sql, args...)
}

func TestWorkerExhaustionReaderErrorBeforeQuery(t *testing.T) {
	// A nil underlying transaction would panic if the read hook touched SQL.
	tx := &exhaustionTestTx{hook: &exhaustionTestBeginner{mode: "read"}}
	rows, err := tx.Query(context.Background(), "-- name: ReadWorkerExhaustionEvidence :many")
	if rows != nil || err == nil || err.Error() != "injected exhaustion evidence reader failure" {
		t.Fatalf("reader injection = %v, %v", rows, err)
	}
}

func TestWorkerExhaustionTransactionalReadFailureLiveDB(t *testing.T) {
	for _, mode := range []string{"read", "sql", "connection"} {
		t.Run(mode, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, _, repo := env.seedCodexInfra(t)
			worker := seedSnapshotWorker(t, env, user, "before")
			run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, 2)
			svc := snapshotSvc(env, testParams())
			svc.SetTxBeginner(&exhaustionTestBeginner{pool: env.pool, mode: mode})
			spy := &stateSpy{}
			svc.SetBroadcaster(spy)
			_, _, err := svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, nil)
			got := exhaustionRun(t, env, run)
			if mode == "connection" {
				if err == nil {
					t.Fatal("unusable transaction reported success")
				}
				if got.Status != "running" || got.ClaimReleasedAt.Valid {
					t.Fatalf("failed transaction changed run: %+v", got)
				}
				if _, ok := spy.statusFor(run); ok {
					t.Fatal("failed transaction published an outcome")
				}
				if nonce := hbWorker(t, env, worker).SnapshotRegisterNonce; !nonce.Valid || nonce.String != "before" {
					t.Fatalf("registration did not roll back: %v", nonce)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "recovery_wait" || !got.ClaimReleasedAt.Valid {
				t.Fatalf("savepoint fallback did not park: %+v", got)
			}
			var evidence store.WorkerRecoveryEvidence
			if err := json.Unmarshal(got.WorkerRecoveryEvidence, &evidence); err != nil {
				t.Fatal(err)
			}
			if !evidence.Unknown || got.RecoveryWaitCause.String != "worker_requeue_exhausted" || strings.Contains(got.FailureReason.String, "injected") || strings.Contains(got.StopReason.String, "injected") {
				t.Fatalf("fallback persisted wrong snapshot/cause or raw error: %+v", got)
			}
			if status, ok := spy.statusFor(run); !ok || status != "recovery_wait" {
				t.Fatalf("fallback broadcast=%q,%v", status, ok)
			}
		})
	}
}

func TestWorkerExhaustionLockFreshnessLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, checker := range []bool{false, true} {
			name := "ordinary"
			if frozen {
				name = "frozen"
			}
			if checker {
				name += "/checker"
			} else {
				name += "/target"
			}
			t.Run(name, func(t *testing.T) {
				env := setupCodexLiveDB(t)
				user, _, repo := env.seedCodexInfra(t)
				worker := seedSnapshotWorker(t, env, user, "nonce")
				run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 1, 2)
				if checker {
					// Parent is outside this worker's recovery targets. The checker is
					// independently exhausted, rather than cancelled by a parent exit.
					parent := run
					env.exec("UPDATE runs SET worker_id=NULL WHERE id=$1", parent)
					run = seedRecoveryCheck(t, env, user, repo, worker, parent)
					env.exec("UPDATE runs SET requeue_count=2 WHERE id=$1", run)
				}
				ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
				defer cancel()
				blocker, err := env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = blocker.Rollback(context.Background()) }()
				if _, err := blocker.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", run); err != nil {
					t.Fatal(err)
				}
				hook := &exhaustionTestBeginner{pool: env.pool, started: make(chan uint32, 1)}
				svc := snapshotSvc(env, testParams())
				svc.SetTxBeginner(hook)
				w := hbWorker(t, env, worker)
				result := make(chan error, 1)
				joined := false
				defer func() {
					cancel()
					if !joined {
						<-result
					}
				}()
				go func() {
					if frozen {
						_, _, err := svc.Register(ctx, w, "fixture", "", nil, nil, nil, nil)
						result <- err
					} else {
						tx, err := hook.Begin(ctx)
						if err != nil {
							result <- err
							return
						}
						defer func() { _ = tx.Rollback(context.Background()) }()
						_, err = store.New(tx).FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: pgconv.UUID(worker), MaxRequeues: 1, FailureReason: pgconv.TextOrNull("lost")})
						if err == nil {
							err = tx.Commit(ctx)
						}
						result <- err
					}
				}()
				var pid uint32
				select {
				case pid = <-hook.started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				// At most ten seconds; a query error or timeout fails this case.
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					var blocked bool
					if err := env.pool.QueryRow(ctx, "SELECT COALESCE((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false)", pid).Scan(&blocked); err != nil {
						t.Fatal(err)
					}
					if blocked {
						break
					}
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatal("writer never waited for target lock")
					}
				}
				if _, err := blocker.Exec(ctx, "UPDATE runs SET checkpoint_tip=$2 WHERE id=$1", run, strings.Repeat("c", 40)); err != nil {
					t.Fatal(err)
				}
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-result:
					joined = true
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if got := exhaustionRun(t, env, run); got.Status != "recovery_wait" || got.FailOrigin.Valid {
					t.Fatalf("fresh checkpoint was missed: status=%s origin=%v", got.Status, got.FailOrigin)
				}
			})
		}
	}
}

func TestWorkerExhaustionEpisodeAccountingLiveDB(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			episode    int64
			spent      int32
			max        int32
			marker     *int64
			wantStatus string
			wantMarker *int64
		}{
			{"initial extra", 0, 1, 1, nil, "queued", exhaustionMarker(2)},
			{"owner ordinary", 1, 0, 1, nil, "queued", nil},
			{"owner preserves marker", 1, 0, 1, exhaustionMarker(2), "queued", exhaustionMarker(2)},
			{"owner exhausted", 1, 1, 1, nil, "failed", nil},
			{"zero cap", 0, 0, 0, nil, "failed", nil},
		} {
			name := tc.name
			if frozen {
				name = "frozen/" + name
			} else {
				name = "ordinary/" + name
			}
			t.Run(name, func(t *testing.T) {
				env := setupCodexLiveDB(t)
				user, _, repo := env.seedCodexInfra(t)
				worker := seedSnapshotWorker(t, env, user, "nonce")
				run := seedOutageRun(t, env, user, repo, worker, "running", "issue", 2, 7+tc.spent)
				env.exec("UPDATE runs SET worker_recovery_episode=$2,requeue_episode_baseline=7,finalize_resume_generation=$3 WHERE id=$1", run, tc.episode, tc.marker)
				var rows []store.RequeueAttestedFinalizeRunsRow
				if frozen {
					params := testParams()
					params.RunMaxRequeues = int(tc.max)
					svc := snapshotSvc(env, params)
					if _, _, err := svc.Register(env.ctx, hbWorker(t, env, worker), "fixture", "", nil, nil, nil, finalizeSnap(false, fin(run, 2))); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := env.q.FailAttestedFinalizeRunsOverCap(env.ctx, store.FailAttestedFinalizeRunsOverCapParams{WorkerID: pgconv.UUID(worker), RunIds: []uuid.UUID{run}, ClaimGenerations: []int64{2}, MaxRequeues: tc.max, FailureReason: pgconv.TextOrNull("lost")}); err != nil {
						t.Fatal(err)
					}
					var err error
					rows, err = env.q.RequeueAttestedFinalizeRuns(env.ctx, store.RequeueAttestedFinalizeRunsParams{WorkerID: pgconv.UUID(worker), RunIds: []uuid.UUID{run}, ClaimGenerations: []int64{2}, MaxRequeues: tc.max})
					if err != nil {
						t.Fatal(err)
					}
					if tc.wantStatus == "queued" && (len(rows) != 1 || rows[0].AllowanceUsed != (tc.name == "initial extra")) {
						t.Fatalf("actual allowance rows=%+v", rows)
					}
				}
				got := exhaustionRun(t, env, run)
				if got.Status != tc.wantStatus {
					t.Fatalf("status=%s want %s", got.Status, tc.wantStatus)
				}
				if got.FinalizeResumeGeneration.Valid != (tc.wantMarker != nil) || (tc.wantMarker != nil && got.FinalizeResumeGeneration.Int64 != *tc.wantMarker) {
					t.Fatalf("lifetime marker=%v want %v", got.FinalizeResumeGeneration, tc.wantMarker)
				}
				wantCount := 7 + tc.spent
				if tc.wantStatus == "queued" {
					wantCount++
				}
				if got.RequeueCount != wantCount || got.RequeueEpisodeBaseline != 7 {
					t.Fatalf("lifetime count=%d baseline=%d", got.RequeueCount, got.RequeueEpisodeBaseline)
				}
			})
		}
	}
}

func exhaustionMarker(value int64) *int64 { return &value }
func exhaustionRun(t *testing.T, env codexTestEnv, id uuid.UUID) store.Run {
	t.Helper()
	run, err := env.q.GetRunByID(env.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}
