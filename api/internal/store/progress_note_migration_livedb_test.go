package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestProgressNoteIndexMigrationRetryLiveDB proves the two PRD #2603 migrations on a FRESH
// disposable database: the users column step and the concurrent partial-index step are
// separate files, and re-running the index step over an INVALID index left behind by an
// interrupted CREATE INDEX CONCURRENTLY converges to a VALID index while the column stays
// present exactly once.
//
// The invalid index is produced the way a failed concurrent build really leaves one: a
// unique build over duplicate rows fails (23505) and Postgres keeps the index with
// indisvalid = false. A migration that used CREATE INDEX IF NOT EXISTS (no preceding drop)
// would skip over it and leave the invalid index in place, which is what this test catches.
func TestProgressNoteIndexMigrationRetryLiveDB(t *testing.T) {
	baseDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()

	// A fresh database on the same server: the shared database is already migrated to head by
	// other tests, and the retry scenario needs the index step NOT yet applied.
	admin, err := OpenPool(ctx, baseDSN)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(admin.Close) // registered first, so it runs after the DROP DATABASE cleanup below
	dbName := "cdr_progress_note_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create fresh database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`); err != nil {
			t.Errorf("drop fresh database %s: %v", dbName, err)
		}
	})
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + dbName
	dsn := u.String()

	// Find the two migration versions by name so a renumber at merge cannot stale this test.
	colVersion := migrationVersionByName(t, "users_now_summary_enabled")
	idxVersion := migrationVersionByName(t, "run_messages_progress_note_index")
	if idxVersion != colVersion+1 {
		t.Fatalf("the column and index steps must be adjacent migrations, got %d and %d", colVersion, idxVersion)
	}

	// Stand the database at the column step: the column exists, the index does not.
	if err := MigrateTo(ctx, dsn, colVersion); err != nil {
		t.Fatalf("migrate to column step: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	if got := countNowSummaryColumns(t, pool); got != 1 {
		t.Fatalf("users.now_summary_enabled columns after the column step = %d, want 1", got)
	}
	if exists, _ := progressNoteIndexState(t, pool); exists {
		t.Fatal("the index step must not have run yet")
	}

	// Leave an INVALID index of the migration's name behind.
	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("pn-mig-%s@example.com", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext) VALUES ($1, $2, 'github', 'https://example.com', 'bot', 1, $3)`, connID, userID, []byte{1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled) VALUES ($1, $2, 1, 'g/pn-mig', 'https://example.com/g/pn-mig', 'main', true)`, repoID, connID)
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'running')`, runID, userID, repoID)
	exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'progress_note', '{}'), ($1, 2, 'progress_note', '{}')`, runID)
	if _, err := pool.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY idx_run_messages_progress_note_seq ON run_messages (run_id) WHERE kind = 'progress_note'`); err == nil {
		t.Fatal("the seeded duplicate rows must make the unique concurrent build fail")
	}
	if exists, valid := progressNoteIndexState(t, pool); !exists || valid {
		t.Fatalf("setup: want an INVALID leftover index, got exists=%v valid=%v", exists, valid)
	}

	// Re-run the migrations to head: the index step must replace the invalid leftover.
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate to head over an invalid leftover index: %v", err)
	}
	exists, valid := progressNoteIndexState(t, pool)
	if !exists || !valid {
		t.Fatalf("after re-running the index step: exists=%v valid=%v, want a VALID index", exists, valid)
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_run_messages_progress_note_seq'`).Scan(&def); err != nil {
		t.Fatalf("read indexdef: %v", err)
	}
	for _, part := range []string{"(run_id, seq)", "kind = 'progress_note'"} {
		if !strings.Contains(def, part) {
			t.Fatalf("index definition %q lacks %q", def, part)
		}
	}
	if strings.Contains(def, "UNIQUE") {
		t.Fatalf("index definition %q is still the unique leftover", def)
	}
	if got := countNowSummaryColumns(t, pool); got != 1 {
		t.Fatalf("users.now_summary_enabled columns after re-running = %d, want exactly 1", got)
	}
}

// migrationVersionByName returns the goose version of the embedded migration whose file name
// ends in "<name>.sql".
func migrationVersionByName(t *testing.T, name string) int64 {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, "_"+name+".sql") {
			var v int64
			if _, err := fmt.Sscanf(n, "%d_", &v); err != nil {
				t.Fatalf("parse version of %s: %v", n, err)
			}
			return v
		}
	}
	t.Fatalf("no embedded migration named *_%s.sql", name)
	return 0
}

func countNowSummaryColumns(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'now_summary_enabled'`).Scan(&n); err != nil {
		t.Fatalf("count users.now_summary_enabled: %v", err)
	}
	return n
}

// progressNoteIndexState reports whether the partial index exists and whether Postgres
// considers it valid (pg_index.indisvalid).
func progressNoteIndexState(t *testing.T, pool *pgxpool.Pool) (exists, valid bool) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = 'idx_run_messages_progress_note_seq'`)
	if err != nil {
		t.Fatalf("read pg_index: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		exists = true
		if err := rows.Scan(&valid); err != nil {
			t.Fatalf("scan indisvalid: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_index rows: %v", err)
	}
	return exists, valid
}
