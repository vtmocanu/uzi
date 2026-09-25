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

	for _, stmt := range migrationDownStatements(t, "00254_input_receipts.sql") {
		mustExec(ctx, t, pool, stmt)
	}
	for id, wantConsumed := range map[int64]bool{pending: false, acked: false, applied: true} {
		n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND consumed_at IS NOT NULL`, id)
		if (n == 1) != wantConsumed {
			t.Errorf("after Down input %d consumed=%v, want %v", id, n == 1, wantConsumed)
		}
	}
	for _, stmt := range migrationUpStatements(t, "00254_input_receipts.sql") {
		mustExec(ctx, t, pool, stmt)
	}
}
