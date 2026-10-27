package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestIdleStallMigrationLifecycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "idle_stall_migration_")
	if err := store.MigrateTo(ctx, dsn, 363); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo, w := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/idle','https://example.com/idle','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'idle migration',$3,'online')", w, user, w[:])
	old := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,recovery_wait_cause) VALUES($1,$2,$3,$4,1,'idle migration','fixture','recovery_wait','provider_outage')", old, user, repo, w)
	if err = store.MigrateTo(ctx, dsn, 364); err != nil {
		t.Fatal(err)
	}
	var valid bool
	for _, name := range []string{"runs_idle_stall_park_count_nonnegative", "runs_recovery_wait_cause_check"} {
		if err = pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname=$1", name).Scan(&valid); err != nil || valid {
			t.Fatalf("short Up validated %s: %v", name, err)
		}
	}
	var count int
	if err = pool.QueryRow(ctx, "SELECT idle_stall_park_count FROM runs WHERE id=$1", old).Scan(&count); err != nil || count != 0 {
		t.Fatalf("default %d %v", count, err)
	}
	if _, err = pool.Exec(ctx, "UPDATE runs SET idle_stall_park_count=-1 WHERE id=$1", old); err == nil {
		t.Fatal("negative accepted")
	}
	if err = store.MigrateTo(ctx, dsn, 365); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runs_idle_stall_park_count_nonnegative", "runs_recovery_wait_cause_check"} {
		if err = pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname=$1", name).Scan(&valid); err != nil || !valid {
			t.Fatalf("Validate %s: %v", name, err)
		}
	}
	// Exercise every preceding cause after the widened check.
	for i, cause := range []string{"forge_unreachable", "empty_turn", "provider_outage", "codex_account_unavailable", "vault_locked", "data_volume_full", "worker_requeue_exhausted", "worker_memory_pressure", "retained_recovery_exhausted"} {
		mustExec(ctx, t, pool, "INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,recovery_wait_cause) VALUES($1,$2,$3,$4,'idle migration','fixture','recovery_wait',$5)", uuid.New(), user, repo, i+10, cause)
	}
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	holds := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	beforeHolds := make([]string, len(ids))
	for i, id := range ids {
		status := "recovery_wait"
		if i == 2 {
			status = "queued"
		}
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,claim_generation,
   recovery_wait_cause,recovery_retry_not_before,idle_stall_park_count)
   VALUES($1,$2,$3,$4,$5,'idle migration','fixture',$6,1,'codex_idle_stall',CASE WHEN $7::boolean THEN now()+interval '1 hour' END,4)`, id, user, repo, w, 100+i, status, i != 1)
		mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,
   original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
   VALUES($1,$2,$3,$4,1,'open',$5,'idle-migration',$5,$4,true)`, holds[i], user, repo, id, w)
		if err = pool.QueryRow(ctx, "SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1", holds[i]).Scan(&beforeHolds[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err = store.MigrateDownTo(ctx, dsn, 363); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var status, origin, reason string
		var clean, finished bool
		if err = pool.QueryRow(ctx, `SELECT status,COALESCE(fail_origin,''),COALESCE(failure_reason,''),
   recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL,finished_at IS NOT NULL
   FROM runs WHERE id=$1`, id).Scan(&status, &origin, &reason, &clean, &finished); err != nil {
			t.Fatal(err)
		}
		if !clean {
			t.Fatal("Down retained new cause/retry")
		}
		if i < 2 && (status != "failed" || origin != "worker_lost" || !strings.Contains(reason, "00364") || !finished) {
			t.Fatalf("active Down %s %s %s %v", status, origin, reason, finished)
		}
		if i == 2 && (status != "queued" || finished) {
			t.Fatal("historical cause changed status")
		}
		var after string
		if err = pool.QueryRow(ctx, "SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1", holds[i]).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != beforeHolds[i] {
			t.Fatal("Down modified custody")
		}
	}
	var exists bool
	if err = pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='runs' AND column_name='idle_stall_park_count')").Scan(&exists); err != nil || exists {
		t.Fatalf("counter survived Down %v", err)
	}
	if _, err = pool.Exec(ctx, "UPDATE runs SET recovery_wait_cause='codex_idle_stall' WHERE id=$1", old); err == nil {
		t.Fatal("Down failed to restore old cause check")
	}
	if err = store.MigrateTo(ctx, dsn, 365); err != nil {
		t.Fatal(err)
	}
}
