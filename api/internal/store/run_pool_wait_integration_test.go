package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for PRD #754 M4's SetRunPoolWait, the non-locking hold an `auto`
// claim transitions to when the token pool is genuinely empty, plus the two schema
// changes 00165 rides: 'pool_wait' in runs_status_check, and its EXCLUSION from
// uq_runs_one_active_per_issue (the non-locking property).
//
// 🔴 EXECUTION IS THE POINT, NOT JUST THE ASSERTIONS. A green `sqlc generate` is not
// evidence a query runs (sqlc's type deduction is not Postgres's), and — the defect no
// other test in the repo catches — a status CHECK that does not spell 'pool_wait' the
// way SetRunPoolWait does migrates cleanly, passes `sqlc generate`, and passes every Go
// package, yet raises 23514 the first time this UPDATE fires. This is the entire
// permanent guard for the widened domain, which matters most at the landing rebase when
// the migration is renumbered and retyped.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunPoolWaitQueriesLiveDB(t *testing.T) {
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

	userID, connID, repoID, secretID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("poolwait-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	// A user_secret to hang limit_dead_secret_id off: the column is a composite FK to
	// user_secrets(user_id, id), and the hold must PRESERVE it (M3's exclude-relax reads
	// it on resume). A NULL here would make the preservation assertion vacuous.
	mustExec(ctx, t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext) VALUES ($1, $2, 'anthropic_token', 'poolwait-key', $3)`,
		secretID, userID, []byte{0x2})

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("poolwait-"), userID[:]...),
		AnthropicBindMode: "auto",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	var iid int64
	// A claimed run carrying a worker, a started_at, a stale health flag, a spent
	// limit_wait budget and a dead-credential exclusion — every field the hold must
	// touch or preserve is set to a value distinct from the hold's target so an
	// assertion cannot pass vacuously.
	newClaimed := func(t *testing.T) (uuid.UUID, int64) {
		t.Helper()
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id,
			                   started_at, health, health_reason, health_since,
			                   limit_wait_count, limit_dead_secret_id)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'claimed', $5, now(), 'stalled', 'no output for 20m', now(), 3, $6)`,
			id, userID, repoID, iid, wkr.ID, secretID)
		return id, iid
	}

	type runRow struct {
		status            string
		startedAt         pgtype.Timestamptz
		workerID          pgtype.UUID
		limitWaitCount    int32
		limitDeadSecretID pgtype.UUID
		health            string
		healthReason      pgtype.Text
		healthSince       pgtype.Timestamptz
	}
	read := func(t *testing.T, id uuid.UUID) runRow {
		t.Helper()
		var r runRow
		err := pool.QueryRow(ctx, `SELECT status, started_at, worker_id, limit_wait_count,
		       limit_dead_secret_id, health, health_reason, health_since
		  FROM runs WHERE id = $1`, id).Scan(&r.status, &r.startedAt, &r.workerID,
			&r.limitWaitCount, &r.limitDeadSecretID, &r.health, &r.healthReason, &r.healthSince)
		if err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return r
	}
	hold := func(t *testing.T, id uuid.UUID, w pgtype.UUID) int64 {
		t.Helper()
		rows, err := q.SetRunPoolWait(ctx, store.SetRunPoolWaitParams{ID: id, WorkerID: w})
		if err != nil {
			// 23514 here means 00165's status CHECK does not spell 'pool_wait' as this
			// query does (check the renumbered migration); 42P08 means sqlc accepted a
			// statement Postgres will not prepare. Nothing else in the repo catches either.
			t.Fatalf("SetRunPoolWait(%s): %v", id, err)
		}
		return rows
	}

	// ── the happy path: claimed → pool_wait, and every column it must write/preserve ──
	t.Run("claimed holds and writes exactly the right columns", func(t *testing.T) {
		id, _ := newClaimed(t)
		if rows := hold(t, id, workerID); rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		r := read(t, id)
		if r.status != "pool_wait" {
			t.Fatalf("status = %q, want pool_wait", r.status)
		}
		if r.startedAt.Valid {
			t.Fatalf("started_at = %v, want NULL — a resumed hold must get a FRESH RUN_TIMEOUT "+
				"wall, or SweepRunningTimeout fails it on its first tick back", r.startedAt)
		}
		if r.workerID != workerID {
			t.Fatalf("worker_id = %v, want it kept for resume affinity", r.workerID)
		}
		if r.limitWaitCount != 3 {
			t.Fatalf("limit_wait_count = %d, want 3 UNCHANGED — a pool_wait hold is NOT a usage "+
				"park (Decision 9) and must not spend the usage-limit wait budget", r.limitWaitCount)
		}
		if !r.limitDeadSecretID.Valid || uuid.UUID(r.limitDeadSecretID.Bytes) != secretID {
			t.Fatalf("limit_dead_secret_id = %v, want %v PRESERVED — M3's exclude-relax reads it "+
				"on resume, so the hold must not clear it", r.limitDeadSecretID, secretID)
		}
		if r.health != "ok" || r.healthReason.Valid || r.healthSince.Valid {
			t.Fatalf("health = %q/%v/%v, want ok/NULL/NULL — the fixture was held while flagged "+
				"`stalled` and nothing revisits a held run to clear it",
				r.health, r.healthReason, r.healthSince)
		}
	})

	// ── the POSITIVE source guard: only a claimed run holds ───────────────────────────
	t.Run("only claimed holds", func(t *testing.T) {
		for _, status := range []string{"queued", "running", "awaiting_approval", "limit_wait", "completed", "failed", "cancelled"} {
			iid++
			id := uuid.New()
			mustExec(ctx, t, pool,
				`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
				 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`, id, userID, repoID, iid, status, wkr.ID)
			if rows := hold(t, id, workerID); rows != 0 {
				t.Fatalf("%s → pool_wait: rows = %d, want 0. The guard is POSITIVE "+
					"(status = 'claimed'); a held run only ever comes from a just-claimed one", status, rows)
			}
			if got := read(t, id).status; got != status {
				t.Fatalf("%s → pool_wait: status became %q", status, got)
			}
		}
	})

	t.Run("a foreign worker cannot hold someone else's run", func(t *testing.T) {
		id, _ := newClaimed(t)
		other := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		if rows := hold(t, id, other); rows != 0 {
			t.Fatalf("foreign worker: rows = %d, want 0", rows)
		}
		if got := read(t, id).status; got != "claimed" {
			t.Fatalf("status became %q under a foreign worker", got)
		}
	})

	t.Run("a judge run never holds", func(t *testing.T) {
		target := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, 8001, 't', 'd', 'completed')`, target, userID, repoID)
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, 'judge', $3, 'review', 'd', 'claimed', $4)`, id, userID, target, wkr.ID)
		if rows := hold(t, id, workerID); rows != 0 {
			t.Fatalf("judge hold: rows = %d, want 0 (Decision 14, kind <> 'judge')", rows)
		}
	})

	// ── the non-locking property: a held run does not lock its issue ──────────────────
	t.Run("a held run does not lock the issue (index excludes pool_wait)", func(t *testing.T) {
		id, sharedIID := newClaimed(t)
		if rows := hold(t, id, workerID); rows != 1 {
			t.Fatalf("hold: rows = %d, want 1", rows)
		}
		// A SECOND issue run with the SAME (repo_id, issue_iid). Under the old index this
		// would 23505; with pool_wait excluded it must insert cleanly — the whole point of
		// Decision 8's non-locking hold.
		second := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, second, userID, repoID, sharedIID)
		if err != nil {
			t.Fatalf("a second run for an issue whose only other run is HELD in pool_wait was "+
				"refused (%v); uq_runs_one_active_per_issue must EXCLUDE pool_wait (non-locking)", err)
		}
		// And the inverse still holds: two NON-held active runs for one issue collide, so the
		// index is still doing its job for everything except a held run.
		third := uuid.New()
		_, err = pool.Exec(ctx,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, third, userID, repoID, sharedIID)
		if err == nil {
			t.Fatal("two non-terminal, non-held runs for one issue both inserted; the index is " +
				"no longer enforcing single-active-per-issue for ordinary runs")
		}
	})

	// ── a held run MUST be cancellable (the PRD requires a clear cancel path) ─────────
	t.Run("a held run cancels server-side", func(t *testing.T) {
		id, _ := newClaimed(t)
		if rows := hold(t, id, workerID); rows != 1 {
			t.Fatalf("hold: rows = %d, want 1", rows)
		}
		rows, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: id, UserID: userID})
		if err != nil {
			t.Fatalf("CancelRunServerSide on a pool_wait run: %v", err)
		}
		if rows != 1 {
			t.Fatalf("cancel rows = %d, want 1 — a held run MUST be cancellable; the cancel "+
				"query's negative guard admits pool_wait for free", rows)
		}
		if got := read(t, id).status; got != "cancelled" {
			t.Fatalf("status = %q after cancel, want cancelled", got)
		}
	})

	// ── PRD #754 M5 GUARD PARITY: a reordered `running` report must not un-hold a run ──
	// The SetRunRunning guard gained `AND status <> 'pool_wait'` alongside limit_wait's.
	// Executing it against a held run proves the guard holds (0-row no-op, status
	// unchanged) — the changed query is exercised, not merely regenerated.
	t.Run("SetRunRunning is a no-op on a held run", func(t *testing.T) {
		id, _ := newClaimed(t)
		if rows := hold(t, id, workerID); rows != 1 {
			t.Fatalf("hold: rows = %d, want 1", rows)
		}
		rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: id, WorkerID: workerID})
		if err != nil {
			t.Fatalf("SetRunRunning on a pool_wait run: %v", err)
		}
		if rows != 0 {
			t.Fatalf("SetRunRunning rows = %d, want 0 — a reordered pre-hold `running` report "+
				"must not un-hold a pool_wait run (guard parity with limit_wait)", rows)
		}
		if got := read(t, id).status; got != "pool_wait" {
			t.Fatalf("status = %q after a `running` report, want pool_wait UNCHANGED", got)
		}
	})
}

