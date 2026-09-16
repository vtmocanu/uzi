package workersvc

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// outageIIDSeq hands each seeded run a distinct issue_iid so the partial-unique
// (repo_id, issue_iid) index over active runs never trips when many runs share one repo.
var outageIIDSeq int64 = time.Now().UnixNano() % 1_000_000_000

func nextOutageIID() int64 { return atomic.AddInt64(&outageIIDSeq, 1) }

// seedOutageRun inserts a run owned by workerID at the given status/kind/generation and
// requeue_count, with every time-based sweep window pushed 3h into the past so ONLY the
// window under test (or the D11 lease/overflow predicate) decides its fate.
func seedOutageRun(t *testing.T, env codexTestEnv, userID, repoID, workerID uuid.UUID, status, kind string, gen int64, requeueCount int32) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, requeue_count)
	          VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, $9)`,
		runID, userID, repoID, kind, nextOutageIID(), status, workerID, gen, requeueCount)
	env.exec(`UPDATE runs SET started_at = now() - interval '3 hours',
	                          status_since = now() - interval '3 hours',
	                          claimed_at = now() - interval '3 hours'
	          WHERE id = $1`, runID)
	return runID
}

// seedOutageChatRun inserts a chat run owned by workerID. A chat run's shape constraint
// (runs_kind_shape) requires repo_id/issue_iid/branch NULL, so it cannot go through
// seedOutageRun. status_since is pushed into the past like the others.
func seedOutageChatRun(t *testing.T, env codexTestEnv, userID, workerID uuid.UUID, gen int64, requeueCount int32) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, requeue_count)
	          VALUES ($1, $2, NULL, 'chat', NULL, 't', 'd', 'running', $3, $4, $5)`,
		runID, userID, workerID, gen, requeueCount)
	env.exec(`UPDATE runs SET started_at = now() - interval '3 hours',
	                          status_since = now() - interval '3 hours',
	                          claimed_at = now() - interval '3 hours'
	          WHERE id = $1`, runID)
	return runID
}

