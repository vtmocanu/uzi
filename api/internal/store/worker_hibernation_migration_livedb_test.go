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

// PRD #4205 M1 migrations 435/436 (438 validates 435's CHECKs): Down keeps worker_hibernation_events, so Up must be
// re-entrant over it and must refuse a leftover whose shape drifted.

func hibernationMigrationDB(t *testing.T) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	ctx, dsn := standIsolatedDB(t, "hib_migration_")
	if err := store.MigrateTo(ctx, dsn, 434); err != nil {
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
	if err := store.MigrateTo(ctx, dsn, 436); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `INSERT INTO worker_hibernation_events(owner_id,worker_id,actor_id,actor_label,action,op_id,reason,occurred_at)
		VALUES($1,$2,$1,'operator','hibernate',gen_random_uuid(),'idle',now())`, owner, worker)

	// Down refuses while a worker is mid-hibernation, and changes nothing.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='preparing', hibernation_op_id=gen_random_uuid() WHERE id=$1`, worker)
	err := store.MigrateDownTo(ctx, dsn, 434)
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("Down with a non-none worker: %v", err)
	}
	if applied, _, _, vErr := store.SchemaVersionStatus(ctx, pool); vErr != nil || applied != 435 {
		// 436 (wake history) rolled back cleanly; 435 refused and stayed applied.
		t.Fatalf("refused Down left version %d: %v", applied, vErr)
	}
	if err = store.MigrateTo(ctx, dsn, 436); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM workers WHERE hibernation_state='preparing'"); n != 1 {
		t.Fatalf("refused Down changed the worker: %d", n)
	}

	// Down of 436 does not refuse on wake rows: they are transient demand memory, and refusing
	// would stall a downgrade behind an idle queue. Down of 435 (above) still refuses while a worker
	// is not awake.
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='none', hibernation_op_id=NULL WHERE id=$1`, worker)
	run := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,kind,issue_title,issue_description,status) VALUES($1,$2,'chat','t','d','queued')`, run, owner)
	mustExec(ctx, t, pool, `INSERT INTO hibernation_wakes(run_id,worker_id,first_woken_at) VALUES($1,$2,now())`, run, worker)
	if err = store.MigrateDownTo(ctx, dsn, 435); err != nil {
		t.Fatalf("Down of 436 with wake rows: %v", err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM information_schema.tables WHERE table_name='hibernation_wakes'"); n != 0 {
		t.Fatalf("Down of 436 left the wake table: %d", n)
	}

	// Clean Down keeps the audit table and restores the old function body.
	if err = store.MigrateDownTo(ctx, dsn, 434); err != nil {
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
	if err = store.MigrateTo(ctx, dsn, 436); err != nil {
		t.Fatalf("re-Up over the leftover events table: %v", err)
	}
	if n := scalarInt(ctx, t, pool, "SELECT count(*) FROM worker_hibernation_events"); n != 1 {
		t.Fatalf("re-Up lost audit history: %d", n)
	}
}

func TestWorkerHibernationMigrationLeftoverShapeLiveDB(t *testing.T) {
	const tbl = "worker_hibernation_events"
	for name, tc := range map[string]struct{ mutate, want string }{
		"dropped column": {`ALTER TABLE ` + tbl + ` DROP COLUMN op_id`, "unexpected shape"},
		"nullable":       {`ALTER TABLE ` + tbl + ` ALTER COLUMN op_id DROP NOT NULL`, "unexpected shape"},
		"extra column":   {`ALTER TABLE ` + tbl + ` ADD COLUMN extra text`, "unexpected shape"},
		"wrong type":     {`ALTER TABLE ` + tbl + ` ALTER COLUMN reason TYPE varchar(10)`, "unexpected shape"},
		"widened action check": {`ALTER TABLE ` + tbl + ` DROP CONSTRAINT ` + tbl + `_action_check;
			ALTER TABLE ` + tbl + ` ADD CONSTRAINT ` + tbl + `_action_check CHECK (action IN ('hibernate','wake','sleep'))`, "unexpected constraints"},
		"loosened actor bound": {`ALTER TABLE ` + tbl + ` DROP CONSTRAINT ` + tbl + `_actor_label_check;
			ALTER TABLE ` + tbl + ` ADD CONSTRAINT ` + tbl + `_actor_label_check CHECK (char_length(actor_label) >= 1)`, "unexpected constraints"},
		"extra check": {`ALTER TABLE ` + tbl + ` ADD CONSTRAINT extra_check CHECK (true)`, "unexpected constraints"},
		"redefined index": {`DROP INDEX ` + tbl + `_worker_time_idx;
			CREATE INDEX ` + tbl + `_worker_time_idx ON ` + tbl + ` (worker_id)`, "is missing or wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, dsn, pool := hibernationMigrationDB(t)
			if err := store.MigrateTo(ctx, dsn, 435); err != nil {
				t.Fatal(err)
			}
			if err := store.MigrateDownTo(ctx, dsn, 434); err != nil {
				t.Fatal(err)
			}
			mustExec(ctx, t, pool, tc.mutate)
			err := store.MigrateTo(ctx, dsn, 435)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Up over a mutated leftover must fail with %q: %v", tc.want, err)
			}
			if applied, _, _, vErr := store.SchemaVersionStatus(ctx, pool); vErr != nil || applied != 434 {
				t.Fatalf("failed Up left version %d: %v", applied, vErr)
			}
		})
	}
}