// Live-DB coverage for PRD #754 M5's two new queries — PromotePoolWaitRun (the
// owner-scoped resume used by both the reactive sweeper pass and `uzi run resume-now`)
// and ListPoolWaitRuns (the oldest-first reactive worklist). EXECUTION is the point: a
// green `sqlc generate` is not evidence either statement runs against Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunPoolWaitResumeQueriesLiveDB(t *testing.T) {
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

	userID, otherID, connID, repoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("poolresume-%s@e2e", userID))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		otherID, fmt.Sprintf("poolresume-other-%s@e2e", otherID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	var iid int64
	// A held run for userID whose started_at/health are set to distinct values, so the
	// promote's resets (started_at → NULL, health → ok) are not vacuously satisfied. The
	// interval on status_since lets the ordering test control who is oldest.
	newHeld := func(t *testing.T, owner uuid.UUID, sinceInterval string) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status,
			                   status_since, started_at, health, health_reason, health_since)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'pool_wait', now() - `+sinceInterval+`, now(), 'stalled', 'no output', now())`,
			id, owner, repoID, iid)
		return id
	}
	readStatus := func(t *testing.T, id uuid.UUID) (status string, startedAt pgtype.Timestamptz, health string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT status, started_at, health FROM runs WHERE id = $1`, id).
			Scan(&status, &startedAt, &health); err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return
	}

	// ── PromotePoolWaitRun: pool_wait → queued, with started_at nulled + health reset ──
	t.Run("promote resumes a held run and resets the wall + health", func(t *testing.T) {
		id := newHeld(t, userID, "interval '1 minute'")
		rows, err := q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: id, UserID: userID})
		if err != nil {
			t.Fatalf("PromotePoolWaitRun: %v", err)
		}
		if rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		status, startedAt, health := readStatus(t, id)
		if status != "queued" {
			t.Fatalf("status = %q, want queued", status)
		}
		if startedAt.Valid {
			t.Fatalf("started_at = %v, want NULL — the resume must get a FRESH RUN_TIMEOUT wall", startedAt)
		}
		if health != "ok" {
			t.Fatalf("health = %q, want ok — the detector re-evaluates from the resume, so the "+
				"held flag must be cleared", health)
		}
	})

	// ── owner-scoping: a foreign user_id is a 0-row no-op (resume-now cannot cross users) ──
	t.Run("promote is owner-scoped", func(t *testing.T) {
		id := newHeld(t, userID, "interval '1 minute'")
		rows, err := q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: id, UserID: otherID})
		if err != nil {
			t.Fatalf("PromotePoolWaitRun (foreign user): %v", err)
		}
		if rows != 0 {
			t.Fatalf("foreign user_id: rows = %d, want 0 — resume-now must not promote a foreign run", rows)
		}
		if status, _, _ := readStatus(t, id); status != "pool_wait" {
			t.Fatalf("status = %q after a foreign promote, want pool_wait UNCHANGED", status)
		}
	})

	// ── the POSITIVE source guard: only a pool_wait run promotes ──────────────────────
	t.Run("promote is a no-op on a non-held run", func(t *testing.T) {
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, id, userID, repoID, iid)
		rows, err := q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: id, UserID: userID})
		if err != nil {
			t.Fatalf("PromotePoolWaitRun (non-held): %v", err)
		}
		if rows != 0 {
			t.Fatalf("non-held run: rows = %d, want 0 — the source guard is POSITIVE (status = 'pool_wait')", rows)
		}
	})

	// ── ListPoolWaitRuns: returns only pool_wait runs, oldest first ───────────────────
	t.Run("list returns only held runs, oldest first", func(t *testing.T) {
		// Isolate: settle every existing pool_wait run so this assertion sees only its own.
		mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed' WHERE status = 'pool_wait'`)
		older := newHeld(t, userID, "interval '10 minutes'")
		newer := newHeld(t, userID, "interval '1 minute'")
		// A non-held run that must NOT appear.
		iid++
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running')`, uuid.New(), userID, repoID, iid)

		got, err := q.ListPoolWaitRuns(ctx)
		if err != nil {
			t.Fatalf("ListPoolWaitRuns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListPoolWaitRuns returned %d rows, want 2 (only the held runs)", len(got))
		}
		if got[0].ID != older || got[1].ID != newer {
			t.Fatalf("order = [%s, %s], want oldest-first [%s, %s] — the reactive pass promotes "+
				"the longest-waiting run of each owner first", got[0].ID, got[1].ID, older, newer)
		}
	})
}
