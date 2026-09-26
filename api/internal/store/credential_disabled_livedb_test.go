package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCredentialDisabledParkPromoteLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)
	mustExec(ctx, t, pool, `UPDATE workers SET snapshot_register_nonce='incarnation-a' WHERE id=$1`, worker)
	run := insertCodexRun(ctx, t, pool, user, repo, worker, 901, "claimed", "credential park")
	started := time.Now().Add(-20 * time.Second).UTC().Truncate(time.Second)
	mustExec(ctx, t, pool, `UPDATE runs SET started_at=$2, status_since=$2, claim_generation=7,
        session_id='resume-session', budget_wall_seconds=60, budget_paused_seconds=3,
        codex_cap_hash=decode('aabb','hex'), codex_claim_epoch=4,
        pause_requested_at=now(), pause_mode='now', recovery_wait_count=2,
        claimed_worker_nonce='incarnation-a'
        WHERE id=$1`, run, started)
	mustExec(ctx, t, pool, `UPDATE workers SET snapshot_register_nonce='incarnation-b' WHERE id=$1`, worker)
	pgWorker := pgtype.UUID{Bytes: worker, Valid: true}
	park := func(id uuid.UUID, w pgtype.UUID, gen int64) int64 {
		t.Helper()
		n, err := q.ParkCredentialDisabledRun(ctx, store.ParkCredentialDisabledRunParams{ID: id, WorkerID: w, ClaimGeneration: gen})
		if err != nil {
			t.Fatalf("park: %v", err)
		}
		return n
	}
	released := insertCodexRun(ctx, t, pool, user, repo, worker, 902, "claimed", "released claim")
	mustExec(ctx, t, pool, `UPDATE runs SET claim_generation=7, claim_released_at=now() WHERE id=$1`, released)
	if n := park(released, pgWorker, 7); n != 0 {
		t.Fatalf("released claim parked %d rows", n)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET claim_released_at=NULL, hold_reason='completion_blocked' WHERE id=$1`, released)
	if n := park(released, pgWorker, 7); n != 0 {
		t.Fatalf("existing hold parked %d rows", n)
	}
	// A delivered flight is never parked (D3): only an undelivered 'claimed' claim is.
	for i, flying := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		flight := insertCodexRun(ctx, t, pool, user, repo, worker, int64(910+i), flying, "flight")
		mustExec(ctx, t, pool, `UPDATE runs SET claim_generation=7 WHERE id=$1`, flight)
		if n := park(flight, pgWorker, 7); n != 0 {
			t.Fatalf("%s flight parked %d rows", flying, n)
		}
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
	var ownerWorker pgtype.UUID
	var pauseAt pgtype.Timestamptz
	var recoveryCount int32
	var claimReleased pgtype.Timestamptz
	read := func() {
		t.Helper()
		ownerWorker = pgtype.UUID{}
		err := pool.QueryRow(ctx, `SELECT status, hold_reason, session_id, started_at,
            budget_paused_seconds, codex_claim_epoch, codex_cap_hash, worker_id,
            pause_requested_at, pause_mode, recovery_wait_count, claim_released_at FROM runs WHERE id=$1`, run).
			Scan(&status, &hold, &session, &gotStarted, &bank, &epoch, &cap, &ownerWorker, &pauseAt, &pauseMode, &recoveryCount, &claimReleased)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
	}
	read()
	var releasedID uuid.UUID
	var releasedNonce string
	if err := pool.QueryRow(ctx, `SELECT released_worker_id, released_worker_nonce FROM runs WHERE id=$1`, run).Scan(&releasedID, &releasedNonce); err != nil {
		t.Fatalf("read released worker: %v", err)
	}
	if releasedID != worker || releasedNonce != "incarnation-a" {
		t.Fatalf("released incarnation = %s/%q, want %s/incarnation-a", releasedID, releasedNonce, worker)
	}
	if status != "paused" || (!hold.Valid || hold.String != "credential_disabled") || bank != 3 || epoch != 5 || cap != nil ||
		!gotStarted.Equal(started) || session != "resume-session" || !ownerWorker.Valid || ownerWorker.Bytes != worker ||
		pauseMode != "now" || recoveryCount != 2 || !pauseAt.Valid || !claimReleased.Valid {
		t.Fatalf("park mutated protected state: status=%s hold=%v bank=%d epoch=%d cap=%x started=%v session=%s worker=%s pause=%s recovery=%d", status, hold, bank, epoch, cap, gotStarted, session, ownerWorker, pauseMode, recoveryCount)
	}
	if n, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: run, WorkerID: pgWorker}); err != nil || n != 0 {
		t.Fatalf("stale running report: rows=%d err=%v", n, err)
	}
	if n, err := q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{ID: run, WorkerID: pgWorker}); err != nil || n != 0 {
		t.Fatalf("stale awaiting-input report: rows=%d err=%v", n, err)
	}
	if n, err := q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: run, WorkerID: pgWorker}); err != nil || n != 0 {
		t.Fatalf("stale cancel report: rows=%d err=%v", n, err)
	}
	if _, err := q.ResumePausedRun(ctx, store.ResumePausedRunParams{ID: run, UserID: user, GlobalTimeoutSeconds: 60}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("generic resume bypassed credential hold: %v", err)
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
		_, err := q.PromoteCredentialDisabledRun(ctx, promoteParams(ctx, t, pool, run, owner, 60))
		return err
	}
	// The requirement guards: a promotion evaluated against a requirement that has since
	// changed (a different override, alias or recorded worker) matches no row.
	for name, skew := range map[string]func(*store.PromoteCredentialDisabledRunParams){
		"override mode": func(p *store.PromoteCredentialDisabledRunParams) {
			p.ExpectedOverrideMode = pgtype.Text{String: "pinned", Valid: true}
		},
		"override secret": func(p *store.PromoteCredentialDisabledRunParams) {
			p.ExpectedOverrideSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		},
		"codex alias": func(p *store.PromoteCredentialDisabledRunParams) {
			p.ExpectedCodexSecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		},
		"worker": func(p *store.PromoteCredentialDisabledRunParams) { p.ExpectedWorkerID = pgtype.UUID{} },
		"released worker": func(p *store.PromoteCredentialDisabledRunParams) {
			p.ExpectedReleasedWorkerID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		},
	} {
		mustExec(ctx, t, pool, `UPDATE runs SET pause_requested_at=NULL WHERE id=$1`, run)
		arg := promoteParams(ctx, t, pool, run, user, 60)
		skew(&arg)
		if _, err := q.PromoteCredentialDisabledRun(ctx, arg); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("stale %s requirement promoted: %v", name, err)
		}
		mustExec(ctx, t, pool, `UPDATE runs SET pause_requested_at=now() WHERE id=$1`, run)
	}
	if err := promote(uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign promotion: %v", err)
	}
	if err := promote(user); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pending owner pause bypassed: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET pause_requested_at=NULL WHERE id=$1`, run)
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
		session != "resume-session" || ownerWorker.Valid || pauseMode != "now" || recoveryCount != 2 {
		t.Fatalf("promotion state: status=%s hold=%v bank=%d epoch=%d session=%s worker=%s worker_valid=%t released=%t pause=%s recovery=%d", status, hold, bank, epoch, session, ownerWorker, ownerWorker.Valid, claimReleased.Valid, pauseMode, recoveryCount)
	}
	if err := promote(user); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("repeat promotion: %v", err)
	}
}

