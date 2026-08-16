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

// Live-DB coverage for PRD #35 M2's two new queries and GetRunClaimContext's new
// projection.
//
// 🔴 EXECUTION IS THE POINT, NOT THE ASSERTIONS. A green `sqlc generate` is not
// evidence a query runs: sqlc's type deduction is not Postgres's, and a statement it
// accepts can be rejected the first time it is PREPARED (42P08, measured on this repo
// in PRD #113 M4 for a CASE arm that reused a parameter alongside a bare NULL).
// SetRunLimitWait has three nullable params, a mixed narg/named parameter list and a
// two-clause source guard; PromoteLimitWaitRuns compares a nullable column against a
// parameter. Neither is verified by anything short of a live server, which is why
// these live here rather than against the fake store.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunLimitWaitQueriesLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("limitwait-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A unique token_hash: the store IT runner shares one DB across every LiveDB test
	// and workers.token_hash is UNIQUE, so a fixed literal collides.
	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("limitwait-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	var iid int64
	newRun := func(t *testing.T, status string) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id,
			                   started_at, health, health_reason, health_since)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, now(), 'stalled', 'no output for 20m', now())`,
			id, userID, repoID, iid, status, wkr.ID)
		return id
	}

	type runRow struct {
		status         string
		limitResetsAt  pgtype.Timestamptz
		retryNotBefore pgtype.Timestamptz
		rateLimitType  pgtype.Text
		limitWaitCount int32
		requeueCount   int32
		sessionID      pgtype.Text
		startedAt      pgtype.Timestamptz
		workerID       pgtype.UUID
		lastSeq        int64
		health         string
		healthReason   pgtype.Text
		healthSince    pgtype.Timestamptz
	}
	read := func(t *testing.T, id uuid.UUID) runRow {
		t.Helper()
		var r runRow
		err := pool.QueryRow(ctx, `SELECT status, limit_resets_at, retry_not_before, rate_limit_type,
		       limit_wait_count, requeue_count, session_id, started_at, worker_id, last_seq,
		       health, health_reason, health_since
		  FROM runs WHERE id = $1`, id).Scan(&r.status, &r.limitResetsAt, &r.retryNotBefore,
			&r.rateLimitType, &r.limitWaitCount, &r.requeueCount, &r.sessionID, &r.startedAt,
			&r.workerID, &r.lastSeq, &r.health, &r.healthReason, &r.healthSince)
		if err != nil {
			t.Fatalf("read run %s: %v", id, err)
		}
		return r
	}

	resetsAt := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Second)
	retryAt := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	park := func(t *testing.T, id uuid.UUID, w pgtype.UUID) int64 {
		t.Helper()
		rows, err := q.SetRunLimitWait(ctx, store.SetRunLimitWaitParams{
			ID: id, WorkerID: w,
			LimitResetsAt:  pgtype.Timestamptz{Time: resetsAt, Valid: true},
			RateLimitType:  pgtype.Text{String: "five_hour", Valid: true},
			RetryNotBefore: pgtype.Timestamptz{Time: retryAt, Valid: true},
			SessionID:      pgtype.Text{String: "sess-abc", Valid: true},
		})
		if err != nil {
			// 🔴 TWO DISTINCT DEFECTS LAND HERE AND NOTHING ELSE IN THE REPO CATCHES
			// EITHER, so the message names both rather than leaving the diagnosis to
			// whoever is looking at a red build.
			//
			//  * SQLSTATE 42P08 — sqlc's type deduction is not Postgres's, so a query
			//    sqlc accepts can be rejected the first time it is PREPARED.
			//
			//  * SQLSTATE 23514 — the status CHECK in 00091 does not spell 'limit_wait'
			//    the way this statement does. Measured: a migration widening the CHECK
			//    to 'limit-wait' instead migrates cleanly, passes `sqlc generate` (which
			//    reads the schema but not CHECK VALUES), and passes all 43 api packages.
			//    NO TEST ANYWHERE ELSE INSERTS OR UPDATES A RUN TO limit_wait, so this
			//    assertion is the entire permanent guard for the widened domain — which
			//    matters most at the landing rebase, when the migration is renumbered
			//    and retyped.
			t.Fatalf("SetRunLimitWait(%s): %v\n"+
				"  23514 here means 00091's status CHECK does not spell 'limit_wait' as this "+
				"query does (check the renumbered migration); 42P08 means sqlc accepted a "+
				"statement Postgres will not prepare.", id, err)
		}
		return rows
	}

	// ── the happy path, and every column it is supposed to write ──────────────────
	t.Run("running parks and writes every column", func(t *testing.T) {
		id := newRun(t, "running")
		mustExec(ctx, t, pool, `UPDATE runs SET last_seq = 42, requeue_count = 2 WHERE id = $1`, id)
		if rows := park(t, id, workerID); rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		r := read(t, id)
		if r.status != "limit_wait" {
			t.Fatalf("status = %q, want limit_wait", r.status)
		}
		if !r.limitResetsAt.Valid || !r.limitResetsAt.Time.Equal(resetsAt) {
			t.Fatalf("limit_resets_at = %v, want %v", r.limitResetsAt, resetsAt)
		}
		if !r.retryNotBefore.Valid || !r.retryNotBefore.Time.Equal(retryAt) {
			t.Fatalf("retry_not_before = %v, want %v — the gate is the SERVER's computed "+
				"stamp, never the worker's reported reset", r.retryNotBefore, retryAt)
		}
		if r.rateLimitType.String != "five_hour" {
			t.Fatalf("rate_limit_type = %q, want five_hour", r.rateLimitType.String)
		}
		if r.limitWaitCount != 1 {
			t.Fatalf("limit_wait_count = %d, want 1 — the budget must be spent in the SAME "+
				"statement as the transition, or a run can be parked without paying for it",
				r.limitWaitCount)
		}
		if r.requeueCount != 2 {
			t.Fatalf("requeue_count = %d, want 2 unchanged — it counts WORKER DEATHS "+
				"(Decision 5) and a park is not one", r.requeueCount)
		}
		if r.sessionID.String != "sess-abc" {
			t.Fatalf("session_id = %q, want sess-abc — the session is what makes the resume a "+
				"resume rather than a restart", r.sessionID.String)
		}
		if r.workerID != workerID {
			t.Fatalf("worker_id = %v, want it kept for affinity", r.workerID)
		}
		if r.lastSeq != 42 {
			t.Fatalf("last_seq = %d, want 42 — the message high-water mark must survive a park", r.lastSeq)
		}
		// The health exit contract, which is mandatory here rather than stylistic: the
		// PRD #47 detector's allowlist is POSITIVE, so it never revisits a parked run and
		// a flag left standing would freeze for the whole park.
		if r.health != "ok" || r.healthReason.Valid || r.healthSince.Valid {
			t.Fatalf("health = %q/%v/%v, want ok/NULL/NULL — the fixture was parked while "+
				"flagged `stalled`, and nothing will ever revisit the row to clear it",
				r.health, r.healthReason, r.healthSince)
		}
	})

	// ── the POSITIVE source guard, one sub-case per status it must refuse ─────────
	t.Run("only running parks", func(t *testing.T) {
		for _, status := range []string{"queued", "claimed", "awaiting_approval", "completed", "failed", "cancelled"} {
			id := newRun(t, status)
			if rows := park(t, id, workerID); rows != 0 {
				t.Fatalf("%s → limit_wait: rows = %d, want 0. The guard is POSITIVE "+
					"(status = 'running') precisely so this is a no-op; a negative guard would "+
					"admit it", status, rows)
			}
			if got := read(t, id).status; got != status {
				t.Fatalf("%s → limit_wait: status became %q", status, got)
			}
		}
	})

	t.Run("re-delivery is idempotent and does not double-bump the budget", func(t *testing.T) {
		id := newRun(t, "running")
		if rows := park(t, id, workerID); rows != 1 {
			t.Fatalf("first park: rows = %d, want 1", rows)
		}
		if rows := park(t, id, workerID); rows != 0 {
			t.Fatalf("re-delivered park: rows = %d, want 0 — limit_wait is not `running`, so "+
				"the source guard makes the retry a no-op", rows)
		}
		if c := read(t, id).limitWaitCount; c != 1 {
			t.Fatalf("limit_wait_count = %d after a re-delivery, want 1: a retried report "+
				"must not burn a second RUN_LIMIT_MAX_WAITS slot on one event", c)
		}
	})

	t.Run("a foreign worker cannot park someone else's run", func(t *testing.T) {
		id := newRun(t, "running")
		other := pgtype.UUID{Bytes: uuid.New(), Valid: true}
		if rows := park(t, id, other); rows != 0 {
			t.Fatalf("foreign worker: rows = %d, want 0", rows)
		}
	})

	// ── Decision 14: judge never parks, and the guard is in the SQL ───────────────
	t.Run("a judge run never parks", func(t *testing.T) {
		target := newRun(t, "completed")
		id := uuid.New()
		// runs_kind_shape: a judge run carries no repo and no issue, and must name its
		// target. issue_title/issue_description stay NOT NULL across every kind, so the
		// shape constraint and the column nullability disagree and both have to be fed.
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, 'judge', $3, 'review', 'd', 'running', $4)`, id, userID, target, wkr.ID)
		if rows := park(t, id, workerID); rows != 0 {
			t.Fatalf("judge park: rows = %d, want 0 (Decision 14). The clause lives in SQL "+
				"rather than only in Go because Go is what composes the judge's better death, "+
				"and a bypass there would silently park", rows)
		}
		if got := read(t, id).status; got != "running" {
			t.Fatalf("judge status became %q", got)
		}
	})

	t.Run("absent optional fields are legal", func(t *testing.T) {
		// The park path must never fail a report on a technicality: a limit death with no
		// reported reset and no reported type still parks, on the server's own stamp.
		id := newRun(t, "running")
		rows, err := q.SetRunLimitWait(ctx, store.SetRunLimitWaitParams{
			ID: id, WorkerID: workerID,
			RetryNotBefore: pgtype.Timestamptz{Time: retryAt, Valid: true},
		})
		if err != nil {
			t.Fatalf("SetRunLimitWait with NULL optionals: %v", err)
		}
		if rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		r := read(t, id)
		if r.limitResetsAt.Valid || r.rateLimitType.Valid {
			t.Fatalf("NULL optionals became %v / %v", r.limitResetsAt, r.rateLimitType)
		}
		if r.sessionID.Valid {
			t.Fatalf("session_id = %v; a NULL narg must COALESCE to the existing value, which "+
				"is NULL here, not overwrite a live session with one", r.sessionID)
		}
	})

	// ── the promotion pass ────────────────────────────────────────────────────────
	t.Run("promotion", func(t *testing.T) {
		due := newRun(t, "running")
		notDue := newRun(t, "running")
		noStamp := newRun(t, "running")
		stillRunning := newRun(t, "running")

		if rows := park(t, due, workerID); rows != 1 {
			t.Fatalf("park due: rows = %d", rows)
		}
		if rows := park(t, notDue, workerID); rows != 1 {
			t.Fatalf("park notDue: rows = %d", rows)
		}
		if rows := park(t, noStamp, workerID); rows != 1 {
			t.Fatalf("park noStamp: rows = %d", rows)
		}
		mustExec(ctx, t, pool, `UPDATE runs SET retry_not_before = now() - interval '1 minute',
		    health = 'stalled', health_reason = 'x', health_since = now() WHERE id = $1`, due)
		mustExec(ctx, t, pool, `UPDATE runs SET retry_not_before = now() + interval '1 hour' WHERE id = $1`, notDue)
		// A parked run with NO stamp must never promote. `NULL <= now()` is NULL, which
		// is not true, so the WHERE excludes it — worth pinning because "NULL fails the
		// comparison" is exactly the SQL fact a rewrite to COALESCE would break.
		mustExec(ctx, t, pool, `UPDATE runs SET retry_not_before = NULL WHERE id = $1`, noStamp)

		promoted, err := q.PromoteLimitWaitRuns(ctx, pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true})
		if err != nil {
			t.Fatalf("PromoteLimitWaitRuns: %v", err)
		}
		got := map[uuid.UUID]store.PromoteLimitWaitRunsRow{}
		for _, r := range promoted {
			got[r.ID] = r
		}
		if _, ok := got[due]; !ok {
			t.Fatalf("the due run was not promoted; promoted set = %v", got)
		}
		if _, ok := got[notDue]; ok {
			t.Fatal("a run whose retry_not_before is in the FUTURE was promoted")
		}
		if _, ok := got[noStamp]; ok {
			t.Fatal("a parked run with a NULL retry_not_before was promoted; `NULL <= now()` " +
				"is NULL, not true, and nothing may paper over that")
		}
		if _, ok := got[stillRunning]; ok {
			t.Fatal("a `running` run was promoted; the source predicate is status = 'limit_wait'")
		}
		if r := got[due]; r.UserID != userID || r.Status != "queued" {
			t.Fatalf("RETURNING = %+v, want user %s and status queued — the caller publishes "+
				"each promotion through the broadcaster/notifier from exactly these columns",
				r, userID)
		}

		r := read(t, due)
		if r.status != "queued" {
			t.Fatalf("status = %q, want queued", r.status)
		}
		if r.startedAt.Valid {
			t.Fatalf("started_at = %v, want NULL. Without the reset, SweepRunningTimeout "+
				"measures the resumed run against a started_at from before a park that may "+
				"have lasted days and fails it on its first tick back", r.startedAt)
		}
		if r.requeueCount != 0 {
			t.Fatalf("requeue_count = %d, want 0 — a promotion is not a worker death", r.requeueCount)
		}
		if r.sessionID.String != "sess-abc" || r.workerID != workerID {
			t.Fatalf("session %v / worker %v: both must survive, they are what make the "+
				"resume a resume and what gives it its own disk back", r.sessionID, r.workerID)
		}
		if r.limitWaitCount != 1 {
			t.Fatalf("limit_wait_count = %d, want 1 kept — the budget spent stays spent", r.limitWaitCount)
		}
		if !r.limitResetsAt.Valid || !r.retryNotBefore.Valid || !r.rateLimitType.Valid {
			t.Fatalf("park history was cleared (%v / %v / %v); the run view renders "+
				"\"attempt N, last paused on <window>\" from it after the resume",
				r.limitResetsAt, r.retryNotBefore, r.rateLimitType)
		}
		if r.health != "ok" || r.healthReason.Valid || r.healthSince.Valid {
			t.Fatalf("health = %q/%v/%v, want ok/NULL/NULL on the way to a fresh queued",
				r.health, r.healthReason, r.healthSince)
		}

		// Idempotent: the second pass finds nothing, because the status predicate has
		// already moved even though the stale stamp is still in the past.
		again, err := q.PromoteLimitWaitRuns(ctx, pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true})
		if err != nil {
			t.Fatalf("second PromoteLimitWaitRuns: %v", err)
		}
		for _, r := range again {
			if r.ID == due {
				t.Fatal("the same run promoted twice; a stale retry_not_before must not " +
					"re-fire once the status has moved")
			}
		}
	})
}

