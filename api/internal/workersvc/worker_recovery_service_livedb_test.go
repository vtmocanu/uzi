package workersvc

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each hook belongs to one service transaction. Channels and the test context
// bound the pause; cancellation releases it even when an assertion fails.
type recoveryTxHook struct {
	pool     *pgxpool.Pool
	started  chan uint32
	captured chan struct{}
	release  chan struct{}
	after    atomic.Int32
}

func (h *recoveryTxHook) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &recoveryHookTx{Tx: tx, hook: h}, nil
}

type recoveryHookTx struct {
	pgx.Tx
	hook *recoveryTxHook
}

func (tx *recoveryHookTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.HasPrefix(sql, "-- name: LockWorkerRecoveryParents ") {
		select {
		case tx.hook.started <- tx.Conn().PgConn().PID():
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		rows, err := tx.Tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		return &recoveryHookRows{Rows: rows, ctx: ctx, hook: tx.hook}, nil
	}
	tx.hook.after.Add(1)
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *recoveryHookTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	// Worker liveness/nonce writes are prerequisites, not snapshot/FK writes.
	if !strings.Contains(sql, "HeartbeatWorker") && !strings.Contains(sql, "RegisterWorker") {
		tx.hook.after.Add(1)
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

type recoveryHookRows struct {
	pgx.Rows
	ctx    context.Context
	hook   *recoveryTxHook
	closed bool
}

func (rows *recoveryHookRows) Close() {
	rows.Rows.Close()
	if rows.closed {
		return
	}
	rows.closed = true
	if rows.hook.captured != nil {
		close(rows.hook.captured)
		select {
		case <-rows.hook.release:
		case <-rows.ctx.Done():
		}
	}
}
func recoveryHook(pool *pgxpool.Pool, pause bool) *recoveryTxHook {
	h := &recoveryTxHook{pool: pool, started: make(chan uint32, 1)}
	if pause {
		h.captured = make(chan struct{})
		h.release = make(chan struct{})
	}
	return h
}
func awaitRecoveryCapture(t *testing.T, ctx context.Context, h *recoveryTxHook) {
	t.Helper()
	select {
	case <-h.captured:
	case <-ctx.Done():
		t.Fatal("parent capture did not finish:", ctx.Err())
	}
}
func observeRecoveryParentWait(t *testing.T, ctx context.Context, env codexTestEnv, h *recoveryTxHook) {
	t.Helper()
	var pid uint32
	select {
	case pid = <-h.started:
	case <-ctx.Done():
		t.Fatal("prelock did not start:", ctx.Err())
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := env.pool.QueryRow(ctx, `SELECT COALESCE((SELECT wait_event_type='Lock' AND query LIKE '-- name: LockWorkerRecoveryParents %' FROM pg_stat_activity WHERE pid=$1),false)`, pid).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			if h.after.Load() != 0 {
				t.Fatal("snapshot/child writes preceded blocked parent prelock")
			}
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("no positive parent lock observation:", ctx.Err())
		}
	}
}

const recoveryCheckAssociationSQL = `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,created_at,deadline_at)
 VALUES($1,'plan',1,1,'plan','[]','{}','{}','s',$2,'',$3,$4,'codex',now()-interval '60 seconds',now()+interval '20 minutes')`

func seedRecoveryCheck(t *testing.T, env codexTestEnv, user, repo, worker, lead uuid.UUID) uuid.UUID {
	t.Helper()
	child := uuid.New()
	env.exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,report_only,budget_wall_seconds,status,claim_generation,issue_title,issue_description)
 VALUES($1,$2,$3,$4,'cross_check',$5,'codex',true,86400,'running',1,'checker','checker')`, child, user, repo, worker, lead)
	env.exec(`UPDATE runs SET started_at=now()-interval '10 minutes',status_since=now()-interval '10 minutes' WHERE id=$1`, child)
	env.exec(recoveryCheckAssociationSQL, lead, strings.Repeat("a", 40), []byte("fixture digest"), child)
	env.exec(`UPDATE runs SET budget_paused_seconds=5 WHERE id=$1`, lead)
	return child
}

func TestWorkerRecoveryFreezeDriftLiveDB(t *testing.T) {
	for _, drift := range []string{"unowned becomes owned", "server generation", "mapping new parent", "mapping held parent", "association appearance", "association disappearance"} {
		t.Run(drift, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			user, _, repo := env.seedCodexInfra(t)
			wa := seedSnapshotWorker(t, env, user, "nonce")
			wb := seedSnapshotWorker(t, env, user, "foreign")
			lead := seedOutageRun(t, env, user, repo, wa, "running", "issue", 1, 0)
			otherWorker := wb
			if drift == "mapping held parent" {
				otherWorker = wa
			}
			other := seedOutageRun(t, env, user, repo, otherWorker, "running", "issue", 1, 0)
			// Block the first parent in canonical order. Otherwise the fixture
			// could lock the replacement first and deadlock its own FK update.
			if drift == "mapping held parent" && lead.String() > other.String() {
				lead, other = other, lead
			}
			owner := wa
			if drift == "unowned becomes owned" {
				owner = wb
			}
			child := seedRecoveryCheck(t, env, user, repo, owner, lead)
			if drift == "association appearance" {
				env.exec(`DELETE FROM cross_checks WHERE checker_run_id=$1`, child)
			}
			snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce", Active: []ActiveRunEntry{
				entry(lead, 1, "running", true), entry(child, 1, "running", false),
			}}
			if otherWorker == wa {
				snap.Active = append(snap.Active, entry(other, 1, "running", true))
			}
			svc := snapshotSvc(env, testParams())
			hook := recoveryHook(env.pool, false)
			svc.SetTxBeginner(hook)
			worker := hbWorker(t, env, wa)
			ctx, cancel := context.WithTimeout(env.ctx, 15*time.Second)
			blocker, err := env.pool.Begin(ctx)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			if _, err = blocker.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, lead); err != nil {
				cancel()
				t.Fatal(err)
			}
			result := make(chan error, 1)
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-result
				}
			}()
			go func() { _, e := svc.Heartbeat(ctx, worker, nil, nil, snap); result <- e }()
			observeRecoveryParentWait(t, ctx, env, hook)
			switch drift {
			case "unowned becomes owned":
				_, err = blocker.Exec(ctx, `UPDATE runs SET worker_id=$2 WHERE id=$1`, child, wa)
			case "server generation":
				_, err = blocker.Exec(ctx, `UPDATE runs SET claim_generation=2 WHERE id=$1`, child)
			case "association appearance":
				_, err = blocker.Exec(ctx, recoveryCheckAssociationSQL, lead, strings.Repeat("a", 40), []byte("fixture digest"), child)
			case "association disappearance":
				_, err = blocker.Exec(ctx, `DELETE FROM cross_checks WHERE checker_run_id=$1`, child)
			default:
				// Change both authoritative association and target tuple while capture is
				// waiting. The replacement may already belong to the captured parent set.
				_, err = blocker.Exec(ctx, `UPDATE cross_checks SET lead_run_id=$2 WHERE checker_run_id=$1`, child, other)
				if err == nil {
					_, err = blocker.Exec(ctx, `UPDATE runs SET target_run_id=$2 WHERE id=$1`, child, other)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			// A parent outside the captured set stays locked through the service
			// finish. KEY SHARE from the fixture FK is compatible with this lock;
			// any later parent FOR UPDATE discovery would block instead.
			var outsideLock pgx.Tx
			if drift == "mapping new parent" {
				outsideLock, err = env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = outsideLock.Rollback(context.Background()) }()
				if _, err = outsideLock.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR NO KEY UPDATE`, other); err != nil {
					t.Fatal(err)
				}
			}
			if err = blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-result:
				joined = true
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := readActiveRun(t, env, wa, child); exists {
				t.Fatal("drifted or newly owned tuple persisted through frozen ledger")
			}
			if got := statusOf(t, env, child); got != "running" {
				t.Fatalf("drifted checker mutated: %s", got)
			}
			if outsideLock != nil {
				if err = outsideLock.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			}
			svc.SetTxBeginner(env.pool)
			snap.SnapshotEpoch = 2
			if drift == "server generation" {
				snap.Active[1].ClaimGeneration = 2
			}
			if _, err = svc.Heartbeat(ctx, hbWorker(t, env, wa), nil, nil, snap); err != nil {
				t.Fatal(err)
			}
			if _, exists := readActiveRun(t, env, wa, child); !exists {
				t.Fatal("next tick did not reconsider deferred tuple")
			}
		})
	}
}

