package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestBaseDivergedMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "base_diverged_migration_")
	if err := store.MigrateTo(ctx, dsn, 344); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, conn, repo, old, newRun := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/base','https://example.com/base','main')`, repo, conn)
	insert := func(id uuid.UUID, origin string) {
		t.Helper()
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,fail_origin) VALUES($1,$2,$3,1,'t','d','failed',$4)`, id, owner, repo, origin)
	}
	insert(old, "provider_policy_refusal")
	for _, origin := range workersvc.AllFailOrigins() {
		if origin != "base_diverged" && origin != "provider_policy_refusal" {
			insert(uuid.New(), origin)
		}
	}
	rows := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(r) ORDER BY id)::text FROM runs r`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := rows()
	if err := store.MigrateTo(ctx, dsn, 345); err != nil {
		t.Fatal(err)
	}
	if rows() != before {
		t.Fatal("Up rewrote historical rows")
	}
	insert(newRun, "base_diverged")
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
   'rows',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM runs r),
   'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_db_version v),
   'constraint',(SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_fail_origin_check'))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	state := snapshot()
	err = store.MigrateDownTo(ctx, dsn, 344)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_fail_origin_check" {
		t.Fatalf("Down incompatible error: %v", err)
	}
	if snapshot() != state {
		t.Fatal("refused Down changed rows, constraint, or Goose history")
	}
	mustExec(ctx, t, pool, "DELETE FROM runs WHERE id=$1", newRun)
	before = rows()
	if err := store.MigrateDownTo(ctx, dsn, 344); err != nil {
		t.Fatal(err)
	}
	if rows() != before {
		t.Fatal("compatible Down rewrote historical rows")
	}
	_, err = pool.Exec(ctx, `UPDATE runs SET fail_origin='base_diverged' WHERE id=$1`, old)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("Down did not restore constraint: %v", err)
	}
}
