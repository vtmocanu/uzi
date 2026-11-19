package workersvc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3), post-commit side of a worker's terminal report: setStateWithTx runs the
// gated MR reconcile, the authoritative re-read and the judge / task-review / then-fix hooks on a
// context detached from the request (context.WithoutCancel) and with NO deadline. These LiveDB
// tests drive the real SetState report path and pin:
//
//   - consumer first: a reconcile that has written but not committed makes a retire pass back off;
//   - a request cancelled while the reconcile waits for the gate still gets its hooks;
//   - a hook that waits for the gate AND then for its own downstream lock is not cut off;
//   - the evidence writes (a deleted source, an ordinary row-lock holder) are never lost;
//   - two gated writers of one report each wait for their own holder.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// postWaitBound bounds every poll for a lock queue to form.
const postWaitBound = 10 * time.Second

// postSetup is a retire-enabled fixture (see raceFixture) whose service may judge, with a gate wait
// longer than any hold of these tests unless a test overrides it.
func postSetup(t *testing.T, judge bool) (*sweepFix, *Service) {
	t.Helper()
	f, svc := raceFixture(t, raceFamily{})
	if judge {
		f.e.exec(t, `UPDATE users SET judge_enabled = true WHERE id = $1`, f.e.userID)
	}
	return f, svc
}

// postWaitUntil polls cond until it holds or postWaitBound passes (a failure).
func postWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(postWaitBound); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// gateWaiting reports whether an admission-side writer is queued on the repository's gate.
func gateWaiting(t *testing.T, f *sweepFix) bool {
	t.Helper()
	return gateLocks(t, f, f.e.repoID, "ShareLock", false) > 0
}

// lockHolder is a test transaction that holds a lock for the duration of its caller's choosing.
type lockHolder struct {
	tx   pgx.Tx
	pid  int32
	done bool
}

func (h *lockHolder) release() {
	if !h.done {
		h.done = true
		_ = h.tx.Rollback(context.Background())
	}
}

func newLockHolder(t *testing.T, f *sweepFix, lockSQL string, args ...any) *lockHolder {
	t.Helper()
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &lockHolder{tx: tx}
	t.Cleanup(h.release)
	if err = tx.QueryRow(f.e.ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.e.ctx, lockSQL, args...); err != nil {
		t.Fatalf("lock %q: %v", lockSQL, err)
	}
	return h
}

// holdRowLock holds the run's row FOR UPDATE, the lock of an ordinary row writer.
func holdRowLock(t *testing.T, f *sweepFix, run uuid.UUID) *lockHolder {
	t.Helper()
	return newLockHolder(t, f, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, run)
}

// holdJudgeTargetLock holds the judge-target advisory lock of run, the one LockRunForJudge takes.
func holdJudgeTargetLock(t *testing.T, f *sweepFix, run uuid.UUID) *lockHolder {
	t.Helper()
	return newLockHolder(t, f, `SELECT pg_advisory_xact_lock($1, hashtext($2::uuid::text))`, store.JudgeTargetLockClass, run)
}

// waitersOn counts the backends blocked by the holder.
func (h *lockHolder) waiters(t *testing.T, f *sweepFix) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, h.pid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// judgeKeyWaiting counts the not-granted waiters on the judge-target advisory key of run.
func judgeKeyWaiting(t *testing.T, f *sweepFix, run uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype='advisory' AND classid::bigint=$1 AND objid::bigint=(hashtext($2::uuid::text)::bigint & 4294967295)
		  AND objsubid=2 AND NOT granted`, int64(store.JudgeTargetLockClass), run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// postReport is the outcome of a SetState call.
type postReport struct {
	run     store.Run
	applied bool
	err     error
	took    time.Duration
}

// reportAsync sends a worker report from a goroutine and delivers its outcome.
func reportAsync(ctx context.Context, svc *Service, f *sweepFix, run uuid.UUID, req StateRequest) <-chan postReport {
	ch := make(chan postReport, 1)
	go func() {
		start := time.Now()
		r, applied, err := svc.SetState(ctx, f.worker, run, req)
		ch <- postReport{run: r, applied: applied, err: err, took: time.Since(start)}
	}()
	return ch
}

// seedReviewTask is a running task of the fixture's worker created with a review requested.
func seedReviewTask(t *testing.T, f *sweepFix, svc *Service) uuid.UUID {
	t.Helper()
	run, err := svc.CreateTaskRun(f.e.ctx, f.e.userID, f.e.repoID, "review me", "", false, true, false, false, nil)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	f.e.exec(t, `UPDATE runs SET status='running', worker_id=$2 WHERE id=$1`, run.ID, f.worker.ID)
	return run.ID
}

// seedCIFixConsumer is a running ci_fix of the fixture's worker with no merge request yet.
func seedCIFixConsumer(t *testing.T, f *sweepFix) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.e.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, status, worker_id, issue_title, issue_description, pipeline_id, pipeline_ref)
		VALUES ($1, $2, $3, 'ci_fix', 'running', $4, 't', 'd', 1, $5)`, id, f.e.userID, f.e.repoID, f.worker.ID, "refs/heads/post-"+uuid.NewString())
	return id
}

