package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The gate returns real DB reads, including the owned claim tuple, before allowing
// Publish to continue. It never manufactures an authorization snapshot.
type exhaustionPublishSnapshot struct {
	Store
	owned    store.Run
	captured chan struct{}
	resume   chan struct{}
}

func (q *exhaustionPublishSnapshot) GetRunOwnedByWorker(ctx context.Context, p store.GetRunOwnedByWorkerParams) (store.Run, error) {
	r, err := q.Store.GetRunOwnedByWorker(ctx, p)
	q.owned = r
	return r, err
}

func (q *exhaustionPublishSnapshot) GetRunClaimContext(ctx context.Context, id uuid.UUID) (store.GetRunClaimContextRow, error) {
	r, err := q.Store.GetRunClaimContext(ctx, id)
	close(q.captured)
	select {
	case <-q.resume:
	case <-ctx.Done():
		return r, ctx.Err()
	}
	return r, err
}

func exhaustionPublishFixture(t *testing.T) (*supersedeFix, store.Worker) {
	t.Helper()
	f := newLiveAttemptFix(t)
	f.discardHold(t)
	f.e.exec(t, "UPDATE runs SET requeue_count=1,requeue_episode_baseline=0 WHERE id=$1", f.oldRun)
	r, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	return f, f.rf.wkr(uuid.UUID(r.WorkerID.Bytes))
}

func exhaustionPublishFail(t *testing.T, ctx context.Context, q *store.Queries, worker store.Worker, run uuid.UUID) {
	t.Helper()
	rows, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		WorkerID: pgconv.UUID(worker.ID), MaxRequeues: 1, FailureReason: pgconv.TextOrNull("worker lost"),
	})
	if err != nil || len(rows) != 1 || rows[0].ID != run {
		t.Fatalf("real exhaustion transition: rows=%v err=%v", rows, err)
	}
}

func exhaustionPublishStart(ctx context.Context, f *supersedeFix, w store.Worker) <-chan publishOut {
	out := make(chan publishOut, 1)
	go func() {
		res, err := f.svc1.Publish(ctx, w, f.oldRun, lateTip, []byte("pack"))
		out <- publishOut{res: res, err: err}
	}()
	return out
}

func TestCheckpointPublishAdmissionPoolOwnershipLiveDB(t *testing.T) {
	f, _ := exhaustionPublishFixture(t)
	r, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	params := store.RecordLiveCheckpointPublishAttemptParams{
		RecordCheckpointPublishAttemptParams: store.RecordCheckpointPublishAttemptParams{
			RunID: f.oldRun, Branch: f.branch, Ref: f.branchRef, Tip: lateTip,
		},
		ExpectedWorkerID: r.WorkerID, ExpectedClaimGeneration: r.ClaimGeneration,
	}
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.e.ctx) }()
	conn, err := f.e.pool.Acquire(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, q := range []*store.Queries{nil, store.New(nil), store.New(tx), store.New(conn.Conn())} {
		if id, err := q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params); err == nil || id != uuid.Nil {
			t.Fatalf("non-pool admission: id=%v err=%v", id, err)
		}
	}
	// The service must also refuse a Tx-backed store before any broker call.
	calls := 0
	f.svc1.q = store.New(tx)
	f.svc1.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		calls++
		return pushbroker.Result{}, nil
	})
	p := &checkpointPush{s: f.svc1, runID: f.oldRun, run: r, branch: f.branch, ref: f.branchRef}
	p.opts.DeclaredTip = lateTip
	if err := p.pushOnce(f.e.ctx); !errors.Is(err, errPushRefused) || calls != 0 {
		t.Fatalf("Tx-backed push: err=%v broker=%d", err, calls)
	}
	if f.attemptCount(t, f.oldRun) != 0 {
		t.Fatal("non-pool admission leaked attempt")
	}
	id, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params)
	if err != nil || id == uuid.Nil {
		t.Fatalf("pool admission: id=%v err=%v", id, err)
	}
	params.AttemptID = id
	if got, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params); err != nil || got != id {
		t.Fatalf("prepared reuse: id=%v err=%v", got, err)
	}
	params.Tip = newerTip
	if got, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params); err == nil || got != uuid.Nil {
		t.Fatalf("mismatched prepared row accepted: id=%v err=%v", got, err)
	}
	if f.attemptCount(t, f.oldRun) != 1 {
		t.Fatal("prepared reuse inserted duplicate evidence")
	}
}

