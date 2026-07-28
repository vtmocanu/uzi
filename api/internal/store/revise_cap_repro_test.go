package store_test

// A DETERMINISTIC REPRODUCTION OF GITLAB ISSUE #106 — the plan-revision cap can be
// breached by two concurrent submits, and the breach is DURABLE.
//
// ┌─────────────────────────────────────────────────────────────────────────────────┐
// │ TestReviseCapForcedInterleaveIssue106 IS EXPECTED TO FAIL until #106 is fixed.   │
// │ A red from it is the artifact working, not a broken test.                        │
// └─────────────────────────────────────────────────────────────────────────────────┘
//
// How to run it:
//
//	UZI_REPRO_106=1 UZI_TEST_DATABASE_URL=postgres://... \
//	  go test -count=1 -v -p 1 -run 'Issue106$' ./internal/store/...
//
// Both tests skip unless UZI_REPRO_106 is set, so they never fire during an ordinary
// `go test ./...` — including for someone who has exported UZI_TEST_DATABASE_URL, which
// this repo documents as a hazard. Without that guard they would hand that person a
// mystifying red from a test that is supposed to fail.
//
// # WHY THESE NAMES DO NOT END IN LiveDB
//
// That suffix is not decoration: it is the selector for both live-DB runners
// (`e2e/run-store-it.sh` and CI's `test:api-store-it` both run `-run 'LiveDB$'`). CI
// additionally asserts `if [ "$ran" -eq 0 ] || [ "$skipped" -gt 0 ]; then exit 1`, so a
// LiveDB-suffixed name here would redden the pipeline BOTH ways — by skipping (the skip
// assertion fires) or by running (it fails by design). Renaming these without reading
// that job is how this file turns into a red pipeline.
//
// # WHEN #106 IS FIXED: RENAME THIS FILE'S TESTS. DO NOT DELETE THEM.
//
// Deleting it is the tempting move and it is the wrong one. The shipped
// TestCreateRunReviseInputIfUnderCapAtomicLiveDB hopes for the interleave instead of
// forcing it, and it was measured catching this defect in roughly 1 run in 50 — so a
// green from that test cannot verify a fix, while this file reproduces the breach on
// every single run. It is the only thing in the repo that can tell a real fix from a
// lucky one, and that is worth more AFTER the fix than before it.
//
// Two edits promote it to the CI regression gate:
//
//  1. rename both tests to end in LiveDB (e.g. TestReviseCapForcedInterleaveLiveDB), and
//  2. drop the UZI_REPRO_106 guard in repro106Open, leaving the ordinary
//     UZI_TEST_DATABASE_URL skip that every other live-DB test uses.
//
// Then invert the forced test's expectation: it currently asserts the CORRECT behaviour
// (`landed == 1`, `persisted <= cap`) and fails, so a fix turns it green with no
// assertion change. Keep BOTH assertions. They are not redundant: a fix that merely makes
// the losing caller retry could satisfy `landed == 1` while still leaving an over-cap row
// behind, and only the `persisted` assertion sees that.
//
// # WHAT MAKES THIS A PROOF RATHER THAN A REPRODUCTION
//
// Everything on the path is the shipped thing — real goose migrations via store.Migrate,
// the real generated store.Queries.CreateRunReviseInputIfUnderCap (never retyped), the
// real store.OpenPool, and a racing caller on the bare pool with no transaction and no
// isolation setting, exactly as workersvc.SubmitInput calls it.
//
// The one thing that is NOT production-shaped is the scheduling device: side A runs the
// statement inside an explicit transaction so the window stays open long enough to be
// observed. That does not manufacture the defect. Under READ COMMITTED every statement
// takes its own snapshot whether or not it sits in an explicit transaction, so A's
// execution is semantically identical either way; the transaction only lets the test HOLD
// the window that production hits by luck.
//
// The lock confirmation below is load-bearing for the same reason. A forced-interleave
// test that cannot confirm the block would report a red whenever it merely failed to
// force anything — a lying failure message, which is the worst kind, because it routes
// the next reader into the wrong subsystem. So a run that cannot observe the block
// Fatalf's itself as INVALID instead of reporting a red it has not earned.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

