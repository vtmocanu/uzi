package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCompletionInterlockSweepCarveOutLiveDB pins PRD #1226 M3 (D3) against a REAL Postgres: the
// SweepRunningTimeout completion-interlock carve-out. A run that has recorded at least one
// completion attempt (completion_attempts > 0) AND whose worker heartbeat is still inside the
// staleness bound (a LIVE worker) is EXCLUDED from the wall-clock sweep — its live lead is working
// the checkpoint-first completion protocol and will itself enter the verified hold (M4). The
// carve-out is DELIBERATELY NARROW:
//
//   - live worker + post-attempt + past wall  → NOT failed (the carve-out; mutation reddens this).
//   - live worker + PRE-attempt  + past wall  → still failed (unchanged; attempts=0 is not carved).
//   - stale worker + post-attempt + past wall → still failed by the sweep (the carve-out needs a
//     LIVE worker, so a dead-worker run can never be held indefinitely).
//   - stale worker + post-attempt + within budget → NOT swept (within wall) then REQUEUED by the
//     existing RequeueRunsOfStaleWorkers path (the carve-out lives only in SweepRunningTimeout and
//     never blocks stale-worker recovery).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the other
// *_livedb / *_integration_test.go in this package.
func TestCompletionInterlockSweepCarveOutLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("sweep-carveout-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// One LIVE worker (fresh heartbeat) and one STALE worker (heartbeat well before the cutoff).
	liveWorker, staleWorker := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'live', $3, 'online', now())`, liveWorker, userID, liveWorker[:])
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'stale', $3, 'offline', now() - interval '10 years')`, staleWorker, userID, staleWorker[:])

	var iid int64
	seedRun := func(worker uuid.UUID, attempts int, pastWall bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		iid++
		startedAt := "now()"
		if pastWall {
			startedAt = "now() - interval '10 years'"
		}
		mustExec(ctx, t, pool,
			fmt.Sprintf(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, completion_attempts, started_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5, $6, %s)`, startedAt),
			id, userID, repoID, iid, worker, attempts)
		return id
	}

	// A: LIVE worker + post-attempt + past wall → the carve-out protects it.
	protectedID := seedRun(liveWorker, 1, true)
	// C: LIVE worker + PRE-attempt + past wall → attempts=0 is not carved → failed.
	preAttemptID := seedRun(liveWorker, 0, true)
	// D: STALE worker + post-attempt + past wall → carve-out needs a LIVE worker → failed by the sweep.
	staleePastWallID := seedRun(staleWorker, 1, true)
	// B: STALE worker + post-attempt + within budget → survives the sweep (within wall), then requeued.
	staleRequeueID := seedRun(staleWorker, 1, false)

	// The staleness bound: the SAME cutoff SweepRunningTimeout's carve-out and the requeue path
	// both key on. liveWorker's heartbeat (now) is >= cutoff (live); staleWorker's (10y ago) is <.
	cutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-5 * time.Minute), Valid: true}

	swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
		FailureReason:        pgtype.Text{String: "run exceeded RUN_TIMEOUT", Valid: true},
		Now:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		GlobalTimeoutSeconds: 3600,
		WorkerStaleCutoff:    cutoff,
	})
	if err != nil {
		t.Fatalf("SweepRunningTimeout: %v", err)
	}
	sweptIDs := map[uuid.UUID]bool{}
	for _, s := range swept {
		sweptIDs[s.ID] = true
	}

	readStatus := func(id uuid.UUID) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return status
	}

	// A: the carve-out excludes it. This is the assertion that reddens if the carve-out WHERE
	// clause is removed (the run would then be swept as a plain past-wall running run).
	if sweptIDs[protectedID] {
		t.Fatalf("a LIVE-worker post-attempt run past its wall was swept by SweepRunningTimeout; the M3 "+
			"carve-out (completion_attempts > 0 AND a live worker heartbeat) must exclude it so its live "+
			"lead can work the completion protocol / enter the hold: %+v", swept)
	}
	if got := readStatus(protectedID); got != "running" {
		t.Fatalf("protected run status = %q after sweep, want running (untouched)", got)
	}

	// C: pre-attempt control MUST be swept — same live worker + past wall, only completion_attempts
	// differs, so this proves the carve-out is gated on attempts > 0, not on the worker being live.
	if !sweptIDs[preAttemptID] {
		t.Fatalf("the PRE-attempt (completion_attempts=0) run was NOT swept; a run past its wall with no "+
			"completion attempt is failed unchanged — the contrast is what makes the carve-out non-vacuous: %+v", swept)
	}
	if got := readStatus(preAttemptID); got != "failed" {
		t.Fatalf("pre-attempt run status = %q after sweep, want failed", got)
	}

	// D: stale-worker post-attempt past-wall MUST be swept — proves the carve-out requires a LIVE
	// worker (a dead-worker post-attempt run is never held indefinitely by the exception).
	if !sweptIDs[staleePastWallID] {
		t.Fatalf("a STALE-worker post-attempt run past its wall was NOT swept; the carve-out is narrow "+
			"(live worker only), so a dead-worker run must still be failed by the wall sweep: %+v", swept)
	}

	// B: stale-worker post-attempt WITHIN budget survives the sweep (not past wall) and then
	// follows the existing requeue path.
	if sweptIDs[staleRequeueID] {
		t.Fatalf("a within-budget stale-worker run was swept by the wall sweep; it should survive to the requeue path")
	}
	requeued, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 3,
		Cutoff:      cutoff,
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	requeuedIDs := map[uuid.UUID]bool{}
	for _, r := range requeued {
		requeuedIDs[r.ID] = true
	}
	if !requeuedIDs[staleRequeueID] {
		t.Fatalf("a stale-worker post-attempt run within budget was NOT requeued; the M3 carve-out lives "+
			"only in SweepRunningTimeout and must never block the stale-worker requeue path: %+v", requeued)
	}
	if got := readStatus(staleRequeueID); got != "queued" {
		t.Fatalf("stale-worker post-attempt run status = %q after requeue, want queued", got)
	}
	// The LIVE protected run must NOT be requeued (its worker is live) — it simply stays running.
	if requeuedIDs[protectedID] {
		t.Fatalf("the live-worker protected run was requeued; a live worker's run is never a requeue target")
	}
	if got := readStatus(protectedID); got != "running" {
		t.Fatalf("protected run status = %q after requeue pass, want running (still untouched)", got)
	}
}

// TestStampCompletionBudgetExhaustedLiveDB pins PRD #1226 M4 (D3) against a REAL Postgres:
// StampCompletionBudgetExhausted is the exact COMPLEMENT of the SweepRunningTimeout carve-out
// tested above. It arms the ONE-SHOT served `budget_exhausted` flag (completion_budget_exhausted_at)
// on EXACTLY the post-attempt live-worker interlocked rows past their wall that the sweep
// DELIBERATELY spared, so their live lead is steered into the completion hold instead of running
// forever. runToDTO surfaces the stamped column as RunDTO.CompletionBudgetExhausted (== .Valid;
// its presence on the wire is pinned by the apitypes contract test), and SetRunCompletionHold
// clears it so a stale ACK cannot re-arm the steer. This proves the complement:
//
//   - live worker + post-attempt + interlocked + past wall → STAMPED (the SAME set the carve-out spared).
//   - PRE-attempt (completion_attempts=0)                  → NOT stamped (the > 0 mutation reddens this).
//   - within budget (not past wall)                        → NOT stamped.
//   - STALE worker                                         → NOT stamped (the live-heartbeat subquery excludes it).
//   - LEGACY (completion_contract_version NULL)            → NOT stamped (a legacy run never interlocks).
//
// and that SetRunCompletionHold CLEARS the stamped flag (a stale ack can't re-arm it).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the sibling above.
func TestStampCompletionBudgetExhaustedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("stamp-exhausted-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// One LIVE worker (fresh heartbeat) and one STALE worker (heartbeat well before the cutoff) —
	// the SAME live/stale split the carve-out keys the exclusion on.
	liveWorker, staleWorker := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'live', $3, 'online', now())`, liveWorker, userID, liveWorker[:])
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'stale', $3, 'offline', now() - interval '10 years')`, staleWorker, userID, staleWorker[:])

	var iid int64
	// seedRun inserts a 'running' issue run. interlocked ⇒ completion_contract_version=1 (a legacy
	// run leaves it NULL). completion_attempts and started_at (past wall vs within budget) mirror
	// the carve-out fixture.
	seedRun := func(worker uuid.UUID, attempts int, pastWall, interlocked bool) uuid.UUID {
		t.Helper()
		id := uuid.New()
		iid++
		startedAt := "now()"
		if pastWall {
			startedAt = "now() - interval '10 years'"
		}
		var contractVersion any
		if interlocked {
			contractVersion = 1
		}
		mustExec(ctx, t, pool,
			fmt.Sprintf(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, completion_attempts, completion_contract_version, contract_revision, started_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5, $6, $7, 1, %s)`, startedAt),
			id, userID, repoID, iid, worker, attempts, contractVersion)
		return id
	}

	// A: live worker + post-attempt + interlocked + past wall → STAMPED (the set the carve-out spared).
	stampedID := seedRun(liveWorker, 1, true, true)
	// B: PRE-attempt (attempts=0) → NOT stamped. The mutation check on `completion_attempts > 0`
	// reddens THIS assertion (dropping the clause would stamp a pre-attempt run).
	preAttemptID := seedRun(liveWorker, 0, true, true)
	// C: within budget (not past wall) → NOT stamped (its wall has not elapsed).
	withinBudgetID := seedRun(liveWorker, 1, false, true)
	// D: STALE worker + post-attempt + past wall → NOT stamped (the live-heartbeat subquery excludes it).
	staleID := seedRun(staleWorker, 1, true, true)
	// E: LEGACY (completion_contract_version NULL) → NOT stamped (a legacy run never interlocks).
	legacyID := seedRun(liveWorker, 1, true, false)

	// The staleness bound: the SAME cutoff the carve-out and the stamp both key on.
	cutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-5 * time.Minute), Valid: true}
	stampArgs := store.StampCompletionBudgetExhaustedParams{
		Now:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		GlobalTimeoutSeconds: 3600,
		WorkerStaleCutoff:    cutoff,
	}

	stamped, err := q.StampCompletionBudgetExhausted(ctx, stampArgs)
	if err != nil {
		t.Fatalf("StampCompletionBudgetExhausted: %v", err)
	}
	// This call must stamp at least the one qualifying run we seeded. The COMPLEMENT is proved by
	// the per-id membership checks below (isStamped), not an absolute count — the sibling carve-out
	// test uses the same membership discipline so a row another test left in the shared DB never
	// makes this flaky, and the mutation on `completion_attempts > 0` is caught by the pre-attempt
	// membership assertion rather than the count.
	if stamped < 1 {
		t.Fatalf("StampCompletionBudgetExhausted stamped %d rows, want at least 1 (the live-worker "+
			"post-attempt interlocked run past its wall)", stamped)
	}

	isStamped := func(id uuid.UUID) bool {
		t.Helper()
		var set bool
		if err := pool.QueryRow(ctx, `SELECT completion_budget_exhausted_at IS NOT NULL FROM runs WHERE id = $1`, id).Scan(&set); err != nil {
			t.Fatalf("read completion_budget_exhausted_at for %s: %v", id, err)
		}
		return set
	}

	// A: the ONLY stamped run — the exact complement of the carve-out's spared set.
	if !isStamped(stampedID) {
		t.Fatal("a LIVE-worker post-attempt interlocked run past its wall was NOT stamped; the D3 served " +
			"steer must arm exactly the row SweepRunningTimeout's carve-out spared")
	}
	// B/C/D/E: none of the negative controls may be stamped.
	if isStamped(preAttemptID) {
		t.Fatal("a PRE-attempt (completion_attempts=0) run was stamped; the > 0 clause must exclude it " +
			"(this is the assertion the completion_attempts > 0 mutation check reddens)")
	}
	if isStamped(withinBudgetID) {
		t.Fatal("a within-budget run (not past wall) was stamped; the deadline math must exclude it")
	}
	if isStamped(staleID) {
		t.Fatal("a STALE-worker run was stamped; the live-heartbeat subquery must exclude it")
	}
	if isStamped(legacyID) {
		t.Fatal("a LEGACY (completion_contract_version NULL) run was stamped; a legacy run never interlocks")
	}

	// One-shot: a second stamp pass finds completion_budget_exhausted_at already set on A and stamps
	// nothing new (the `completion_budget_exhausted_at IS NULL` guard), so a stale ACK cannot re-arm it.
	again, err := q.StampCompletionBudgetExhausted(ctx, stampArgs)
	if err != nil {
		t.Fatalf("StampCompletionBudgetExhausted (second pass): %v", err)
	}
	if again != 0 {
		t.Fatalf("a second stamp pass stamped %d rows, want 0 (one-shot: completion_budget_exhausted_at IS NULL)", again)
	}

	// SetRunCompletionHold CLEARS the stamped flag — the D3 "the worker acting on the served steer
	// clears it so a stale ack cannot re-arm" contract. The held run is owned and interlocked with a
	// recorded attempt (attempts=1), so the hold's guard admits it.
	held, err := q.SetRunCompletionHold(ctx, store.SetRunCompletionHoldParams{
		ID:               stampedID,
		WorkerID:         pgtype.UUID{Bytes: liveWorker, Valid: true},
		HoldCapturedHead: pgtype.Text{String: "deadbeef", Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunCompletionHold: %v", err)
	}
	if held.Status != "paused" {
		t.Fatalf("held run status = %q, want paused", held.Status)
	}
	if held.CompletionBudgetExhaustedAt.Valid {
		t.Fatal("SetRunCompletionHold must CLEAR completion_budget_exhausted_at (set it back to NULL); " +
			"the returned row still carries the flag")
	}
	if isStamped(stampedID) {
		t.Fatal("completion_budget_exhausted_at is still set in the DB after the hold; the worker acting " +
			"on the served steer must clear it so a stale ACK cannot re-arm the steer")
	}
}

// TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB pins the M4/D3 served steer against the
// M5/D7 hold-clear: a NORMAL `running` report (SetRunRunning) MUST NOT clear the served
// budget_exhausted flag (completion_budget_exhausted_at). SetState's running arm calls SetRunRunning
// and THEN re-reads the row for the worker ACK (runToDTO -> RunDTO.CompletionBudgetExhausted ==
// .Valid), which the worker reads as served.budgetExhausted and routes into the completion hold. If
// SetRunRunning cleared the flag, the very report meant to CARRY the steer would DISARM it — the ACK
// would always report budgetExhausted=false and the worker could never enter the hold on the
// server's steer.
//
// The flag is ARMED here via the REAL StampCompletionBudgetExhausted (not a manual UPDATE), so this
// exercises the genuine arm-then-heartbeat path. The flag's ONLY legitimate clear is
// SetRunCompletionHold (the worker actually entering the hold), pinned by
// TestStampCompletionBudgetExhaustedLiveDB above; hold_reason/hold_captured_head DO clear on
// SetRunRunning (the D7 resume->running clear), pinned by TestCompletionDecisionPausedResumesLiveDB.
//
// This is the assertion that REDDENS on the bug (an unconditional `completion_budget_exhausted_at =
// NULL` in SetRunRunning).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the siblings above.
func TestSetRunRunningPreservesCompletionBudgetExhaustedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("running-preserves-exhausted-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A LIVE worker (fresh heartbeat) — the stamp's live-heartbeat subquery admits it.
	liveWorker := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'live', $3, 'online', now())`, liveWorker, userID, liveWorker[:])

	// An interlocked (completion_contract_version=1) running run with a recorded attempt, past its
	// wall — exactly the row StampCompletionBudgetExhausted arms.
	runID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id,
		    completion_attempts, completion_contract_version, contract_revision, started_at)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'issue', 'running', $4, 1, 1, 1, now() - interval '10 years')`,
		runID, userID, repoID, liveWorker)

	cutoff := pgtype.Timestamptz{Time: time.Now().UTC().Add(-5 * time.Minute), Valid: true}
	stamped, err := q.StampCompletionBudgetExhausted(ctx, store.StampCompletionBudgetExhaustedParams{
		Now:                  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		GlobalTimeoutSeconds: 3600,
		WorkerStaleCutoff:    cutoff,
	})
	if err != nil {
		t.Fatalf("StampCompletionBudgetExhausted: %v", err)
	}
	if stamped < 1 {
		t.Fatalf("StampCompletionBudgetExhausted stamped %d rows, want at least 1 (the seeded run)", stamped)
	}

	isStamped := func() bool {
		t.Helper()
		var set bool
		if err := pool.QueryRow(ctx, `SELECT completion_budget_exhausted_at IS NOT NULL FROM runs WHERE id = $1`, runID).Scan(&set); err != nil {
			t.Fatalf("read completion_budget_exhausted_at: %v", err)
		}
		return set
	}
	if !isStamped() {
		t.Fatal("precondition: the seeded run must be stamped before the running report")
	}

	// A NORMAL running report (running -> running heartbeat), the params a running report carries.
	rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
		IterationCount:           1,
		RunMaxIterations:         5,
		MilestoneBudgetCap:       10,
		RunTimeoutSeconds:        7200,
		BudgetWallCeilingSeconds: 86400,
		ID:                       runID,
		WorkerID:                 pgtype.UUID{Bytes: liveWorker, Valid: true},
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunRunning affected %d rows, want 1", rows)
	}

	// THE ASSERTION that reddens on the bug: the served steer must SURVIVE the running report.
	if !isStamped() {
		t.Fatal("SetRunRunning CLEARED completion_budget_exhausted_at; the served budget_exhausted steer " +
			"MUST survive the running report so the worker can read it off the ACK (RunDTO.CompletionBudgetExhausted) " +
			"and route into the completion hold. Its only clear is SetRunCompletionHold on hold entry, not this report.")
	}
}