// A bound CHECK that is absent from the leftover table is restored by Up (unlike
// one present with a different definition, which Up refuses).
func TestWorkerHibernationMigrationRestoresBoundChecksLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	if err := store.MigrateTo(ctx, dsn, 435); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 434); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `ALTER TABLE worker_hibernation_events
		DROP CONSTRAINT worker_hibernation_events_actor_label_check,
		DROP CONSTRAINT worker_hibernation_events_reason_check`)
	if err := store.MigrateTo(ctx, dsn, 435); err != nil {
		t.Fatalf("Up must re-add missing bound checks: %v", err)
	}
	for name, tc := range map[string]struct{ label, reason, con string }{
		"empty actor label": {"", "r", "worker_hibernation_events_actor_label_check"},
		"empty reason":      {"op", "", "worker_hibernation_events_reason_check"},
	} {
		t.Run(name, func(t *testing.T) {
			id := uuid.New()
			_, err := pool.Exec(ctx, `INSERT INTO worker_hibernation_events(owner_id,worker_id,actor_id,actor_label,action,op_id,reason,occurred_at)
				VALUES($1,$1,$1,$2,'wake',$1,$3,now())`, id, tc.label, tc.reason)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.ConstraintName != tc.con {
				t.Fatalf("%s not enforced: %v", tc.con, err)
			}
		})
	}
}

func TestWorkerHibernationMigrationRestoresActionCheckLiveDB(t *testing.T) {
	ctx, dsn, pool := hibernationMigrationDB(t)
	if err := store.MigrateTo(ctx, dsn, 435); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDownTo(ctx, dsn, 434); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, `ALTER TABLE worker_hibernation_events DROP CONSTRAINT worker_hibernation_events_action_check`)
	if err := store.MigrateTo(ctx, dsn, 435); err != nil {
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
	const unvalidated = "SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND NOT convalidated AND (conname LIKE '%hibernat%' OR conname LIKE 'workers_wake%')"
	if err := store.MigrateTo(ctx, dsn, 435); err != nil {
		t.Fatal(err)
	}
	// 435 installs the five CHECKs NOT VALID; 438 validates them.
	if n := scalarInt(ctx, t, pool, unvalidated); n != 5 {
		t.Fatalf("after 435 want 5 unvalidated hibernation constraints, got %d", n)
	}
	if err := store.MigrateTo(ctx, dsn, 438); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(ctx, t, pool, unvalidated); n != 0 {
		t.Fatalf("%d hibernation constraints are not validated", n)
	}
	for name, tc := range map[string]struct{ set, con string }{
		"state outside enum":   {`hibernation_state='sleeping', hibernation_op_id=gen_random_uuid()`, "workers_hibernation_state_check"},
		"no op id":             {`hibernation_state='preparing'`, "workers_hibernation_coherent"},
		"hibernated no stamp":  {`hibernation_state='hibernated', hibernation_op_id=gen_random_uuid()`, "workers_hibernated_at_set"},
		"waking no request":    {`hibernation_state='waking', hibernation_op_id=gen_random_uuid()`, "workers_wake_requested_at_set"},
		"ephemeral":            {`hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), ephemeral=true`, "workers_hibernation_coherent"},
		"external":             {`hibernation_state='preparing', hibernation_op_id=gen_random_uuid(), kind='external', hosted_size=NULL`, "workers_hibernation_coherent"},
		"overbound wake error": {`wake_error=repeat('x',501)`, "workers_hibernation_text_bounds"},
		"overbound reason":     {`hibernation_reason=repeat('x',501)`, "workers_hibernation_text_bounds"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, "UPDATE workers SET "+tc.set+" WHERE id=$1", worker)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != tc.con {
				t.Fatalf("CHECK rejection by %s: %v", tc.con, err)
			}
		})
	}
	mustExec(ctx, t, pool, `UPDATE workers SET hibernation_state='hibernated', hibernation_op_id=gen_random_uuid(), hibernated_at=now(),
		wake_error=repeat('x',500), hibernation_reason=repeat('r',500) WHERE id=$1`, worker)
}
