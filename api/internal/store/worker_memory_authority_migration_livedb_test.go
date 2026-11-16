package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4395: 00465_worker_memory_authority_lost_at.sql adds workers.memory_authority_lost_at
// (NULL for every existing row) and its Down drops it. Uses a separate database: never rolls
// back the shared store-IT database.
func TestWorkerMemoryAuthorityMigrationLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	version := migrationVersionByName(t, "worker_memory_authority_lost_at")
	pool, dsn := codexJudgeMigrationDB(t, version-1)
	column := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		    WHERE table_name='workers' AND column_name='memory_authority_lost_at'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if column() != 0 {
		t.Fatal("column exists before the migration")
	}
	user, worker := uuid.New(), uuid.New()
	cjExec(t, pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", user, user.String()+"@example.test")
	cjExec(t, pool, `INSERT INTO workers (id,user_id,name,token_hash) VALUES ($1,$2,'w',$3)`, worker, user, []byte(worker.String()))
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	if column() != 1 {
		t.Fatal("Up did not add memory_authority_lost_at")
	}
	var at *time.Time
	if err := pool.QueryRow(ctx, `SELECT memory_authority_lost_at FROM workers WHERE id=$1`, worker).Scan(&at); err != nil || at != nil {
		t.Fatalf("existing worker row: lost_at=%v err=%v, want NULL", at, err)
	}
	cjExec(t, pool, `UPDATE workers SET memory_authority_lost_at=now() WHERE id=$1`, worker)
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatal(err)
	}
	if column() != 0 {
		t.Fatal("Down did not drop memory_authority_lost_at")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE id=$1`, worker).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down lost the worker row: n=%d err=%v", n, err)
	}
}
