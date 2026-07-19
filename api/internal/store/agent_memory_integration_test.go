package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentMemoryLiveDB exercises the PRD #90 M1/M4 schema + queries against a REAL
// Postgres: the (user,repo)-scoped read, the per-run count the write cap keys off,
// the oldest-eviction that keeps the newest N, the cross-user/cross-repo isolation,
// and the owner-scoped delete. None of these are visible to a fake store (Handler.q
// is a concrete type, and the eviction ordering is a live SQL property).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func TestAgentMemoryLiveDB(t *testing.T) {
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

	// Two users, and for user 1 two repos; user 2 gets one. Distinct forge_project_id
	// per connection keeps the repos legal.
	u1, u2 := uuid.New(), uuid.New()
	conn1, conn2 := uuid.New(), uuid.New()
	r1, r1b, r2 := uuid.New(), uuid.New(), uuid.New()
	run1 := uuid.New()

	for _, u := range []uuid.UUID{u1, u2} {
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			u, fmt.Sprintf("mem-%s@e2e", u))
	}
	mkConn := func(id, user uuid.UUID) {
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, id, user, []byte{0x1})
	}
	mkConn(conn1, u1)
	mkConn(conn2, u2)
	mkRepo := func(id, conn uuid.UUID, pid int, path string) {
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/'||$4, 'main', true)`, id, conn, pid, path)
	}
	mkRepo(r1, conn1, 1, "u1/a")
	mkRepo(r1b, conn1, 2, "u1/b")
	mkRepo(r2, conn2, 3, "u2/a")
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'desc', 'running', 'issue')`, run1, u1, r1)

	// --- CRUD + provenance ---
	mem, err := q.InsertAgentMemory(ctx, store.InsertAgentMemoryParams{
		UserID: u1, RepoID: r1, RunID: pgtype.UUID{Bytes: run1, Valid: true},
		Title: "build flag", Body: "use -tags foo",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if mem.Title != "build flag" || mem.Body != "use -tags foo" || mem.UserID != u1 || mem.RepoID != r1 {
		t.Fatalf("inserted row mismatch: %+v", mem)
	}
	if n, _ := q.CountAgentMemoryForRun(ctx, pgtype.UUID{Bytes: run1, Valid: true}); n != 1 {
		t.Fatalf("per-run count = %d, want 1", n)
	}

	// --- Isolation: user 2's entry on their own repo is invisible to user 1 ---
	if _, err := q.InsertAgentMemory(ctx, store.InsertAgentMemoryParams{
		UserID: u2, RepoID: r2, Title: "u2 secret", Body: "b",
	}); err != nil {
		t.Fatalf("insert u2: %v", err)
	}
	// --- Cross-repo: user 1's OTHER repo is a separate scope ---
	if _, err := q.InsertAgentMemory(ctx, store.InsertAgentMemoryParams{
		UserID: u1, RepoID: r1b, Title: "other repo", Body: "b",
	}); err != nil {
		t.Fatalf("insert r1b: %v", err)
	}

	scoped, err := q.ListAgentMemoryForUserRepo(ctx, store.ListAgentMemoryForUserRepoParams{UserID: u1, RepoID: r1})
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Title != "build flag" {
		t.Fatalf("(u1,r1) scope = %+v, want exactly the build-flag entry (no cross-user/cross-repo bleed)", scoped)
	}

	forUser, err := q.ListAgentMemoryForUser(ctx, u1)
	if err != nil {
		t.Fatalf("list for user: %v", err)
	}
	if len(forUser) != 2 {
		t.Fatalf("user list = %d entries, want 2 (both of u1's repos, none of u2's)", len(forUser))
	}
	for _, m := range forUser {
		if m.RepoName == "" {
			t.Errorf("user list must carry repo_name (the JOIN), got empty for %s", m.ID)
		}
	}

	// --- Oldest-eviction keeps the newest N ---
	evUser, evConn, evRepo := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, evUser, fmt.Sprintf("ev-%s@e2e", evUser))
	mkConn(evConn, evUser)
	mkRepo(evRepo, evConn, 9, "ev/a")
	base := time.Now().Add(-time.Hour)
	const total, keep = 25, 20
	for i := 0; i < total; i++ {
		mustExec(ctx, t, pool,
			`INSERT INTO agent_memory (user_id, repo_id, title, body, created_at) VALUES ($1, $2, $3, 'b', $4)`,
			evUser, evRepo, fmt.Sprintf("m%02d", i), base.Add(time.Duration(i)*time.Second))
	}
	if err := q.EvictAgentMemoryOverCap(ctx, store.EvictAgentMemoryOverCapParams{UserID: evUser, RepoID: evRepo, KeepCount: keep}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	survivors, err := q.ListAgentMemoryForUserRepo(ctx, store.ListAgentMemoryForUserRepoParams{UserID: evUser, RepoID: evRepo})
	if err != nil {
		t.Fatalf("list survivors: %v", err)
	}
	if len(survivors) != keep {
		t.Fatalf("after evict = %d entries, want %d (oldest evicted)", len(survivors), keep)
	}
	// Newest first: survivors[0] is m24, and the oldest survivor is m05 (m00..m04 gone).
	if survivors[0].Title != "m24" || survivors[keep-1].Title != "m05" {
		t.Fatalf("eviction kept the wrong window: newest=%s oldest=%s, want m24..m05", survivors[0].Title, survivors[keep-1].Title)
	}

	// --- Owner-scoped delete: a foreign user removes nothing; the owner removes 1 ---
	n, err := q.DeleteAgentMemory(ctx, store.DeleteAgentMemoryParams{ID: mem.ID, UserID: u2})
	if err != nil {
		t.Fatalf("delete cross-user: %v", err)
	}
	if n != 0 {
		t.Fatalf("cross-user delete affected %d rows, want 0 (never a foreign delete)", n)
	}
	n, err = q.DeleteAgentMemory(ctx, store.DeleteAgentMemoryParams{ID: mem.ID, UserID: u1})
	if err != nil {
		t.Fatalf("delete owner: %v", err)
	}
	if n != 1 {
		t.Fatalf("owner delete affected %d rows, want 1", n)
	}
}
