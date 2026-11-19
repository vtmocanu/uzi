package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3), admission side of the consumer gate: which creates take it and on which
// key, what the failure modes return, and that a refused create leaves nothing behind. The retire-vs-
// admission interleavings are in terminal_consumer_race_livedb_test.go; the retire side is in
// terminal_retire_gate_livedb_test.go. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (./e2e/run-store-it.sh).

// shortGateWait is the test-only admission gate wait for the tests that expect a timeout.
const shortGateWait = 300 * time.Millisecond

// holdGateExclusive takes the repository's gate exclusively in a transaction the cleanup rolls back,
// the way a retirement holds it. release ends the hold early.
func holdGateExclusive(ctx context.Context, t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, repo uuid.UUID) (release func()) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := false
	rollback := func() {
		if !done {
			done = true
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(rollback)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", store.TerminalConsumerGateLockClass, store.TerminalConsumerGateObjID(repo)); err != nil {
		t.Fatalf("hold exclusive gate: %v", err)
	}
	return rollback
}

// gateShareWaiters counts the not-granted ShareLock waiters on the repository's gate key.
func gateShareWaiters(ctx context.Context, t *testing.T, env codexTestEnv, repo uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
		WHERE locktype='advisory' AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=2 AND mode='ShareLock' AND NOT granted`,
		int64(store.TerminalConsumerGateLockClass), int64(uint32(store.TerminalConsumerGateObjID(repo)))).Scan(&n); err != nil { //nolint:gosec // reinterpret the int4 lock key as pg_locks' oid
		t.Fatal(err)
	}
	return n
}

// seedOtherRepo copies the repository row of repoID (same forge connection and owner) under a new id.
func seedOtherRepo(t *testing.T, env codexTestEnv, repoID uuid.UUID) uuid.UUID {
	t.Helper()
	other := uuid.New()
	env.exec(`INSERT INTO repos(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		SELECT $1, connection_id, forge_project_id+1000, path_with_namespace||'-other', web_url||'-other', default_branch, true
		FROM repos WHERE id=$2`, other, repoID)
	return other
}

// consumerRunsOf counts the runs of the user other than the source run.
func consumerRunsOf(t *testing.T, env codexTestEnv, userID, source uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE user_id=$1 AND id<>$2`, userID, source).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// While a retirement holds the gate, creates that are not consumers of this repository's terminal
// refs (a prompt run and a plain task on the same repository, an issue run on another repository) complete and never
// queue on the gate.
func TestTerminalConsumerGatePositiveControlsLiveDB(t *testing.T) {
	f, svc := raceFixture(t, raceFamily{})
	bounds := retireTestBounds
	f.svc.terminal.retireBounds = &bounds
	g := newRaceGate(t)
	f.svc.terminal.beforeRetiringCommit = g.pause
	sweepDone := make(chan error, 1)
	go func() { sweepDone <- f.sweepErr() }()
	raceReached(t, g, "the retirement decision window")
	if got := gateLocks(t, f, f.e.repoID, "ExclusiveLock", true); got != 1 {
		g.release()
		t.Fatalf("exclusive gate holders during the window = %d, want 1", got)
	}

	start := time.Now()
	sched := uuid.New()
	f.e.exec(t, `INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, run_at, timezone)
		VALUES ($1, $2, $3, 'prompt', 'positive control', 'once', now(), 'UTC')`, sched, f.e.userID, f.e.repoID)
	if _, err := svc.CreatePromptRun(f.e.ctx, f.e.userID, f.e.repoID, sched, "t", "p", false, false, nil, nil, false, nil, nil); err != nil {
		g.release()
		t.Fatalf("prompt run on the gated repository: %v", err)
	}
	if n := gateLocks(t, f, f.e.repoID, "ShareLock", false); n != 0 {
		g.release()
		t.Fatalf("a prompt run queued on the gate (%d waiters)", n)
	}

	// A plain task (no continued issue) matches no blocker predicate and takes no gate.
	taskStart := time.Now()
	if _, err := svc.CreateTaskRun(f.e.ctx, f.e.userID, f.e.repoID, "plain task", "", false, false, false, false, nil); err != nil {
		g.release()
		t.Fatalf("plain task run on the gated repository: %v", err)
	}
	// A delayed grant must not hide a wrongly gated task: check elapsed time as
	// well as the error and the still-paused retirement below.
	if d := time.Since(taskStart); d > time.Second {
		g.release()
		t.Fatalf("plain task run took %v while the gate was held: it queued on the gate", d)
	}
	if n := gateLocks(t, f, f.e.repoID, "ShareLock", false); n != 0 {
		g.release()
		t.Fatalf("a plain task run queued on the gate (%d waiters)", n)
	}

	other := uuid.New()
	f.e.exec(t, `INSERT INTO repos(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		SELECT $1, connection_id, forge_project_id+1000, path_with_namespace||'-other', web_url||'-other', default_branch, true
		FROM repos WHERE id=$2`, other, f.e.repoID)
	f.e.exec(t, `INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		VALUES ($1, $2, 41, 'other repo issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), other)
	if _, err := svc.CreateRun(f.e.ctx, f.e.userID, other, 41, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		g.release()
		t.Fatalf("issue run on another repository: %v", err)
	}
	if n := gateLocks(t, f, f.e.repoID, "ShareLock", false); n != 0 {
		g.release()
		t.Fatalf("an issue run on another repository queued on the gated repository's gate (%d waiters)", n)
	}
	select {
	case err := <-sweepDone:
		t.Fatalf("the retirement finished while it was meant to be paused: %v", err)
	default:
	}
	t.Logf("both controls completed in %v while the gate was held", time.Since(start))

	g.release()
	if err := raceAwait(t, sweepDone, "the retire pass"); err != nil {
		t.Fatalf("retire pass: %v", err)
	}
}

// The judge gate key is the TARGET run's repository: a holder on the target's repository makes a
// rerun busy, a holder on any other repository does not.
func TestTerminalConsumerGateJudgeKeyLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	svc.SetSettings(fakeSettings{enabled: true})
	env.exec(`UPDATE user_secrets SET is_default = true WHERE user_id = $1 AND kind = 'anthropic_token'`, userID)
	svc.consumerGateWait = shortGateWait
	const n int64 = 44881
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 448810)

	release := holdGateExclusive(env.ctx, t, env.pool, repoID)
	if _, err := svc.RerunJudge(env.ctx, userID, false, source); !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("RerunJudge behind a holder on the target's repository = %v, want ErrTerminalConsumerGateBusy", err)
	}
	if got := consumerRunsOf(t, env, userID, source); got != 0 {
		t.Fatalf("a busy rerun left %d run row(s)", got)
	}
	release()

	holdGateExclusive(env.ctx, t, env.pool, uuid.New())
	if _, err := svc.RerunJudge(env.ctx, userID, false, source); err != nil {
		t.Fatalf("RerunJudge behind a holder on another repository: %v", err)
	}
}

// A target that moved to another repository between the caller's read and the gate is refused after
// the gate, and nothing is created: the revalidation inside the judge closure.
func TestTerminalConsumerGateJudgeIdentityLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	svc.SetSettings(fakeSettings{enabled: true})
	env.exec(`UPDATE user_secrets SET is_default = true WHERE user_id = $1 AND kind = 'anthropic_token'`, userID)
	const n int64 = 44882
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 448820)
	other := seedOtherRepo(t, env, repoID)
	svc.judgeBeforeLockHook = func(context.Context) {
		env.exec(`UPDATE runs SET repo_id=$2 WHERE id=$1`, source, other)
	}
	if _, err := svc.RerunJudge(env.ctx, userID, false, source); !errors.Is(err, ErrTerminalConsumerGateIdentity) {
		t.Fatalf("RerunJudge = %v, want ErrTerminalConsumerGateIdentity", err)
	}
	if got := consumerRunsOf(t, env, userID, source); got != 0 {
		t.Fatalf("a refused rerun left %d run row(s)", got)
	}
}

// Then-fix, review and MR-rework children are gated on the repository the caller names and refuse a
// target of another repository once the gate is held; nothing is inserted.
func TestTerminalConsumerGateIdentityLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 44883
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 448830)
	other := seedOtherRepo(t, env, repoID)
	branch := "uzi/task/" + uuid.NewString()

	cases := []struct {
		name string
		call func() error
	}{
		{"then-fix", func() error {
			_, err := svc.CreateThenFixRun(env.ctx, userID, other, source, branch, "main", "fix", pgtype.Int4{}, pgtype.Int4{}, HarnessClaude)
			return err
		}},
		{"task review", func() error {
			_, err := svc.CreateTaskReviewRun(env.ctx, userID, other, source, branch, "main", HarnessClaude)
			return err
		}},
		{"mr rework", func() error {
			_, err := svc.CreateAutoMRReworkRun(env.ctx, userID, other, agentIssueBranch(n), 448830, source, "t", "d", nil)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrTerminalConsumerGateIdentity) {
				t.Fatalf("create with a target of another repository = %v, want ErrTerminalConsumerGateIdentity", err)
			}
			if got := consumerRunsOf(t, env, userID, source); got != 0 {
				t.Fatalf("a refused create left %d run row(s)", got)
			}
		})
	}
}

// An MR-rework create that advances the review ledger in the same transaction, whose gate wait times
// out behind an exclusive holder, returns ErrTerminalConsumerGateBusy and leaves neither the run row
// nor the ledger row; once the holder is gone the same create succeeds.
func TestTerminalConsumerGateBusyRollsBackLedgerLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	svc.consumerGateWait = shortGateWait
	const n int64 = 44884
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 448840)
	res := reviewResultOf(sampleReviewSnapshot())

	release := holdGateExclusive(env.ctx, t, env.pool, repoID)
	start := time.Now()
	_, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 448840, source, "Rework", "d", res, 5)
	if !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("create behind an exclusive holder = %v, want ErrTerminalConsumerGateBusy", err)
	}
	if took := time.Since(start); took < shortGateWait/2 {
		t.Fatalf("returned after %v, before the gate wait %v", took, shortGateWait)
	}
	if got := consumerRunsOf(t, env, userID, source); got != 0 {
		t.Fatalf("%d run row(s) survived a busy create", got)
	}
	if _, lerr := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch}); !errors.Is(lerr, pgx.ErrNoRows) {
		t.Fatalf("ledger row present after a busy create (err=%v)", lerr)
	}

	release()
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 448840, source, "Rework", "d", res, 5); err != nil {
		t.Fatalf("create after the holder ended: %v", err)
	}
	if led, lerr := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch}); lerr != nil || led.AttemptCount != 1 {
		t.Fatalf("ledger after the successful create = %+v, %v, want attempt_count 1", led, lerr)
	}
}

// An MR-rework create that has taken the gate and written BOTH its run row and its ledger upsert, and
// then fails before COMMIT (the request context ends at the commit), rolls back both writes together
// and releases the gate; the retried create then succeeds.
func TestTerminalConsumerGateRollbackAfterWritesLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 44885
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 448850)
	res := reviewResultOf(sampleReviewSnapshot())

	gateObj := int64(uint32(store.TerminalConsumerGateObjID(repoID))) //nolint:gosec // the int4 lock key read back as pg_locks' oid
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	var hookRun store.Run
	var gateHeld int
	svc.createBeforeCommitHook = func(run store.Run) {
		hookRun = run
		// Reaching the hook already implies the ledger upsert ran (it precedes the hook in the insert
		// closure and a 0-row upsert errors first). Additionally require the shared gate and a write lock
		// on the ledger table on ONE backend: the create's own transaction.
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_locks g JOIN pg_locks w ON w.pid = g.pid
			JOIN pg_class c ON c.oid = w.relation
			WHERE g.locktype='advisory' AND g.classid::bigint=$1 AND g.objid::bigint=$2 AND g.objsubid=2
			  AND g.mode='ShareLock' AND g.granted
			  AND c.relname = 'mr_rework_ledger' AND w.mode = 'RowExclusiveLock' AND w.granted`,
			int64(store.TerminalConsumerGateLockClass), gateObj).Scan(&gateHeld); err != nil {
			t.Error(err)
		}
		cancel()
	}
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(ctx, userID, repoID, branch, 448850, source, "Rework", "d", res, 5); err == nil {
		t.Fatal("create whose commit ran on an ended context succeeded")
	}
	if hookRun.ID == uuid.Nil {
		t.Fatal("the create never reached its commit: the failure was not after the writes")
	}
	if gateHeld != 1 {
		t.Fatalf("at the commit point: backends holding both the shared gate and a ledger write lock = %d, want 1", gateHeld)
	}
	if got := consumerRunsOf(t, env, userID, source); got != 0 {
		t.Fatalf("%d run row(s) survived the rolled-back create", got)
	}
	if _, lerr := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch}); !errors.Is(lerr, pgx.ErrNoRows) {
		t.Fatalf("ledger row present after the rolled-back create (err=%v)", lerr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var held int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory'
			AND classid::bigint=$1 AND objid::bigint=$2 AND objsubid=2`,
			int64(store.TerminalConsumerGateLockClass), gateObj).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gate is still held %d time(s) after the rollback", held)
		}
		time.Sleep(10 * time.Millisecond)
	}

	svc.createBeforeCommitHook = nil
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 448850, source, "Rework", "d", res, 5); err != nil {
		t.Fatalf("retried create: %v", err)
	}
	if led, lerr := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: branch}); lerr != nil || led.AttemptCount != 1 {
		t.Fatalf("ledger after the retried create = %+v, %v, want attempt_count 1", led, lerr)
	}
}

// A service with no transaction beginner (the test-only fallback) still takes the gate on a gated
// create: it queues behind an exclusive holder and completes once the holder ends. An ungated create
// is unchanged.
func TestTerminalConsumerGateNoBeginnerFallbackLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedClaudeOnlyOriginUser(t, env)
	svc := New(env.q, env.box, Params{}) // no SetTxBeginner

	release := holdGateExclusive(env.ctx, t, env.pool, repoID)

	// Ungated: a prompt run is not held up by the gate.
	sched := seedPromptScheduleRow(t, env, userID, repoID)
	start := time.Now()
	if _, err := svc.CreatePromptRun(env.ctx, userID, repoID, sched, "t", "p", false, false, nil, nil, false, nil, nil); err != nil {
		t.Fatalf("ungated prompt run behind a holder: %v", err)
	}
	if n := gateShareWaiters(env.ctx, t, env, repoID); n != 0 {
		t.Fatalf("the ungated create queued on the gate (%d waiters)", n)
	}
	t.Logf("ungated create took %v", time.Since(start))

	// Gated: a self-improve create queues on the gate until the holder ends.
	done := make(chan error, 1)
	go func() {
		_, err := svc.CreateSelfImproveRun(env.ctx, userID, repoID, 44885, "t", "d", nil, nil, false, nil)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for gateShareWaiters(env.ctx, t, env, repoID) == 0 {
		if time.Now().After(deadline) {
			release()
			t.Fatal("the gated create never queued on the gate")
		}
		select {
		case err := <-done:
			release()
			t.Fatalf("the gated create returned while the gate was held exclusively: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	release()
	if err := raceAwait(t, done, "the gated create"); err != nil {
		t.Fatalf("gated create after the holder ended: %v", err)
	}

	// A gate wait that elapses is reported busy by the fallback too.
	svc.consumerGateWait = shortGateWait
	holdGateExclusive(env.ctx, t, env.pool, repoID)
	if _, err := svc.CreateSelfImproveRun(env.ctx, userID, repoID, 44886, "t", "d", nil, nil, false, nil); !errors.Is(err, ErrTerminalConsumerGateBusy) {
		t.Fatalf("gated create behind a holder with a short wait = %v, want ErrTerminalConsumerGateBusy", err)
	}
}
