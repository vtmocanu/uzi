package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M1 migration 427: Down keeps worker_hibernation_events, so Up must be
// re-entrant over it and must refuse a leftover whose shape drifted.

func hibernationMigrationDB(t *testing.T) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "hib_migration_")
	if err := store.MigrateTo(ctx, dsn, 426); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, dsn, pool
}

func hibWorker(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (owner, worker uuid.UUID) {
	t.Helper()
	owner, worker = uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,kind,hosted_size,template_declared) VALUES($1,$2,'h',$3,'hosted','m','base')", worker, owner, worker[:])
	return owner, worker
}

func TestWorkerHibernationMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	owner, worker := hibWorker(ctx, t, pool)
	if err := store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `INSERT INTO worker_hibernation_events(owner_id,worker_id,actor_id,actor_label,action,op_id,reason,occurred_at)
		VALUES($1,$2,$1,'operator','hibernate',gen_random_uuid(),'idle',now())`, owner, worker)

	// Down refuses while a worker is mid-hibernation, and changes nothing.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='preparing', hibernation_op_id=gen_random_uuid() WHERE id=$1`, worker)
	err := store.MigrateDownTo(ctx, dsn, 426)
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("Down with a non-none worker: %v", err)
	}
	if applied, _, _, vErr := store.SchemaVersionStatus(ctx, pool); vErr != nil || applied != 427 {
		t.Fatalf("refused Down moved the version: %d %v", applied, vErr)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM workers WHERE hibernation_state='preparing'"); n != 1 {
		t.Fatalf("refused Down changed the worker: %d", n)
	}

	// Down refuses while wake history exists.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='none', hibernation_op_id=NULL WHERE id=$1`, worker)
	run := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status) VALUES($1,$2,'chat','t','d','queued')`, run, owner)
	mustExec(ctx, t, pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, run, worker)
	err = store.MigrateDownTo(ctx, dsn, 426)
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("Down with wake history: %v", err)
	}
	mustExec(ctx, t, pool, `DELETE FROM hibernation_wakes`)

	// Clean Down keeps the audit table and restores the old function body.
	if err = store.MigrateDownTo(ctx, dsn, 426); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM worker_hibernation_events"); n != 1 {
		t.Fatalf("Down dropped audit history: %d", n)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM information_schema.columns WHERE table_name='workers' AND column_name LIKE '%hibernat%'"); n != 0 {
		t.Fatalf("Down left %d worker columns", n)
	}
	var def string
	if err = pool.QueryRow(ctx, "SELECT pg_get_functiondef('fn_worker_accepts_new_intake(workers)'::regprocedure)").Scan(&def); err != nil || strings.Contains(def, "hibernation") {
		t.Fatalf("function body after Down: %v %s", err, def)
	}

	// Up again keeps the row.
	if err = store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatalf("re-Up over the leftover events table: %v", err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM worker_hibernation_events"); n != 1 {
		t.Fatalf("re-Up lost audit history: %d", n)
	}
}

func TestWorkerHibernationMigrationLeftoverShapeLiveDB(t *testing.T) {
	for name, mutate := range map[string]string{
		"dropped column": `ALTER TABLE worker_hibernation_events DROP COLUMN op_id`,
		"nullable":       `ALTER TABLE worker_hibernation_events ALTER COLUMN op_id DROP NOT NULL`,
		"extra column":   `ALTER TABLE worker_hibernation_events ADD COLUMN extra text`,
		"wrong type":     `ALTER TABLE worker_hibernation_events ALTER COLUMN reason TYPE varchar(10)`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, dsn, pool := hibernationMigrationDB(t)
			if err := store.MigrateTo(ctx, dsn, 427); err != nil {
				t.Fatal(err)
			}
			if err := store.MigrateDownTo(ctx, dsn, 426); err != nil {
				t.Fatal(err)
			}
			mustExec(ctx, t, pool, mutate)
			err := store.MigrateTo(ctx, dsn, 427)
			if err == nil || !strings.Contains(err.Error(), "unexpected shape") {
				t.Fatalf("Up over a mutated leftover must fail on the shape check: %v", err)
			}
			if applied, _, _, vErr := store.SchemaVersionStatus(ctx, pool); vErr != nil || applied != 426 {
				t.Fatalf("failed Up left version %d: %v", applied, vErr)
			}
		})
	}
}

func TestWorkerHibernationMigrationRestoresActionCheckLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	if err := store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 426); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `ALTER TABLE worker_hibernation_events DROP CONSTRAINT worker_hibernation_events_action_check`)
	if err := store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatalf("Up must re-add a missing action check: %v", err)
	}
	owner := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO worker_hibernation_events(owner_id,worker_id,actor_id,actor_label,action,op_id,occurred_at)
		VALUES($1,$1,$1,'op','sleep',$1,now())`, owner)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "worker_hibernation_events_action_check" {
		t.Fatalf("action check not enforced: %v", err)
	}
}

func TestWorkerHibernationCheckConstraintsLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	_, worker := hibWorker(ctx, t, pool)
	if err := store.MigrateTo(ctx, dsn, 427); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND NOT convalidated AND (conname LIKE '%hibernat%' OR conname LIKE 'workers_wake%')"); n != 0 {
		t.Fatalf("%d hibernation constraints are not validated", n)
	}
	for name, set := range map[string]string{
		"state outside enum":   `hibernation_state='sleeping'`,
		"no op id":             `hibernation_state='preparing'`,
		"hibernated no stamp":  `hibernation_state='hibernated', hibernation_op_id=gen_random_uuid()`,
		"waking no request":    `hibernation_state='waking', hibernation_op_id=gen_random_uuid()`,
		"ephemeral":            `hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), ephemeral=true`,
		"external":             `hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), kind='external', hosted_size=NULL`,
		"overbound wake error": `wake_error=repeat('x',501)`,
		"overbound reason":     `hibernation_reason=repeat('x',501)`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, "UPDATE workers SET "+set+" WHERE id=$1", worker)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" {
				t.Fatalf("CHECK rejection: %v", err)
			}
		})
	}
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='hibernated', hibernation_op_id=gen_random_uuid(), hibernated_at=now(),
		wake_error=repeat('x',500), hibernation_reason=repeat('r',500) WHERE id=$1`, worker)
}
