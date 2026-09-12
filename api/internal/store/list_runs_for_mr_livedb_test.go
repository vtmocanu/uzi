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

// TestListRunsForMRLiveDB exercises ListRunsForMR (PRD #1255 M2a) against a REAL
// Postgres: it returns every run on a (repo, mr_iid) newest-first regardless of kind or
// status, and an empty slice for an unmatched pair. The forge-view pulls list uses it to
// link each open PR to its newest uzi run.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-it
// sweep runs it (the LiveDB suffix). Never exported into `task gate:api`.
func TestListRunsForMRLiveDB(t *testing.T) {
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

	userID, connID, repoID, otherRepoID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("lrfm-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/other', 'https://forge.e2e/g/other', 'main', true)`, otherRepoID, connID)

	// seedRun inserts a run on (repo, mr_iid) with an explicit created_at so ordering is
	// deterministic. mr_iid nil leaves the column NULL. Any kind/status is allowed.
	seedRun := func(repo uuid.UUID, mrIID *int64, kind, status string, createdAt time.Time) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, mr_iid, created_at)
			 VALUES ($1, $2, $3, 7, 'X', 'd', $4, $5, $6, $7)`,
			id, userID, repo, status, kind, pgconvInt8Ptr(mrIID), createdAt)
		return id
	}

	now := time.Now().UTC()
	iid42 := int64(42)
	// Two runs on (repo, 42): a newer completed issue run and an older cancelled
	// self_improve run — different kinds and statuses, both must be returned
	// newest-first. (Both kinds satisfy runs_kind_shape with repo_id + issue_iid; a
	// ci_fix run would additionally require pipeline_id/pipeline_ref, irrelevant here.)
	newer := seedRun(repoID, &iid42, "issue", "completed", now)
	_ = seedRun(repoID, &iid42, "self_improve", "cancelled", now.Add(-2*time.Hour))
	// Distractors that must NOT appear: a run on the same repo but a different MR, and a
	// run on a different repo with the same MR iid.
	iid99 := int64(99)
	_ = seedRun(repoID, &iid99, "issue", "completed", now)
	_ = seedRun(otherRepoID, &iid42, "issue", "completed", now)

	got, err := q.ListRunsForMR(ctx, store.ListRunsForMRParams{RepoID: repoID, MrIid: pgtype.Int8{Int64: iid42, Valid: true}})
	if err != nil {
		t.Fatalf("ListRunsForMR: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListRunsForMR(repo, 42) returned %d runs, want 2 (both kinds/statuses on this MR)", len(got))
	}
	if got[0].ID != newer {
		t.Errorf("ListRunsForMR must return newest first: got[0]=%s, want the newer run %s", got[0].ID, newer)
	}
	if !got[0].CreatedAt.Time.After(got[1].CreatedAt.Time) {
		t.Errorf("rows not ordered created_at DESC: got[0]=%s got[1]=%s", got[0].CreatedAt.Time, got[1].CreatedAt.Time)
	}

	// An unmatched (repo, mr_iid) pair returns empty, not an error.
	empty, err := q.ListRunsForMR(ctx, store.ListRunsForMRParams{RepoID: repoID, MrIid: pgtype.Int8{Int64: 12345, Valid: true}})
	if err != nil {
		t.Fatalf("ListRunsForMR(unmatched): %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListRunsForMR(repo, 12345) returned %d runs, want 0", len(empty))
	}
}

// pgconvInt8Ptr mirrors pgconv.Int8Ptr locally so this test file has no dependency
// beyond the store package (a nil pointer is SQL NULL).
func pgconvInt8Ptr(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}