// seedOutageWorker inserts a fresh online worker whose last heartbeat is `staleFor` in the
// past (0 = a fresh heartbeat now).
func seedOutageWorker(t *testing.T, env codexTestEnv, userID uuid.UUID, staleFor time.Duration) uuid.UUID {
	t.Helper()
	workerID := uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, "w-"+workerID.String(), workerID[:])
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - make_interval(secs => $2) WHERE id = $1`,
		workerID, staleFor.Seconds())
	return workerID
}

// insertActiveLease writes a worker_active_runs row for (workerID, runID) at gen, with the
// given terminal_pending flag and a terminal_pending_until at now()+untilInterval (pass a
// negative interval like '-1 minute' for an expired lease).
func insertActiveLease(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID, gen int64, terminalPending bool, untilInterval string) {
	t.Helper()
	env.exec(`INSERT INTO worker_active_runs
	              (worker_id, run_id, claim_generation, phase, terminal_pending, terminal_pending_until, snapshot_epoch, reported_at)
	          VALUES ($1, $2, $3, 'running', $4, now() + $5::interval, 1, now())`,
		workerID, runID, gen, terminalPending, untilInterval)
}

// setOverflow flags/clears the worker-level pending_overflow closure with an expiry at
// now()+untilInterval.
func setOverflow(t *testing.T, env codexTestEnv, workerID uuid.UUID, untilInterval string) {
	t.Helper()
	env.exec(`UPDATE workers SET pending_overflow = true, pending_overflow_until = now() + $2::interval WHERE id = $1`,
		workerID, untilInterval)
}

// terminalWriter models one D11-honouring terminal writer: how to seed a run for it, how to
// fire it, and the status the run reaches when it IS swept.
type terminalWriter struct {
	name        string
	initStatus  string
	sweptStatus string
	// staleFor sets the seeded worker's heartbeat age (the stale passes need it).
	staleFor time.Duration
	fire     func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID)
}

func outageTerminalWriters() []terminalWriter {
	fireSweepRunningTimeout := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.SweepRunningTimeout(env.ctx, store.SweepRunningTimeoutParams{
			FailureReason:        pgconv.TextOrNull("run exceeded RUN_TIMEOUT"),
			Now:                  pgconv.Time(time.Now()),
			GlobalTimeoutSeconds: 7200,
			WorkerStaleCutoff:    pgconv.Time(time.Now().Add(-45 * time.Second)),
		}); err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
	}
	fireClaimedNeverStarted := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.SweepClaimedNeverStarted(env.ctx, pgconv.Time(time.Now().Add(-5*time.Minute))); err != nil {
			t.Fatalf("SweepClaimedNeverStarted: %v", err)
		}
	}
	fireAutoStop := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.FailRunAutoStop(env.ctx, store.FailRunAutoStopParams{
			ID:            runID,
			FailureReason: pgconv.TextOrNull("uzi stopped this run"),
		}); err != nil {
			t.Fatalf("FailRunAutoStop: %v", err)
		}
	}
	fireStaleFail := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
			FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
			MaxRequeues:   1,
			FailCutoff:    pgconv.Time(time.Now().Add(-90 * time.Second)),
		}); err != nil {
			t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
		}
	}
	fireStaleRequeue := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.RequeueRunsOfStaleWorkers(env.ctx, store.RequeueRunsOfStaleWorkersParams{
			MaxRequeues: 1,
			Cutoff:      pgconv.Time(time.Now().Add(-45 * time.Second)),
		}); err != nil {
			t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
		}
	}
	fireRegisterFail := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.FailWorkerRunsOverCap(env.ctx, store.FailWorkerRunsOverCapParams{
			FailureReason: pgconv.TextOrNull("worker restarted; exceeded re-queue budget"),
			WorkerID:      pgtype.UUID{Bytes: workerID, Valid: true},
			MaxRequeues:   1,
		}); err != nil {
			t.Fatalf("FailWorkerRunsOverCap: %v", err)
		}
	}
	fireRegisterRequeue := func(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) {
		if _, err := env.q.RequeueWorkerRuns(env.ctx, store.RequeueWorkerRunsParams{
			WorkerID:    pgtype.UUID{Bytes: workerID, Valid: true},
			MaxRequeues: 1,
		}); err != nil {
			t.Fatalf("RequeueWorkerRuns: %v", err)
		}
	}
	return []terminalWriter{
		{name: "SweepRunningTimeout", initStatus: "running", sweptStatus: "failed", staleFor: 0, fire: fireSweepRunningTimeout},
		{name: "SweepClaimedNeverStarted", initStatus: "claimed", sweptStatus: "queued", staleFor: 0, fire: fireClaimedNeverStarted},
		{name: "FailRunAutoStop", initStatus: "running", sweptStatus: "failed", staleFor: 0, fire: fireAutoStop},
		{name: "FailRunsOfStaleWorkersOverCap", initStatus: "running", sweptStatus: "failed", staleFor: 120 * time.Second, fire: fireStaleFail},
		{name: "RequeueRunsOfStaleWorkers", initStatus: "running", sweptStatus: "queued", staleFor: 60 * time.Second, fire: fireStaleRequeue},
		{name: "FailWorkerRunsOverCap", initStatus: "running", sweptStatus: "failed", staleFor: 0, fire: fireRegisterFail},
		{name: "RequeueWorkerRuns", initStatus: "running", sweptStatus: "queued", staleFor: 0, fire: fireRegisterRequeue},
	}
}

// requeueCountFor gives a writer a run whose requeue_count matches its budget predicate:
// the over-cap fail paths need requeue_count >= 1 (== RUN_MAX_REQUEUES), the requeue paths
// need requeue_count < 1.
func requeueCountFor(w terminalWriter) int32 {
	switch w.name {
	case "FailRunsOfStaleWorkersOverCap", "FailWorkerRunsOverCap":
		return 1
	default:
		return 0
	}
}

// TestD11TerminalPendingLeaseProtectsAllWritersLiveDB (PRD #1390 M1, D11): an unexpired
// terminal-pending lease on a run's current generation stops EVERY terminal writer from
// moving it, and an expired lease lets each one sweep it. Each writer gets its own fresh
// worker+run so their staleness/scope requirements never interfere.
func TestD11TerminalPendingLeaseProtectsAllWritersLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	for _, w := range outageTerminalWriters() {
		t.Run(w.name, func(t *testing.T) {
			// (1) Unexpired lease ⇒ protected.
			wkLeased := seedOutageWorker(t, env, userID, w.staleFor)
			runLeased := seedOutageRun(t, env, userID, repoID, wkLeased, w.initStatus, "issue", 1, requeueCountFor(w))
			insertActiveLease(t, env, wkLeased, runLeased, 1, true, "1 hour")
			w.fire(t, env, wkLeased, runLeased)
			if got := statusOf(t, env, runLeased); got != w.initStatus {
				t.Fatalf("%s: leased run status = %q, want unchanged %q (D11 lease must protect it)", w.name, got, w.initStatus)
			}

			// (2) Expired lease ⇒ swept as usual.
			wkExpired := seedOutageWorker(t, env, userID, w.staleFor)
			runExpired := seedOutageRun(t, env, userID, repoID, wkExpired, w.initStatus, "issue", 1, requeueCountFor(w))
			insertActiveLease(t, env, wkExpired, runExpired, 1, true, "-1 minute")
			w.fire(t, env, wkExpired, runExpired)
			if got := statusOf(t, env, runExpired); got != w.sweptStatus {
				t.Fatalf("%s: expired-lease run status = %q, want swept to %q", w.name, got, w.sweptStatus)
			}
		})
	}
}

// TestD11PendingOverflowClosureProtectsAllWritersLiveDB (PRD #1390 M1, D11): a worker flagged
// pending_overflow (with an unexpired pending_overflow_until) closes every run it owns to every
// terminal writer — even a run with NO worker_active_runs row of its own, since the worker-level
// closure stands in for the row-level lease it could not express. Expiry reopens it.
func TestD11PendingOverflowClosureProtectsAllWritersLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	for _, w := range outageTerminalWriters() {
		t.Run(w.name, func(t *testing.T) {
			// (1) Unexpired overflow ⇒ protected, with NO active-run lease.
			wkFlagged := seedOutageWorker(t, env, userID, w.staleFor)
			setOverflow(t, env, wkFlagged, "1 hour")
			runFlagged := seedOutageRun(t, env, userID, repoID, wkFlagged, w.initStatus, "issue", 1, requeueCountFor(w))
			w.fire(t, env, wkFlagged, runFlagged)
			if got := statusOf(t, env, runFlagged); got != w.initStatus {
				t.Fatalf("%s: run under pending_overflow status = %q, want unchanged %q (closure must protect it)", w.name, got, w.initStatus)
			}

			// (2) Expired overflow ⇒ swept as usual.
			wkExpired := seedOutageWorker(t, env, userID, w.staleFor)
			setOverflow(t, env, wkExpired, "-1 minute")
			runExpired := seedOutageRun(t, env, userID, repoID, wkExpired, w.initStatus, "issue", 1, requeueCountFor(w))
			w.fire(t, env, wkExpired, runExpired)
			if got := statusOf(t, env, runExpired); got != w.sweptStatus {
				t.Fatalf("%s: run under expired overflow status = %q, want swept to %q", w.name, got, w.sweptStatus)
			}
		})
	}
}

// TestD11ChatRunSweptDespiteOverflowLiveDB (PRD #1390 D10): the lease/overflow protection is
// PROTECTION only — a chat run is exempt from it and is still swept/transitioned, so the guard
// must be `kind = 'chat' OR NOT EXISTS(...)`, never a top-level `kind <> 'chat'`. Proven against
// the stale requeue and the register requeue, with a non-chat sibling under the same overflow
// left protected as the contrast.
func TestD11ChatRunSweptDespiteOverflowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	t.Run("stale requeue sweeps chat under overflow", func(t *testing.T) {
		wk := seedOutageWorker(t, env, userID, 60*time.Second)
		setOverflow(t, env, wk, "1 hour")
		chatRun := seedOutageChatRun(t, env, userID, wk, 1, 0)
		issueRun := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		if _, err := env.q.RequeueRunsOfStaleWorkers(env.ctx, store.RequeueRunsOfStaleWorkersParams{
			MaxRequeues: 1,
			Cutoff:      pgconv.Time(time.Now().Add(-45 * time.Second)),
		}); err != nil {
			t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
		}
		if got := statusOf(t, env, chatRun); got != "queued" {
			t.Fatalf("chat run status = %q, want queued (D10: chat is swept despite overflow)", got)
		}
		if got := statusOf(t, env, issueRun); got != "running" {
			t.Fatalf("issue sibling status = %q, want unchanged running (overflow protects the run lane)", got)
		}
	})

	t.Run("register requeue sweeps chat under overflow", func(t *testing.T) {
		wk := seedOutageWorker(t, env, userID, 0)
		setOverflow(t, env, wk, "1 hour")
		chatRun := seedOutageChatRun(t, env, userID, wk, 1, 0)
		issueRun := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		if _, err := env.q.RequeueWorkerRuns(env.ctx, store.RequeueWorkerRunsParams{
			WorkerID:    pgtype.UUID{Bytes: wk, Valid: true},
			MaxRequeues: 1,
		}); err != nil {
			t.Fatalf("RequeueWorkerRuns: %v", err)
		}
		if got := statusOf(t, env, chatRun); got != "queued" {
			t.Fatalf("chat run status = %q, want queued (D10: chat is swept despite overflow)", got)
		}
		if got := statusOf(t, env, issueRun); got != "running" {
			t.Fatalf("issue sibling status = %q, want unchanged running (overflow protects the run lane)", got)
		}
	})
}

// bootGraceParams returns testParams with the boot grace overridden.
func bootGraceParams(grace time.Duration) Params {
	p := testParams()
	p.SweeperBootGrace = grace
	return p
}

// TestSweepBootGraceSkipsStalePassesLiveDB (PRD #1390 M1, D1) drives the FULL Sweep through the
// Service: while the boot grace is active (listener just became ready) a sweep leaves a stale
// worker's run running and does NOT mark the worker offline; once the grace has elapsed the next
// sweep runs the stale passes and requeues the run. This exercises sweep.go's gating and the
// listener-anchored readyAt, not just the pure helper.
func TestSweepBootGraceSkipsStalePassesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	svc := New(env.q, env.box, bootGraceParams(60*time.Second))
	svc.SetBackground(func(func()) {}) // no detached work outliving the pool

	// A worker stale for 120s (well past both windows) with a within-budget running run whose
	// wall clock is fresh (so SweepRunningTimeout never fires and only the stale passes matter).
	wk := seedOutageWorker(t, env, userID, 120*time.Second)
	run := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
	env.exec(`UPDATE runs SET started_at = now() - interval '5 minutes', status_since = now() - interval '5 minutes' WHERE id = $1`, run)

	// Grace ACTIVE: the listener just became ready.
	svc.SetReadyAt(time.Now())
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatalf("Sweep (grace active): %v", err)
	}
	if got := statusOf(t, env, run); got != "running" {
		t.Fatalf("run status = %q under active boot grace, want unchanged running (stale passes skipped)", got)
	}
	var wkStatus string
	if err := env.pool.QueryRow(env.ctx, `SELECT status FROM workers WHERE id = $1`, wk).Scan(&wkStatus); err != nil {
		t.Fatalf("read worker status: %v", err)
	}
	if wkStatus != "online" {
		t.Fatalf("worker status = %q under active boot grace, want online (MarkStaleWorkersOffline skipped)", wkStatus)
	}

	// Grace ELAPSED: anchor readyAt 2 minutes ago so now > readyAt+60s.
	svc.SetReadyAt(time.Now().Add(-2 * time.Minute))
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatalf("Sweep (grace elapsed): %v", err)
	}
	if got := statusOf(t, env, run); got != "queued" {
		t.Fatalf("run status = %q after the boot grace elapsed, want requeued to queued", got)
	}
}

// TestStaleFailTwoWindowLiveDB (PRD #1390 M1, D9): the over-cap FAIL waits for TWO consecutive
// stale windows (fail_cutoff = now - 2*WORKER_HEARTBEAT_STALE) while the REQUEUE fires after
// one. A worker stale for ~60s (past one 45s window, short of two) has its within-budget run
// requeued but its over-budget run NOT failed; once it crosses the second window the fail lands.
func TestStaleFailTwoWindowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	// Worker stale for 60s: past one window (45s), short of two (90s).
	wk := seedOutageWorker(t, env, userID, 60*time.Second)
	overBudget := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 1) // requeue_count == max
	withinBudget := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)

	failCutoff := pgconv.Time(time.Now().Add(-90 * time.Second))
	staleCutoff := pgconv.Time(time.Now().Add(-45 * time.Second))

	// The requeue fires after ONE window.
	if _, err := env.q.RequeueRunsOfStaleWorkers(env.ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 1, Cutoff: staleCutoff,
	}); err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	if got := statusOf(t, env, withinBudget); got != "queued" {
		t.Fatalf("within-budget run status = %q, want queued (requeue fires after one window)", got)
	}
	// D2 provenance: the requeue stamped stale_requeue_generation = claim_generation.
	var srg pgtype.Int8
	if err := env.pool.QueryRow(env.ctx, `SELECT stale_requeue_generation FROM runs WHERE id = $1`, withinBudget).Scan(&srg); err != nil {
		t.Fatalf("read stale_requeue_generation: %v", err)
	}
	if !srg.Valid || srg.Int64 != 1 {
		t.Fatalf("stale_requeue_generation = %+v, want 1 (D2 provenance stamped by the requeue)", srg)
	}

	// The over-cap fail does NOT fire at one window (worker is stale 60s < 90s).
	if _, err := env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
		MaxRequeues:   1, FailCutoff: failCutoff,
	}); err != nil {
		t.Fatalf("FailRunsOfStaleWorkersOverCap (one window): %v", err)
	}
	if got := statusOf(t, env, overBudget); got != "running" {
		t.Fatalf("over-budget run status = %q, want unchanged running (fail waits for the second window)", got)
	}

	// Age the worker past the second window and re-run the fail: now it lands.
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '120 seconds' WHERE id = $1`, wk)
	if _, err := env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
		MaxRequeues:   1, FailCutoff: pgconv.Time(time.Now().Add(-90 * time.Second)),
	}); err != nil {
		t.Fatalf("FailRunsOfStaleWorkersOverCap (two windows): %v", err)
	}
	if got := statusOf(t, env, overBudget); got != "failed" {
		t.Fatalf("over-budget run status = %q, want failed once the second window elapsed", got)
	}
}