// TestGetRunClaimContextHumanPlanApprovedLiveDB pins the new projection against a
// real server. It is a cast EXPRESSION rather than a column, which is the shape sqlc
// infers worst (an uncast EXISTS types as interface{}), and it is correlated by
// run_id, which no unit test over a fake store can check.
//
// The stakes are why it is asserted from four directions rather than one:
// plan_approved tells the worker to SKIP the plan gate and implement plan_md. A
// projection that is true too often is a gate bypass.
func TestGetRunClaimContextHumanPlanApprovedLiveDB(t *testing.T) {
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
	runID, otherRun := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("planapproved-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	for i, id := range []uuid.UUID{runID, otherRun} {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running')`, id, userID, repoID, 900+i)
	}

	approved := func(t *testing.T) bool {
		t.Helper()
		rc, err := q.GetRunClaimContext(ctx, runID)
		if err != nil {
			t.Fatalf("GetRunClaimContext: %v", err)
		}
		return rc.HumanPlanApproved
	}

	if approved(t) {
		t.Fatal("human_plan_approved is true with no approve_plan input at all")
	}

	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'approve_plan')`, runID)
	if approved(t) {
		t.Fatal("an UNCONSUMED approve_plan made human_plan_approved true; a verdict the " +
			"worker has not acted on is not an approval it can skip the gate on")
	}

	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, consumed_at) VALUES ($1, 'approve_plan', now())`, otherRun)
	if approved(t) {
		t.Fatal("a DIFFERENT run's consumed approve_plan made human_plan_approved true; the " +
			"EXISTS is supposed to be correlated by run_id")
	}

	mustExec(ctx, t, pool,
		`INSERT INTO run_user_inputs (run_id, kind, consumed_at) VALUES ($1, 'follow_up', now())`, runID)
	if approved(t) {
		t.Fatal("a consumed input of a DIFFERENT KIND made human_plan_approved true")
	}

	mustExec(ctx, t, pool,
		`UPDATE run_user_inputs SET consumed_at = now() WHERE run_id = $1 AND kind = 'approve_plan'`, runID)
	if !approved(t) {
		t.Fatal("a consumed approve_plan on this very run did NOT make human_plan_approved " +
			"true; the resume would re-plan, re-park at the gate in front of a human who " +
			"already approved, and can fail with REASON_NO_PLAN")
	}
}