type admissionBaseFailure struct{ Store }

func (q admissionBaseFailure) GetRunCheckpointTipForRetention(context.Context, uuid.UUID) (pgtype.Text, error) {
	return pgtype.Text{}, errors.New("fixture base read refused")
}

func TestCheckpointPublishAdmissionBaseFailureLiveDB(t *testing.T) {
	f, w := exhaustionPublishFixture(t)
	f.svc1.q = admissionBaseFailure{Store: f.e.q}
	calls := 0
	f.svc1.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
		calls++
		return pushbroker.Result{}, nil
	})
	res, err := f.svc1.Publish(f.e.ctx, w, f.oldRun, lateTip, []byte("pack"))
	if err != nil || res.Published || res.Skipped != "not_descendant" || calls != 0 {
		t.Fatalf("pre-broker base failure: result=%+v err=%v calls=%d", res, err, calls)
	}
	if f.attemptCount(t, f.oldRun) != 0 {
		t.Fatal("pre-broker base failure leaked prepared attempt")
	}
}

func TestCheckpointPublishAdmissionStatusesLiveDB(t *testing.T) {
	f, _ := exhaustionPublishFixture(t)
	r, err := f.e.q.GetRunByID(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatal(err)
	}
	params := store.RecordLiveCheckpointPublishAttemptParams{
		RecordCheckpointPublishAttemptParams: store.RecordCheckpointPublishAttemptParams{
			RunID: f.oldRun, Branch: f.branch, Ref: f.branchRef, Tip: lateTip,
		},
		ExpectedWorkerID: r.WorkerID, ExpectedClaimGeneration: r.ClaimGeneration,
	}
	for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup",
		"limit_wait", "pool_wait", "paused", "recovery_wait", "queued", "failed", "completed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			f.e.exec(t, "UPDATE runs SET status=$2 WHERE id=$1", f.oldRun, status)
			id, err := f.e.q.RecordLiveCheckpointPublishAttempt(f.e.ctx, params)
			allowed := status != "queued" && status != "failed" && status != "completed" && status != "cancelled"
			if allowed {
				if err != nil || id == uuid.Nil {
					t.Fatalf("allowed status %s: id=%v err=%v", status, id, err)
				}
				if _, err := f.e.q.DeleteCheckpointPublishAttempt(f.e.ctx, id); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pgx.ErrNoRows) || id != uuid.Nil {
				t.Fatalf("refused status %s: id=%v err=%v", status, id, err)
			}
			if f.attemptCount(t, f.oldRun) != 0 {
				t.Fatal("status admission leaked attempt")
			}
		})
	}
}

