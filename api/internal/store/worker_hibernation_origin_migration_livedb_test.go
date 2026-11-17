package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4395: the op-bound origin markers (00467 adds the columns and NOT VALID CHECKs, 00468
// validates them). Uses a separate database: never rolls back the shared store-IT database.
func TestWorkerHibernationOriginMigrationLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	add := migrationVersionByName(t, "worker_hibernation_origin")
	validate := migrationVersionByName(t, "validate_worker_hibernation_origin")
	if validate != add+1 {
		t.Fatalf("validate migration %d must directly follow %d", validate, add)
	}
	pool, dsn := codexJudgeMigrationDB(t, add-1)
	constraints := []string{
		"workers_hibernation_admit_origin_domain", "workers_hibernation_wake_origin_domain", "workers_hibernation_origin_coherent",
	}
	columns := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='workers'
		   AND column_name IN ('hibernation_origin_op_id','hibernation_admit_origin','hibernation_wake_origin','last_manual_wake_settled_at')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	validated := func(name string) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conname=$1 AND conrelid='workers'::regclass`, name).Scan(&ok); err != nil {
			t.Fatalf("constraint %s: %v", name, err)
		}
		return ok
	}
	if columns() != 0 {
		t.Fatal("columns exist before the migration")
	}

	// A row from before the migration reads as legacy: every new column NULL.
	user, worker := uuid.New(), uuid.New()
	cjExec(t, pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", user, user.String()+"@example.test")
	cjExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared)
	   VALUES ($1,$2,$3,$4,'online','hosted','m','base')`,
		worker, user, "w-"+worker.String(), worker[:])

	if err := MigrateTo(ctx, dsn, add); err != nil {
		t.Fatal(err)
	}
	if columns() != 4 {
		t.Fatalf("columns after 00467 = %d, want 4", columns())
	}
	for _, c := range constraints {
		if validated(c) {
			t.Fatalf("%s is validated before the validate migration", c)
		}
	}
	var legacy int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE id=$1 AND hibernation_origin_op_id IS NULL
	   AND hibernation_admit_origin IS NULL AND hibernation_wake_origin IS NULL AND last_manual_wake_settled_at IS NULL`, worker).Scan(&legacy); err != nil || legacy != 1 {
		t.Fatalf("legacy row not NULL-backed: %d, %v", legacy, err)
	}

	if err := MigrateTo(ctx, dsn, validate); err != nil {
		t.Fatal(err)
	}
	for _, c := range constraints {
		if !validated(c) {
			t.Fatalf("%s not validated after 00468", c)
		}
	}

	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	op := uuid.New()
	for _, ok := range []struct{ admit, wake string }{{"manual", ""}, {"auto_long", ""}, {"auto_surplus", ""}, {"", "manual"}, {"", "demand"}, {"", "system"}, {"", "warm"}} {
		if err := exec(`UPDATE workers SET hibernation_origin_op_id=$2, hibernation_admit_origin=NULLIF($3,''), hibernation_wake_origin=NULLIF($4,'') WHERE id=$1`,
			worker, op, ok.admit, ok.wake); err != nil {
			t.Fatalf("valid origins %+v rejected: %v", ok, err)
		}
	}
	// Domain violations.
	if err := exec(`UPDATE workers SET hibernation_origin_op_id=$2, hibernation_admit_origin='warm' WHERE id=$1`, worker, op); err == nil {
		t.Fatal("an admit origin outside the domain was accepted")
	}
	if err := exec(`UPDATE workers SET hibernation_origin_op_id=$2, hibernation_wake_origin='auto_long' WHERE id=$1`, worker, op); err == nil {
		t.Fatal("a wake origin outside the domain was accepted")
	}
	// Coherence violations: an origin without its op id.
	if err := exec(`UPDATE workers SET hibernation_origin_op_id=NULL, hibernation_admit_origin='manual', hibernation_wake_origin=NULL WHERE id=$1`, worker); err == nil {
		t.Fatal("an admit origin without an origin op id was accepted")
	}
	if err := exec(`UPDATE workers SET hibernation_origin_op_id=NULL, hibernation_admit_origin=NULL, hibernation_wake_origin='warm' WHERE id=$1`, worker); err == nil {
		t.Fatal("a wake origin without an origin op id was accepted")
	}
	// The grace anchor needs no op id.
	if err := exec(`UPDATE workers SET hibernation_origin_op_id=NULL, hibernation_admit_origin=NULL, hibernation_wake_origin=NULL, last_manual_wake_settled_at=now() WHERE id=$1`, worker); err != nil {
		t.Fatalf("a bare grace anchor was rejected: %v", err)
	}

	if err := MigrateDownTo(ctx, dsn, add-1); err != nil {
		t.Fatal(err)
	}
	if columns() != 0 {
		t.Fatalf("columns after Down = %d, want 0", columns())
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid='workers'::regclass AND conname LIKE 'workers_hibernation_%origin%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("constraints after Down = %d, %v", n, err)
	}
	// Up again: both migrations are re-appliable.
	if err := MigrateTo(ctx, dsn, validate); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
}