// TestSetRunWaitOnLimitLiveDB pins the per-run toggle's SQL semantics (PRD #35
// Decision 7, the surface the user chose over a start-run modal).
//
// 🔴 THE STATUS ASSERTIONS ARE THE POINT, NOT THE FLAG ONES. The toggle changes
// FUTURE limit behaviour only, so flipping it off on a PARKED run must not un-park
// or cancel it: Decision 11's cancel is that control, and silently failing someone's
// run because they changed a preference would destroy work they never asked to lose.
// A statement that touched status would pass every flag assertion here.
func TestSetRunWaitOnLimitLiveDB(t *testing.T) {
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
	for _, u := range []uuid.UUID{userID, otherID} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("wol-%s@e2e", u))
	}
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	var iid int64 = 5000
	newRun := func(t *testing.T, status string, wait bool) uuid.UUID {
		t.Helper()
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, wait_on_limit)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6)`, id, userID, repoID, iid, status, wait)
		return id
	}
	read := func(t *testing.T, id uuid.UUID) (string, bool) {
		t.Helper()
		var status string
		var wait bool
		if err := pool.QueryRow(ctx, `SELECT status, wait_on_limit FROM runs WHERE id = $1`, id).
			Scan(&status, &wait); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return status, wait
	}
	set := func(t *testing.T, id, owner uuid.UUID, enabled bool) int64 {
		t.Helper()
		rows, err := q.SetRunWaitOnLimit(ctx, store.SetRunWaitOnLimitParams{
			ID: id, UserID: owner, WaitOnLimit: enabled,
		})
		if err != nil {
			t.Fatalf("SetRunWaitOnLimit: %v", err)
		}
		return rows
	}

	t.Run("flips a non-terminal run and touches nothing else", func(t *testing.T) {
		for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "limit_wait"} {
			id := newRun(t, status, false)
			if rows := set(t, id, userID, true); rows != 1 {
				t.Fatalf("%s: rows = %d, want 1 — the guard is CancelRunServerSide's negative "+
					"predicate, which covers every non-terminal status including limit_wait", status, rows)
			}
			gotStatus, gotWait := read(t, id)
			if !gotWait {
				t.Fatalf("%s: wait_on_limit did not flip", status)
			}
			if gotStatus != status {
				t.Fatalf("%s: status became %q. The toggle governs FUTURE limit behaviour and "+
					"must never move the state machine", status, gotStatus)
			}
		}
	})

	t.Run("turning it OFF on a parked run leaves it parked", func(t *testing.T) {
		id := newRun(t, "limit_wait", true)
		if rows := set(t, id, userID, false); rows != 1 {
			t.Fatalf("rows = %d, want 1", rows)
		}
		status, wait := read(t, id)
		if wait {
			t.Fatal("the flag did not clear")
		}
		if status != "limit_wait" {
			t.Fatalf("status = %q, want limit_wait still. Un-parking here would fail or "+
				"resume a run because the user changed a PREFERENCE — Decision 11's cancel "+
				"is the control for stopping a parked run, and it is a different request", status)
		}
	})

	t.Run("a terminal run is a no-op", func(t *testing.T) {
		for _, status := range []string{"completed", "failed", "cancelled"} {
			id := newRun(t, status, false)
			if rows := set(t, id, userID, true); rows != 0 {
				t.Fatalf("%s: rows = %d, want 0", status, rows)
			}
			if _, wait := read(t, id); wait {
				t.Fatalf("%s: the flag moved on a terminal run", status)
			}
		}
	})

	t.Run("a foreign run is untouched", func(t *testing.T) {
		id := newRun(t, "running", false)
		if rows := set(t, id, otherID, true); rows != 0 {
			t.Fatalf("rows = %d, want 0 — ownership is this statement's OWN predicate, not a "+
				"fact maintained by the caller", rows)
		}
		if _, wait := read(t, id); wait {
			t.Fatal("a non-owner flipped someone else's run")
		}
	})
}