const repro106Cap = 3

// repro106Open applies BOTH guards and explains itself in the skip message, so anyone who
// trips it learns what this file is without opening it.
func repro106Open(ctx context.Context, t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("UZI_REPRO_106") == "" {
		t.Skip("UZI_REPRO_106 not set: this file deliberately reproduces OPEN issue #106 " +
			"(the plan-revision cap can be breached by two concurrent submits) and its forced " +
			"test is EXPECTED TO FAIL until that is fixed. Set UZI_REPRO_106=1 with " +
			"UZI_TEST_DATABASE_URL to run it. See this file's header before renaming anything.")
	}
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; point it at a throwaway Postgres")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	return pool
}

// repro106SeedRun builds a fresh user/connection/repo/run. The cap count subquery filters
// on run_id, so a fresh run excludes any other test's rows by CONSTRUCTION rather than by
// argument — which is what lets this file reproduce identically on a reused database and
// on a virgin one.
func repro106SeedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tag string) uuid.UUID {
	t.Helper()
	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("repro106-%s-%s@e2e", tag, userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'awaiting_approval')`, runID, userID, repoID)
	return runID
}

func repro106Params(runID uuid.UUID, body string) store.CreateRunReviseInputIfUnderCapParams {
	return store.CreateRunReviseInputIfUnderCapParams{
		RunID:        runID,
		Body:         pgtype.Text{String: body, Valid: true},
		MaxRevisions: repro106Cap,
	}
}

func repro106ReviseCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'revise_plan'`,
		runID).Scan(&n); err != nil {
		t.Fatalf("count revise rows: %v", err)
	}
	return n
}

