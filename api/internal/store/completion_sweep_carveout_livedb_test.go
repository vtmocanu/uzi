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
