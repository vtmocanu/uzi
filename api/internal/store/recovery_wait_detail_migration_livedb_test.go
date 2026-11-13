package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4177: migration 00433 adds runs.recovery_wait_detail with a closed-set CHECK installed
// NOT VALID, 00434 validates it, and 00433's Down refuses while a run is still parked under an
// active secret remediation. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRecoveryWaitDetailMigrationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "recovery_wait_detail_migration_")
	if err := store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, conn, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/detail','https://example.com/detail','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,$4,'issue',4177,'detail','fixture','running',1)`, run, user, repo, worker)

	if err := store.MigrateTo(ctx, dsn, 428); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_detail_check'").Scan(&valid); err != nil || valid {
		t.Fatalf("00433 must install the CHECK NOT VALID: valid=%v err=%v", valid, err)
	}
	_, err = pool.Exec(ctx, "UPDATE runs SET recovery_wait_detail='not_in_the_set' WHERE id=$1", run)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "runs_recovery_wait_detail_check" {
		t.Fatalf("non-enum detail: %v, want CHECK violation", err)
	}
	mustExec(ctx, t, pool, "UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_recovery_adoption_failed', recovery_wait_detail='secret_remediation_active' WHERE id=$1", run)

	b, err := os.ReadFile(filepath.Join("migrations", "00433_recovery_wait_detail.sql"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "-- +goose Down")
	if i < 0 {
		t.Fatal("00433 has no Down block")
	}
	down := string(b)[i+len("-- +goose Down"):]

	// A live active-remediation park makes the Down refuse, the column untouched.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "refusing to roll back 00433") {
		t.Fatalf("Down with a live active park: %v, want refusal", err)
	}
	_ = tx.Rollback(ctx)
	var detail string
	if err := pool.QueryRow(ctx, "SELECT recovery_wait_detail FROM runs WHERE id=$1", run).Scan(&detail); err != nil || detail != "secret_remediation_active" {
		t.Fatalf("refused Down changed the row: %q %v", detail, err)
	}

	// Once the run is no longer parked, the Down drops the column and the CHECK (rolled back).
	mustExec(ctx, t, pool, "UPDATE runs SET status='cancelled', finished_at=now() WHERE id=$1", run)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down without a live active park: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name='runs' AND column_name='recovery_wait_detail'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("Down left the column: %d %v", n, err)
	}
	_ = tx.Rollback(ctx)

	if err := store.MigrateTo(ctx, dsn, 429); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_detail_check'").Scan(&valid); err != nil || !valid {
		t.Fatalf("00434 must validate the CHECK: valid=%v err=%v", valid, err)
	}
}
