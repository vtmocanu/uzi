package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCountInProgressRunsForUserLiveDB exercises PRD #239's Runs-nav-badge count
// against a REAL Postgres: the query must count only the caller's NON-TERMINAL runs
// (Decision 1), of the kinds the /runs page lists (Decision 4 — chat and judge
// excluded), and nobody else's.
//
// The fixture spans every run status — the six non-terminal ones (queued, claimed,
// running, awaiting_approval, awaiting_input, limit_wait) and all three terminal ones
// (completed, failed, cancelled) — plus a non-terminal chat run and a non-terminal
// judge run (both must be excluded by kind), plus a second user's non-terminal run
// (must be excluded by owner scope). The expected count is therefore the six
// non-terminal issue runs, and nothing else.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the e2e runner
// (e2e/run-store-it.sh) provides one. `go test ./...` without it SKIPs.
func TestCountInProgressRunsForUserLiveDB(t *testing.T) {
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

	// Fresh ids isolate this run from leftover rows (the count is user_id-scoped).
	userID, otherID, connID, repoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, u := range []uuid.UUID{userID, otherID} {
		mustExec(ctx, t, pool,
			`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("inprog-%s@e2e", u))
	}
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID)

	// issueRun inserts one kind='issue' run for a user. Each gets a distinct issue_iid
	// so the one-non-terminal-run-per-issue partial unique index never fires. The
	// returned id lets a judge run point at it as its target.
	issueRun := func(u uuid.UUID, iid int64, status string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'issue')`,
			id, u, repoID, iid, status)
		return id
	}

	// The six NON-TERMINAL statuses — every one must count.
	var iid int64
	nonTerminal := []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "limit_wait"}
	for _, st := range nonTerminal {
		iid++
		issueRun(userID, iid, st)
	}
	// The three TERMINAL statuses — none may count.
	var aTerminal uuid.UUID
	for _, st := range []string{"completed", "failed", "cancelled"} {
		iid++
		aTerminal = issueRun(userID, iid, st)
	}

	// A non-terminal CHAT run and a non-terminal JUDGE run — excluded by kind even
	// though their status is non-terminal. Both are repo/issue/branch-less; the judge
	// points at a terminal issue run above as its target.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, issue_title, issue_description, status, kind)
		 VALUES ($1, 't', 'd', 'running', 'chat')`,
		userID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (user_id, issue_title, issue_description, status, kind, target_run_id)
		 VALUES ($1, 't', 'd', 'running', 'judge', $2)`,
		userID, aTerminal)

	// A DIFFERENT user's non-terminal issue run — excluded by owner scope. It shares
	// the repo but not the user, so only the user_id filter keeps it out of the count.
	issueRun(otherID, 999, "running")

	// Expect exactly the six non-terminal issue runs for userID.
	got, err := q.CountInProgressRunsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("CountInProgressRunsForUser: %v", err)
	}
	if want := int64(len(nonTerminal)); got != want {
		t.Fatalf("CountInProgressRunsForUser = %d, want %d (the six non-terminal issue runs; "+
			"terminal, chat, judge, and the other user's run must all be excluded)", got, want)
	}

	// The other user sees only their own single non-terminal run — the owner filter cuts both ways.
	if other, err := q.CountInProgressRunsForUser(ctx, otherID); err != nil || other != 1 {
		t.Fatalf("CountInProgressRunsForUser(other) = %d, %v; want 1", other, err)
	}
}