func TestCredentialDisabledSameWorkerReclaimNeedsGenerationLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	worker := fx.worker("reclaim", wpWorker{nonce: "incarnation-a"})
	run := fx.run(wpRun{status: "claimed", worker: &worker, claimGen: 4, budgetWall: p32(36000)})
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claimed_worker_nonce='incarnation-a' WHERE id=$1`, run)
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET snapshot_register_nonce='incarnation-b' WHERE id=$1`, worker)
	if n, err := fx.q.ParkCredentialDisabledRun(fx.ctx, store.ParkCredentialDisabledRunParams{
		ID: run, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 4,
	}); err != nil || n != 1 {
		t.Fatalf("park old incarnation: rows=%d err=%v", n, err)
	}
	if _, err := fx.q.PromoteCredentialDisabledRun(fx.ctx, promoteParams(fx.ctx, t, fx.pool, run, fx.userID, 36000)); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := fx.claim(worker, "incarnation-b", false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("legacy same-ID reclaim = %v, want refused", err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=ARRAY['credential_switch_v1']::text[] WHERE id=$1`, worker)
	claimed, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: fx.userID,
		HeartbeatCutoff: wpAgo(45 * time.Second), AffinityCutoff: wpAgo(2 * time.Minute),
		SpreadCutoff: wpAgo(9 * time.Second), SnapshotFreshCutoff: wpAgo(45 * time.Second),
		WorkerIdentity: "incarnation-b", WorkerProtocolCaps: []string{"credential_switch_v1"},
	})
	if err != nil || claimed.ID != run || claimed.ClaimedWorkerNonce.String != "incarnation-b" {
		t.Fatalf("generation-capable reclaim: run=%s nonce=%v err=%v", claimed.ID, claimed.ClaimedWorkerNonce, err)
	}
}

func TestCredentialDisabledWorklistCursorLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)
	older := insertCodexRun(ctx, t, pool, user, repo, worker, 903, "running", "budget blocked")
	newer := insertCodexRun(ctx, t, pool, user, repo, worker, 904, "running", "promotable")
	for _, id := range []uuid.UUID{older, newer} {
		mustExec(ctx, t, pool, `UPDATE runs SET status='paused', hold_reason='credential_disabled',
            claim_released_at=now(), started_at=now()-interval '20 seconds',
            budget_wall_seconds=60 WHERE id=$1`, id)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET started_at=now()-interval '200 seconds',
        status_since=now()-interval '120 seconds' WHERE id=$1`, older)
	mustExec(ctx, t, pool, `UPDATE runs SET status_since=now()-interval '10 seconds' WHERE id=$1`, newer)
	first, err := q.ListCredentialDisabledRuns(ctx, store.ListCredentialDisabledRunsParams{UserID: user, PageSize: 1})
	if err != nil || len(first) != 1 || first[0].ID != older {
		t.Fatalf("first page: %v, %v", first, err)
	}
	if _, err := q.PromoteCredentialDisabledRun(ctx, promoteParams(ctx, t, pool, older, user, 60)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("budget-blocked oldest row: %v", err)
	}
	second, err := q.ListCredentialDisabledRuns(ctx, store.ListCredentialDisabledRunsParams{
		UserID: user, PageSize: 1, AfterStatusSince: first[0].StatusSince,
		AfterID: pgtype.UUID{Bytes: first[0].ID, Valid: true},
	})
	if err != nil || len(second) != 1 || second[0].ID != newer {
		t.Fatalf("second page: %v, %v", second, err)
	}
	if _, err := q.PromoteCredentialDisabledRun(ctx, promoteParams(ctx, t, pool, newer, user, 60)); err != nil {
		t.Fatalf("promote second page: %v", err)
	}
}

