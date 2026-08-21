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

// TestListRunsIssueWebURLLiveDB proves PRD #411 M1's LEFT JOIN issues actually RUNS and
// populates issue_web_url against a REAL Postgres — the SQL contract a fake-store handler
// test cannot reach (it would prove only Go-side mapping). The M1 change added
//
//	LEFT JOIN issues i ON i.repo_id = r.repo_id AND i.forge_issue_iid = r.issue_iid
//	... i.web_url AS issue_web_url
//
// to ListRunsForUser and ListActiveRunsAll.
//
// Discriminating fixture: two runs that differ ONLY in whether their issue is cached in
// the issues table — run A (iid 101) has a matching issues row, run B (iid 202) does not.
// This is what makes the test non-vacuous in two independent directions:
//   - a bug that hard-codes issue_web_url to NULL fails run A's Valid==true assertion;
//   - a bug that fans the LEFT JOIN out (e.g. dropping the UNIQUE(repo_id,forge_issue_iid)
//     assumption, or a wrong join predicate) fails the exact-2-rows count.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; mirrors the other
// *LiveDB tests in this package (see run_usage_integration_test.go for the harness).
func TestListRunsIssueWebURLLiveDB(t *testing.T) {
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
	runA, runB := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("issueweburl-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// One cached issue for iid 101 only. web_url is NOT NULL in 00002_forge.sql; labels
	// defaults [] and has_prd_link defaults false, so they are omitted here.
	const wantURL = "https://forge.e2e/g/r/-/issues/101"
	mustExec(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, web_url, forge_updated_at, synced_at)
		 VALUES ($1, 101, 't', 'opened', $2, now(), now())`, repoID, wantURL)

	// Two running issue runs. kind='issue' passes ListRunsForUser's kind NOT IN
	// ('chat','judge') filter, and status='running' passes ListActiveRunsAll's
	// non-terminal filter, so both rows appear in both queries. Run A's issue_iid
	// matches the cached issue; run B's (202) has no issues row → NULL join.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 101, 'run A', 'd', 'running', 'issue')`, runA, userID, repoID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 202, 'run B', 'd', 'running', 'issue')`, runB, userID, repoID)

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}

	// --- ListRunsForUser: scoped to this user, so exactly 2 rows. RepoID/IssueIid are
	// left zero (invalid) → no narrowing. The exact-2 count is the fan-out guard: the
	// LEFT JOIN onto a UNIQUE(repo_id, forge_issue_iid) issues row must not multiply rows.
	rows, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{
		UserID:                userID,
		BackgroundGraceCutoff: cutoff,
	})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListRunsForUser returned %d rows, want exactly 2 (the LEFT JOIN must not fan out)", len(rows))
	}
	byID := map[uuid.UUID]store.ListRunsForUserRow{}
	for _, r := range rows {
		byID[r.Run.ID] = r
	}
	if r, ok := byID[runA]; !ok {
		t.Fatalf("ListRunsForUser missing run A")
	} else if !r.IssueWebUrl.Valid || r.IssueWebUrl.String != wantURL {
		t.Fatalf("run A issue_web_url = %+v, want valid %q (issue is cached)", r.IssueWebUrl, wantURL)
	}
	if r, ok := byID[runB]; !ok {
		t.Fatalf("ListRunsForUser missing run B")
	} else if r.IssueWebUrl.Valid {
		t.Fatalf("run B issue_web_url = %+v, want NULL (issue not cached)", r.IssueWebUrl)
	}

	// --- ListActiveRunsAll: admin-wide (no user filter), both runs non-terminal so both
	// appear. The store IT runner may share the DB with other *LiveDB tests, so assert on
	// THIS test's runs by id rather than a total count; the presence/absence of the join
	// is what matters.
	active, err := q.ListActiveRunsAll(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListActiveRunsAll: %v", err)
	}
	activeByID := map[uuid.UUID]store.ListActiveRunsAllRow{}
	for _, r := range active {
		activeByID[r.Run.ID] = r
	}
	if r, ok := activeByID[runA]; !ok {
		t.Fatalf("ListActiveRunsAll missing run A")
	} else if !r.IssueWebUrl.Valid || r.IssueWebUrl.String != wantURL {
		t.Fatalf("ListActiveRunsAll run A issue_web_url = %+v, want valid %q", r.IssueWebUrl, wantURL)
	}
	if r, ok := activeByID[runB]; !ok {
		t.Fatalf("ListActiveRunsAll missing run B")
	} else if r.IssueWebUrl.Valid {
		t.Fatalf("ListActiveRunsAll run B issue_web_url = %+v, want NULL (issue not cached)", r.IssueWebUrl)
	}
}