// TestD11ForeignWorkerLeaseDoesNotProtectSiblingReclaimLiveDB (PRD #1390 M2a, D11 worker-scoping
// fix): the terminal-pending-lease NOT EXISTS sub-clause keys on the run's CURRENT owner
// (a.worker_id = runs.worker_id), so a FOREIGN worker's stale lease — planted at a forged future
// generation for a run it once owned — cannot survive a sibling worker's reclaim and block that
// sibling's worker-loss recovery. This is the cross-worker CONSUMPTION path the existing D11
// tests do not cover: they exercise the same-worker lease and the write-side ownership drop, not
// a foreign row consumed by a sibling's recovery pass. The same-worker lease (honest
// re-adoption / #1391 boot-replay) still protects the run, proven as the positive contrast.
//
// Without the a.worker_id predicate the first subtest FAILS: worker A's forged lease at
// generation g+1 matches runs.claim_generation (also g+1 after B's reclaim) purely by run_id, so
// the NOT EXISTS is false and R stays running under a foreign worker's lease.
func TestD11ForeignWorkerLeaseDoesNotProtectSiblingReclaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	const g int64 = 5

	fireStaleRequeue := func(t *testing.T) {
		t.Helper()
		if _, err := env.q.RequeueRunsOfStaleWorkers(env.ctx, store.RequeueRunsOfStaleWorkersParams{
			MaxRequeues: 1,
			Cutoff:      pgconv.Time(time.Now().Add(-45 * time.Second)),
		}); err != nil {
			t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
		}
	}

	t.Run("foreign lease does not protect the sibling reclaim", func(t *testing.T) {
		// Worker A is alive; worker B is stale past the requeue window. R starts owned by A at
		// generation g, then A journals a terminal-pending lease at a FORGED future generation
		// g+1 (ReplaceWorkerActiveRuns validates an entry's claim_generation only as >= 0, never
		// against the run's real generation).
		workerA := seedOutageWorker(t, env, userID, 0)
		workerB := seedOutageWorker(t, env, userID, 120*time.Second)
		runR := seedOutageRun(t, env, userID, repoID, workerA, "running", "issue", g, 0)
		insertActiveLease(t, env, workerA, runR, g+1, true, "1 hour")

		// A relinquishes R and the SIBLING B reclaims it: generation advances to g+1 (ClaimRun
		// increments by exactly 1), so A's forged lease now equals runs.claim_generation. A's
		// (worker A, run R) row is not cleaned up when B reclaims — it is a foreign lease.
		env.exec(`UPDATE runs SET worker_id = $2, claim_generation = $3, status = 'running',
		                          status_since = now() - interval '3 hours',
		                          started_at = now() - interval '3 hours',
		                          claimed_at = now() - interval '3 hours'
		          WHERE id = $1`, runR, workerB, g+1)

		// B is stale → its worker-loss recovery must requeue R. A's foreign lease must NOT
		// protect it, because A is no longer the run's owner.
		fireStaleRequeue(t)
		if got := statusOf(t, env, runR); got != "queued" {
			t.Fatalf("run status = %q, want queued: a FOREIGN worker's stale lease must not block a sibling's worker-loss recovery", got)
		}
	})

	t.Run("same-worker lease still protects the run", func(t *testing.T) {
		// The honest re-adoption case is unbroken: B owns R at generation g+1 and holds its OWN
		// terminal-pending lease at that generation, so the run is legitimately protected from
		// the sweep (its outcome is journaled on the owning worker, #1391).
		workerB := seedOutageWorker(t, env, userID, 120*time.Second)
		runR := seedOutageRun(t, env, userID, repoID, workerB, "running", "issue", g+1, 0)
		insertActiveLease(t, env, workerB, runR, g+1, true, "1 hour")

		fireStaleRequeue(t)
		if got := statusOf(t, env, runR); got != "running" {
			t.Fatalf("run status = %q, want unchanged running: the run's OWN worker lease must still protect it (honest re-adoption)", got)
		}
	})
}
