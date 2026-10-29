package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The migration that widens checker_effort_source accepts the cross-check default,
// still rejects unknown values, and refuses to roll back while a row records it.
func TestCrossCheckDefaultEffortSourceMigrationLiveDB(t *testing.T) {
	const widened, previous = 374, 373 // renumbered with the migration at merge
	ctx, dsn := standIsolatedDB(t, "xc_effort_source_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')`,
		user, fmt.Sprintf("xc-effort-%s@example.com", user))
	mustExec(ctx, t, pool, `INSERT INTO forge_connections
		(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github','https://example.com','bot',1,$3)`, conn, user, []byte{1})
	mustExec(ctx, t, pool, `INSERT INTO repos
		(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)`, repo, conn)
	insertCheck := func(source string) (uuid.UUID, error) {
		lead, child := uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,claim_generation)
			VALUES($1,$2,$3,abs(hashtext($4)::bigint)+1,'lead','body','running',1)`, lead, user, repo, lead.String())
		mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,issue_title,issue_description,status)
			VALUES($1,$2,$3,'cross_check',$4,'codex',true,1800,'checker','body','running')`, child, user, repo, lead)
		_, err := pool.Exec(ctx, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,
			candidate_digest,checker_run_id,checker_harness,checker_model,checker_effort,checker_model_source,checker_effort_source,
			verdict,created_at,deadline_at)
			VALUES($1,'plan',1,1,'plan','[]','s',repeat('a',40),$2,$3,'codex','gpt-6.1-sol','high','worker default',$4,
			'pending',now(),now()+interval '10 minutes')`, lead, []byte("digest"), child, source)
		return lead, err
	}
	lead, err := insertCheck("cross-check default")
	if err != nil {
		t.Fatalf("widened constraint refused the cross-check default: %v", err)
	}
	if _, err := insertCheck("bogus"); err == nil || !strings.Contains(err.Error(), "cross_checks_checker_effort_source_check") {
		t.Fatalf("unknown source accepted or wrong refusal: %v", err)
	}

	if err := store.MigrateDownTo(ctx, dsn, previous); err == nil ||
		!strings.Contains(err.Error(), "refusing to narrow the constraint") {
		t.Fatalf("rollback with a recorded cross-check default: %v", err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT checker_effort_source FROM cross_checks WHERE lead_run_id=$1`, lead).Scan(&source); err != nil ||
		source != "cross-check default" {
		t.Fatalf("refused rollback changed history: %q %v", source, err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version); err != nil || version < widened {
		t.Fatalf("refused rollback left version %d: %v", version, err)
	}

	mustExec(ctx, t, pool, `DELETE FROM runs WHERE user_id=$1`, user)
	if err := store.MigrateDownTo(ctx, dsn, previous); err != nil {
		t.Fatalf("rollback without new-source rows: %v", err)
	}
	if _, err := insertCheck("cross-check default"); err == nil {
		t.Fatal("narrowed constraint accepted the cross-check default")
	}
	if _, err := insertCheck("worker default"); err != nil {
		t.Fatalf("narrowed constraint refused a historical source: %v", err)
	}
}