func TestCheckpointPublishSlotAdmissionLiveDB(t *testing.T) {
	for _, scenario := range []string{"slow_slot_generation", "slow_slot_release", "slot_refusal", "retry_generation", "retry_release", "retry_success"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSupersedeFix(t)
			w := f.rf.wkr(f.newWorker)
			retry := scenario == "retry_generation" || scenario == "retry_release" || scenario == "retry_success"
			if retry {
				// Initial claim lists retained rows only; the fallback must drive this settling row.
				f.discardHold(t)
				f.e.exec(t, "UPDATE checkpoint_retentions SET state='settling' WHERE run_id=$1", f.oldRun)
			}
			assertCommitted := func() {
				t.Helper()
				tx, err := f.e.pool.Begin(f.e.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(f.e.ctx) }()
				var n int
				if err := tx.QueryRow(f.e.ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", f.newRun).Scan(&n); err != nil || n != 1 {
					t.Fatalf("slot I/O needs one committed attempt: n=%d err=%v", n, err)
				}
				if _, err := tx.Exec(f.e.ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", f.newRun); err != nil {
					t.Fatalf("slot I/O still holds admission lock: %v", err)
				}
			}
			writes := 0
			f.svc2.SetCreateRefFn(func(ctx context.Context, o pushbroker.CreateRefOptions) error {
				assertCommitted()
				writes++
				if scenario == "slot_refusal" {
					return errors.New("fixture slot refused")
				}
				if scenario == "slow_slot_generation" {
					f.e.exec(t, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", f.newRun)
				}
				if scenario == "slow_slot_release" {
					f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.newRun)
				}
				return f.forge.createRef(ctx, o)
			})
			f.svc2.SetDeleteCheckpointFn(func(ctx context.Context, o pushbroker.DeleteOptions) error {
				assertCommitted()
				writes++
				return f.forge.deleteRef(ctx, o)
			})
			calls := 0
			f.svc2.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
				calls++
				if retry && calls == 1 {
					if scenario == "retry_generation" {
						f.e.exec(t, "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", f.newRun)
					}
					if scenario == "retry_release" {
						f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.newRun)
					}
					return pushbroker.Result{}, pushbroker.ErrNotDescendant
				}
				return f.forge.publish(ctx, o)
			})
			res, err := f.svc2.Publish(f.e.ctx, w, f.newRun, supersedeNewTip, []byte("pack"))
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "retry_success" {
				if calls != 2 || writes != 1 || !res.Published {
					t.Fatalf("one admitted retry: calls=%d writes=%d result=%+v", calls, writes, res)
				}
			} else {
				wantCalls := 0
				if retry {
					wantCalls = 1
					if writes != 0 {
						t.Fatal("stale retry admission allowed slot writes")
					}
				}
				if calls != wantCalls || res.Published || res.Skipped != "not_descendant" {
					t.Fatalf("slot admission: calls=%d writes=%d result=%+v", calls, writes, res)
				}
			}
			if f.attemptCount(t, f.newRun) != 0 {
				t.Fatal("unsent/refused/successfully tracked attempt leaked")
			}
		})
	}
}

