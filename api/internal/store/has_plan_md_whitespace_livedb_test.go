package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #342 against a REAL Postgres. The board card's has_plan_md is computed in SQL
// with btrim; the run DTO's is_planning is computed in Go with strings.TrimSpace. If the
// two whitespace sets disagree, a plan_md made entirely of whitespace can read as present
// on one surface and absent on the other, so the board and the run view disagree on
// is_planning. This is only knowable by executing the SQL: sqlc/go build/go vet all pass
// regardless of which characters btrim actually strips, and Postgres — not Go — decides
// what E'\uXXXX' means. The test pins the SQL predicate to Go's unicode.IsSpace set by
// requiring the query's HasPlanMd to equal strings.TrimSpace(plan_md) != "" for every
// input, ASCII and non-ASCII alike.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestHasPlanMdWhitespaceLiveDB(t *testing.T) {
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
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("plan342-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, max_concurrent_runs)
		 VALUES ($1, $2, 'w', $3, 4)`, workerID, userID, fmt.Sprintf("hash-%s", workerID))

	// One issue (one run) per plan_md input so each surfaces as its own row in the
	// DISTINCT ON board list. wantHasPlan is asserted to equal the Go predicate below,
	// so the case table cannot drift from strings.TrimSpace on its own.
	cases := []struct {
		name        string
		iid         int64
		planMd      string
		wantHasPlan bool
	}{
		{"ascii_whitespace_only", 1, "\t\n ", false},
		// U+00A0 NBSP, U+2003 EM SPACE, U+3000 IDEOGRAPHIC SPACE — all whitespace to
		// unicode.IsSpace but NONE in the old ASCII-only btrim set (the issue #342 bug).
		{"unicode_whitespace_only", 2, "  　", false},
		{"real_content", 3, "# Plan\nreal content", true},
	}

	for _, c := range cases {
		// The Go definition of "empty plan" — the exact predicate runToDTO feeds into
		// isPlanningPhase. The whole test is that the SQL result must equal this.
		goPredicate := strings.TrimSpace(c.planMd) != ""
		if goPredicate != c.wantHasPlan {
			t.Fatalf("fixture %q: strings.TrimSpace(plan)!=\"\" is %v but the case expects %v — fix the case table",
				c.name, goPredicate, c.wantHasPlan)
		}
		runID := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status, kind, plan_md)
			 VALUES ($1, $2, $3, $4, $5, 't', 'd', 'running', 'issue', $6)`,
			runID, userID, repoID, workerID, c.iid, c.planMd)
	}

	list, err := q.ListLatestRunsForRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("ListLatestRunsForRepo: %v", err)
	}
	listByIID := make(map[int64]bool, len(list))
	for _, row := range list {
		listByIID[row.IssueIid.Int64] = row.HasPlanMd.Bool
	}

	for _, c := range cases {
		want := strings.TrimSpace(c.planMd) != "" // == c.wantHasPlan, re-derived to lock the two definitions together.

		got, ok := listByIID[c.iid]
		if !ok {
			t.Fatalf("ListLatestRunsForRepo returned no row for issue %d (%s)", c.iid, c.name)
		}
		if got != want {
			t.Fatalf("ListLatestRunsForRepo has_plan_md=%v for %s (plan_md=%q), want %v to match strings.TrimSpace(plan)!=\"\" — "+
				"the board card and the run DTO disagree on emptiness (issue #342)", got, c.name, c.planMd, want)
		}

		single, err := q.GetLatestRunForIssue(ctx, store.GetLatestRunForIssueParams{
			RepoID:   repoID,
			IssueIid: pgtype.Int8{Int64: c.iid, Valid: true},
		})
		if err != nil {
			t.Fatalf("GetLatestRunForIssue(%d): %v", c.iid, err)
		}
		if single.HasPlanMd.Bool != want {
			t.Fatalf("GetLatestRunForIssue has_plan_md=%v for %s (plan_md=%q), want %v to match strings.TrimSpace(plan)!=\"\" (issue #342)",
				single.HasPlanMd.Bool, c.name, c.planMd, want)
		}
	}
}
