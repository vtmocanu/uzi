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

// Issue #3767: migration 00424 widens runs_recovery_wait_cause_check with 'anthropic_spend_limit'
// (NOT VALID), 00425 validates it, and 00424's Down never relabels a live hold. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAnthropicSpendLimitMigrationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "spend_limit_migration_")
	if err := store.MigrateTo(ctx, dsn, 423); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	held, queued := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/spend-limit','https://example.com/spend-limit','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
	for i, id := range []uuid.UUID{held, queued} {
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,$4,'issue',$5,'spend limit','fixture','running',1)`, id, user, repo, worker, 3767+i)
	}
	rejected := func(id uuid.UUID, cause string) {
		t.Helper()
		_, err := pool.Exec(ctx, "UPDATE runs SET recovery_wait_cause=$2 WHERE id=$1", id, cause)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "runs_recovery_wait_cause_check" {
			t.Fatalf("cause %q: %v, want CHECK violation", cause, err)
		}
	}
	rejected(held, "anthropic_spend_limit") // absent before 00424

	if err := store.MigrateTo(ctx, dsn, 424); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_cause_check'").Scan(&valid); err != nil || valid {
		t.Fatalf("00424 must install the CHECK NOT VALID: valid=%v err=%v", valid, err)
	}
	rejected(held, "not_a_recovery_cause")
	mustExec(ctx, t, pool, "UPDATE runs SET status='recovery_wait', recovery_wait_cause='anthropic_spend_limit' WHERE id=$1", held)
	mustExec(ctx, t, pool, "UPDATE runs SET recovery_wait_cause='anthropic_spend_limit' WHERE id=$1", queued) // stale cause, as set-token leaves it

	b, err := os.ReadFile(filepath.Join("migrations", "00424_anthropic_spend_limit.sql"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "-- +goose Down")
	if i < 0 {
		t.Fatal("00424 has no Down block")
	}
	down := string(b)[i+len("-- +goose Down"):]

	// (i) a live hold makes the Down refuse, rows untouched.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, down); err == nil || !strings.Contains(err.Error(), "Resume, set-token or Cancel") {
		t.Fatalf("Down with a live hold: %v, want refusal", err)
	}
	_ = tx.Rollback(ctx)
	var cause string
	if err := pool.QueryRow(ctx, "SELECT recovery_wait_cause FROM runs WHERE id=$1", held).Scan(&cause); err != nil || cause != "anthropic_spend_limit" {
		t.Fatalf("refused Down changed the hold: %q %v", cause, err)
	}

	// (ii) with the hold gone (owner cancel leaves the cause stale), the Down NULLs every row
	// carrying it and narrows the CHECK. Run in a rolled-back tx.
	mustExec(ctx, t, pool, "UPDATE runs SET status='cancelled', finished_at=now() WHERE id=$1", held)
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down without a live hold: %v", err)
	}
	var left int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM runs WHERE recovery_wait_cause IS NOT NULL").Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows still carrying a cause after Down: %d %v", left, err)
	}
	if _, err := tx.Exec(ctx, "SAVEPOINT narrow"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE runs SET recovery_wait_cause='anthropic_spend_limit' WHERE id=$1", queued); err == nil {
		t.Fatal("Down CHECK still admits anthropic_spend_limit")
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT narrow"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)

	// (iii) 00425 validates the widened CHECK over the existing rows.
	if err := store.MigrateTo(ctx, dsn, 425); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_cause_check'").Scan(&valid); err != nil || !valid {
		t.Fatalf("00425 must validate the CHECK: valid=%v err=%v", valid, err)
	}
}
