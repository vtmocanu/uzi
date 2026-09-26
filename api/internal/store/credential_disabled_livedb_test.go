package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCredentialDisabledParkPromoteLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)
	run := insertCodexRun(ctx, t, pool, user, repo, worker, 901, "running", "credential park")
	started := time.Now().Add(-20 * time.Second).UTC().Truncate(time.Second)
	mustExec(ctx, t, pool, `UPDATE runs SET started_at=$2, status_since=$2, claim_generation=7,
        session_id='resume-session', budget_wall_seconds=60, budget_paused_seconds=3,
        codex_cap_hash=decode('aabb','hex'), codex_claim_epoch=4,
        pause_requested_at=now(), pause_mode='now', recovery_wait_count=2
        WHERE id=$1`, run, started)
	pgWorker := pgtype.UUID{Bytes: worker, Valid: true}
	park := func(id uuid.UUID, w pgtype.UUID, gen int64) int64 {
		t.Helper()
		n, err := q.ParkCredentialDisabledRun(ctx, store.ParkCredentialDisabledRunParams{ID: id, WorkerID: w, ClaimGeneration: gen})
		if err != nil {
			t.Fatalf("park: %v", err)
		}
		return n
	}
	released := insertCodexRun(ctx, t, pool, user, repo, worker, 902, "running", "released claim")
	mustExec(ctx, t, pool, `UPDATE runs SET claim_generation=7, claim_released_at=now() WHERE id=$1`, released)
	if n := park(released, pgWorker, 7); n != 0 {
		t.Fatalf("released claim parked %d rows", n)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET claim_released_at=NULL, hold_reason='completion_blocked' WHERE id=$1`, released)
	if n := park(released, pgWorker, 7); n != 0 {
		t.Fatalf("existing hold parked %d rows", n)
	}
	if n := park(run, pgWorker, 6); n != 0 {
		t.Fatalf("stale generation parked %d rows", n)
	}
	if n := park(run, pgtype.UUID{Bytes: uuid.New(), Valid: true}, 7); n != 0 {
		t.Fatalf("wrong worker parked %d rows", n)
	}
	if n := park(run, pgWorker, 7); n != 1 {
		t.Fatalf("valid claim parked %d rows", n)
	}
	if n := park(run, pgWorker, 7); n != 0 {
		t.Fatalf("repeat park moved %d rows", n)
	}
	var status, session, pauseMode string
	var hold pgtype.Text
	var gotStarted time.Time
	var bank int32
	var epoch int64
	var cap []byte
	var ownerWorker uuid.UUID
	var pauseAt time.Time
	var recoveryCount int32
	read := func() {
		t.Helper()
		err := pool.QueryRow(ctx, `SELECT status, hold_reason, session_id, started_at,
            budget_paused_seconds, codex_claim_epoch, codex_cap_hash, worker_id,
            pause_requested_at, pause_mode, recovery_wait_count FROM runs WHERE id=$1`, run).
			Scan(&status, &hold, &session, &gotStarted, &bank, &epoch, &cap, &ownerWorker, &pauseAt, &pauseMode, &recoveryCount)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
	}
	read()
	if status != "paused" || (!hold.Valid || hold.String != "credential_disabled") || bank != 3 || epoch != 5 || cap != nil ||
		!gotStarted.Equal(started) || session != "resume-session" || ownerWorker != worker ||
		pauseMode != "now" || recoveryCount != 2 || pauseAt.IsZero() {
		t.Fatalf("park mutated protected state: status=%s hold=%v bank=%d epoch=%d cap=%x started=%v session=%s worker=%s pause=%s recovery=%d", status, hold, bank, epoch, cap, gotStarted, session, ownerWorker, pauseMode, recoveryCount)
	}
	list, err := q.ListCredentialDisabledRuns(ctx, store.ListCredentialDisabledRunsParams{UserID: user, PageSize: 1})
	if err != nil || len(list) != 1 || list[0].ID != run {
		t.Fatalf("worklist = %v, %v", list, err)
	}
	list, err = q.ListCredentialDisabledRuns(ctx, store.ListCredentialDisabledRunsParams{UserID: uuid.New(), PageSize: 1})
	if err != nil || len(list) != 0 {
		t.Fatalf("foreign worklist = %v, %v", list, err)
	}
	promote := func(owner uuid.UUID) error {
		t.Helper()
		_, err := q.PromoteCredentialDisabledRun(ctx, store.PromoteCredentialDisabledRunParams{ID: run, UserID: owner, GlobalTimeoutSeconds: 60})
		return err
	}
	if err := promote(uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign promotion: %v", err)
	}
	// A spent active budget must block promotion, even after a long held interval.
	mustExec(ctx, t, pool, `UPDATE runs SET started_at=now()-interval '120 seconds',
        status_since=now()-interval '10 seconds', budget_wall_seconds=60 WHERE id=$1`, run)
	if err := promote(user); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exhausted promotion: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET started_at=now()-interval '20 seconds',
        status_since=now()-interval '10 seconds' WHERE id=$1`, run)
	if err := promote(user); err != nil {
		t.Fatalf("promote: %v", err)
	}
	read()
	if status != "queued" || hold.Valid || bank < 12 || bank > 15 || epoch != 6 || cap != nil ||
		session != "resume-session" || ownerWorker != worker || pauseMode != "now" || recoveryCount != 2 {
		t.Fatalf("promotion state: status=%s hold=%v bank=%d epoch=%d session=%s worker=%s pause=%s recovery=%d", status, hold, bank, epoch, session, ownerWorker, pauseMode, recoveryCount)
	}
	if err := promote(user); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("repeat promotion: %v", err)
	}
}
