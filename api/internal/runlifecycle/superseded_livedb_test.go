package runlifecycle

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// #4704: GetRunMoveContext.superseded is true only when a strictly later-created run
// exists on the same repo AND issue. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres.
func TestGetRunMoveContextSupersededLiveDB(t *testing.T) {
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
	t.Cleanup(pool.Close)
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	user, conn := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("superseded-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, user, "https://"+uuid.NewString()+".forge.test", []byte("sealed"))
	newRepo := func() uuid.UUID {
		t.Helper()
		repo := uuid.New()
		exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      VALUES ($1, $2, $3, $4, 'https://forge.test/g/s', 'main', true)`,
			repo, conn, 1_000_000_000+int64(uuid.New().ID()), "g/superseded-"+repo.String())
		return repo
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	newRun := func(repo uuid.UUID, iid int64, createdAt time.Time) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, created_at)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'completed', $6)`,
			id, user, repo, iid, "agent/issue-"+id.String(), createdAt)
		return id
	}

	repoA, repoB := newRepo(), newRepo()
	oldRun := newRun(repoA, 41, now.Add(-2*time.Hour))
	newerRun := newRun(repoA, 41, now.Add(-time.Hour))
	otherIssue := newRun(repoA, 42, now.Add(-2*time.Hour))
	otherRepo := newRun(repoB, 41, now.Add(-2*time.Hour))

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"older run, later run on the same issue", oldRun, true},
		{"newest run on the issue", newerRun, false},
		{"run on another issue", otherIssue, false},
		{"same issue number on another repo", otherRepo, false},
	} {
		row, err := q.GetRunMoveContext(ctx, tc.id)
		if err != nil {
			t.Fatalf("%s: GetRunMoveContext: %v", tc.name, err)
		}
		if row.Superseded != tc.want {
			t.Errorf("%s: Superseded = %v, want %v", tc.name, row.Superseded, tc.want)
		}
	}
}
