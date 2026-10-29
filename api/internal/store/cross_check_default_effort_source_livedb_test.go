package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The two-step migration that widens checker_effort_source (00375 NOT VALID, 00376
// VALIDATE) accepts the cross-check default, still rejects unknown values at every step,
// and refuses to roll back past 00375 while a row records it.
func TestCrossCheckDefaultEffortSourceMigrationLiveDB(t *testing.T) {
	const validated, widened, previous = 376, 375, 374 // renumbered with the migrations at merge
	ctx, dsn := standIsolatedDB(t, "xc_effort_source_")
	// Stop after 00375 first: its Up must leave the widened CHECK NOT VALID, which a
	// head-only start cannot observe once 00376 has validated it.
	if err := store.MigrateTo(ctx, dsn, widened); err != nil {
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
	isValidated := func(label string, want bool) {
		t.Helper()
		var got bool
		if err := pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
			WHERE conname='cross_checks_checker_effort_source_check' AND conrelid='cross_checks'::regclass`).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: convalidated=%v err=%v, want %v", label, got, err, want)
		}
	}
	appliedVersion := func() int64 {
		t.Helper()
		var v int64
		if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	writes := func(label string) uuid.UUID {
		t.Helper()
		lead, err := insertCheck("cross-check default")
		if err != nil {
			t.Fatalf("%s: widened constraint refused the cross-check default: %v", label, err)
		}
		if _, err := insertCheck("bogus"); err == nil || !strings.Contains(err.Error(), "cross_checks_checker_effort_source_check") {
			t.Fatalf("%s: unknown source accepted or wrong refusal: %v", label, err)
		}
		return lead
	}

	isValidated("after 00375 Up", false)
	writes("after 00375 Up")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	isValidated("head", true)
	lead := writes("head")
	// Down 00376 restores the NOT VALID state of 00375; new writes are still checked.
	if err := store.MigrateDownTo(ctx, dsn, widened); err != nil {
		t.Fatalf("rollback of the validation step: %v", err)
	}
	isValidated("after 00376 Down", false)
	writes("after 00376 Down")

	if err := store.MigrateDownTo(ctx, dsn, previous); err == nil ||
		!strings.Contains(err.Error(), "refusing to narrow the constraint") {
		t.Fatalf("rollback with a recorded cross-check default: %v", err)
	}
	var source string
	if err := pool.QueryRow(ctx, `SELECT checker_effort_source FROM cross_checks WHERE lead_run_id=$1`, lead).Scan(&source); err != nil ||
		source != "cross-check default" {
		t.Fatalf("refused rollback changed history: %q %v", source, err)
	}
	if v := appliedVersion(); v != widened {
		t.Fatalf("refused rollback left version %d, want %d", v, widened)
	}
	isValidated("after refused 00375 Down", false)
	if _, err := insertCheck("bogus"); err == nil {
		t.Fatal("refused rollback dropped the constraint")
	}
	// A row written after the Down guard (a race) is still caught: the narrowing re-adds
	// the two-value CHECK validated, so it fails on its own and the Down rolls back.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `ALTER TABLE cross_checks DROP CONSTRAINT cross_checks_checker_effort_source_check;
		ALTER TABLE cross_checks ADD CONSTRAINT cross_checks_checker_effort_source_check
			CHECK (checker_effort_source IN ('pin', 'worker default'))`)
	_ = tx.Rollback(ctx)
	if err == nil || !strings.Contains(err.Error(), "violated by some row") {
		t.Fatalf("validated narrowing accepted a cross-check default row: %v", err)
	}
	isValidated("after unguarded narrowing rollback", false)

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
	isValidated("after 00375 Down", true)
	if v := appliedVersion(); v != previous {
		t.Fatalf("rollback left version %d, want %d", v, previous)
	}
	// Forward again: both steps re-apply cleanly.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if v := appliedVersion(); v < validated {
		t.Fatalf("re-apply reached version %d, want >= %d", v, validated)
	}
	isValidated("re-applied", true)
}
