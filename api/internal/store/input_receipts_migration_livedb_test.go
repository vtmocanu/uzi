package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1673: rolling 00254 back must hand an ACKed but unapplied input back to the legacy
// consume-on-read drain, which selects consumed_at IS NULL. Keeping its consumed_at would make it
// look delivered forever once applied_at is gone. Replays the migration's own Down/Up SQL.
func TestInputReceiptsMigrationDownLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, run := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("receipt-down-%s@e2e", user))
	mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status) VALUES ($1,$2,'chat','t','d','running')`, run, user)
	insert := func(consumed, applied bool) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,applied_at)
			 VALUES ($1,'follow_up','x',CASE WHEN $2 THEN now() END,CASE WHEN $3 THEN now() END) RETURNING id`,
			run, consumed, applied).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending, acked, applied := insert(false, false), insert(true, false), insert(true, true)

	var viewBefore string
	if err := pool.QueryRow(ctx, `SELECT pg_get_viewdef('codex_active_run_cooldown'::regclass)`).Scan(&viewBefore); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil {
			t.Errorf("rollback migration replay: %v", err)
			return
		}
		var viewAfter string
		if err := pool.QueryRow(ctx, `SELECT pg_get_viewdef('codex_active_run_cooldown'::regclass)`).Scan(&viewAfter); err != nil {
			t.Error(err)
		} else if viewAfter != viewBefore {
			t.Error("rollback did not restore codex_active_run_cooldown definition")
		}
		if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND applied_at IS NOT NULL`, applied); n != 1 {
			t.Error("rollback did not restore the applied input's applied_at")
		}
	}()
	exec := func(stmt string) {
		t.Helper()
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	// Undo the modern view dependency before replaying 00254; rollback restores it.
	exec(`DROP VIEW codex_active_run_cooldown`)
	for _, stmt := range migrationDownStatements(t, "00254_input_receipts.sql") {
		exec(stmt)
	}
	for id, wantConsumed := range map[int64]bool{pending: false, acked: false, applied: true} {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND consumed_at IS NOT NULL`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if (n == 1) != wantConsumed {
			t.Errorf("after Down input %d consumed=%v, want %v", id, n == 1, wantConsumed)
		}
	}
	for _, stmt := range migrationUpStatements(t, "00254_input_receipts.sql") {
		exec(stmt)
	}
}

// Replays 00284's own Down/Up SQL over seeded rows: Down drops both inclusion columns and keeps
// the rows, and Up re-adds them with no backfill (included_at NULL, inclusion_reported false).
func TestInputIncludedAtMigrationReplayLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, run := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')`, user, fmt.Sprintf("incl-mig-%s@e2e", user))
	mustExec(ctx, t, pool, `INSERT INTO runs (id,user_id,kind,issue_title,issue_description,status) VALUES ($1,$2,'chat','t','d','running')`, run, user)
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,applied_at,included_at,inclusion_reported)
		VALUES ($1,'follow_up','x',now(),now(),now(),true) RETURNING id`, run).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range migrationDownStatements(t, "00284_input_included_at.sql") {
		mustExec(ctx, t, pool, stmt)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='run_user_inputs' AND column_name IN ('included_at','inclusion_reported')`); n != 0 {
		t.Fatalf("Down left %d inclusion columns", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1`, id); n != 1 {
		t.Fatal("Down lost the seeded row")
	}
	for _, stmt := range migrationUpStatements(t, "00284_input_included_at.sql") {
		mustExec(ctx, t, pool, stmt)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND included_at IS NULL AND inclusion_reported = false AND applied_at IS NOT NULL`, id); n != 1 {
		t.Fatal("Up backfilled included_at or inclusion_reported, or lost applied_at")
	}
}
