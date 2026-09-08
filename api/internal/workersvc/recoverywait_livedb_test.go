package workersvc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRecoveryWaitLiveDB exercises the transient-recovery park (issue #1197) end to end
// against a REAL Postgres — the parts the pure-Go unit tests structurally cannot answer,
// because the park's whole contract lives in migration 00203 and the two queries' SQL:
//
//	(1) migration 00203 applies and its WIDENED runs_status_check ACCEPTS the twelfth
//	    member 'recovery_wait' (and the two new columns round-trip);
//	(2) SetRunRecoveryWait on a running autopilot row parks it: status='recovery_wait',
//	    recovery_wait_count incremented, recovery_retry_not_before set, and plan_md /
//	    session_id / worker_id / checkpoint_tip all PRESERVED (non-terminal ⇒ nothing
//	    terminal-cleaned);
//	(3) REPEATED parks bump recovery_wait_count monotonically (1,2,3…), each non-terminal
//	    with plan_md preserved — the "no lifetime cap" property;
//	(4) PromoteRecoveryWaitRuns promotes a due run (recovery_retry_not_before <= now) to
//	    'queued' with session_id/worker_id preserved (affinity) and started_at reset, and
//	    does NOT promote a run whose stamp is still in the FUTURE;
//	(5) an owner CancelRunServerSide ends a recovery_wait run at 'cancelled' (the negative
//	    admit-set covers the new park for free);
//	(6) park→promote→svc.Claim round-trips: the claim carries the preserved plan_md with
//	    PlanApproved=true, so a recovered autopilot run enters implementation, not re-plan.
//
// Non-vacuity: (2)-(3) re-read the row and assert the preserved columns did NOT move; a raw
// UPDATE to a bogus status must FAIL 23514 on runs_status_check, proving 'recovery_wait' is
// green because the CHECK ADMITS it, not because no constraint exists.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness). A package that prints `ok` with PASS=0 is
// INVALID, not green.
func TestRecoveryWaitLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	// Schema at HEAD — HEAD includes 00203_run_recovery_wait.sql. If its CHECK re-add or a
	// column add did not parse/apply, Migrate fails here, not on a later assert.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	box := newBox(t)
	q := store.New(pool)
	svc := New(q, box, testParams())

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	sealedPAT, err := box.Seal([]byte("bot-pat-1197-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal PAT: %v", err)
	}
	sealedAnthropic, err := box.Seal([]byte("anthropic-1197-token-abcdef1234567890"))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rw-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, sealedPAT)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/rw', 'https://forge.e2e/g/rw', 'main', true)`, repoID, connID)
	exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), sealedAnthropic)

	wkrID := uuid.New()
	tokenHash := wkrID[:] // unique per run (workers_token_hash_key); Migrate does not truncate
	exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'w-rw', $3, 'offline')`,
		wkrID, userID, tokenHash)
	if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: wkrID}); err != nil {
		t.Fatalf("RegisterWorker: %v", err)
	}
	wkr, err := q.GetWorkerByID(ctx, wkrID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}

	const planBody = "## Approved plan\n1. recover\n2. implement\n"
	const sessionID = "sess-1197-abcdef"
	const checkpointTip = "0123456789abcdef0123456789abcdef01234567"

	// seedRun inserts one running autopilot issue run owned by wkr, with plan_md/session_id/
	// checkpoint_tip set so the "preserved across the park" assertions are non-vacuous.
	// Distinct issue_iid per call so uq_runs_one_active_per_issue never collides.
	seedRun := func(issueIID int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		         status, worker_id, auto_approve, plan_source, plan_md, session_id, checkpoint_tip, started_at)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5, true, 'agent', $6, $7, $8, now())`,
			id, userID, repoID, issueIID, wkrID, planBody, sessionID, checkpointTip)
		return id
	}
	reread := func(id uuid.UUID) store.Run {
		t.Helper()
		r, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatalf("GetRunByID(%s): %v", id, err)
		}
		return r
	}
	park := func(id uuid.UUID, retryNotBefore time.Time) int64 {
		t.Helper()
		rows, err := q.SetRunRecoveryWait(ctx, store.SetRunRecoveryWaitParams{
			RetryNotBefore: pgconv.Time(retryNotBefore),
			SessionID:      pgconv.TextOrNull(sessionID),
			ID:             id,
			WorkerID:       pgconv.UUID(wkrID),
		})
		if err != nil {
			t.Fatalf("SetRunRecoveryWait(%s): %v", id, err)
		}
		return rows
	}

	now := time.Now().UTC()

	t.Run("park preserves plan/session/worker/checkpoint and stamps the retry", func(t *testing.T) {
		id := seedRun(1)
		// Postgres timestamptz stores MICROSECOND precision, so truncate the expected stamp to
		// microseconds before it is both STORED and COMPARED — otherwise the sub-microsecond
		// remainder of a nanosecond-precision time.Now() is lost in the round-trip and .Equal
		// fails every run (same convention as store/schedule_pause_livedb_test.go).
		retry := now.Add(time.Minute).Truncate(time.Microsecond)
		if rows := park(id, retry); rows != 1 {
			t.Fatalf("park rows = %d, want 1 (the running autopilot row must park)", rows)
		}
		r := reread(id)
		if r.Status != "recovery_wait" {
			t.Fatalf("status = %q, want recovery_wait", r.Status)
		}
		if r.RecoveryWaitCount != 1 {
			t.Fatalf("recovery_wait_count = %d, want 1 (bumped in the same statement as the transition)", r.RecoveryWaitCount)
		}
		if !r.RecoveryRetryNotBefore.Valid || !r.RecoveryRetryNotBefore.Time.Equal(retry) {
			t.Fatalf("recovery_retry_not_before = %v, want the stamped %v", r.RecoveryRetryNotBefore, retry)
		}
		if !r.PlanMd.Valid || r.PlanMd.String != planBody {
			t.Fatalf("plan_md = {valid=%t %q}, want it preserved — a recovery park must not lose the approved plan", r.PlanMd.Valid, r.PlanMd.String)
		}
		if !r.SessionID.Valid || r.SessionID.String != sessionID {
			t.Fatalf("session_id = {valid=%t %q}, want it preserved", r.SessionID.Valid, r.SessionID.String)
		}
		if !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != wkrID {
			t.Fatalf("worker_id lost; affinity must survive the park")
		}
		if !r.CheckpointTip.Valid || r.CheckpointTip.String != checkpointTip {
			t.Fatalf("checkpoint_tip = {valid=%t %q}, want it preserved — a non-terminal park does not clean up the checkpoint", r.CheckpointTip.Valid, r.CheckpointTip.String)
		}
	})

	t.Run("a stale approval report cannot unpark or replace the recovery plan", func(t *testing.T) {
		id := seedRun(8)
		if rows := park(id, now.Add(time.Hour)); rows != 1 {
			t.Fatalf("park rows = %d, want 1", rows)
		}
		before := reread(id)
		rows, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd:    pgconv.TextOrNull("stale pre-park plan"),
			SessionID: pgconv.TextOrNull("stale pre-park session"),
			ID:        id,
			WorkerID:  pgconv.UUID(wkrID),
		})
		if err != nil {
			t.Fatalf("SetRunAwaitingApproval: %v", err)
		}
		if rows != 0 {
			t.Fatalf("stale approval report rows = %d, want 0; recovery_wait may exit only through server promotion or owner cancel", rows)
		}
		after := reread(id)
		if after.Status != "recovery_wait" || after.PlanMd != before.PlanMd ||
			after.PlanSource != before.PlanSource || after.AutoApprove != before.AutoApprove ||
			after.SessionID != before.SessionID || after.RecoveryWaitCount != before.RecoveryWaitCount ||
			after.RecoveryRetryNotBefore != before.RecoveryRetryNotBefore || after.UpdatedAt != before.UpdatedAt {
			t.Fatal("a refused approval report mutated the recovery hold or its preserved plan/session")
		}
	})

	t.Run("repeated parks increment the count monotonically", func(t *testing.T) {
		id := seedRun(2)
		for want := int32(1); want <= 3; want++ {
			if rows := park(id, now.Add(time.Duration(want)*time.Minute)); rows != 1 {
				t.Fatalf("park #%d rows = %d, want 1", want, rows)
			}
			r := reread(id)
			if r.RecoveryWaitCount != want {
				t.Fatalf("after park #%d recovery_wait_count = %d, want %d — no lifetime cap, so it just keeps counting", want, r.RecoveryWaitCount, want)
			}
			if !r.PlanMd.Valid || r.PlanMd.String != planBody {
				t.Fatalf("park #%d dropped plan_md; each park stays non-terminal with the plan preserved", want)
			}
			// Raw-set back to running so the POSITIVE source guard (status='running') admits
			// the next park — the sweeper does this via promotion in production.
			exec(`UPDATE runs SET status = 'running' WHERE id = $1`, id)
		}
	})

	t.Run("promote brings a due run to queued and skips a future one", func(t *testing.T) {
		due := seedRun(3)
		if rows := park(due, now.Add(-time.Minute)); rows != 1 { // already elapsed
			t.Fatalf("due park rows = %d, want 1", rows)
		}
		future := seedRun(4)
		if rows := park(future, now.Add(time.Hour)); rows != 1 { // far out
			t.Fatalf("future park rows = %d, want 1", rows)
		}

		promoted, err := q.PromoteRecoveryWaitRuns(ctx, pgconv.Time(now))
		if err != nil {
			t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
		}
		var sawDue bool
		for _, p := range promoted {
			if p.ID == due {
				sawDue = true
			}
			if p.ID == future {
				t.Fatal("promoted a run whose recovery_retry_not_before is still in the future")
			}
		}
		if !sawDue {
			t.Fatal("the due run was not promoted; recovery_retry_not_before <= now must promote it")
		}
		r := reread(due)
		if r.Status != "queued" {
			t.Fatalf("promoted run status = %q, want queued", r.Status)
		}
		if r.StartedAt.Valid {
			t.Fatal("started_at was not reset on promotion; the resumed run must get a fresh RUN_TIMEOUT wall")
		}
		if !r.SessionID.Valid || r.SessionID.String != sessionID {
			t.Fatalf("session_id lost across promotion; the resume must keep it (%v)", r.SessionID)
		}
		if !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != wkrID {
			t.Fatal("worker_id lost across promotion; affinity must survive")
		}
		if reread(future).Status != "recovery_wait" {
			t.Fatal("the future-stamped run left recovery_wait; it must stay parked")
		}
	})

	t.Run("owner cancel ends a recovery-parked run", func(t *testing.T) {
		id := seedRun(5)
		if rows := park(id, now.Add(time.Hour)); rows != 1 {
			t.Fatalf("park rows = %d, want 1", rows)
		}
		rows, err := q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
			StopReason: pgconv.TextOrNull("owner cancelled"),
			ID:         id,
			UserID:     userID,
		})
		if err != nil {
			t.Fatalf("CancelRunServerSide: %v", err)
		}
		if rows != 1 {
			t.Fatalf("cancel rows = %d, want 1 — the negative admit-set must cover a recovery_wait run", rows)
		}
		if r := reread(id); r.Status != "cancelled" {
			t.Fatalf("status = %q, want cancelled", r.Status)
		}
	})

	t.Run("park then promote then svc.Claim carries the plan and PlanApproved", func(t *testing.T) {
		// Settle every other run of this user so exactly ONE claimable run exists when
		// svc.Claim runs — earlier subtests leave a queued run (#3) that would otherwise
		// make the claim pick nondeterministically.
		exec(`UPDATE runs SET status = 'completed' WHERE user_id = $1 AND status <> 'completed'`, userID)
		id := seedRun(6)
		if rows := park(id, now.Add(-time.Minute)); rows != 1 {
			t.Fatalf("park rows = %d, want 1", rows)
		}
		if _, err := q.PromoteRecoveryWaitRuns(ctx, pgconv.Time(now)); err != nil {
			t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
		}
		if r := reread(id); r.Status != "queued" {
			t.Fatalf("pre-claim status = %q, want queued", r.Status)
		}

		payload, err := svc.Claim(ctx, wkr)
		if err != nil {
			t.Fatalf("svc.Claim: %v", err)
		}
		if payload == nil {
			t.Fatal("svc.Claim returned nil (idle) — the promoted recovery run was not claimed")
		}
		if payload.RunID != id.String() {
			t.Fatalf("claimed the wrong run: got %s, want %s", payload.RunID, id)
		}
		if payload.PlanMd == nil || *payload.PlanMd != planBody {
			t.Fatalf("ClaimPayload.PlanMd = %v, want the preserved body %q — a recovered run would re-plan without it", payload.PlanMd, planBody)
		}
		if !payload.PlanApproved {
			t.Fatal("ClaimPayload.PlanApproved = false, want true (auto_approve=true) — the recovered autopilot run must enter implementation, not the gate")
		}
	})

	// Non-vacuity: the CHECK is REAL. A raw UPDATE to a bogus status must be rejected 23514,
	// so the recovery_wait writes above cannot be green against an absent/renamed constraint.
	t.Run("the status CHECK admits recovery_wait but rejects a bogus value", func(t *testing.T) {
		id := seedRun(7)
		if rows := park(id, now.Add(time.Minute)); rows != 1 { // recovery_wait accepted
			t.Fatalf("park rows = %d, want 1 — the CHECK must admit recovery_wait", rows)
		}
		_, rawErr := pool.Exec(ctx, `UPDATE runs SET status = 'not_a_status' WHERE id = $1`, id)
		if rawErr == nil {
			t.Fatal("raw UPDATE to status='not_a_status' SUCCEEDED; runs_status_check is NOT enforced — the positive cases are vacuous")
		}
		var pgErr *pgconn.PgError
		if !errors.As(rawErr, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("bogus status rejected with %v, want a CHECK violation (SQLSTATE 23514)", rawErr)
		}
		if pgErr.ConstraintName != "runs_status_check" {
			t.Errorf("CHECK violation on constraint %q, want runs_status_check", pgErr.ConstraintName)
		}
	})
}
