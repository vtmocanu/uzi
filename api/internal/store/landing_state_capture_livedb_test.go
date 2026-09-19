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

// TestLandingStateCaptureLiveDB is the issue #1418 M1 seam-proof: the live-DB gate for the
// two read-path queries that supply the landing_state derivation's capture-availability
// fact — the has_available_capture column ListRunsForUser now projects, and the standalone
// RunHasAvailableCapture GetRun uses. It statically references both new query surfaces so
// `deadcode -test ./...` sees them reachable, AND asserts their real behaviour against a
// real Postgres: false before any available capture exists, true once an 'available'
// recovery_captures row is inserted for the run+owner.
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh and the CI
// test-api-store-it job run `-run 'LiveDB$'` over ./internal/store/... only.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. A package that
// prints `ok` with PASS=0 is INVALID, not green.
func TestLandingStateCaptureLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
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

	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("landing-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/landing', 'https://forge.e2e/g/landing', 'main', true)`, repoID, connID)
	workerName := "w-" + workerID.String()
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, workerName, workerID[:])

	// A failed run whose fail_origin is human-landable (push_secret_blocked), with NO
	// preserved_patch and NO capture yet: the "unrecoverable" shape until a capture lands.
	runID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin)
	      VALUES ($1, $2, $3, 'issue', 1, 'seam', 'ctx', 'failed', 'push_secret_blocked')`, runID, userID, repoID)

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-10 * time.Minute), Valid: true}

	// ── Before any available capture: BOTH read paths report false. ──
	rows, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userID, BackgroundGraceCutoff: cutoff})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	if got := findRunRow(t, rows, runID).HasAvailableCapture; got {
		t.Fatalf("ListRunsForUser has_available_capture = %t before any capture, want false", got)
	}
	if got, err := q.RunHasAvailableCapture(ctx, store.RunHasAvailableCaptureParams{RunID: runID, UserID: userID}); err != nil {
		t.Fatalf("RunHasAvailableCapture(before): %v", err)
	} else if got {
		t.Fatalf("RunHasAvailableCapture = %t before any capture, want false", got)
	}

	// ── Insert an 'available' capture for this run+owner (recovery_captures.hold_id is a
	// NOT NULL FK, so a custody hold is opened first). BOTH read paths now report true. ──
	holdID, capID := uuid.New(), uuid.New()
	exec(`INSERT INTO recovery_custody_holds
	        (id, user_id, repo_id, run_id, generation, state,
	         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	      VALUES ($1, $2, $3, $4, 1, 'open', $5, $6, $5, $4)`,
		holdID, userID, repoID, runID, workerID, workerName)
	exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	      VALUES ($1, $2, $3, $4, $5, 'H0', 'kavail', 'available')`, capID, holdID, runID, userID, workerName)

	rows, err = q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userID, BackgroundGraceCutoff: cutoff})
	if err != nil {
		t.Fatalf("ListRunsForUser(after): %v", err)
	}
	if got := findRunRow(t, rows, runID).HasAvailableCapture; !got {
		t.Fatalf("ListRunsForUser has_available_capture = %t after an available capture, want true", got)
	}
	if got, err := q.RunHasAvailableCapture(ctx, store.RunHasAvailableCaptureParams{RunID: runID, UserID: userID}); err != nil {
		t.Fatalf("RunHasAvailableCapture(after): %v", err)
	} else if !got {
		t.Fatalf("RunHasAvailableCapture = %t after an available capture, want true", got)
	}

	// Owner-scoped: a foreign owner reading the same run+capture still reads false.
	if got, err := q.RunHasAvailableCapture(ctx, store.RunHasAvailableCaptureParams{RunID: runID, UserID: uuid.New()}); err != nil {
		t.Fatalf("RunHasAvailableCapture(foreign owner): %v", err)
	} else if got {
		t.Fatalf("RunHasAvailableCapture(foreign owner) = %t, want false (owner-scoped)", got)
	}

	// State-scoped: a capture that exists but is NOT 'available' (here 'preparing') must not
	// count — this guards the `AND c.state = 'available'` predicate in BOTH read paths
	// (dropping it would make a still-uploading capture read needs_landing).
	run2 := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin)
	      VALUES ($1, $2, $3, 'issue', 2, 'seam2', 'ctx', 'failed', 'push_secret_blocked')`, run2, userID, repoID)
	hold2, cap2 := uuid.New(), uuid.New()
	exec(`INSERT INTO recovery_custody_holds
	        (id, user_id, repo_id, run_id, generation, state,
	         original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	      VALUES ($1, $2, $3, $4, 1, 'open', $5, $6, $5, $4)`,
		hold2, userID, repoID, run2, workerID, workerName)
	exec(`INSERT INTO recovery_captures (id, hold_id, run_id, user_id, original_worker_identity, source_sha, idempotency_key, state)
	      VALUES ($1, $2, $3, $4, $5, 'H2', 'kprep', 'preparing')`, cap2, hold2, run2, userID, workerName)

	rows, err = q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userID, BackgroundGraceCutoff: cutoff})
	if err != nil {
		t.Fatalf("ListRunsForUser(state): %v", err)
	}
	if got := findRunRow(t, rows, run2).HasAvailableCapture; got {
		t.Fatalf("ListRunsForUser has_available_capture = %t for a 'preparing' capture, want false (state-scoped)", got)
	}
	if got, err := q.RunHasAvailableCapture(ctx, store.RunHasAvailableCaptureParams{RunID: run2, UserID: userID}); err != nil {
		t.Fatalf("RunHasAvailableCapture(state): %v", err)
	} else if got {
		t.Fatalf("RunHasAvailableCapture = %t for a 'preparing' capture, want false (state-scoped)", got)
	}
}

func findRunRow(t *testing.T, rows []store.ListRunsForUserRow, runID uuid.UUID) store.ListRunsForUserRow {
	t.Helper()
	for _, r := range rows {
		if r.Run.ID == runID {
			return r
		}
	}
	t.Fatalf("ListRunsForUser did not return run %s", runID)
	return store.ListRunsForUserRow{}
}