func TestWorkerRecoveryCrossedServicesLiveDB(t *testing.T) {
	for _, register := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("register=%v/reverse=%v", register, reverse), func(t *testing.T) {
				env := setupCodexLiveDB(t)
				user, _, repo := env.seedCodexInfra(t)
				wa := seedSnapshotWorker(t, env, user, "nonce-A")
				wb := seedSnapshotWorker(t, env, user, "nonce-B")
				la := seedOutageRun(t, env, user, repo, wa, "running", "issue", 1, 0)
				lb := seedOutageRun(t, env, user, repo, wb, "running", "issue", 1, 0)
				// Force both parent-ID orders relative to A's owned, omitted lead.
				if (la.String() > lb.String()) != reverse {
					la, lb = lb, la
					env.exec(`UPDATE runs SET worker_id=CASE WHEN id=$1 THEN $3::uuid ELSE $4::uuid END WHERE id IN($1,$2)`, la, lb, wa, wb)
				}
				env.exec(`UPDATE runs SET requeue_count=1,finalize_resume_generation=1 WHERE id=$1`, lb)
				ca := seedRecoveryCheck(t, env, user, repo, wa, lb)
				cb := seedRecoveryCheck(t, env, user, repo, wb, la)
				p := testParams()
				p.RunMaxRequeues = 1
				a, b := snapshotSvc(env, p), snapshotSvc(env, p)
				ha, hb := recoveryHook(env.pool, true), recoveryHook(env.pool, false)
				a.SetTxBeginner(ha)
				b.SetTxBeginner(hb)
				sa := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{entry(ca, 1, "running", false)}}
				sb := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-B", Active: []ActiveRunEntry{entry(cb, 1, "running", false)}}
				if register {
					sa.SnapshotEpoch = 0
					sb.SnapshotEpoch = 0
					sa.FinalizeResume = []FinalizeResumeEntry{{RunID: la.String(), ClaimGeneration: 1}, {RunID: ca.String(), ClaimGeneration: 1}}
					sb.FinalizeResume = []FinalizeResumeEntry{{RunID: lb.String(), ClaimGeneration: 1}, {RunID: cb.String(), ClaimGeneration: 1}}
				}
				workerA, workerB := hbWorker(t, env, wa), hbWorker(t, env, wb)
				ctx, cancel := context.WithTimeout(env.ctx, 15*time.Second)
				results := make(chan error, 2)
				started := 0
				// Join every launched service before pool cleanup, including assertion failures.
				defer func() {
					cancel()
					for i := 0; i < started; i++ {
						<-results
					}
				}()
				call := func(s *Service, w store.Worker, snap *ActiveSnapshot) {
					var err error
					if register {
						_, _, err = s.Register(ctx, w, "fixture", "", nil, nil, nil, snap)
					} else {
						_, err = s.Heartbeat(ctx, w, nil, nil, snap)
					}
					results <- err
				}
				started++
				go call(a, workerA, sa)
				awaitRecoveryCapture(t, ctx, ha)
				started++
				go call(b, workerB, sb)
				observeRecoveryParentWait(t, ctx, env, hb)
				// Both service calls hold worker rows; B must still be at parent capture.
				var persisted int
				if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM worker_active_runs WHERE worker_id IN($1,$2)`, wa, wb).Scan(&persisted); err != nil {
					t.Fatal(err)
				}
				if persisted != 0 {
					t.Fatal("snapshot FK rows written before full prelock")
				}
				close(ha.release)
				for started > 0 {
					select {
					case err := <-results:
						started--
						if err != nil {
							t.Fatalf("service contender: %v", err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				for _, tc := range []struct {
					id     uuid.UUID
					status string
				}{{la, "queued"}, {lb, "failed"}, {ca, "cancelled"}, {cb, "cancelled"}} {
					if got := statusOf(t, env, tc.id); got != tc.status {
						t.Fatalf("%s status=%s want=%s", tc.id, got, tc.status)
					}
				}
				a.SetTxBeginner(env.pool)
				for _, lead := range []uuid.UUID{la, lb} {
					var verdict string
					var expected, actual int64
					if err := env.pool.QueryRow(env.ctx, `SELECT cc.verdict,5+GREATEST(0,CEIL(EXTRACT(EPOCH FROM(cc.decided_at-cc.created_at)))::bigint),r.budget_paused_seconds FROM cross_checks cc JOIN runs r ON r.id=cc.lead_run_id WHERE r.id=$1`, lead).Scan(&verdict, &expected, &actual); err != nil {
						t.Fatal(err)
					}
					if verdict != "failed" || actual != expected {
						t.Fatalf("settlement verdict=%s credit=%d want=%d", verdict, actual, expected)
					}
					before := actual
					// A further real tick cannot credit a decided check again.
					if register {
						_, _, err := a.Register(env.ctx, hbWorker(t, env, wa), "fixture", "", nil, nil, nil, nil)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						sa.SnapshotEpoch++
						_, err := a.Heartbeat(env.ctx, hbWorker(t, env, wa), nil, nil, sa)
						if err != nil {
							t.Fatal(err)
						}
						if got := workerEpoch(t, env, wa); got != sa.SnapshotEpoch {
							t.Fatalf("repeat heartbeat was not applied: epoch=%d want=%d", got, sa.SnapshotEpoch)
						}
					}
					if got := int64(budgetPausedOf(t, env, lead)); got != before {
						t.Fatalf("repeat credit=%d want=%d", got, before)
					}
				}
			})
		}
	}
}