func TestCheckpointPublishExhaustionAdmissionLiveDB(t *testing.T) {
	t.Run("insertion_first_owner_hold", func(t *testing.T) {
		f, w := exhaustionPublishFixture(t)
		ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
		defer cancel()
		entered := make(chan error, 1)
		release := make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		f.svc1.SetPublishFn(func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
			// This independent connection must see the committed attempt, and
			// NOWAIT must acquire the run lock while the forge call is in flight.
			tx, err := f.e.pool.Begin(ctx)
			if err == nil {
				defer func() { _ = tx.Rollback(context.Background()) }()
				var n int
				err = tx.QueryRow(ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", f.oldRun).Scan(&n)
				if err == nil && n != 1 {
					err = fmt.Errorf("broker sees %d committed attempts, want 1", n)
				}
				if err == nil {
					_, err = tx.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT", f.oldRun)
				}
				if err == nil {
					err = tx.Commit(ctx)
				}
			}
			entered <- err
			select {
			case <-release:
			case <-ctx.Done():
			}
			return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, errors.New("fixture unresolved forge outcome")
		})
		out := exhaustionPublishStart(ctx, f, w)
		defer func() { unblock(); cancel(); <-out }()
		select {
		case err := <-entered:
			if err != nil {
				t.Fatalf("committed attempt / no run lock during forge IO: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		exhaustionPublishFail(t, ctx, f.e.q, w, f.oldRun)
		r, err := f.e.q.GetRunByID(ctx, f.oldRun)
		if err != nil {
			t.Fatal(err)
		}
		var evidence store.WorkerRecoveryEvidence
		if err := json.Unmarshal(r.WorkerRecoveryEvidence, &evidence); err != nil {
			t.Fatal(err)
		}
		if r.Status != "recovery_wait" || !r.ClaimReleasedAt.Valid || !evidence.PublicationUncertain ||
			r.RecoveryWaitCause.String != "worker_requeue_exhausted" || r.FailOrigin.Valid {
			t.Fatalf("insertion first must give owner hold with publication_uncertain: status=%s released=%v evidence=%+v cause=%v origin=%v",
				r.Status, r.ClaimReleasedAt.Valid, evidence, r.RecoveryWaitCause, r.FailOrigin)
		}
	})

	for _, scenario := range []string{"exhaustion_first", "stale_worker", "stale_generation", "released_claim"} {
		t.Run(scenario, func(t *testing.T) {
			f, w := exhaustionPublishFixture(t)
			ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
			defer cancel()
			g := &exhaustionPublishSnapshot{Store: f.svc1.q, captured: make(chan struct{}), resume: make(chan struct{})}
			f.svc1.q = g
			var calls atomic.Int32
			broker := make(chan struct{}, 1)
			f.svc1.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				calls.Add(1)
				broker <- struct{}{}
				return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, errors.New("fixture unresolved forge outcome")
			})
			out := exhaustionPublishStart(ctx, f, w)
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(g.resume) }) }
			joined := false
			var tx pgx.Tx
			defer func() {
				resume()
				cancel()
				if tx != nil {
					_ = tx.Rollback(context.Background())
				}
				if !joined {
					<-out
				}
			}()
			select {
			case <-g.captured:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if g.owned.Status != "running" || g.owned.ClaimGeneration != 1 || g.owned.ClaimReleasedAt.Valid ||
				uuid.UUID(g.owned.WorkerID.Bytes) != w.ID {
				t.Fatal("gate did not capture a DB-backed live owned unreleased generation-1 tuple")
			}
			if scenario == "exhaustion_first" {
				var err error
				tx, err = f.e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				qtx := store.New(tx)
				if _, err := qtx.GetWorkerForUpdate(ctx, w.ID); err != nil {
					t.Fatal(err)
				}
				ids, err := qtx.LockFailWorkerRunsOverCap(ctx, store.LockFailWorkerRunsOverCapParams{WorkerID: pgconv.UUID(w.ID), MaxRequeues: 1})
				if err != nil || len(ids) != 1 || ids[0] != f.oldRun {
					t.Fatalf("lock exhausted run: ids=%v err=%v", ids, err)
				}
				e, err := qtx.ReadWorkerExhaustionEvidence(ctx, ids)
				if err != nil || len(e) != 1 {
					t.Fatalf("read absence: %v %v", e, err)
				}
				if e[0].CheckpointTip.Valid || e[0].PublicationUncertain || e[0].CustodyUncertain ||
					e[0].AvailableCapture || e[0].CaptureUncertain || e[0].UnknownEvidence {
					t.Fatalf("fixture must have real absence of recovery evidence: %+v", e[0])
				}
				exhaustionPublishFail(t, ctx, qtx, w, f.oldRun)
				var pid int
				if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
					t.Fatal(err)
				}
				resume()
				// Each poll has the enclosing 15-second deadline. A broker call
				// fails immediately; a lockwait proves admission is serialized.
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				waiting := false
				for !waiting {
					select {
					case <-broker:
						t.Fatalf("exhaustion_first broker count before lockwait = %d, want ZERO", calls.Load())
					default:
					}
					if err := f.e.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))", pid).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if !waiting {
						select {
						case <-broker:
							t.Fatalf("exhaustion_first broker count before lockwait = %d, want ZERO", calls.Load())
						case <-ticker.C:
						case <-ctx.Done():
							t.Fatal("Publish never waited on exhaustion run lock")
						}
					}
				}
				if calls.Load() != 0 {
					t.Fatal("broker called while run row lock held")
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				tx = nil
			} else {
				switch scenario {
				case "stale_worker":
					f.e.exec(t, "UPDATE runs SET worker_id=$2 WHERE id=$1", f.oldRun, f.newWorker)
				case "stale_generation":
					f.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.oldRun)
				case "released_claim":
					f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.oldRun)
				}
				resume()
			}
			select {
			case got := <-out:
				joined = true
				if calls.Load() != 0 {
					t.Errorf("%s stale live admission broker count = %d, want ZERO", scenario, calls.Load())
				}
				if n := f.attemptCount(t, f.oldRun); n != 0 {
					t.Errorf("%s stale live insertion left %d attempts, want ZERO", scenario, n)
				}
				if got.res.Published {
					t.Errorf("stale live snapshot published: %+v", got.res)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}

	t.Run("terminal_wired_positive_control", func(t *testing.T) {
		f, w := exhaustionPublishFixture(t)
		f.e.exec(t, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.oldRun)
		calls := 0
		f.svc1.SetPublishFn(func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
			calls++
			var n int
			if err := f.e.pool.QueryRow(ctx, "SELECT count(*) FROM checkpoint_publish_attempts WHERE run_id=$1", f.oldRun).Scan(&n); err != nil {
				return pushbroker.Result{}, err
			}
			if n != 1 {
				return pushbroker.Result{}, fmt.Errorf("terminal broker sees %d attempts", n)
			}
			return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, errors.New("fixture unresolved forge outcome")
		})
		_, _ = f.svc1.Publish(f.e.ctx, w, f.oldRun, lateTip, []byte("pack"))
		if calls != 1 || f.attemptCount(t, f.oldRun) != 1 {
			t.Fatal("authorized terminal wired snapshot must reach broker with committed attempt")
		}
	})

	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("unwired_terminal_%v", terminal), func(t *testing.T) {
			f, w := exhaustionPublishFixture(t)
			if terminal {
				f.e.exec(t, "UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.oldRun)
			}
			f.svc1.SetTxBeginner(nil)
			f.svc1.SetRetentionLockPool(nil)
			var calls atomic.Int32
			f.svc1.SetPublishFn(func(context.Context, pushbroker.Options) (pushbroker.Result, error) {
				calls.Add(1)
				return pushbroker.Result{Disposition: pushbroker.PublishOutcomeUnknown}, errors.New("fixture unresolved forge outcome")
			})
			if terminal {
				_, _ = f.svc1.Publish(f.e.ctx, w, f.oldRun, lateTip, []byte("pack"))
			} else {
				ctx, cancel := context.WithTimeout(f.e.ctx, 15*time.Second)
				defer cancel()
				g := &exhaustionPublishSnapshot{Store: f.svc1.q, captured: make(chan struct{}), resume: make(chan struct{})}
				f.svc1.q = g
				out := exhaustionPublishStart(ctx, f, w)
				var resumeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(g.resume) }) }
				joined := false
				defer func() {
					resume()
					cancel()
					if !joined {
						<-out
					}
				}()
				select {
				case <-g.captured:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if g.owned.Status != "running" || g.owned.ClaimGeneration != 1 || g.owned.ClaimReleasedAt.Valid ||
					uuid.UUID(g.owned.WorkerID.Bytes) != w.ID {
					t.Fatal("gate did not capture a DB-backed live owned unreleased generation-1 tuple")
				}
				f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.oldRun)
				resume()
				select {
				case got := <-out:
					joined = true
					if got.res.Published {
						t.Errorf("stale unwired live snapshot published: %+v", got.res)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			want := int32(0)
			if terminal {
				want = 1
			}
			if calls.Load() != want {
				t.Errorf("unwired terminal=%v broker calls=%d, want %d", terminal, calls.Load(), want)
			}
			if n := f.attemptCount(t, f.oldRun); n != 0 {
				t.Errorf("unwired terminal=%v attempts=%d, want ZERO", terminal, n)
			}
		})
	}
}
