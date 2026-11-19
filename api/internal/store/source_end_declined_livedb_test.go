package store_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSourceEndDeclinedLegacyResumeClaimFenceLiveDB covers SQL admission only.
// Worker source-use refusal is exercised separately at the worker's three seams.
func TestSourceEndDeclinedLegacyResumeClaimFenceLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	worker := fx.worker("legacy-source", capOf(2), false)
	const nonce = "legacy-source-registration"
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", worker, nonce)
	run := fx.queuedRun()
	prior, err := fx.claim(worker, false, nil)
	if err != nil || prior.ID != run || prior.ClaimGeneration != 1 {
		t.Fatalf("prior claim: run=%s generation=%d err=%v", prior.ID, prior.ClaimGeneration, err)
	}
	hold := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO recovery_custody_holds
 (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,$6,$5,$4,true)`,
		hold, fx.userID, fx.repoID, run, worker, worker.String())
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='recovery_wait',
 recovery_wait_cause='retained_recovery_adoption_failed', recovery_wait_detail='source_end_declined',
 claim_released_at=now(), released_worker_id=$2, released_worker_nonce=$3,
 registration_evidence_generation=1, registration_evidence_nonce=$3,
 requeue_count=3, recovery_wait_count=2, retained_park_count=1
 WHERE id=$1`, run, worker, nonce)
	readHold := func() string {
		t.Helper()
		var row string
		if err := fx.pool.QueryRow(fx.ctx, "SELECT row_to_json(h)::text FROM recovery_custody_holds h WHERE id=$1", hold).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	held := readHold()
	assertCustody := func() {
		t.Helper()
		var total, open int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*), count(*) FILTER (WHERE state='open')
 FROM recovery_custody_holds WHERE run_id=$1`, run).Scan(&total, &open); err != nil {
			t.Fatal(err)
		}
		if total != 1 || open != 1 || readHold() != held {
			t.Fatalf("custody changed: total=%d open=%d", total, open)
		}
	}
	legacyBytes, err := os.ReadFile("testdata/legacy_resume_worker_recovery_episode_97440d596.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.NewReplacer("@global_timeout_seconds", "$3", "@user_id", "$2", "@id", "$1").Replace(string(legacyBytes))
	var gotRun, gotUser uuid.UUID
	var status string
	if err := fx.pool.QueryRow(fx.ctx, legacy, run, fx.userID, int32(3600)).Scan(&gotRun, &gotUser, &status); err != nil || gotRun != run || gotUser != fx.userID || status != "queued" {
		t.Fatalf("baseline Resume: run=%s user=%s status=%q err=%v", gotRun, gotUser, status, err)
	}
	assertQueued := func() {
		t.Helper()
		var detail *string
		var released uuid.UUID
		var gotNonce string
		var generation int64
		var requeues, waits, parks int32
		var releasedClaim bool
		if err := fx.pool.QueryRow(fx.ctx, `SELECT status, recovery_wait_detail,
 released_worker_id, released_worker_nonce, claim_generation, requeue_count,
 recovery_wait_count, retained_park_count, claim_released_at IS NOT NULL
 FROM runs WHERE id=$1`, run).Scan(&status, &detail, &released, &gotNonce, &generation, &requeues, &waits, &parks, &releasedClaim); err != nil {
			t.Fatal(err)
		}
		if status != "queued" || detail != nil || released != worker || gotNonce != nonce || generation != 1 || requeues != 3 || waits != 2 || parks != 1 || !releasedClaim {
			t.Fatalf("legacy queued fence: status=%q detail=%v worker=%s nonce=%q generation=%d requeues=%d waits=%d parks=%d released=%v",
				status, detail, released, gotNonce, generation, requeues, waits, parks, releasedClaim)
		}
		assertCustody()
	}
	assertQueued()
	// Three independent claim attempts are bounded and must all remain write-free.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := fx.claim(worker, false, nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("same-incarnation claim %d: want ErrNoRows, got %v", attempt, err)
		}
		assertQueued()
	}
	// Change only the registration nonce: a real successful claim rules out other
	// eligibility guards masking the released-incarnation exclusion.
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET snapshot_register_nonce=$2 WHERE id=$1", worker, "new-source-registration")
	claimed, err := fx.claim(worker, false, nil)
	if err != nil || claimed.ID != run || claimed.ClaimGeneration != 2 || claimed.Status != "claimed" {
		t.Fatalf("changed-nonce control: run=%s generation=%d status=%q err=%v", claimed.ID, claimed.ClaimGeneration, claimed.Status, err)
	}
	if claimed.RequeueCount != 3 || claimed.RecoveryWaitCount != 2 || claimed.RetainedParkCount != 1 || claimed.ClaimReleasedAt.Valid || claimed.ReleasedWorkerID.Valid || claimed.ReleasedWorkerNonce.Valid {
		t.Fatalf("control counters/fence: requeues=%d waits=%d parks=%d released=%v worker=%v nonce=%v",
			claimed.RequeueCount, claimed.RecoveryWaitCount, claimed.RetainedParkCount,
			claimed.ClaimReleasedAt.Valid, claimed.ReleasedWorkerID.Valid, claimed.ReleasedWorkerNonce.Valid)
	}
	assertCustody()
}

func TestSourceEndDeclinedMigrationAndLegacyResumeLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "source_end_declined_")
	if err := store.MigrateTo(ctx, dsn, 461); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	user, conn, repo, worker, run := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github','https://example.com','bot',1,'x')`, conn, user)
	mustExec(ctx, t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,'example/detail','https://example.com/detail','main')`, repo, conn)
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", worker, user, worker.String(), worker[:])
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,$4,'issue',4177,'detail','fixture','running',1)`, run, user, repo, worker)

	if err := store.MigrateTo(ctx, dsn, 462); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_detail_check'").Scan(&valid); err != nil || valid {
		t.Fatalf("462 CHECK NOT VALID: valid=%v err=%v", valid, err)
	}
	_, err = pool.Exec(ctx, "UPDATE runs SET recovery_wait_detail='unknown-detail' WHERE id=$1", run)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" || pe.ConstraintName != "runs_recovery_wait_detail_check" {
		t.Fatalf("unknown detail must violate CHECK: %v", err)
	}
	mustExec(ctx, t, pool, "UPDATE workers SET snapshot_register_nonce='released-source' WHERE id=$1", worker)
	mustExec(ctx, t, pool, `UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_recovery_adoption_failed',
 recovery_wait_detail='source_end_declined', claim_released_at=now(), released_worker_id=$2,
 released_worker_nonce='released-source', registration_evidence_generation=1, registration_evidence_nonce='released-source'
 WHERE id=$1`, run, worker)
	if err := store.MigrateTo(ctx, dsn, 463); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT convalidated FROM pg_constraint WHERE conrelid='runs'::regclass AND conname='runs_recovery_wait_detail_check'").Scan(&valid); err != nil || !valid {
		t.Fatalf("463 CHECK validated: valid=%v err=%v", valid, err)
	}
	b, err := os.ReadFile("migrations/00462_retained_source_end_hold.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(b), "-- +goose Down", 2)
	if len(parts) != 2 {
		t.Fatal("missing Down")
	}
	down := parts[1]
	for _, status := range []string{"recovery_wait", "cancelled"} {
		mustExec(ctx, t, pool, "UPDATE runs SET status=$2 WHERE id=$1", run, status)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, down)
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "refusing rollback 00462") {
			t.Fatalf("Down must refuse %s evidence: %v", status, err)
		}
		var detail, nonce string
		if err := pool.QueryRow(ctx, "SELECT recovery_wait_detail, released_worker_nonce FROM runs WHERE id=$1", run).Scan(&detail, &nonce); err != nil || detail != "source_end_declined" || nonce != "released-source" {
			t.Fatalf("Down lost evidence: %q %q %v", detail, nonce, err)
		}
	}
	mustExec(ctx, t, pool, "UPDATE runs SET status='recovery_wait' WHERE id=$1", run)
	// Generated queries expand workers.* and runs.* to current columns, so bring the
	// schema to head before calling them. The 462/463 assertions above stay pinned.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	q := store.New(pool)
	if _, err := q.ResumeWorkerRecoveryEpisode(ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: user, GlobalTimeoutSeconds: 3600}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new SQL must refuse unchanged nonce: %v", err)
	}
	// This is the unchanged baseline named SQL region, not a recreation of the old API.
	// There is intentionally no trigger: the legacy writer MAY clear detail and queue.
	legacyBytes, err := os.ReadFile("testdata/legacy_resume_worker_recovery_episode_97440d596.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.NewReplacer("@global_timeout_seconds", "$3", "@user_id", "$2", "@id", "$1").Replace(string(legacyBytes))
	var gotRun, gotUser uuid.UUID
	var status string
	if err := pool.QueryRow(ctx, legacy, run, user, int32(3600)).Scan(&gotRun, &gotUser, &status); err != nil || status != "queued" || gotRun != run || gotUser != user {
		t.Fatalf("legacy writer may queue: %s %v", status, err)
	}
	var detail *string
	var released uuid.UUID
	var nonce string
	var generation int64
	if err := pool.QueryRow(ctx, "SELECT recovery_wait_detail, released_worker_id, released_worker_nonce, claim_generation FROM runs WHERE id=$1", run).Scan(&detail, &released, &nonce, &generation); err != nil || detail != nil || released != worker || nonce != "released-source" || generation != 1 {
		t.Fatalf("legacy resume changed released identity/generation bounds: detail=%v worker=%s nonce=%q gen=%d err=%v", detail, released, nonce, generation, err)
	}
	// With no new-detail value remaining, narrowing is permitted. Roll it back so
	// this test does not alter the migration bookkeeping behind goose's back.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("Down without new detail: %v", err)
	}
}
