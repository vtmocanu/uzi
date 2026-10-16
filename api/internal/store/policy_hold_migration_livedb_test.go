package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPolicyHoldMigrationGuardsLiveDB(t *testing.T) {
	base := os.Getenv("UZI_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := OpenPool(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "cdr_policy_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn := u.String()
	version := migrationVersionByName(t, "policy_hold")
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, worker, connection, repo := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(t, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")
	exec(t, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'rollback',$3,'online')", worker, owner, worker[:])
	exec(t, "INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'gitlab','https://forge.example.com','fixture',1,$3)", connection, owner, []byte{1})
	exec(t, "INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'fixtures/policy','https://forge.example.com/fixtures/policy','main',true)", repo, connection)
	for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "pool_wait", "recovery_wait"} {
		t.Run("refuse_"+status, func(t *testing.T) {
			id := uuid.New()
			exec(t, "INSERT INTO runs(id,user_id,kind,status,repo_id,harness,issue_title,issue_description,worker_id,claim_generation,policy_refusal_count,session_id) VALUES($1,$2,'prompt',$3,$5,'codex','fixture','d',$4,1,1,'keep')", id, owner, status, worker, repo)
			if err := MigrateDownTo(ctx, dsn, version-1); err == nil {
				t.Fatal("rollback accepted live constrained claim")
			}
			var got string
			var count int
			if err := pool.QueryRow(ctx, "SELECT status,policy_refusal_count FROM runs WHERE id=$1", id).Scan(&got, &count); err != nil || got != status || count != 1 {
				t.Fatalf("failed rollback mutated row: %s %d %v", got, count, err)
			}
			exec(t, "DELETE FROM runs WHERE id=$1", id)
		})
	}
	// Even without a worker, a non-convertible status cannot lose the constraint.
	orphan := uuid.New()
	exec(t, "INSERT INTO runs(id,user_id,kind,status,repo_id,harness,issue_title,issue_description,policy_fresh_claim_generation) VALUES($1,$2,'prompt','running',$3,'codex','fixture','d',1)", orphan, owner, repo)
	if err := MigrateDownTo(ctx, dsn, version-1); err == nil {
		t.Fatal("rollback accepted constrained orphan")
	}
	exec(t, "DELETE FROM runs WHERE id=$1", orphan)
	ids := map[uuid.UUID]string{}
	for _, status := range []string{"paused", "queued", "limit_wait", "pool_wait", "recovery_wait", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		id := uuid.New()
		ids[id] = status
		exec(t, "INSERT INTO runs(id,user_id,kind,status,repo_id,harness,issue_title,issue_description,worker_id,claim_released_at,policy_refusal_count,session_id,hold_reason) VALUES($1,$2,'prompt',$3,$5,'codex','fixture','d',$4,now(),1,'keep','budget_exhausted')", id, owner, status, worker, repo)
	}
	for id := range ids {
		exec(t, "INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id) VALUES($1,$2,$3,1,'open',$4,'rollback fixture',$4,$3)", uuid.New(), owner, id, worker)
	}
	untouched := uuid.New()
	exec(t, "INSERT INTO runs(id,user_id,kind,status,repo_id,harness,issue_title,issue_description) VALUES($1,$2,'prompt','queued',$3,'codex','fixture','d')", untouched, owner, repo)
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatal(err)
	}
	for id, status := range ids {
		var got, session, origin string
		if err := pool.QueryRow(ctx, "SELECT status,session_id,fail_origin FROM runs WHERE id=$1", id).Scan(&got, &session, &origin); err != nil || got != "failed" || session != "keep" || origin != "provider_policy_refusal" {
			t.Fatalf("convert %s: %s %s %s %v", status, got, session, origin, err)
		}
	}
	var held int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM recovery_custody_holds WHERE user_id=$1 AND state='open' AND live_worker_id=$2 AND live_run_id=run_id AND original_worker_id=$2", owner, worker).Scan(&held); err != nil || held != len(ids) {
		t.Fatalf("rollback changed custody: held=%d want=%d err=%v", held, len(ids), err)
	}
	var got string
	if err := pool.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", untouched).Scan(&got); err != nil || got != "queued" {
		t.Fatalf("unrelated row=%s %v", got, err)
	}
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../../fixtures/policy-hold/correlation.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Name, Value string
		Valid       bool
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		t.Run("correlation_"+vector.Name, func(t *testing.T) {
			_, err := pool.Exec(ctx, "UPDATE runs SET policy_last_correlation_id=$2 WHERE id=$1", untouched, vector.Value)
			if (err == nil) != vector.Valid {
				t.Fatalf("database correlation acceptance=%v want=%v error=%v", err == nil, vector.Valid, err)
			}
		})
	}

}
