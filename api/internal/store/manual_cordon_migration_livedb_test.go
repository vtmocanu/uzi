package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only the isolated database is migrated backwards; the harness marker is checked
// on the parent database before creating it.
func manualMigrationDB(t *testing.T) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via store-IT runner")
	}
	ctx := context.Background()
	parent, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var marked bool
	err = parent.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM uzi_test_harness.database_marker WHERE marker='recovery-reset-v1')").Scan(&marked)
	parent.Close()
	if err != nil || !marked {
		t.Fatalf("test database marker required: marked=%v err=%v", marked, err)
	}
	ctx, isolated := standIsolatedDB(t, "manual_migration_")
	if err := store.MigrateTo(ctx, isolated, 348); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, isolated)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, isolated, pool
}

func TestManualCordonMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn, pool := manualMigrationDB(t)
	owner, worker := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'migration',$3)", worker, owner, worker[:])
	if err := store.MigrateTo(ctx, dsn, 349); err != nil {
		t.Fatal(err)
	}
	validated := func(want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='workers'::regclass AND conname='workers_manual_cordon_coherent'").Scan(&got); err != nil || got != want {
			t.Fatalf("convalidated=%v want=%v err=%v", got, want, err)
		}
	}
	validated(false)
	for name, assignment := range map[string]string{
		"timestamp-only":   "manual_cordoned_at=now()",
		"actor-only":       "manual_cordoned_by=$2",
		"label-only":       "manual_cordoned_by_label='operator'",
		"reason-only":      "manual_cordon_reason='rotation'",
		"ephemeral":        "ephemeral=true,manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label='operator'",
		"empty-label":      "manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label=''",
		"overbound-label":  "manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label=repeat('x',201)",
		"empty-reason":     "manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label='operator',manual_cordon_reason=''",
		"overbound-reason": "manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label='operator',manual_cordon_reason=repeat('x',501)",
	} {
		t.Run(name, func(t *testing.T) {
			args := []any{worker}
			if strings.Contains(assignment, "$2") {
				args = append(args, owner)
			}
			_, err := pool.Exec(ctx, "UPDATE workers SET "+assignment+" WHERE id=$1", args...)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "workers_manual_cordon_coherent" {
				t.Fatalf("CHECK rejection: %v", err)
			}
		})
	}
	// Boundary values are accepted and validation preserves them.
	mustExec(ctx, t, pool, "UPDATE workers SET manual_cordoned_at=now(),manual_cordoned_by=$2,manual_cordoned_by_label=repeat('x',200),manual_cordon_reason=repeat('r',500) WHERE id=$1", worker, owner)
	manualFields := func() string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, "SELECT jsonb_build_array(manual_cordoned_at,manual_cordoned_by,manual_cordoned_by_label,manual_cordon_reason)::text FROM workers WHERE id=$1", worker).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	beforeValidation := manualFields()
	if err := store.MigrateTo(ctx, dsn, 350); err != nil {
		t.Fatal(err)
	}
	validated(true)
	if after := manualFields(); after != beforeValidation {
		t.Fatalf("constraint validation changed manual fields: before=%s after=%s", beforeValidation, after)
	}
	if err := store.MigrateTo(ctx, dsn, 351); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 349); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, "SELECT jsonb_build_object('worker',(SELECT to_jsonb(w) FROM workers w WHERE id=$1),'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_db_version v))::text", worker).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, historyOnly := range []bool{false, true} {
		if historyOnly {
			mustExec(ctx, t, pool, "INSERT INTO worker_cordon_events(owner_id,worker_id,actor_id,actor_label,action,occurred_at,reason) VALUES($1,$2,$1,'snapshot','cordon',now(),'rotation')", owner, worker)
			mustExec(ctx, t, pool, "UPDATE workers SET manual_cordoned_at=NULL,manual_cordoned_by=NULL,manual_cordoned_by_label=NULL,manual_cordon_reason=NULL WHERE id=$1", worker)
		}
		before := snapshot()
		err := store.MigrateDownTo(ctx, dsn, 348)
		if err == nil || !strings.Contains(err.Error(), "manual cordon intent or audit history exists; rollback refused") {
			t.Fatalf("guarded Down historyOnly=%v: %v", historyOnly, err)
		}
		if snapshot() != before {
			t.Fatal("refused Down changed worker or migration history")
		}
	}
	mustExec(ctx, t, pool, "DELETE FROM worker_cordon_events")
	if err := store.MigrateDownTo(ctx, dsn, 348); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
}

func TestManualCordonMigrationLockRetryLiveDB(t *testing.T) {
	ctx, dsn, pool := manualMigrationDB(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "LOCK TABLE workers IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = store.Migrate(ctx, dsn)
	elapsed := time.Since(started)
	t.Logf("startup migration blocked lock duration=%s", elapsed)
	if err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("expected actual lock timeout: %v", err)
	}
	if elapsed < 5*time.Second {
		t.Fatalf("lock timeout returned before 5s: %s", elapsed)
	}
	applied, _, _, err := store.SchemaVersionStatus(ctx, pool)
	if err != nil || applied != 348 {
		t.Fatalf("failed migration version=%d err=%v", applied, err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='workers' AND column_name LIKE 'manual_cordon%'"); n != 0 {
		t.Fatalf("failed migration left %d columns", n)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("startup retry: %v", err)
	}
	applied, head, atHead, err := store.SchemaVersionStatus(ctx, pool)
	if err != nil || !atHead || applied != head {
		t.Fatalf("startup retry version=%d head=%d atHead=%v err=%v", applied, head, atHead, err)
	}
	// This measures lock acquisition only, not validation-scan duration.
}