// promoteParams reads the run's current requirement columns as the promoter's locked re-read
// would, so a promotion here is guarded on exactly the stored requirement.
func promoteParams(ctx context.Context, t *testing.T, pool *pgxpool.Pool, run, owner uuid.UUID, global int32) store.PromoteCredentialDisabledRunParams {
	t.Helper()
	arg := store.PromoteCredentialDisabledRunParams{ID: run, UserID: owner, GlobalTimeoutSeconds: global}
	if err := pool.QueryRow(ctx, `SELECT credential_override_mode, credential_override_secret_id, codex_secret_id,
	    worker_id, credential_disable_released_worker_id FROM runs WHERE id=$1`, run).Scan(
		&arg.ExpectedOverrideMode, &arg.ExpectedOverrideSecretID, &arg.ExpectedCodexSecretID,
		&arg.ExpectedWorkerID, &arg.ExpectedReleasedWorkerID); err != nil {
		t.Fatalf("read requirement columns: %v", err)
	}
	return arg
}

// TestCredentialDisabledUsersWorklistLiveDB: the Sweep fallback's owner worklist pages owners
// with a held run in uuid order and omits owners whose runs are not held on this reason.
func TestCredentialDisabledUsersWorklistLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)
	held := insertCodexRun(ctx, t, pool, user, repo, worker, 920, "running", "held")
	mustExec(ctx, t, pool, `UPDATE runs SET status='paused', hold_reason='credential_disabled' WHERE id=$1`, held)
	var all []uuid.UUID
	var after pgtype.UUID
	for {
		page, err := q.ListCredentialDisabledUsers(ctx, store.ListCredentialDisabledUsersParams{AfterUserID: after, PageSize: 1})
		if err != nil {
			t.Fatalf("owners page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		if after.Valid && uuid.UUID(after.Bytes).String() >= page[0].String() {
			t.Fatalf("owner page not ascending: %s after %s", page[0], uuid.UUID(after.Bytes))
		}
		all = append(all, page...)
		after = pgtype.UUID{Bytes: page[len(page)-1], Valid: true}
	}
	found := 0
	for _, u := range all {
		if u == user {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("owner %s listed %d times across pages %v, want once", user, found, all)
	}
	mustExec(ctx, t, pool, `UPDATE runs SET hold_reason='completion_blocked' WHERE id=$1`, held)
	page, err := q.ListCredentialDisabledUsers(ctx, store.ListCredentialDisabledUsersParams{
		AfterUserID: pgtype.UUID{Bytes: user, Valid: false}, PageSize: 1000,
	})
	if err != nil {
		t.Fatalf("owners: %v", err)
	}
	for _, u := range page {
		if u == user {
			t.Fatal("an owner with no credential_disabled hold was listed")
		}
	}
}
