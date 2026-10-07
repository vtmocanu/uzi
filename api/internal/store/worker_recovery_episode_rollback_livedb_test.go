package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Replay the actual 00309 Down/Up over production exhaustion holds. All fixture
// writes and schema changes share one rollback-only transaction, including when
// Down aborts while validating runs_recovery_wait_cause_check.
func TestWorkerRecoveryEpisodeRollbackLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("fixture migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture open pool: %v", err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := tx.Rollback(cleanup); err != nil {
			t.Errorf("fixture rollback: %v", err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture SQL: %v", err)
		}
	}
	assertSQL := func(name, sql string, args ...any) {
		t.Helper()
		var ok bool
		if err := tx.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !ok {
			t.Fatalf("%s", name)
		}
	}
	snapshot := func(sql string, id uuid.UUID) string {
		t.Helper()
		var value string
		if err := tx.QueryRow(ctx, sql, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}

	user, conn, repo, worker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("rollback-%s@e2e", user))
	exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, conn, user, []byte{1})
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		VALUES($1,$2,1,'g/r','https://forge.e2e/g/r','main',true)`, repo, conn)
	nonce := "rollback-incarnation-" + worker.String()
	exec(`INSERT INTO workers(id,user_id,name,token_hash,max_concurrent_runs,snapshot_register_nonce)
		VALUES($1,$2,'rollback-worker',$3,4,$4)`, worker, user, "hash-"+worker.String(), nonce)
	held, noHold := uuid.New(), uuid.New()
	tip := strings.Repeat("a", 40)
	for i, id := range []uuid.UUID{held, noHold} {
		exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,kind,issue_iid,issue_title,issue_description,status,
			requeue_count,worker_recovery_episode,requeue_episode_baseline,claim_generation,checkpoint_tip,checkpoint_tip_at)
			VALUES($1,$2,$3,$4,'issue',$5,'rollback','fixture','running',4,2,1,7,$6,now())`,
			id, user, repo, worker, i+1, tip)
	}
	hold, capture := uuid.New(), uuid.New()
	exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,
		original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		VALUES($1,$2,$3,$4,7,'open',$5,'rollback-worker',$5,$4)`, hold, user, repo, held, worker)
	exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
		source_sha,idempotency_key,state) VALUES($1,$2,$3,$4,$5,'rollback-worker',$6,'rollback-capture','available')`,
		capture, hold, held, user, worker, tip)

	attempt := uuid.New()
	exec(`INSERT INTO checkpoint_publish_attempts(id,run_id,branch,ref,tip)
		VALUES($1,$2,'agent/issue-1','refs/uzi-checkpoints/agent/issue-1',$3)`, attempt, held, tip)

	q := store.New(pool).WithTx(tx)
	outcomes, err := q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		WorkerID: pgU(worker), MaxRequeues: 3, FailureReason: pgT("fixture worker lost"),
	})
	if err != nil {
		t.Fatalf("production exhaustion: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("production exhaustion returned %d outcomes, want two holds", len(outcomes))
	}
	for _, id := range []uuid.UUID{held, noHold} {
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var evidence store.WorkerRecoveryEvidence
		if err := json.Unmarshal(run.WorkerRecoveryEvidence, &evidence); err != nil {
			t.Fatal(err)
		}
		if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "worker_requeue_exhausted" ||
			run.FinishedAt.Valid || evidence.CheckpointTip == nil || *evidence.CheckpointTip != tip ||
			evidence.Unknown || evidence.RecordedAt.IsZero() ||
			evidence.CustodyUncertain != (id == held) || evidence.AvailableCapture != (id == held) ||
			evidence.PublicationUncertain != (id == held) {
			t.Fatalf("production exhaustion did not create the expected evidence-backed hold for %s", id)
		}
		// Explicit fences, not just a broad row snapshot.
		assertSQL("production hold must release the exact generation and incarnation",
			`SELECT claim_generation=7 AND claim_released_at IS NOT NULL AND worker_id=$2
				AND released_worker_id=$2 AND released_worker_nonce=$3
				AND requeue_count=4 AND worker_recovery_episode=2 AND requeue_episode_baseline=1
				FROM runs WHERE id=$1`, id, worker, nonce)
		exec(`UPDATE runs SET recovery_retry_not_before=now()+interval '1 hour',
			status_since='2001-01-01',updated_at='2001-01-01' WHERE id=$1`, id)
	}
	// Outstanding publication records must survive terminalization unchanged.
	for i, id := range []uuid.UUID{held, noHold} {
		branch := fmt.Sprintf("agent/issue-%d", i+1)
		exec(`INSERT INTO checkpoint_publish_attempts(id,run_id,branch,ref,tip,checks,last_error)
			VALUES($1,$2,$3,'refs/uzi-checkpoints/'||$3,$4,2,'fixture pending publication')`,
			uuid.New(), id, branch, strings.Repeat("b", 40))
	}
	assertSQL("exhaustion holds must not already have terminal retention rows",
		"SELECT NOT EXISTS(SELECT 1 FROM checkpoint_retentions WHERE run_id IN ($1,$2))", held, noHold)

	// Obsolete causes on non-held rows must only lose the cause and retry.
	nonheld := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range nonheld {
		status := []string{"queued", "running", "failed"}[i]
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,issue_iid,status,
			recovery_wait_cause,recovery_retry_not_before,failure_reason,fail_origin,finished_at,status_since,updated_at)
			VALUES($1,$2,$3,'issue','nonheld','fixture',$5,$4,'worker_requeue_exhausted',
			now()+interval '2 hours','existing reason','worker_lost',
			CASE WHEN $4='failed' THEN '2001-01-01'::timestamptz END,'2001-01-01','2001-01-01')`,
			id, user, repo, status, i+3)
	}
	otherWaits := make([]uuid.UUID, 0, 6)
	for i, cause := range []string{"forge_unreachable", "empty_turn", "provider_outage", "codex_account_unavailable", "vault_locked", "data_volume_full"} {
		id := uuid.New()
		otherWaits = append(otherWaits, id)
		exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_title,issue_description,issue_iid,status,
			recovery_wait_cause,recovery_retry_not_before,status_since,updated_at)
			VALUES($1,$2,$3,'issue','other wait','fixture',$5,'recovery_wait',$4,
			now()+interval '3 hours','2001-01-01','2001-01-01')`, id, user, repo, cause, i+6)
	}

	// Compare every surviving field except those the rollback policy permits to change.
	const survivingRow = `SELECT (to_jsonb(r)-ARRAY['worker_recovery_episode','requeue_episode_baseline','worker_recovery_evidence'])::text FROM runs r WHERE id=$1`
	const nonheldRow = `SELECT (to_jsonb(r)-ARRAY['worker_recovery_episode','requeue_episode_baseline','worker_recovery_evidence','recovery_wait_cause','recovery_retry_not_before'])::text FROM runs r WHERE id=$1`
	const heldRow = `SELECT (to_jsonb(r)-ARRAY['worker_recovery_episode','requeue_episode_baseline','worker_recovery_evidence',
		'status','fail_origin','failure_reason','finished_at','status_since','updated_at','recovery_wait_cause','recovery_retry_not_before'])::text FROM runs r WHERE id=$1`
	before := make(map[uuid.UUID]string)
	for _, id := range nonheld {
		before[id] = snapshot(nonheldRow, id)
	}
	for _, id := range otherWaits {
		before[id] = snapshot(survivingRow, id)
	}
	for _, id := range []uuid.UUID{held, noHold} {
		before[id] = snapshot(heldRow, id)
	}
	const holdRow = "SELECT to_jsonb(h)::text FROM recovery_custody_holds h WHERE id=$1"
	const captureRow = "SELECT to_jsonb(c)::text FROM recovery_captures c WHERE id=$1"
	holdBefore, captureBefore := snapshot(holdRow, hold), snapshot(captureRow, capture)
	const attemptRow = "SELECT to_jsonb(p)::text FROM checkpoint_publish_attempts p WHERE id=$1"
	attemptBefore := snapshot(attemptRow, attempt)
	const attemptsRow = "SELECT jsonb_agg(to_jsonb(a) ORDER BY id)::text FROM checkpoint_publish_attempts a WHERE run_id=$1"
	attemptsBefore := map[uuid.UUID]string{
		held: snapshot(attemptsRow, held), noHold: snapshot(attemptsRow, noHold),
	}

	for _, stmt := range migrationDownStatements(t, "00309_worker_recovery_episode.sql") {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("DownMustTerminalizeExhaustedRecoveryHoldsBeforeNarrowingCauseCheck: actual 00309 Down rejected a production held row: %v", err)
		}
	}
	assertSQL("DownMustDropEpisodeColumns", `SELECT count(*)=0 FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='runs'
		AND column_name IN ('worker_recovery_episode','requeue_episode_baseline','worker_recovery_evidence')`)
	for _, id := range []uuid.UUID{held, noHold} {
		assertSQL("DownMustTerminalizeExhaustedRecoveryHolds",
			`SELECT status='failed' AND fail_origin='worker_lost' AND position('rollback' in lower(failure_reason))>0
			AND finished_at=now() AND status_since=now() AND updated_at=now()
			AND recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL FROM runs WHERE id=$1`, id)
		assertSQL("DownMustPreserveReleasedClaimFences",
			`SELECT claim_generation=7 AND claim_released_at IS NOT NULL AND worker_id=$2
				AND released_worker_id=$2 AND released_worker_nonce=$3 FROM runs WHERE id=$1`, id, worker, nonce)
		if snapshot(heldRow, id) != before[id] || snapshot(attemptsRow, id) != attemptsBefore[id] {
			t.Fatal("DownMustPreserveCheckpointAndReleasedClaimFields")
		}
		state := "settling"
		if id == held {
			state = "retained"
		}
		assertSQL("DownMustFireTerminalRetentionTrigger",
			`SELECT EXISTS(SELECT 1 FROM checkpoint_retentions WHERE run_id=$1 AND state=$2
			AND tip=$3 AND branch=$4 AND ref='refs/uzi-checkpoints/'||$4
			AND user_id=$5 AND repo_id=$6)`, id, state, tip,
			map[uuid.UUID]string{held: "agent/issue-1", noHold: "agent/issue-2"}[id], user, repo)
	}
	for _, id := range nonheld {
		assertSQL("DownMustClearOnlyObsoleteCauseAndRetryOnNonheldRows",
			"SELECT recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL FROM runs WHERE id=$1", id)
		if snapshot(nonheldRow, id) != before[id] {
			t.Fatal("DownMustPreserveNonheldRowFields")
		}
	}
	for _, id := range otherWaits {
		if snapshot(survivingRow, id) != before[id] {
			t.Fatal("DownMustPreserveOtherRecoveryWaitCausesAndRows")
		}
	}
	if snapshot(holdRow, hold) != holdBefore || snapshot(captureRow, capture) != captureBefore {
		t.Fatal("DownMustPreserveCustodyAndCaptureRows")
	}

	if snapshot(attemptRow, attempt) != attemptBefore {
		t.Fatal("DownMustPreservePublicationAttempt")
	}
	assertSQL("DownMustNotStrandExhaustionHoldsInUntimedWait",
		`SELECT NOT EXISTS(SELECT 1 FROM runs WHERE id IN ($1,$2)
		AND status='recovery_wait' AND recovery_wait_cause IS NULL AND recovery_retry_not_before IS NULL)`, held, noHold)

	// Probe the narrower CHECK in a savepoint so the expected rejection cannot
	// abort the outer transaction needed for Up replay.
	probe, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, checkErr := probe.Exec(ctx, "UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1", nonheld[0])
	if err := probe.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(checkErr, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "runs_recovery_wait_cause_check" {
		t.Fatalf("DownMustNarrowCauseCheck: got %v", checkErr)
	}

	afterDown := make(map[uuid.UUID]string)
	all := append([]uuid.UUID{held, noHold}, nonheld...)
	all = append(all, otherWaits...)
	for _, id := range all {
		afterDown[id] = snapshot(survivingRow, id)
	}
	for _, stmt := range migrationUpStatements(t, "00309_worker_recovery_episode.sql") {
		exec(stmt)
	}
	for _, id := range all {
		assertSQL("UpMustRestoreEpisodeDefaultsWithoutEvidence",
			`SELECT worker_recovery_episode=0 AND requeue_episode_baseline=0
			AND worker_recovery_evidence IS NULL FROM runs WHERE id=$1`, id)
		if want, ok := attemptsBefore[id]; ok && snapshot(attemptsRow, id) != want {
			t.Fatal("UpMustPreservePublicationAttempts")
		}
		if snapshot(survivingRow, id) != afterDown[id] {
			t.Fatal("UpMustNotReverseTerminalizationOrChangeSurvivingFields")
		}
	}
	if snapshot(holdRow, hold) != holdBefore || snapshot(captureRow, capture) != captureBefore {
		t.Fatal("UpMustPreserveCustodyAndCaptureRows")
	}
	if snapshot(attemptRow, attempt) != attemptBefore {
		t.Fatal("UpMustPreservePublicationAttempt")
	}
}
