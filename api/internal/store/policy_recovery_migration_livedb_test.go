package store_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPolicyRecoveryIdentityMigrationLiveDB pins migration 419: the four columns are additive and
// uncoupled (a row in the earlier compact-hold shape stays valid), the checks are closed, and Down
// drops exactly these four columns.
func TestPolicyRecoveryIdentityMigrationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "policy_recovery_identity_")
	if err := store.MigrateTo(ctx, dsn, 418); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner, worker, conn, repo, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'policy fixture',$3)", worker, owner, []byte(worker.String()))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, owner)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES($1,$2,1,'example/policy','https://example.com/policy','main')`, repo, conn)
	// An earlier-shape policy row written before the columns exist (rolling deploy).
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,harness,worker_id,claim_generation,session_id,
		policy_outcome_id,policy_continuation_mode,policy_applying_worker_id,policy_applying_generation,policy_source_generation,policy_preserve_required)
		VALUES($1,$2,$3,3607,'t','d','paused','codex',$4,1,'s',$5,'same_conversation',$4,1,1,true)`, run, owner, repo, worker, uuid.New())
	version := func(want int64) {
		t.Helper()
		var v int64
		if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&v); err != nil || v != want {
			t.Fatalf("goose version = %d (%v), want %d", v, err, want)
		}
	}
	version(418)
	if err := store.MigrateTo(ctx, dsn, 419); err != nil {
		t.Fatal(err)
	}
	version(419)
	var validated bool
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conname='runs_policy_required_recovery_shape'").Scan(&validated); err != nil || !validated {
		t.Fatalf("runs_policy_required_recovery_shape validated = %v (%v), want a validated check", validated, err)
	}
	var nulls bool
	if err := pool.QueryRow(ctx, `SELECT policy_required_head IS NULL AND policy_recovery_verified_generation IS NULL
		AND policy_recovery_verified_head IS NULL AND policy_hold_cause IS NULL FROM runs WHERE id=$1`, run).Scan(&nulls); err != nil || !nulls {
		t.Fatalf("existing rows must gain NULL columns: %v %v", nulls, err)
	}
	valid40, valid64 := strings.Repeat("a", 40), strings.Repeat("b", 64)
	for _, ok := range []string{valid40, valid64} {
		mustExec(ctx, t, pool, "UPDATE runs SET policy_required_head=$2,policy_recovery_verified_head=$2 WHERE id=$1", run, ok)
	}
	for name, bad := range map[string]string{
		"uppercase": strings.ToUpper(valid40), "short": strings.Repeat("a", 39), "41 chars": strings.Repeat("a", 41),
		"empty": "", "non-hex": strings.Repeat("g", 40), "newline": valid40 + "\n",
	} {
		if _, err := pool.Exec(ctx, "UPDATE runs SET policy_required_head=$2 WHERE id=$1", run, bad); err == nil {
			t.Fatalf("required head %s accepted", name)
		}
		if _, err := pool.Exec(ctx, "UPDATE runs SET policy_recovery_verified_head=$2 WHERE id=$1", run, bad); err == nil {
			t.Fatalf("verified head %s accepted", name)
		}
	}
	for _, cause := range []string{"refused", "attempt_path", "saved_work_missing", "session_missing", "transcript_unknown",
		"provider_resume_rejected", "pending_conversion", "fresh_start_uncertain"} {
		mustExec(ctx, t, pool, "UPDATE runs SET policy_hold_cause=$2 WHERE id=$1", run, cause)
	}
	for _, bad := range []string{"", "Refused", "other"} {
		if _, err := pool.Exec(ctx, "UPDATE runs SET policy_hold_cause=$2 WHERE id=$1", run, bad); err == nil {
			t.Fatalf("cause %q accepted", bad)
		}
	}
	mustExec(ctx, t, pool, "UPDATE runs SET policy_hold_cause=NULL WHERE id=$1", run)
	if err := store.MigrateDownTo(ctx, dsn, 418); err != nil {
		t.Fatalf("Down must convert the paused R-bearing row, not refuse it: %v", err)
	}
	version(418)
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", run).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("paused R-bearing row status = %q (%v), want failed", status, err)
	}
	var present, kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE column_name IN ('policy_required_head','policy_recovery_verified_generation','policy_recovery_verified_head','policy_hold_cause')),
		count(*) FILTER (WHERE column_name IN ('policy_preserve_required','policy_outcome_id','policy_hold_published'))
		FROM information_schema.columns WHERE table_name='runs'`).Scan(&present, &kept); err != nil {
		t.Fatal(err)
	}
	if present != 0 || kept != 3 {
		t.Fatalf("Down left %d new columns and kept %d of 3 earlier policy columns", present, kept)
	}
}