// repro106WaitBlockedOnLock proves, from a THIRD connection, that the racing caller is
// parked on a lock rather than merely slow. Returns false if it cannot establish that;
// the caller must then invalidate the run rather than report a red.
func repro106WaitBlockedOnLock(ctx context.Context, t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		// The generated const carries its own `-- name: CreateRunReviseInputIfUnderCap :one`
		// header, so pg_stat_activity.query identifies the waiter unambiguously.
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%CreateRunReviseInputIfUnderCap%'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			t.Logf("confirmed: %d caller(s) blocked with wait_event_type=Lock on CreateRunReviseInputIfUnderCap", n)
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

type repro106Result struct {
	landed bool
	err    error
}

// repro106CallOnPool is the production caller shape: the generated query, on the bare
// pool, with no transaction and no isolation setting — what workersvc.SubmitInput does.
func repro106CallOnPool(ctx context.Context, q *store.Queries, runID uuid.UUID, body string) repro106Result {
	_, err := q.CreateRunReviseInputIfUnderCap(ctx, repro106Params(runID, body))
	if errors.Is(err, pgx.ErrNoRows) {
		return repro106Result{landed: false}
	}
	return repro106Result{landed: err == nil, err: err}
}

// TestReviseCapForcedInterleaveIssue106 is the shipped atomicity test's assertion with the
// luck removed. EXPECTED TO FAIL until #106 is fixed — see this file's header.
func TestReviseCapForcedInterleaveIssue106(t *testing.T) {
	ctx := context.Background()
	pool := repro106Open(ctx, t)
	defer pool.Close()
	q := store.New(pool)

	runID := repro106SeedRun(ctx, t, pool, "forced")

	// Prefill to cap-1: two revisions in, exactly one slot left.
	for i := 0; i < repro106Cap-1; i++ {
		if _, err := q.CreateRunReviseInputIfUnderCap(ctx, repro106Params(runID, fmt.Sprintf("fill %d", i))); err != nil {
			t.Fatalf("prefill %d: %v", i, err)
		}
	}
	if got := repro106ReviseCount(ctx, t, pool, runID); got != repro106Cap-1 {
		t.Fatalf("prefill left %d rows, want %d", got, repro106Cap-1)
	}

	// --- side A: the SAME generated query inside a transaction, held open.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint // no-op after Commit
	if _, err := q.WithTx(tx).CreateRunReviseInputIfUnderCap(ctx, repro106Params(runID, "A")); err != nil {
		t.Fatalf("A's submit should have landed (it was at cap-1): %v", err)
	}
	// A now holds the runs row FOR UPDATE and has an INSERTED-BUT-UNCOMMITTED third row.

	// --- side B: the production caller shape, concurrently.
	bCh := make(chan repro106Result, 1)
	go func() { bCh <- repro106CallOnPool(ctx, q, runID, "B") }()

	// B must be parked on A's row lock BEFORE A commits, or the interleave is not the one
	// under test and any result is meaningless.
	if !repro106WaitBlockedOnLock(ctx, t, pool) {
		t.Fatalf("B never blocked on a lock within 15s — interleave NOT established, this run is INVALID (not a red)")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	rb := <-bCh
	if rb.err != nil && !errors.Is(rb.err, pgx.ErrNoRows) {
		t.Fatalf("B errored: %v", rb.err)
	}

	landed := 1 // A landed; asserted above
	if rb.landed {
		landed++
	}
	persisted := repro106ReviseCount(ctx, t, pool, runID)
	t.Logf("forced interleave: landed=%d persisted=%d cap=%d", landed, persisted, repro106Cap)

	// Same assertion as the shipped TestCreateRunReviseInputIfUnderCapAtomicLiveDB.
	if landed != 1 {
		t.Errorf("concurrent submits at N-1 landed %d rows, want exactly 1 (the cap must serialize)", landed)
	}
	// NOT redundant with the above: a fix that only makes the loser retry could satisfy
	// `landed == 1` while still leaving an over-cap row behind. Only this sees that.
	if persisted > repro106Cap {
		t.Errorf("persisted revise rows = %d, want <= %d — the cap was breached and the row is DURABLE", persisted, repro106Cap)
	}
}

// TestReviseCapSequentialControlIssue106 is the INVERTED positive control: the identical
// code path with the interleave removed (B starts only after A commits). It MUST PASS,
// today and after the fix. Without it, a red from the forced test above is equally
// consistent with a broken harness.
func TestReviseCapSequentialControlIssue106(t *testing.T) {
	ctx := context.Background()
	pool := repro106Open(ctx, t)
	defer pool.Close()
	q := store.New(pool)

	runID := repro106SeedRun(ctx, t, pool, "seq")

	for i := 0; i < repro106Cap-1; i++ {
		if _, err := q.CreateRunReviseInputIfUnderCap(ctx, repro106Params(runID, fmt.Sprintf("fill %d", i))); err != nil {
			t.Fatalf("prefill %d: %v", i, err)
		}
	}

	// Side A, same transaction shape as the forced test — but COMMITTED before B starts.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := q.WithTx(tx).CreateRunReviseInputIfUnderCap(ctx, repro106Params(runID, "A")); err != nil {
		t.Fatalf("A's submit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	// Only now does B run — its snapshot is taken after A's commit.
	rb := repro106CallOnPool(ctx, q, runID, "B")
	if rb.err != nil && !errors.Is(rb.err, pgx.ErrNoRows) {
		t.Fatalf("B errored: %v", rb.err)
	}
	persisted := repro106ReviseCount(ctx, t, pool, runID)
	t.Logf("sequential control: B.landed=%v persisted=%d cap=%d", rb.landed, persisted, repro106Cap)

	if rb.landed {
		t.Errorf("sequential B landed a row; the cap was already full so it must be refused")
	}
	if persisted != repro106Cap {
		t.Errorf("persisted = %d, want exactly %d", persisted, repro106Cap)
	}
}
