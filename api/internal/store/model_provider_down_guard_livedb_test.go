package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestModelProviderDownGuardsLockBeforeTheCheck: each count-then-ALTER Down locks the table it
// guards, with a bounded lock wait, BEFORE the guard reads it. Without the lock a writer committing
// between the guard and the ALTER is silently destroyed (the 00198 refusal is only as good as the
// snapshot it reads). Text order, because the interleaving needs a live database (see
// TestModelProviderDownLockSerializesWithAWriterLiveDB).
func TestModelProviderDownGuardsLockBeforeTheCheck(t *testing.T) {
	// ALTER Downs take their final ACCESS EXCLUSIVE lock up front (no lock upgrade).
	type guardSpec struct{ table, mode string }
	// The shared Codex Downs lock three tables and read each guarded one in turn. The engine and
	// key-scope migrations predate users.codex_provider_scope_id, so they read no selection, though they still lock users.
	sharedCodexDownNoSelection := []guardSpec{{"model_providers", "ACCESS EXCLUSIVE"}, {"user_secrets", "ACCESS EXCLUSIVE"}}
	sharedCodexDown := append(append([]guardSpec{}, sharedCodexDownNoSelection...), guardSpec{"users", "ACCESS EXCLUSIVE"})
	specsByFile := map[string][]guardSpec{
		"00476_model_providers.sql":                  {{"model_providers", "SHARE ROW EXCLUSIVE"}},
		"00477_user_secrets_provider_key.sql":        {{"user_secrets", "ACCESS EXCLUSIVE"}},
		"00479_users_claude_provider_source.sql":     {{"users", "ACCESS EXCLUSIVE"}},
		"00481_runs_model_provider_binding.sql":      {{"runs", "ACCESS EXCLUSIVE"}},
		"00488_runs_provider_gateway_limit_type.sql": {{"runs", "ACCESS EXCLUSIVE"}},
		"00524_model_providers_shared_scope.sql":     {{"model_providers", "ACCESS EXCLUSIVE"}},
		"00527_user_secrets_provider_scope.sql":      {{"user_secrets", "ACCESS EXCLUSIVE"}},
		"00528_users_claude_provider_scope.sql":      {{"users", "ACCESS EXCLUSIVE"}},
	}
	// The six shared Codex migrations are draft-numbered and renumbered at merge, so each is resolved
	// by its stable name suffix (exactly one file) rather than by a literal number.
	for name, specs := range map[string][]guardSpec{
		"model_providers_shared_codex_engine":          sharedCodexDownNoSelection,
		"user_secrets_shared_codex_scope":              sharedCodexDownNoSelection,
		"users_codex_provider_scope":                   sharedCodexDown,
		"validate_model_providers_shared_codex_engine": sharedCodexDown,
		"validate_user_secrets_shared_codex_scope":     sharedCodexDown,
		"validate_users_codex_provider_scope":          sharedCodexDown,
	} {
		matches, err := filepath.Glob(filepath.Join("migrations", "*_"+name+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		// "*_<name>.sql" also matches a longer name ending in it (validate_<name>); keep the exact one.
		var exact []string
		for _, m := range matches {
			if _, rest, ok := strings.Cut(filepath.Base(m), "_"); ok && rest == name+".sql" {
				exact = append(exact, filepath.Base(m))
			}
		}
		if len(exact) != 1 {
			t.Fatalf("migration %s resolves to %v, want exactly one file", name, exact)
		}
		specsByFile[exact[0]] = specs
	}
	for file, specs := range specsByFile {
		b, err := os.ReadFile(filepath.Join("migrations", file)) //nolint:gosec // G304: a fixed name inside this package's migrations dir.
		if err != nil {
			t.Fatal(err)
		}
		_, down, ok := strings.Cut(string(b), "-- +goose Down")
		if !ok {
			t.Fatalf("%s has no Down block", file)
		}
		down = stripSQLLineComments(down)
		if strings.Contains(file, "shared_codex") || strings.Contains(file, "codex_provider_scope") {
			// The six shared Codex Downs lock all three tables, strictly in this order, before the first guard.
			first := strings.Index(down, "IF EXISTS")
			prev := -1
			for _, table := range []string{"model_providers", "user_secrets", "users"} {
				pos := strings.Index(down, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE;")
				if pos < 0 || pos <= prev || first < 0 || pos >= first {
					t.Errorf("%s Down: LOCK TABLE %s@%d (previous lock@%d, first IF EXISTS@%d); want all three present in order before the first guard", file, table, pos, prev, first)
				}
				prev = pos
			}
		}
		for _, spec := range specs {
			table := spec.table
			timeout := strings.Index(down, "SET LOCAL lock_timeout = '5s';")
			lock := strings.Index(down, "LOCK TABLE "+table+" IN "+spec.mode+" MODE;")
			guard := strings.Index(down, "IF EXISTS (SELECT 1 FROM "+table)
			if timeout < 0 || lock < 0 || guard < 0 || timeout >= lock || lock >= guard {
				t.Errorf("%s Down: lock_timeout@%d, LOCK TABLE %s@%d, IF EXISTS guard@%d; want all present in that order", file, timeout, table, lock, guard)
			}
		}
		if strings.Contains(down, "count(*)") {
			t.Errorf("%s Down still counts rows instead of IF EXISTS", file)
		}
	}
}

// TestUsersCodexScopeUpLocksBeforeAddColumn: the 00538 Up (resolved by name, so a renumber needs no
// edit) takes ACCESS EXCLUSIVE on model_providers and users before it adds the users column.
func TestUsersCodexScopeUpLocksBeforeAddColumn(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var file string
	for _, e := range entries {
		if _, rest, ok := strings.Cut(e.Name(), "_"); ok && rest == "users_codex_provider_scope.sql" {
			file = e.Name()
		}
	}
	if file == "" {
		t.Fatal("no migration named users_codex_provider_scope")
	}
	b, err := os.ReadFile(filepath.Join("migrations", file)) //nolint:gosec // G304: a name found in this package's migrations dir.
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(b), "-- +goose Down")
	up = stripSQLLineComments(up)
	lock := strings.Index(up, "LOCK TABLE model_providers, users IN ACCESS EXCLUSIVE MODE;")
	add := strings.Index(up, "ALTER TABLE users ADD COLUMN")
	if lock < 0 || add < 0 || lock >= add {
		t.Errorf("%s Up: LOCK TABLE model_providers, users@%d, ALTER TABLE users ADD COLUMN@%d; want both present, lock first", file, lock, add)
	}
}

// TestModelProviderDownLockSerializesWithAWriterLiveDB: the 00481 Down started while another session
// holds an uncommitted bound-run insert waits for it (the table lock), then sees the committed row
// and refuses. Without the lock the guard reads its snapshot, passes, and the column drop destroys
// the binding the writer just committed.
func TestModelProviderDownLockSerializesWithAWriterLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "model_provider_down_lock_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, provider, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO model_providers(id,owner_user_id,scope_owner_id,name,engine,current_revision) VALUES($1,$2,$2,'p','claude',1)", provider, user)
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/down','https://example.com/down','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])

	b, err := os.ReadFile(filepath.Join("migrations", "00481_runs_model_provider_binding.sql"))
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(b), "-- +goose Down")

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,
 model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev)
 VALUES($1,$2,$3,$4,'issue',1,'t','d','queued',0,$5,1,'user_key',$6,0)`, uuid.New(), user, repo, worker, provider, uuid.New()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, down)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the Down finished (%v) while a bound-run insert was uncommitted; it must wait for the table lock", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "refusing to roll back 00481") {
			t.Fatalf("Down after the writer committed: %v, want the refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Down never finished after the writer committed")
	}
}

// TestModelProviderDownGuardsLiveDB: rolling back 00479 refuses while any user has a Claude provider
// source selected, and rolling back 00481 refuses while any run is bound to a provider (the 00198 /
// 00476 convention: refuse rather than silently turn a bound run or a selected source into a vendor
// fallback). Once the rows are gone the Down runs. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres.
func TestModelProviderDownGuardsLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "model_provider_down_guard_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, provider, conn, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO model_providers(id,owner_user_id,scope_owner_id,name,engine,current_revision) VALUES($1,$2,$2,'p','claude',1)", provider, user)
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/down','https://example.com/down','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])

	down := func(file string) string {
		b, err := os.ReadFile(filepath.Join("migrations", file)) //nolint:gosec // G304: a fixed name inside this package's migrations dir.
		if err != nil {
			t.Fatal(err)
		}
		_, d, ok := strings.Cut(string(b), "-- +goose Down")
		if !ok {
			t.Fatalf("%s has no Down block", file)
		}
		return d
	}
	// run executes the Down in a transaction that is always rolled back.
	run00 := func(file string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, down(file))
		return err
	}

	// 00479: a selected source blocks the Down.
	mustExec(ctx, t, pool, "UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1", user, provider)
	if err := run00("00479_users_claude_provider_source.sql"); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00479") {
		t.Fatalf("00479 Down with a selected source: %v, want a refusal", err)
	}
	mustExec(ctx, t, pool, "UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1", user)
	if err := run00("00479_users_claude_provider_source.sql"); err != nil {
		t.Fatalf("00479 Down with no selected source: %v", err)
	}

	// 00488: a run still waiting (limit_wait) on a provider gateway limit blocks the Down (it would otherwise
	// be coerced to 'unknown' and stranded as an owner-wait the sweeper and Resume can no longer match).
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,rate_limit_type)
 VALUES($1,$2,$3,$4,'issue',2,'t','d','limit_wait',0,'provider_gateway_limit')`, run, user, repo, worker)
	if err := run00("00488_runs_provider_gateway_limit_type.sql"); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00488") {
		t.Fatalf("00488 Down with a limit_wait provider_gateway_limit run: %v, want a refusal", err)
	}
	// Once the run is terminal the type is only history: it does not block, and it is mapped to unknown.
	mustExec(ctx, t, pool, "UPDATE runs SET status = 'cancelled' WHERE id = $1", run)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, down("00488_runs_provider_gateway_limit_type.sql")); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("00488 Down with a terminal provider_gateway_limit run: %v, want it to pass", err)
	}
	var mapped string
	if err := tx.QueryRow(ctx, "SELECT rate_limit_type FROM runs WHERE id = $1", run).Scan(&mapped); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if mapped != "unknown" {
		t.Fatalf("00488 Down mapped a terminal provider_gateway_limit run to %q, want unknown", mapped)
	}
	mustExec(ctx, t, pool, "DELETE FROM runs WHERE id = $1", run)

	// 00481: a bound run blocks the Down.
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,
 model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev)
 VALUES($1,$2,$3,$4,'issue',1,'t','d','queued',0,$5,1,'user_key',$6,0)`, run, user, repo, worker, provider, uuid.New())
	if err := run00("00481_runs_model_provider_binding.sql"); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00481") {
		t.Fatalf("00481 Down with a bound run: %v, want a refusal", err)
	}
	mustExec(ctx, t, pool, "DELETE FROM runs WHERE id = $1", run)
	if err := run00("00481_runs_model_provider_binding.sql"); err != nil {
		t.Fatalf("00481 Down with no bound run: %v", err)
	}
}

// stripSQLLineComments drops every "--" line comment, so a commented-out statement cannot satisfy
// a position check on the executable SQL. The migrations under test carry no "--" inside literals.
func stripSQLLineComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, l := range lines {
		if c := strings.Index(l, "--"); c >= 0 {
			lines[i] = l[:c]
		}
	}
	return strings.Join(lines, "\n")
}
