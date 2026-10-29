package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCompactPolicyHoldMigrationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "compact_policy_hold_")
	if err := store.MigrateTo(ctx, dsn, 376); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, worker, conn, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'policy fixture',$3)", worker, owner, []byte(worker.String()))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/policy','https://example.com/policy','main')`, repo, conn)
	seed := func(status string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,harness,worker_id,claim_generation,session_id) VALUES($1,$2,$3,3607,'policy fixture','desc',$4,'codex',$5,1,'saved-session')`, id, owner, repo, status, worker)
		return id
	}
	ordinary := seed("running")
	if err := store.MigrateTo(ctx, dsn, 377); err != nil {
		t.Fatal(err)
	}
	var preserve, pending bool
	if err := pool.QueryRow(ctx, "SELECT policy_preserve_required,policy_pending_fresh FROM runs WHERE id=$1", ordinary).Scan(&preserve, &pending); err != nil {
		t.Fatal(err)
	}
	if preserve || pending {
		t.Fatal("legacy rows acquired policy constraints")
	}
	held, queued, resolved, live := seed("paused"), seed("queued"), seed("running"), seed("running")
	enroll := func(id uuid.UUID, resolved bool) {
		t.Helper()
		mustExec(ctx, t, pool, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode=CASE WHEN $4 THEN 'fresh_from_saved_work' ELSE 'same_conversation' END,policy_preserve_required=NOT $4,policy_resolved_at=CASE WHEN $4 THEN now() ELSE NULL END,policy_applying_worker_id=$3,policy_applying_generation=1,policy_source_generation=1 WHERE id=$1`, id, uuid.New(), worker, resolved)
	}
	for _, id := range []uuid.UUID{held, queued, live} {
		enroll(id, false)
	}
	enroll(resolved, true)
	// A known paused preserving row is convertible even after a legitimate later
	// pause changed the displayed hold; rollback must not key on hold_reason alone.
	mustExec(ctx, t, pool, "UPDATE runs SET hold_reason='credential_disabled' WHERE id=$1", held)
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('rows',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM runs r),'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM goose_db_version v))::text`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	if err := store.MigrateDownTo(ctx, dsn, 376); err == nil {
		t.Fatal("Down dropped a live preserving constraint")
	}
	if snapshot() != before {
		t.Fatal("refused Down partially converted rows or history")
	}
	mustExec(ctx, t, pool, "UPDATE runs SET status='queued' WHERE id=$1", live)
	if err := store.MigrateDownTo(ctx, dsn, 376); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{held, queued, live} {
		var status, session string
		if err := pool.QueryRow(ctx, "SELECT status,session_id FROM runs WHERE id=$1", id).Scan(&status, &session); err != nil {
			t.Fatal(err)
		}
		if status != "failed" || session != "saved-session" {
			t.Fatal("Down failed to terminalize safely or lost session")
		}
	}
	for _, id := range []uuid.UUID{ordinary, resolved} {
		var status string
		if err := pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "running" {
			t.Fatal("Down converted an ordinary or resolved-attempt row")
		}
	}
	if err := store.MigrateTo(ctx, dsn, 377); err != nil {
		t.Fatal(err)
	}
}