func countWhere(t *testing.T, f *sweepFix, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func reviewsOf(t *testing.T, f *sweepFix, target uuid.UUID) int {
	t.Helper()
	return countWhere(t, f, `SELECT count(*) FROM runs WHERE review_target_run_id = $1`, target)
}

func judgesOf(t *testing.T, f *sweepFix, target uuid.UUID) int {
	t.Helper()
	return countWhere(t, f, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, target)
}

func runMR(t *testing.T, f *sweepFix, id uuid.UUID) (mr pgtype.Int8, branch pgtype.Text) {
	t.Helper()
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT mr_iid, branch FROM runs WHERE id = $1`, id).Scan(&mr, &branch); err != nil {
		t.Fatal(err)
	}
	return mr, branch
}

// A nonterminal ci_fix gains the source's merge request through the real report path. While the
// gated reconcile has written but not committed, a retire pass backs off on the gate and deletes
// nothing; once the consumer is committed the next pass keeps the ref on its own blocker.
func TestTerminalConsumerFirstReconcileRunMRLiveDB(t *testing.T) {
	start := time.Now()
	const mr = int64(778)
	f, svc := raceFixture(t, raceFamily{arm: func(t *testing.T, f *sweepFix) {
		raceBranch(t, f)
		f.e.exec(t, `UPDATE runs SET mr_iid=$2 WHERE id=$1`, f.run, mr)
	}})
	consumer := seedCIFixConsumer(t, f)
	g := newRaceGate(t)
	svc.reconcileBeforeCommitHook = func(uuid.UUID) { g.pause() }

	m := mr
	done := reportAsync(context.Background(), svc, f, consumer, StateRequest{State: "running", MrIID: &m})
	raceReached(t, g, "the reconcile to reach its commit")

	f.sweep()
	p, _ := f.pres()
	retired := f.deleteCount() != 0 || p.State != "live"
	gateBusy := p.LastError.String == "retire kept: consumer_gate_busy"
	g.release()
	if rep := raceAwait(t, done, "the report"); rep.err != nil {
		t.Fatalf("report: %v", rep.err)
	}
	if retired {
		t.Fatalf("retired while the reconcile was in flight (deletes=%d state=%s)", f.deleteCount(), p.State)
	}
	if !gateBusy {
		t.Fatalf("first pass last_error = %q, want the gate-busy keep", p.LastError.String)
	}
	if got, _ := runMR(t, f, consumer); !got.Valid || got.Int64 != mr {
		t.Fatalf("the consumer's mr_iid = %+v, want %d", got, mr)
	}

	f.due()
	f.sweep()
	p = f.mustPres("live")
	if p.LastError.String != "retire kept: mr_run_active" || f.deleteCount() != 0 {
		t.Fatalf("last_error = %q deletes=%d, want %q and no delete", p.LastError.String, f.deleteCount(), "retire kept: mr_run_active")
	}
	t.Logf("runtime %v", time.Since(start))
}

// A request context cancelled while the reconcile waits for the gate (after the state transaction
// committed) still gets its re-read and its hook: the detached post-commit context carries them.
func TestTerminalPostCommitCancelledRequestLiveDB(t *testing.T) {
	const mr = int64(779)
	cases := []struct {
		name  string
		seed  func(t *testing.T, f *sweepFix, svc *Service) uuid.UUID
		count func(t *testing.T, f *sweepFix, id uuid.UUID) int
	}{
		{"task review", func(t *testing.T, f *sweepFix, svc *Service) uuid.UUID { return seedReviewTask(t, f, svc) }, reviewsOf},
		{"judge", func(t *testing.T, f *sweepFix, _ *Service) uuid.UUID { return f.e.seedLegacyRunningRun(t, f.worker.ID) }, judgesOf},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			f, svc := postSetup(t, tc.name == "judge")
			svc.consumerGateWait = 20 * time.Second // longer than the hold: the wait itself must not time out
			id := tc.seed(t, f, svc)
			f.e.exec(t, `UPDATE runs SET branch = COALESCE(branch, $2) WHERE id = $1`, id, "uzi/task/"+uuid.NewString())

			release := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := mr
			done := reportAsync(ctx, svc, f, id, StateRequest{State: "completed", MrIID: &m})
			postWaitUntil(t, "the reconcile to queue on the gate", func() bool { return gateWaiting(t, f) })

			cancel()
			time.Sleep(150 * time.Millisecond) // the detached wait must survive the cancellation
			if !gateWaiting(t, f) {
				release()
				t.Fatal("the reconcile gave up its gate wait when the request was cancelled")
			}
			release()
			rep := raceAwait(t, done, "the report")
			if rep.err != nil {
				t.Fatalf("report after a cancelled request = %v, want the authoritative re-read to succeed", rep.err)
			}
			if rep.run.Status != "completed" {
				t.Fatalf("returned status %q, want completed", rep.run.Status)
			}
			if got, _ := runMR(t, f, id); !got.Valid || got.Int64 != mr {
				t.Fatalf("mr_iid = %+v, want %d recorded", got, mr)
			}
			if n := tc.count(t, f, id); n != 1 {
				t.Fatalf("%s runs created = %d, want 1", tc.name, n)
			}
			t.Logf("runtime %v", time.Since(start))
		})
	}
}

// One hook that waits for the gate and then for its own downstream lock must not be cut off by any
// deadline: the first wait is below the 6s gate wait, the combined wait is well over 8s.
func TestTerminalPostCommitSameHookNoDeadlineLiveDB(t *testing.T) {
	const (
		gateHold  = 3500 * time.Millisecond
		totalHold = 10 * time.Second
	)
	cases := []struct {
		name string
		seed func(t *testing.T, f *sweepFix, svc *Service) uuid.UUID
		// second takes the downstream lock the hook's create queues on, and reports whether it does.
		second func(t *testing.T, f *sweepFix, id uuid.UUID) (h *lockHolder, queued func() bool)
		count  func(t *testing.T, f *sweepFix, id uuid.UUID) int
	}{
		{
			name: "judge",
			seed: func(t *testing.T, f *sweepFix, _ *Service) uuid.UUID { return f.e.seedLegacyRunningRun(t, f.worker.ID) },
			second: func(t *testing.T, f *sweepFix, id uuid.UUID) (*lockHolder, func() bool) {
				return holdJudgeTargetLock(t, f, id), func() bool { return judgeKeyWaiting(t, f, id) > 0 }
			},
			count: judgesOf,
		},
		{
			// A review create takes no run-branch lock; its downstream wait is the foreign-key check on
			// the reviewed target, which queues behind a row-lock holder of that target.
			name: "task review",
			seed: func(t *testing.T, f *sweepFix, svc *Service) uuid.UUID { return seedReviewTask(t, f, svc) },
			second: func(t *testing.T, f *sweepFix, id uuid.UUID) (*lockHolder, func() bool) {
				h := holdRowLock(t, f, id)
				return h, func() bool { return h.waiters(t, f) > 0 }
			},
			count: reviewsOf,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, svc := postSetup(t, tc.name == "judge")
			id := tc.seed(t, f, svc)

			release := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
			start := time.Now()
			done := reportAsync(context.Background(), svc, f, id, StateRequest{State: "completed"})
			postWaitUntil(t, "the hook to queue on the gate", func() bool { return gateWaiting(t, f) })
			gateStart := time.Now()
			time.Sleep(gateHold - time.Since(gateStart))

			// Take the downstream lock first, then free the gate: the hook must move from one queue to the other.
			holder, queued := tc.second(t, f, id)
			release()
			postWaitUntil(t, "the hook to queue on its downstream lock", queued)
			if gateWaiting(t, f) {
				t.Fatal("the hook still waits on the gate after it was released")
			}
			secondStart := time.Now()
			gateWait := secondStart.Sub(gateStart)
			time.Sleep(totalHold - gateWait - time.Since(secondStart))
			holder.release()
			holdWait := time.Since(secondStart)

			rep := raceAwait(t, done, "the report")
			if rep.err != nil {
				t.Fatalf("report: %v", rep.err)
			}
			if n := tc.count(t, f, id); n != 1 {
				t.Fatalf("%s runs created after both waits = %d, want 1 (a deadline cut the hook off)", tc.name, n)
			}
			if combined := gateWait + holdWait; combined <= 8*time.Second {
				t.Fatalf("combined wait inside the hook = %v, want over 8s", combined)
			}
			t.Logf("gate wait %v, downstream wait %v, report %v, total %v", gateWait, holdWait, rep.took, time.Since(start))
		})
	}
}

// The evidence writes of a report are never dropped by the gate: a source whose preservation is
// already deleted, and a consumer row locked by an ordinary writer for longer than the gate wait.
func TestTerminalPostCommitEvidenceLiveDB(t *testing.T) {
	t.Run("deleted source", func(t *testing.T) {
		start := time.Now()
		f, svc := postSetup(t, false)
		if n := f.sweep(); n != 2 {
			t.Fatalf("retire pass changed %d rows, want 2", n)
		}
		f.mustPres("deleted")
		// A terminal run whose worker still holds the claim (the retire pass does not need the claim
		// released) is the shape of a late report on a deleted source; a released claim refuses reports.
		f.e.exec(t, `UPDATE runs SET claim_released_at = NULL WHERE id = $1`, f.run)

		srcMR, srcBranch := int64(801), "agent/issue-post-801"
		ciMR, ciBranch := int64(802), "agent/issue-post-802"
		consumer := seedCIFixConsumer(t, f)
		for _, tc := range []struct {
			name   string
			run    uuid.UUID
			state  string
			mr     int64
			branch string
		}{
			{"source", f.run, "running", srcMR, srcBranch},
			{"ci_fix consumer", consumer, "running", ciMR, ciBranch},
		} {
			mr, branch := tc.mr, tc.branch
			rep := <-reportAsync(context.Background(), svc, f, tc.run, StateRequest{State: tc.state, MrIID: &mr, Branch: &branch})
			if rep.err != nil {
				t.Fatalf("%s report: %v", tc.name, rep.err)
			}
			gotMR, gotBranch := runMR(t, f, tc.run)
			if !gotMR.Valid || gotMR.Int64 != tc.mr || gotBranch.String != tc.branch {
				t.Fatalf("%s: mr_iid=%+v branch=%+v, want %d and %q", tc.name, gotMR, gotBranch, tc.mr, tc.branch)
			}
		}
		f.mustPres("deleted")
		t.Logf("runtime %v", time.Since(start))
	})

	t.Run("row-lock holder past the gate wait", func(t *testing.T) {
		const (
			mr       = int64(803)
			rowHold  = 6500 * time.Millisecond
			gateWait = 6 * time.Second // terminalConsumerGateWait: the wait this test must outlast
		)
		f, svc := postSetup(t, false)
		consumer := seedCIFixConsumer(t, f)

		release := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
		start := time.Now()
		m := mr
		done := reportAsync(context.Background(), svc, f, consumer, StateRequest{State: "running", MrIID: &m})
		postWaitUntil(t, "the reconcile to queue on the gate", func() bool { return gateWaiting(t, f) })

		holder := holdRowLock(t, f, consumer)
		release()
		postWaitUntil(t, "the reconcile write to queue on the row", func() bool { return holder.waiters(t, f) > 0 })
		rowStart := time.Now()
		time.Sleep(rowHold)
		holder.release()
		held := time.Since(rowStart)

		rep := raceAwait(t, done, "the report")
		if rep.err != nil {
			t.Fatalf("report: %v", rep.err)
		}
		if held < gateWait {
			t.Fatalf("row held for %v, want over the %v gate wait", held, gateWait)
		}
		if got, _ := runMR(t, f, consumer); !got.Valid || got.Int64 != mr {
			t.Fatalf("mr_iid = %+v after the row holder committed, want %d (the write must outwait a row lock longer than the gate wait)", got, mr)
		}
		t.Logf("row held %v, report %v, total %v", held, rep.took, time.Since(start))
	})
}

// One terminal report with two gated writers (the MR reconcile, then the review hook) behind two
// successive exclusive holders: each writer waits for its own holder only, the evidence and the
// review land, and the deleted preservation is not reopened.
func TestTerminalPostCommitCombinedContentionLiveDB(t *testing.T) {
	const (
		mr   = int64(804)
		hold = time.Second
	)
	f, svc := postSetup(t, false)
	if n := f.sweep(); n != 2 {
		t.Fatalf("retire pass changed %d rows, want 2", n)
	}
	f.mustPres("deleted")
	task := seedReviewTask(t, f, svc)
	f.e.exec(t, `UPDATE runs SET branch = COALESCE(branch, $2) WHERE id = $1`, task, "uzi/task/"+uuid.NewString())

	// While the reconcile holds the gate shared (before its commit), a second exclusive holder queues
	// behind it: it is granted the moment the reconcile commits, ahead of the hook's gate request.
	secondTx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	second := &lockHolder{tx: secondTx}
	waitCtx, cancelWait := context.WithCancel(f.e.ctx)
	finished := make(chan struct{})
	var startOnce sync.Once
	var waitErr error
	startSecond := func() {
		startOnce.Do(func() {
			go func() {
				defer close(finished)
				_, waitErr = secondTx.Exec(waitCtx, "SELECT pg_advisory_xact_lock($1, $2)", store.TerminalConsumerGateLockClass, store.TerminalConsumerGateObjID(f.e.repoID))
			}()
		})
	}
	// Own the transaction before any waiter starts. On failure, cancellation and
	// joining precede rollback, including when the report never reaches the hook.
	t.Cleanup(func() {
		cancelWait()
		startSecond()
		<-finished
		second.release()
	})
	svc.reconcileBeforeCommitHook = func(uuid.UUID) {
		startSecond()
		for deadline := time.Now().Add(postWaitBound); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if waitCtx.Err() != nil {
				return
			}
			if gateLocks(t, f, f.e.repoID, "ExclusiveLock", false) > 0 {
				return
			}
		}
		t.Error("the second holder never queued behind the reconcile")
	}

	first := holdGateExclusive(f.e.ctx, t, f.e.pool, f.e.repoID)
	start := time.Now()
	m := mr
	done := reportAsync(context.Background(), svc, f, task, StateRequest{State: "completed", MrIID: &m})
	postWaitUntil(t, "the reconcile to queue behind the first holder", func() bool { return gateWaiting(t, f) })
	time.Sleep(hold)
	first()

	raceAwait(t, finished, "the second holder")
	if waitErr != nil {
		t.Fatalf("the second holder: %v", waitErr)
	}
	postWaitUntil(t, "the review hook to queue behind the second holder", func() bool { return gateWaiting(t, f) })
	time.Sleep(hold)
	second.release()

	rep := raceAwait(t, done, "the report")
	if rep.err != nil {
		t.Fatalf("report: %v", rep.err)
	}
	if got, _ := runMR(t, f, task); !got.Valid || got.Int64 != mr {
		t.Fatalf("mr_iid = %+v, want %d recorded", got, mr)
	}
	if n := reviewsOf(t, f, task); n != 1 {
		t.Fatalf("review runs created = %d, want 1", n)
	}
	// Each writer waits for its own holder: the gate-added time is about the two 1s holds. The upper
	// bound is a loose sanity limit; a writer that hit busy fails the mr_iid and review checks above.
	if rep.took < 2*hold || rep.took > 2*4*time.Second+2*time.Second {
		t.Fatalf("report took %v, want between %v and 10s", rep.took, 2*hold)
	}
	f.mustPres("deleted")
	t.Logf("report %v, total %v", rep.took, time.Since(start))
}
