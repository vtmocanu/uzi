package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestModelProviderDownGuardsLiveDB: rolling back 00469 refuses while any user has a Claude provider
// source selected, and rolling back 00471 refuses while any run is bound to a provider (the 00198 /
// 00466 convention: refuse rather than silently turn a bound run or a selected source into a vendor
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
	mustExec(ctx, t, pool, "INSERT INTO model_providers(id,owner_user_id,name,engine,current_revision) VALUES($1,$2,'p','claude',1)", provider, user)
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

	// 00469: a selected source blocks the Down.
	mustExec(ctx, t, pool, "UPDATE users SET claude_provider_id = $2 WHERE id = $1", user, provider)
	if err := run00("00469_users_claude_provider_source.sql"); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00469") {
		t.Fatalf("00469 Down with a selected source: %v, want a refusal", err)
	}
	mustExec(ctx, t, pool, "UPDATE users SET claude_provider_id = NULL WHERE id = $1", user)
	if err := run00("00469_users_claude_provider_source.sql"); err != nil {
		t.Fatalf("00469 Down with no selected source: %v", err)
	}

	// 00471: a bound run blocks the Down.
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,
 model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev)
 VALUES($1,$2,$3,$4,'issue',1,'t','d','queued',0,$5,1,'user_key',$6,0)`, run, user, repo, worker, provider, uuid.New())
	if err := run00("00471_runs_model_provider_binding.sql"); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00471") {
		t.Fatalf("00471 Down with a bound run: %v, want a refusal", err)
	}
	mustExec(ctx, t, pool, "DELETE FROM runs WHERE id = $1", run)
	if err := run00("00471_runs_model_provider_binding.sql"); err != nil {
		t.Fatalf("00471 Down with no bound run: %v", err)
	}
}
