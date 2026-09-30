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

// TestListRunsRepoLessJobLiveDB pins PRD #1908: ListRunsForUser and ListActiveRunsAll LEFT JOIN
// repos and forge_connections, so a repo-less kind='job' run (repo_id NULL) is listed with NULL
// repo_path and forge_type, while the repo-less chat and judge meta-runs stay excluded and a
// repo-backed run is unchanged. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; run via ./e2e/run-store-it.sh.
func TestListRunsRepoLessJobLiveDB(t *testing.T) {
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
	jobRun, judgeRun, chatRun, issueRun := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("repoless-list-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 1, 'issue run', 'd', 'running', 'issue')`, issueRun, userID, repoID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status)
		 VALUES ($1, $2, 'job', 'research', 'job run', 'prompt', true, '{}', 'queued')`, jobRun, userID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, trigger_source)
		 VALUES ($1, $2, 'judge', $3, 'judge run', 'd', 'queued', 'judge')`, judgeRun, userID, issueRun)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, 'chat run', 'd', 'running', 'chat')`, chatRun, userID)

	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}

	rows, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userID, BackgroundGraceCutoff: cutoff})
	if err != nil {
		t.Fatalf("ListRunsForUser: %v", err)
	}
	byID := map[uuid.UUID]store.ListRunsForUserRow{}
	for _, r := range rows {
		byID[r.Run.ID] = r
	}
	if len(rows) != 2 {
		t.Fatalf("ListRunsForUser returned %d rows, want exactly the job and issue runs", len(rows))
	}
	if r, ok := byID[jobRun]; !ok {
		t.Fatal("ListRunsForUser dropped the repo-less job run")
	} else if r.RepoPath.Valid || r.ForgeType.Valid {
		t.Fatalf("job run repo_path/forge_type = %+v / %+v, want NULL", r.RepoPath, r.ForgeType)
	}
	if r, ok := byID[issueRun]; !ok {
		t.Fatal("ListRunsForUser dropped the repo-backed issue run")
	} else if r.RepoPath.String != "g/r" || r.ForgeType.String != "gitlab" {
		t.Fatalf("issue run repo_path/forge_type = %+v / %+v, want g/r / gitlab", r.RepoPath, r.ForgeType)
	}
	for _, excluded := range []uuid.UUID{judgeRun, chatRun} {
		if _, ok := byID[excluded]; ok {
			t.Fatalf("ListRunsForUser listed the repo-less meta-run %s", excluded)
		}
	}

	// A repo narrowing still matches only the repo's runs (a job has no repo).
	narrowed, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{
		UserID: userID, BackgroundGraceCutoff: cutoff, RepoID: pgtype.UUID{Bytes: repoID, Valid: true},
	})
	if err != nil {
		t.Fatalf("ListRunsForUser narrowed: %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].Run.ID != issueRun {
		t.Fatalf("repo-narrowed list = %d rows, want only the issue run", len(narrowed))
	}

	active, err := q.ListActiveRunsAll(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListActiveRunsAll: %v", err)
	}
	activeByID := map[uuid.UUID]store.ListActiveRunsAllRow{}
	for _, r := range active {
		activeByID[r.Run.ID] = r
	}
	if r, ok := activeByID[jobRun]; !ok {
		t.Fatal("ListActiveRunsAll dropped the repo-less job run")
	} else if r.RepoPath.Valid || r.ForgeType.Valid {
		t.Fatalf("active job run repo_path/forge_type = %+v / %+v, want NULL", r.RepoPath, r.ForgeType)
	}
	if _, ok := activeByID[issueRun]; !ok {
		t.Fatal("ListActiveRunsAll dropped the repo-backed issue run")
	}
	for _, excluded := range []uuid.UUID{judgeRun, chatRun} {
		if _, ok := activeByID[excluded]; ok {
			t.Fatalf("ListActiveRunsAll listed the repo-less meta-run %s", excluded)
		}
	}
}
