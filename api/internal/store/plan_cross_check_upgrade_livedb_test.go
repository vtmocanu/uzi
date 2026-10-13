package store_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This database begins at the default branch's head, with real pre-upgrade data.
// The shared store-IT database is already at head and cannot prove this upgrade.
func TestPlanCrossCheckDefaultBranchUpgradeLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store-IT runner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	name := "cross_check_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	admin.Close()
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		admin, err := store.OpenPool(cleanupCtx, dsn)
		if err != nil {
			t.Errorf("cleanup admin: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("cleanup database: %v", err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	upgradeDSN := u.String()
	if err := store.MigrateTo(ctx, upgradeDSN, 298); err != nil {
		t.Fatalf("MigrateTo(298): %v", err)
	}
	pool, err = store.OpenPool(ctx, upgradeDSN)
	if err != nil {
		t.Fatal(err)
	}
	assertSQL := func(label, query string, args ...any) {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, query, args...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if !ok {
			t.Fatalf("%s: assertion false", label)
		}
	}
	assertSQL("default version", "SELECT max(version_id) = 298 FROM goose_db_version WHERE is_applied")
	assertSQL("cross-check table absent", "SELECT to_regclass('public.cross_checks') IS NULL")
	assertSQL("cross-check function absent", "SELECT to_regprocedure('settle_exited_plan_cross_check()') IS NULL")
	assertSQL("cross-check columns absent", `SELECT NOT EXISTS (
		SELECT 1 FROM information_schema.columns WHERE table_schema='public'
		AND column_name IN ('plan_cross_check_enabled','plan_cross_check_required','plan_cross_check_gate_reason'))`)
	defaultFeatures := `SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='workers' AND column_name='maintenance_fenced')
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='workers' AND column_name='dind_meter_epoch')
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='ephemeral_docker_enabled')
		AND EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='recovery_custody_holds' AND column_name='terminal_record_rejection')
		AND to_regprocedure('fn_ephemeral_docker_preference_applies(boolean,boolean,uuid,text,uuid,uuid[])') IS NOT NULL
		AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname='recovery_custody_holds_terminal_record_rejection_check')`
	assertSQL("default features at 298", defaultFeatures)

	user, connection, repo, worker, lead, hold := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash,ephemeral_docker_enabled)
		VALUES($1,$2,'x',true)`, user, fmt.Sprintf("upgrade-%s@e2e", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://gitlab.example','bot',1,$3)`, connection, user, []byte("fixture"))
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url)
		VALUES($1,$2,1,'upgrade/repo','https://gitlab.example/upgrade/repo')`, repo, connection)
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,dind_meter_epoch,maintenance_phase,maintenance_fenced)
		VALUES($1,$2,$3,$4,'online',7,'requested',true)`, worker, user, worker.String(), worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,$4,1,'default data','preserved','running',1)`, lead, user, repo, worker)
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,
		original_worker_id,original_worker_identity,live_worker_id,live_run_id,terminal_record_rejection)
		VALUES($1,$2,$3,$4,1,'open',$5,$6,$5,$4,'mac_failure')`, hold, user, repo, lead, worker, worker.String())
	if err := store.MigrateTo(ctx, upgradeDSN, 302); err != nil {
		t.Fatalf("MigrateTo(302): %v", err)
	}
	assertSQL("upgraded version", "SELECT max(version_id) = 302 FROM goose_db_version WHERE is_applied")
	assertSQL("default features after upgrade", defaultFeatures)
	assertSQL("default data preserved", `SELECT u.ephemeral_docker_enabled AND NOT u.plan_cross_check_enabled
		AND w.dind_meter_epoch=7 AND w.maintenance_phase='requested' AND w.maintenance_fenced
		AND r.issue_title='default data' AND r.issue_description='preserved' AND NOT r.plan_cross_check_required
		AND h.terminal_record_rejection='mac_failure'
		FROM users u JOIN workers w ON w.user_id=u.id JOIN runs r ON r.worker_id=w.id
		JOIN recovery_custody_holds h ON h.run_id=r.id WHERE h.id=$1`, hold)
	assertSQL("shape constraint", `SELECT position('cross_check' in pg_get_constraintdef(oid)) > 0
		AND position('report_only' in pg_get_constraintdef(oid)) > 0
		AND position('budget_wall_seconds' in pg_get_constraintdef(oid)) > 0
		FROM pg_constraint WHERE conname='runs_kind_shape'`)
	assertSQL("kind and trigger source constraints", `SELECT count(*)=2 AND bool_and(position('cross_check' in pg_get_constraintdef(oid))>0)
		FROM pg_constraint WHERE conname IN ('runs_kind_check','runs_trigger_source_check')`)
	assertSQL("cross-check constraints and FK actions", `SELECT
		EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='cross_checks'::regclass AND conname='cross_checks_plan_shape')
		AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='cross_checks'::regclass AND conname='cross_checks_reason_class_check')
		AND EXISTS(SELECT 1 FROM pg_constraint WHERE conname='cross_checks_lead_run_id_fkey' AND confdeltype='c')
		AND EXISTS(SELECT 1 FROM pg_constraint WHERE conname='cross_checks_checker_run_id_fkey' AND confdeltype='n')
		AND EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='runs' AND column_name='plan_cross_check_gate_reason')`)
	assertSQL("one pending index", `SELECT i.indisunique AND pg_get_expr(i.indpred,i.indrelid) LIKE '%pending%'
		FROM pg_index i WHERE i.indexrelid='cross_checks_one_pending'::regclass`)
	assertSQL("round uniqueness", `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='cross_checks'::regclass
		AND contype='u' AND pg_get_constraintdef(oid)='UNIQUE (lead_run_id, stage, round)')`)
	assertSQL("final settlement trigger", `SELECT pg_get_triggerdef(oid) LIKE '%AFTER UPDATE OF status, claim_generation, claim_released_at%'
		AND pg_get_triggerdef(oid) LIKE '%settle_exited_plan_cross_check()%'
		FROM pg_trigger WHERE tgname='runs_settle_exited_plan_cross_check' AND NOT tgisinternal`)
	assertSQL("docker preference function preserved", `SELECT fn_ephemeral_docker_preference_applies(true,true,$1,'issue',NULL,ARRAY[$1]::uuid[])`, repo)
	// The generated GetUserByID selects every CURRENT users column, so it only runs once the
	// database is at head (below); a users column added after version 302 does not exist yet here.
	assertSQL("upgraded user defaults", `SELECT ephemeral_docker_enabled AND NOT plan_cross_check_enabled FROM users WHERE id=$1`, user)
	child := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,
		issue_title,issue_description,status) VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','candidate','running')`,
		child, user, repo, lead)
	mustExec(ctx, t, pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		size_class,base_commit,candidate_digest,checker_run_id,deadline_at,created_at)
		VALUES($1,'plan',1,1,'plan','[]','s',repeat('a',40),$2,$3,now()+interval '30 minutes',now()-interval '10 seconds')`,
		lead, []byte("digest"), child)
	mustExec(ctx, t, pool, "UPDATE runs SET status='failed' WHERE id=$1", lead)
	assertSQL("settlement executes", `SELECT cc.verdict='failed' AND cc.reason_class='superseded'
		AND child.status='cancelled' AND child.claim_released_at IS NOT NULL AND lead.budget_paused_seconds>=10
		FROM cross_checks cc JOIN runs child ON child.id=cc.checker_run_id JOIN runs lead ON lead.id=cc.lead_run_id
		WHERE cc.lead_run_id=$1`, lead)

	// The recovery seams run the generated head-schema queries, so bring the upgraded
	// database to head first (the stages above pin versions 298 and 302 on purpose).
	if err := store.Migrate(ctx, upgradeDSN); err != nil {
		t.Fatalf("Migrate to head: %v", err)
	}
	q := store.New(pool)
	settings, err := q.GetUserByID(ctx, user)
	if err != nil || !settings.EphemeralDockerEnabled || settings.PlanCrossCheckEnabled {
		t.Fatalf("generated GetUserByID: %+v, %v", settings, err)
	}

	// All four public recovery seams must obey the claim assembler's lead -> child order.
	// Each case has a 10-second deadline, one recovery attempt, and no sibling work;
	// cancellation and transaction rollback precede joining the goroutine on every exit.
	for _, method := range []string{"stale", "register", "attested", "snapshot"} {
		t.Run("parent-before-child/"+method, func(t *testing.T) {
			testCrossCheckRecoveryLockOrder(t, pool, user, repo, method)
		})
	}
}

func testCrossCheckRecoveryLockOrder(t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID, method string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	worker, lead, child := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
		VALUES($1,$2,$3,$4,'online',now()-interval '1 hour')`, worker, user, worker.String(), worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,claim_generation)
		VALUES($1,$2,$3,abs(hashtext($4)::bigint)+10,'lead','plan','running',1)`, lead, user, repo, lead.String())
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,target_run_id,harness,report_only,budget_wall_seconds,
		issue_title,issue_description,status,claim_generation,requeue_count,status_since)
		VALUES($1,$2,$3,$4,'cross_check',$5,'codex',true,1800,'checker','candidate','running',1,3,now()-interval '10 minutes')`,
		child, user, repo, worker, lead)
	mustExec(ctx, t, pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		size_class,base_commit,candidate_digest,checker_run_id,deadline_at)
		VALUES($1,'plan',1,1,'plan','[]','s',repeat('a',40),$2,$3,now()+interval '30 minutes')`,
		lead, []byte("digest"), child)
	// Create the live-pointer FKs before recovery owns the worker row. Updating
	// only the annotation below must not acquire a conflicting worker FK lock.
	hold := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,
		original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		VALUES($1,$2,$3,$4,1,'open',$5,$6,$5,$4)`, hold, user, repo, child, worker, worker.String())
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rollback := func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer rollbackCancel()
		_ = holder.Rollback(rollbackCtx)
	}
	defer rollback()
	var id uuid.UUID
	if err := holder.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", lead).Scan(&id); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	done := make(chan error, 1)
	var holderPID int32
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	go func() {
		q := store.New(conn.Conn())
		w := pgtype.UUID{Bytes: worker, Valid: true}
		reason := pgtype.Text{String: "worker lost", Valid: true}
		switch method {
		case "stale":
			_, err := q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
				FailCutoff: ts(time.Now().Add(-time.Minute)), MaxRequeues: 3, FailureReason: reason})
			done <- err
		case "register":
			_, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{WorkerID: w, MaxRequeues: 3, FailureReason: reason})
			done <- err
		case "attested":
			_, err := q.FailAttestedFinalizeRunsOverCap(ctx, store.FailAttestedFinalizeRunsOverCapParams{
				WorkerID: w, RunIds: []uuid.UUID{child}, ClaimGenerations: []int64{1}, MaxRequeues: 0, FailureReason: reason})
			done <- err
		case "snapshot":
			_, err := q.FailRunsMissingFromSnapshot(ctx, store.FailRunsMissingFromSnapshotParams{
				WorkerID: w, MissingCutoff: ts(time.Now().Add(-time.Minute)), MaxRequeues: 3,
				Now: ts(time.Now()), GlobalTimeoutSeconds: 3600, FailureReason: reason})
			done <- err
		}
	}()
	joined := false
	defer func() {
		cancel()
		rollback()
		if !joined {
			<-done
		}
	}()
	waiting := false
	for !waiting {
		if err := pool.QueryRow(ctx, "SELECT $1::int = ANY(pg_blocking_pids($2::int))",
			holderPID, conn.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatalf("observe recovery blocked on held lead: %v", err)
		}
		if !waiting {
			select {
			case err := <-done:
				joined = true
				t.Fatalf("recovery returned before waiting on lead: %v", err)
			case <-ctx.Done():
				t.Fatal("recovery never waited on lead")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if _, err := holder.Exec(ctx, "SET LOCAL lock_timeout='500ms'"); err != nil {
		t.Fatal(err)
	}
	// With the old helper, recovery already owns child and this fails (or deadlocks).
	if err := holder.QueryRow(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", child).Scan(&id); err != nil {
		t.Fatalf("lead-owning claim assembler could not lock checker: %v", err)
	}
	// Commit a custody annotation while recovery waits. Only the writer's separate
	// READ COMMITTED statement can see it after the locking query's old snapshot.
	if _, err := holder.Exec(ctx, "UPDATE recovery_custody_holds SET terminal_record_rejection='mac_failure' WHERE id=$1", hold); err != nil {
		t.Fatalf("annotate custody after locking checker: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	joined = true
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	var parked bool
	if err := pool.QueryRow(ctx, `SELECT status='recovery_wait'
		AND recovery_wait_cause='worker_requeue_exhausted' AND fail_origin IS NULL
		AND failure_reason IS NULL AND finished_at IS NULL AND claim_released_at IS NOT NULL
		AND (worker_recovery_evidence->>'custody_uncertain')::boolean
		AND EXISTS (SELECT 1 FROM recovery_custody_holds WHERE id=$2
			AND state='open' AND terminal_record_rejection='mac_failure')
		FROM runs WHERE id=$1`, child, hold).Scan(&parked); err != nil || !parked {
		t.Fatalf("fresh custody must park and retain the committed MAC annotation: parked=%t err=%v", parked, err)
	}
}
