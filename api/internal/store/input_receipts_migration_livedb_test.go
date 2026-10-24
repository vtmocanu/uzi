package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1673: rolling 00254 back must hand an ACKed but unapplied input back to the legacy
// consume-on-read drain, which selects consumed_at IS NULL. Keeping its consumed_at would make it
// look delivered forever once applied_at is gone. Uses an isolated database and Goose
// version transitions 254 -> 253 -> 254.
func TestInputReceiptsMigrationDownLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "receipt_down_")
	if err := store.MigrateTo(ctx, dsn, 254); err != nil {
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

	if err := store.MigrateDownTo(ctx, dsn, 253); err != nil {
		t.Fatal(err)
	}
	for id, wantConsumed := range map[int64]bool{pending: false, acked: false, applied: true} {
		n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND consumed_at IS NOT NULL`, id)
		if (n == 1) != wantConsumed {
			t.Errorf("after Down input %d consumed=%v, want %v", id, n == 1, wantConsumed)
		}
	}
	if err := store.MigrateTo(ctx, dsn, 254); err != nil {
		t.Fatal(err)
	}
}

// Uses an isolated database and Goose version transitions 284 -> 283 -> 284 over seeded
// rows: Down drops both inclusion columns and keeps the rows, and Up re-adds them with no
// backfill (included_at NULL, inclusion_reported false).
func TestInputIncludedAtMigrationReplayLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "included_replay_")
	if err := store.MigrateTo(ctx, dsn, 284); err != nil {
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
	if err := store.MigrateDownTo(ctx, dsn, 283); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM information_schema.columns WHERE table_name='run_user_inputs' AND column_name IN ('included_at','inclusion_reported')`); n != 0 {
		t.Fatalf("Down left %d inclusion columns", n)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1`, id); n != 1 {
		t.Fatal("Down lost the seeded row")
	}
	if err := store.MigrateTo(ctx, dsn, 284); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, `SELECT count(*) FROM run_user_inputs WHERE id=$1 AND included_at IS NULL AND inclusion_reported = false AND applied_at IS NOT NULL`, id); n != 1 {
		t.Fatal("Up backfilled included_at or inclusion_reported, or lost applied_at")
	}
}
