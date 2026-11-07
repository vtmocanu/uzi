package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimedNeverStartedWitnessConstraintValidationLiveDB pins the split: 00404 must not scan
// either widened CHECK while holding ACCESS EXCLUSIVE; 00405 validates them separately. An
// unvalidated CHECK still rejects invalid writes. Validation preserves the run and custody
// byte for byte.
func TestClaimedNeverStartedWitnessConstraintValidationLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "claimed_witness_validation_")
	if err := store.MigrateTo(ctx, dsn, 403); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	user, conn, repo, worker, run, hold := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/claimed-witness-validation','https://example.com/claimed-witness-validation','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,
 status,recovery_wait_cause,claim_generation)
 VALUES($1,$2,$3,$4,'issue',3859,'constraint migration','fixture','recovery_wait','provider_outage',1)`, run, user, repo, worker)
	mustExec(ctx, t, pool, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,
 original_worker_id,original_worker_identity,live_worker_id,live_run_id)
 VALUES($1,$2,$3,$4,1,'open',$5,'constraint migration',$5,$4)`, hold, user, repo, run, worker)

	if err := store.MigrateTo(ctx, dsn, 404); err != nil {
		t.Fatal(err)
	}
	names := []string{"runs_worker_recovery_evidence_check", "runs_recovery_wait_cause_check"}
	readChecks := func(wantValid bool) {
		t.Helper()
		for _, name := range names {
			var valid bool
			if err := pool.QueryRow(ctx, `SELECT convalidated
 FROM pg_constraint WHERE conrelid='runs'::regclass AND conname=$1`, name).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if valid != wantValid {
				t.Fatalf("%s convalidated=%v, want %v", name, valid, wantValid)
			}
		}
	}
	readChecks(false) // This is red if either NOT VALID is removed from 00404.
	assertRejected := func(query, constraint string) {
		t.Helper()
		_, err := pool.Exec(ctx, query, run)
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != constraint {
			t.Fatalf("invalid write: %v, want CHECK violation from %s", err, constraint)
		}
	}
	assertInvalidWritesRejected := func() {
		t.Helper()
		assertRejected("UPDATE runs SET recovery_wait_cause='not_a_recovery_cause' WHERE id=$1", names[1])
		assertRejected(`UPDATE runs SET worker_recovery_evidence='{"unexpected":true}'::jsonb WHERE id=$1`, names[0])
	}
	assertInvalidWritesRejected()
	// The widened checks accept the new cause and observation keys before validation too.
	mustExec(ctx, t, pool, `UPDATE runs SET recovery_wait_cause='retained_continuation_unwitnessed',
 worker_recovery_evidence=jsonb_build_object('checkpoint_tip',NULL,'available_capture',false,
 'publication_uncertain',false,'capture_uncertain',false,'custody_uncertain',false,'unknown',false,
 'recorded_at','2026-11-05T00:00:00Z','presence','p1','registration_evidence_epoch',1,
 'worker_snapshot_epoch',1,'deadline','2026-11-05T00:10:00Z') WHERE id=$1`, run)
	snapshot := func() [2]string {
		t.Helper()
		var rows [2]string
		if err := pool.QueryRow(ctx, "SELECT to_jsonb(r)::text FROM runs r WHERE id=$1", run).Scan(&rows[0]); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&rows[1]); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	before := snapshot()
	if err := store.MigrateTo(ctx, dsn, 405); err != nil {
		t.Fatal(err)
	}
	readChecks(true)
	assertInvalidWritesRejected()
	if got := snapshot(); got != before {
		t.Fatal("validation changed the run or custody")
	}

}
