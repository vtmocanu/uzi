package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1497 M1's wall-park store surface: it EXECUTES the new
// SetRunWallPark / ParkRunsAtWall / RequestWallParks / ExtendAndResumeWallPark / StopWallPark /
// RecordWallParkCapturedHead / CountOnlineWorkersClaimableForRun statements, the wall-request guards
// on the pause/resume/extend/complete statements, and the DEVIATION-3 legacy-completion fence,
// against a REAL Postgres. sqlc's type deduction is not Postgres's, so these guarded, multi-CTE
// statements can pass `sqlc generate` yet fail at prepare/execute, and a guard that migrates cleanly
// can still admit the wrong row the first time an UPDATE fires. Skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway Postgres (./e2e/run-store-it.sh provides one).

// wpFixture is a per-test live-DB harness for the wall-park store queries: one user + forge
// connection + repo, plus helpers to seed runs (with full budget/hold/claim control) and workers.
type wpFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	connID uuid.UUID
	repoID uuid.UUID
	iid    int64
}

func newWPFixture(t *testing.T) *wpFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	fx := &wpFixture{t: t, ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), connID: uuid.New(), repoID: uuid.New()}
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		fx.userID, fmt.Sprintf("wallpark-%s@e2e", fx.userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, fx.connID, fx.userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, $3, $4, 'main', true)`, fx.repoID, fx.connID, "g/"+fx.repoID.String(), "https://forge.e2e/g/"+fx.repoID.String())
	return fx
}

// wpRun is the option set for the run helper. Zero values give a running issue run, started 3h ago
// (well past any small budget), status_since 30s ago, at claim_generation 1, with no worker attached.
type wpRun struct {
	status              string
	kind                string
	interactive         bool
	harness             string        // "" leaves the 'claude' default; 'codex' marks a Codex run
	worker              *uuid.UUID    // nil => NULL worker_id
	startedAgo          time.Duration // started_at = now - startedAgo (default 3h)
	sinceAgo            time.Duration // status_since = now - sinceAgo (default 30s)
	claimGen            int64         // default 1
	claimReleased       bool
	budgetWall          *int32
	budgetExtension     int32
	budgetFinalize      int32
	budgetPaused        int32
	completionAttempts  int32
	completionContract  *int32
	milestonesCompleted string         // "" => NULL; e.g. `["m1","m2"]`
	holdReason          string         // "" => NULL
	holdCapturedHead    string         // "" => NULL
	pauseMode           string         // "" => NULL (also arms pause_requested_at when set)
	pauseRequestedAgo   *time.Duration // override pause_requested_at age (default: sinceAgo when pauseMode set)
	releasedWorker      *uuid.UUID
	releasedNonce       string
	requiredCaps        []string
	codexSecret         bool
}

func nullUUID(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// run inserts an issue-shaped run (unless a non-issue kind is given) with the requested budget/hold/
// claim state and returns its id.
func (fx *wpFixture) run(o wpRun) uuid.UUID {
	fx.t.Helper()
	fx.iid++
	id := uuid.New()
	if o.status == "" {
		o.status = "running"
	}
	if o.kind == "" {
		o.kind = "issue"
	}
	if o.startedAgo == 0 {
		o.startedAgo = 3 * time.Hour
	}
	if o.sinceAgo == 0 {
		o.sinceAgo = 30 * time.Second
	}
	if o.claimGen == 0 {
		o.claimGen = 1
	}
	now := time.Now().UTC()
	startedAt := pgtype.Timestamptz{Time: now.Add(-o.startedAgo), Valid: true}
	statusSince := pgtype.Timestamptz{Time: now.Add(-o.sinceAgo), Valid: true}
	var releasedAt any
	if o.claimReleased {
		releasedAt = now
	}
	var pauseAt any
	if o.pauseMode != "" {
		age := o.sinceAgo
		if o.pauseRequestedAgo != nil {
			age = *o.pauseRequestedAgo
		}
		pauseAt = now.Add(-age)
	}
	var harness any
	if o.harness != "" {
		harness = o.harness
	} else {
		harness = "claude"
	}
	var codexSecret any
	if o.codexSecret {
		codexSecret = uuid.New()
	}
	caps := o.requiredCaps
	if caps == nil {
		caps = []string{}
	}
	var milestones any
	if o.milestonesCompleted != "" {
		milestones = o.milestonesCompleted
	}
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (
		id, user_id, repo_id, issue_iid, issue_title, issue_description,
		status, kind, interactive, harness, worker_id, started_at, status_since, claim_generation, claim_released_at,
		budget_wall_seconds, budget_extension_seconds, budget_finalize_seconds, budget_paused_seconds,
		completion_attempts, completion_contract_version, milestones_completed,
		hold_reason, hold_captured_head, pause_requested_at, pause_mode,
		released_worker_id, released_worker_nonce, required_capabilities, codex_secret_id)
		VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
		id, fx.userID, fx.repoID, fx.iid, o.status, o.kind, o.interactive, harness, nullUUID(o.worker),
		startedAt, statusSince, o.claimGen, releasedAt,
		nullInt32(o.budgetWall), o.budgetExtension, o.budgetFinalize, o.budgetPaused,
		o.completionAttempts, nullInt32(o.completionContract), milestones,
		nullText(o.holdReason), nullText(o.holdCapturedHead), pauseAt, nullText(o.pauseMode),
		nullUUID(o.releasedWorker), nullText(o.releasedNonce), caps, codexSecret)
	return id
}

func nullInt32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

// wpWorker is the option set for the worker helper.
type wpWorker struct {
	offline       bool
	heartbeatAgo  time.Duration // last_heartbeat_at = now - heartbeatAgo (default 1s)
	docker        bool
	caps          []string
	protocolCaps  []string
	nonce         string
	draining      bool
	ephemeral     bool
	ephemeralRun  *uuid.UUID
	maxConcurrent *int32
}

func (fx *wpFixture) worker(name string, o wpWorker) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	status := "online"
	if o.offline {
		status = "offline"
	}
	if o.heartbeatAgo == 0 {
		o.heartbeatAgo = time.Second
	}
	hb := pgtype.Timestamptz{Time: time.Now().UTC().Add(-o.heartbeatAgo), Valid: true}
	caps := o.caps
	if caps == nil {
		caps = []string{}
	}
	pcaps := o.protocolCaps
	if pcaps == nil {
		pcaps = []string{}
	}
	var draining any
	if o.draining {
		draining = time.Now().UTC()
	}
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO workers (
		id, user_id, name, token_hash, status, last_heartbeat_at, docker_enabled, capabilities,
		protocol_capabilities, snapshot_register_nonce, draining_since, ephemeral, ephemeral_run_id, max_concurrent_runs)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		id, fx.userID, name, tokenHash(), status, hb, o.docker, caps, pcaps,
		nullText(o.nonce), draining, o.ephemeral, nullUUID(o.ephemeralRun), nullInt32(o.maxConcurrent))
	return id
}

func (fx *wpFixture) status(id uuid.UUID) string {
	fx.t.Helper()
	var s string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id=$1`, id).Scan(&s); err != nil {
		fx.t.Fatalf("read status %s: %v", id, err)
	}
	return s
}

func (fx *wpFixture) mustRun(id uuid.UUID) store.Run {
	fx.t.Helper()
	r, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		fx.t.Fatalf("GetRunByID %s: %v", id, err)
	}
	return r
}

func wpAgo(d time.Duration) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: time.Now().UTC().Add(-d), Valid: true}
}

func wpNow() pgtype.Timestamptz { return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true} }

// unconsumedWall returns how many unconsumed kind='pause' body='wall' inputs the run carries.
func (fx *wpFixture) unconsumedWall(id uuid.UUID) int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause' AND body='wall' AND consumed_at IS NULL`, id).Scan(&n); err != nil {
		fx.t.Fatalf("count unconsumed wall: %v", err)
	}
	return n
}

// --- Scenario 1: SetRunWallPark -------------------------------------------------------------------

// TestSetRunWallParkLiveDB pins the worker's own capture-first park (PRD #1497 D4/D14/D15): it parks
// a requested past-deadline row and a past-deadline UNrequested (pre-attempt trip) row, refuses an
// extended-in-the-window row even while pause_mode='wall', and refuses a post-attempt row. Mutation:
// the completion_attempts=0 guard is the sole thing keeping a post-attempt row out of this park.
func TestSetRunWallParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	wkr := fx.worker("w", wpWorker{})
	wid := pgtype.UUID{Bytes: wkr, Valid: true}
	gen := int64(1)
	call := func(id uuid.UUID) (store.Run, error) {
		return fx.q.SetRunWallPark(fx.ctx, store.SetRunWallParkParams{
			ID: id, WorkerID: wid, HoldCapturedHead: pgtype.Text{String: "abc123", Valid: true},
			Now: wpNow(), GlobalTimeoutSeconds: 7200, ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}})
	}

	t.Run("parks a requested past-deadline row", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, budgetWall: p32(60), pauseMode: "wall"})
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
		r, err := call(id)
		if err != nil {
			t.Fatalf("SetRunWallPark: %v", err)
		}
		if r.Status != "paused" || !r.HoldReason.Valid || r.HoldReason.String != "budget_exhausted" {
			t.Fatalf("parked row = (%q,%q), want (paused,budget_exhausted)", r.Status, r.HoldReason.String)
		}
		if !r.HoldCapturedHead.Valid || r.HoldCapturedHead.String != "abc123" {
			t.Fatalf("hold_captured_head = %+v, want abc123", r.HoldCapturedHead)
		}
		if r.PauseMode.Valid || r.PauseRequestedAt.Valid {
			t.Fatal("SetRunWallPark must clear the pause columns")
		}
		if fx.unconsumedWall(id) != 0 {
			t.Fatal("SetRunWallPark must consume the wall input (D18)")
		}
	})

	t.Run("parks a past-deadline UNrequested row (pre-attempt trip)", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, budgetWall: p32(60)}) // no pending request
		r, err := call(id)
		if err != nil {
			t.Fatalf("SetRunWallPark (unrequested): %v", err)
		}
		if r.Status != "paused" {
			t.Fatalf("status = %q, want paused (the deadline is the sole authority)", r.Status)
		}
	})

	t.Run("refuses an extended-in-the-window row even while pause_mode='wall'", func(t *testing.T) {
		// started 3h ago but a huge budget → NOT past the deadline; pause_mode still 'wall'.
		id := fx.run(wpRun{worker: &wkr, budgetWall: p32(60), budgetExtension: 86400, pauseMode: "wall"})
		_, err := call(id)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("extended-in-window park = %v, want pgx.ErrNoRows (the deadline moved)", err)
		}
		if fx.status(id) != "running" {
			t.Fatal("an extended row must stay running")
		}
	})

	t.Run("refuses a post-attempt row (D14 backstop)", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, budgetWall: p32(60), completionAttempts: 1, completionContract: p32(1)})
		_, err := call(id)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("post-attempt park = %v, want pgx.ErrNoRows (the completion hold owns it)", err)
		}
		if fx.status(id) != "running" {
			t.Fatal("a post-attempt row must stay running (mutation: dropping completion_attempts=0 lets it park)")
		}
	})
}

func p32(n int32) *int32 { return &n }

// --- Scenario 2: ExtendAndResumeWallPark ----------------------------------------------------------

// TestExtendAndResumeWallParkLiveDB pins the owner's Extend-and-resume (PRD #1497 D7): banking the
// overrun so an extension of 3600 yields a deadline exactly 3600s of ACTIVE time away regardless of
// how late the park landed; honouring the cap; and the D19 worker rule (a server-parked row resumes
// worker_id NULL with the released pair preserved; a worker-parked row keeps worker_id).
func TestExtendAndResumeWallParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	wkr := fx.worker("w", wpWorker{})
	extend := func(id uuid.UUID, secs, capv int32) (int32, error) {
		return fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
			ID: id, UserID: fx.userID, Secs: secs, Cap: capv, GlobalTimeoutSeconds: 7200,
			Body: pgtype.Text{String: "extend+resume", Valid: true}})
	}
	// deadline = started_at + (base + ext + finalize + paused). We assert it is ~3600s from now.
	deadlineFromNow := func(id uuid.UUID) float64 {
		r := fx.mustRun(id)
		base := int32(7200)
		if r.BudgetWallSeconds.Valid {
			base = r.BudgetWallSeconds.Int32
		}
		total := int(base) + int(r.BudgetExtensionSeconds) + int(r.BudgetFinalizeSeconds) + int(r.BudgetPausedSeconds)
		deadline := r.StartedAt.Time.Add(time.Duration(total) * time.Second)
		return deadline.Sub(time.Now().UTC()).Seconds()
	}

	t.Run("banks the overrun; extension of 3600 lands 3600s of active budget, any lateness", func(t *testing.T) {
		for _, lateness := range []time.Duration{30 * time.Second, 90 * time.Minute} {
			// budget_wall=3600; started 5h ago so the run genuinely OVERRAN (active-at-park >> budget)
			// for both latenesses; parked `lateness` ago. The banking invariant then holds: the
			// overrun + the parked wait are banked, leaving exactly 3600s of active budget after Extend.
			id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted",
				budgetWall: p32(3600), startedAgo: 5 * time.Hour, sinceAgo: lateness})
			if newExt, err := extend(id, 3600, 57600); err != nil || newExt == 0 {
				t.Fatalf("extend (lateness %s) = (%d,%v), want (>0,nil)", lateness, newExt, err)
			}
			if d := deadlineFromNow(id); d < 3590 || d > 3610 {
				t.Fatalf("lateness %s: deadline is %.0fs from now, want ~3600 (the overrun must be banked)", lateness, d)
			}
			if fx.status(id) != "queued" {
				t.Fatalf("lateness %s: resumed status = %q, want queued", lateness, fx.status(id))
			}
		}
	})

	t.Run("honours the cap (refuses when ext + secs > cap)", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", budgetWall: p32(3600), budgetExtension: 50000})
		newExt, err := extend(id, 10000, 57600) // 50000 + 10000 > 57600
		if err != nil {
			t.Fatalf("cap-exceeded extend err = %v, want nil (0-row scalar)", err)
		}
		if newExt != 0 {
			t.Fatalf("cap-exceeded extend returned %d, want 0 (refused)", newExt)
		}
		if fx.status(id) != "paused" {
			t.Fatal("a cap-refused extend must leave the run paused")
		}
	})

	t.Run("server-parked row resumes worker_id NULL, released pair preserved", func(t *testing.T) {
		rel := uuid.New()
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", budgetWall: p32(3600),
			startedAgo: 2 * time.Hour, claimReleased: true, releasedWorker: &rel, releasedNonce: "nonce-A"})
		if _, err := extend(id, 3600, 57600); err != nil {
			t.Fatalf("extend server-parked: %v", err)
		}
		r := fx.mustRun(id)
		if r.WorkerID.Valid {
			t.Fatal("a server-parked resume must NULL worker_id (D19)")
		}
		if !r.ReleasedWorkerID.Valid || uuid.UUID(r.ReleasedWorkerID.Bytes) != rel || !r.ReleasedWorkerNonce.Valid || r.ReleasedWorkerNonce.String != "nonce-A" {
			t.Fatalf("released pair = (%v,%v), want it PRESERVED (D19)", r.ReleasedWorkerID, r.ReleasedWorkerNonce)
		}
	})

	t.Run("worker-parked row keeps worker_id", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", budgetWall: p32(3600), startedAgo: 2 * time.Hour})
		if _, err := extend(id, 3600, 57600); err != nil {
			t.Fatalf("extend worker-parked: %v", err)
		}
		r := fx.mustRun(id)
		if !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != wkr {
			t.Fatal("a worker-side park (claim_released_at NULL) must KEEP worker_id")
		}
	})
}

// --- Scenario 3: StopWallPark (pins Fix 3) --------------------------------------------------------

// TestStopWallParkLiveDB pins the owner's Stop (PRD #1497 D9): it grants 1800 into
// budget_finalize_seconds exactly once, leaves budget_extension_seconds and the cap untouched,
// refuses a second time / zero completed milestones / a non-issue kind, sets the scope ceiling at the
// completed count, and writes the scope+resume audit rows. It ALSO pins Fix 3: a refused Stop (0
// stopped rows) must NOT supersede a prior pending scope audit row. Mutation: dropping the
// budget_finalize_seconds=0 once-only guard lets a second Stop re-grant.
func TestStopWallParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	wkr := fx.worker("w", wpWorker{})
	stop := func(id uuid.UUID, ceiling int32) (uuid.UUID, error) {
		return fx.q.StopWallPark(fx.ctx, store.StopWallParkParams{
			ID: id, UserID: fx.userID, ScopeCeiling: ceiling, GlobalTimeoutSeconds: 7200,
			Body: pgtype.Text{String: "stop at wall", Valid: true}})
	}
	scopeRows := func(id uuid.UUID) (pending, superseded int) {
		fx.t.Helper()
		if err := fx.pool.QueryRow(fx.ctx, `SELECT
			count(*) FILTER (WHERE disposition IS NULL),
			count(*) FILTER (WHERE disposition = 'superseded')
			FROM run_user_inputs WHERE run_id=$1 AND kind='scope'`, id).Scan(&pending, &superseded); err != nil {
			t.Fatalf("count scope rows: %v", err)
		}
		return
	}

	t.Run("grants 1800 once, leaves extension+cap untouched, sets ceiling, writes audit", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted",
			milestonesCompleted: `["m1","m2"]`, budgetExtension: 1200, startedAgo: 2 * time.Hour})
		if _, err := stop(id, 2); err != nil {
			t.Fatalf("StopWallPark: %v", err)
		}
		r := fx.mustRun(id)
		if r.BudgetFinalizeSeconds != 1800 {
			t.Fatalf("budget_finalize_seconds = %d, want 1800", r.BudgetFinalizeSeconds)
		}
		if r.BudgetExtensionSeconds != 1200 {
			t.Fatalf("budget_extension_seconds = %d, want it UNTOUCHED at 1200 (finalize is outside the cap)", r.BudgetExtensionSeconds)
		}
		if !r.ScopeCeiling.Valid || r.ScopeCeiling.Int32 != 2 {
			t.Fatalf("scope_ceiling = %+v, want 2 (the completed count)", r.ScopeCeiling)
		}
		if r.Status != "queued" {
			t.Fatalf("status = %q, want queued", r.Status)
		}
		var scope, resume int
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT count(*) FILTER (WHERE kind='scope'), count(*) FILTER (WHERE kind='resume') FROM run_user_inputs WHERE run_id=$1`, id).Scan(&scope, &resume); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if scope != 1 || resume != 1 {
			t.Fatalf("audit rows: scope=%d resume=%d, want 1 and 1", scope, resume)
		}

		// Second Stop: allowance already used → refused, nothing re-granted (mutation: dropping the
		// budget_finalize_seconds=0 guard lets this re-grant and re-resume). Re-arm the paused hold
		// (the first Stop resumed it to queued) so only the once-only guard can refuse the second.
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status='paused', hold_reason='budget_exhausted' WHERE id=$1`, id)
		if _, err := stop(id, 2); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second StopWallPark = %v, want pgx.ErrNoRows (allowance already used)", err)
		}
		if r2 := fx.mustRun(id); r2.BudgetFinalizeSeconds != 1800 {
			t.Fatalf("second Stop re-granted: budget_finalize_seconds = %d, want it still 1800", r2.BudgetFinalizeSeconds)
		}
	})

	t.Run("refuses zero completed milestones and a non-issue kind", func(t *testing.T) {
		zero := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", milestonesCompleted: `[]`, startedAgo: 2 * time.Hour})
		if _, err := stop(zero, 0); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("Stop with 0 completed = %v, want pgx.ErrNoRows", err)
		}
		// A prompt-kind budget_exhausted hold: not a milestone issue run → refused.
		fx.iid++
		pid := uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, worker_id, started_at, status_since, hold_reason, milestones_completed)
			VALUES ($1,$2,$3,'t','d','paused','prompt',$4, now()-interval '2 hours', now()-interval '30 seconds', 'budget_exhausted', '["m1"]'::jsonb)`, pid, fx.userID, fx.repoID, wkr)
		if _, err := stop(pid, 1); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("Stop on a prompt run = %v, want pgx.ErrNoRows", err)
		}
	})

	// Fix 3: a refused Stop must NOT supersede a prior pending scope audit row.
	t.Run("a refused Stop does not supersede a prior pending scope row (Fix 3)", func(t *testing.T) {
		// A running (NOT paused) row carrying a prior pending scope audit row — Stop's guard refuses it.
		id := fx.run(wpRun{worker: &wkr, milestonesCompleted: `["m1"]`, startedAgo: 2 * time.Hour})
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'scope','prior ceiling')`, id)
		pendingBefore, supersededBefore := scopeRows(id)
		if pendingBefore != 1 || supersededBefore != 0 {
			t.Fatalf("precondition: scope rows = (pending %d, superseded %d), want (1,0)", pendingBefore, supersededBefore)
		}
		if _, err := stop(id, 1); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("Stop on a running row = %v, want pgx.ErrNoRows (0 stopped rows)", err)
		}
		pendingAfter, supersededAfter := scopeRows(id)
		if pendingAfter != 1 || supersededAfter != 0 {
			t.Fatalf("a refused Stop touched the scope audit rows: (pending %d, superseded %d), want unchanged (1,0) [Fix 3]", pendingAfter, supersededAfter)
		}
	})
}

// --- Scenario 4: RecordWallParkCapturedHead -------------------------------------------------------

// TestRecordWallParkCapturedHeadLiveDB pins the late-head record (PRD #1497): it records the head at
// the parked generation, an older generation's does NOT, it never changes status, and it only fills a
// NULL hold_captured_head.
func TestRecordWallParkCapturedHeadLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	wkr := fx.worker("w", wpWorker{})
	wid := pgtype.UUID{Bytes: wkr, Valid: true}
	rec := func(id uuid.UUID, gen int64, head string) int64 {
		fx.t.Helper()
		n, err := fx.q.RecordWallParkCapturedHead(fx.ctx, store.RecordWallParkCapturedHeadParams{
			ID: id, WorkerID: wid, ClaimGeneration: gen, Head: pgtype.Text{String: head, Valid: true}})
		if err != nil {
			t.Fatalf("RecordWallParkCapturedHead: %v", err)
		}
		return n
	}

	t.Run("records the head at the parked generation, no status change", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", claimGen: 5, claimReleased: true})
		if n := rec(id, 5, "deadbeef"); n != 1 {
			t.Fatalf("rows = %d, want 1", n)
		}
		r := fx.mustRun(id)
		if !r.HoldCapturedHead.Valid || r.HoldCapturedHead.String != "deadbeef" {
			t.Fatalf("hold_captured_head = %+v, want deadbeef", r.HoldCapturedHead)
		}
		if r.Status != "paused" {
			t.Fatalf("status = %q, want it unchanged at paused", r.Status)
		}
	})

	t.Run("an older generation records nothing", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", claimGen: 6, claimReleased: true})
		if n := rec(id, 5, "stale"); n != 0 {
			t.Fatalf("older-generation rows = %d, want 0", n)
		}
		if fx.mustRun(id).HoldCapturedHead.Valid {
			t.Fatal("an older generation must not record a head")
		}
	})

	t.Run("only fills a NULL hold_captured_head", func(t *testing.T) {
		id := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", claimGen: 7, claimReleased: true, holdCapturedHead: "original"})
		if n := rec(id, 7, "override"); n != 0 {
			t.Fatalf("non-NULL head rows = %d, want 0", n)
		}
		if fx.mustRun(id).HoldCapturedHead.String != "original" {
			t.Fatal("a set hold_captured_head must not be overwritten")
		}
	})
}

// --- Scenario 5 (DEVIATION-3): pins Fix 1 ---------------------------------------------------------

// TestWallParkDeviation3LegacyCompletedRejectedLiveDB is the DEVIATION-3 regression pinning Fix 1: a
// legacy (nil-generation, non-interlocked) SetRunCompleted on a SERVER-parked (paused +
// claim_released_at) row is REJECTED (0 rows), so an old flight's late `completed` can never complete
// the run the sweep just parked. Mutation: dropping the `status <> 'paused' AND claim_released_at IS
// NULL` guard from SetRunCompleted lets it complete.
func TestWallParkDeviation3LegacyCompletedRejectedLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	wkr := fx.worker("w", wpWorker{})
	wid := pgtype.UUID{Bytes: wkr, Valid: true}

	// A server-parked row: paused, hold_reason budget_exhausted, claim_released_at set, worker_id KEPT
	// (ParkRunsAtWall keeps it informational) — the exact state a legacy completion would target.
	parked := fx.run(wpRun{worker: &wkr, status: "paused", holdReason: "budget_exhausted", claimReleased: true})
	rows, err := fx.q.SetRunCompleted(fx.ctx, store.SetRunCompletedParams{ID: parked, WorkerID: wid, Branch: pgconvText("b")})
	if err != nil {
		t.Fatalf("SetRunCompleted (legacy, on parked row) err = %v, want nil (0-row no-op)", err)
	}
	if rows != 0 {
		t.Fatalf("SetRunCompleted on a server-parked row applied %d rows, want 0 (DEVIATION-3; mutation: dropping the guard lets it complete)", rows)
	}
	if fx.status(parked) != "paused" {
		t.Fatalf("status = %q, want it to STAY paused (the legacy completion must not walk the park back)", fx.status(parked))
	}

	// Control: the SAME legacy completion DOES apply to a genuine running, non-released row.
	live := fx.run(wpRun{worker: &wkr})
	if rows, err := fx.q.SetRunCompleted(fx.ctx, store.SetRunCompletedParams{ID: live, WorkerID: wid, Branch: pgconvText("b")}); err != nil || rows != 1 {
		t.Fatalf("SetRunCompleted on a live running row = (%d,%v), want (1,nil) [non-vacuity]", rows, err)
	}
}

// --- Scenario 5 (D19 claim fence race) ------------------------------------------------------------

// TestWallParkClaimFenceRaceLiveDB is the long-pole D19 fence (PRD #1497 M1): server park →
// released_worker pair captured, snapshot dropped, claim fence armed; the old flight's SetRunRunning
// and (legacy) SetRunCompleted are 0 rows with and without a stamped generation; the released
// incarnation's own ClaimRun returns no row; a PEER ClaimRun succeeds and clears both released
// columns; and with no peer the same worker RE-REGISTERS (new nonce) and reclaims.
func TestWallParkClaimFenceRaceLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	// The failing incarnation: STALE + INCAPABLE (no wall_park_v1) so ParkRunsAtWall parks its run.
	incA := fx.worker("incA", wpWorker{heartbeatAgo: time.Hour, nonce: "nonce-A", docker: false})
	widA := pgtype.UUID{Bytes: incA, Valid: true}
	gen := int64(3)
	id := fx.run(wpRun{worker: &incA, budgetWall: p32(60), claimGen: gen, startedAgo: 2 * time.Hour})
	// A live snapshot row for the run under incA, to prove ParkRunsAtWall drops it.
	mustExec(fx.ctx, t, fx.pool,
		`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, snapshot_epoch, reported_at) VALUES ($1,$2,$3,'running',1, now())`, incA, id, gen)

	staleCutoff := wpAgo(45 * time.Second)
	// ParkRunsAtWall is a GLOBAL sweep (no user filter); assert this run is IN the parked set rather
	// than assuming it is the only one on the shared throwaway DB.
	parked, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: staleCutoff, GraceSeconds: 600})
	if err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	parkedSet := map[uuid.UUID]bool{}
	for _, r := range parked {
		parkedSet[r.ID] = true
	}
	if !parkedSet[id] {
		t.Fatalf("run %s not in the parked set %v", id, parked)
	}
	r := fx.mustRun(id)
	if r.Status != "paused" || !r.ClaimReleasedAt.Valid {
		t.Fatalf("parked row = (%q, released=%v), want (paused, released)", r.Status, r.ClaimReleasedAt.Valid)
	}
	if !r.ReleasedWorkerID.Valid || uuid.UUID(r.ReleasedWorkerID.Bytes) != incA || !r.ReleasedWorkerNonce.Valid || r.ReleasedWorkerNonce.String != "nonce-A" {
		t.Fatalf("released pair = (%v,%v), want (incA, nonce-A) captured under the lock", r.ReleasedWorkerID, r.ReleasedWorkerNonce)
	}
	var snap int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM worker_active_runs WHERE run_id=$1`, id).Scan(&snap); err != nil {
		t.Fatalf("count snapshot: %v", err)
	}
	if snap != 0 {
		t.Fatal("ParkRunsAtWall must delete the run's worker_active_runs rows")
	}

	// oldRunReport / oldCompleted run the old flight's reports; each must be a 0-row no-op. At the
	// STORE level SetRunRunning is generation-blind (its `status <> 'paused'` guard rejects a paused
	// row for any generation; once resumed, worker_id is NULL so the worker_id=@incA guard fails); the
	// WITH/WITHOUT-a-stamped-generation distinction is SetState's Go fence wrapper, covered by
	// credential_switch_livedb_test's D16 cases.
	oldRunReport := func(when string) {
		fx.t.Helper()
		if n, err := fx.q.SetRunRunning(fx.ctx, store.SetRunRunningParams{
			IterationCount: 1, ID: id, WorkerID: widA, RunMaxIterations: 30, MilestoneBudgetCap: 7,
			RunTimeoutSeconds: 7200, BudgetWallCeilingSeconds: 28800}); err != nil || n != 0 {
			t.Fatalf("old flight SetRunRunning %s resume = (%d,%v), want (0,nil)", when, n, err)
		}
		// The legacy (nil-generation) SetRunCompleted is rejected by the DEVIATION-3 guard.
		if n, err := fx.q.SetRunCompleted(fx.ctx, store.SetRunCompletedParams{ID: id, WorkerID: widA, Branch: pgconvText("b")}); err != nil || n != 0 {
			t.Fatalf("old flight SetRunCompleted (legacy) %s resume = (%d,%v), want (0,nil) [DEVIATION-3]", when, n, err)
		}
	}
	oldRunReport("before")

	// Owner Extend-and-resume: the row moves paused -> queued, worker_id NULL, released pair preserved.
	if _, err := fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
		ID: id, UserID: fx.userID, Secs: 3600, Cap: 57600, GlobalTimeoutSeconds: 7200,
		Body: pgtype.Text{String: "extend+resume", Valid: true}}); err != nil {
		t.Fatalf("ExtendAndResumeWallPark: %v", err)
	}
	rr := fx.mustRun(id)
	if rr.Status != "queued" || rr.WorkerID.Valid {
		t.Fatalf("resumed row = (%q, worker=%v), want (queued, NULL worker)", rr.Status, rr.WorkerID.Valid)
	}
	if !rr.ReleasedWorkerID.Valid || uuid.UUID(rr.ReleasedWorkerID.Bytes) != incA {
		t.Fatal("resume must PRESERVE the released pair (D19)")
	}
	oldRunReport("after")

	// The released incarnation's OWN ClaimRun returns no row (barred by the released pair).
	if _, err := fx.claim(incA, "nonce-A", false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("released incarnation ClaimRun = %v, want pgx.ErrNoRows (barred by the released pair)", err)
	}

	// A PEER claims at once and CLEARS both released columns.
	peer := fx.worker("peer", wpWorker{nonce: "nonce-P"})
	claimed, err := fx.claim(peer, "nonce-P", false)
	if err != nil {
		t.Fatalf("peer ClaimRun: %v", err)
	}
	if claimed.ID != id {
		t.Fatalf("peer claimed %s, want %s", claimed.ID, id)
	}
	if claimed.ReleasedWorkerID.Valid || claimed.ReleasedWorkerNonce.Valid {
		t.Fatal("ClaimRun must clear both released columns on reclaim")
	}
}

// TestWallParkReleasedWorkerReRegisterLiveDB pins the no-peer path: with no peer, the SAME worker
// re-registers (a restart rotates snapshot_register_nonce) and then reclaims — the leg that keeps a
// single-worker deployment live (Extend never strands a run behind its only worker).
func TestWallParkReleasedWorkerReRegisterLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	only := fx.worker("only", wpWorker{heartbeatAgo: time.Hour, nonce: "nonce-old"})
	id := fx.run(wpRun{worker: &only, budgetWall: p32(60), claimGen: 2, startedAgo: 2 * time.Hour})
	if _, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second), GraceSeconds: 600}); err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	// Resume (owner extends): worker_id NULL, released pair preserved (nonce-old).
	if _, err := fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
		ID: id, UserID: fx.userID, Secs: 3600, Cap: 57600, GlobalTimeoutSeconds: 7200,
		Body: pgtype.Text{String: "x", Valid: true}}); err != nil {
		t.Fatalf("ExtendAndResumeWallPark: %v", err)
	}
	// The same worker still on nonce-old CANNOT reclaim (its incarnation is the excluded one).
	if _, err := fx.claim(only, "nonce-old", false); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale-incarnation reclaim = %v, want pgx.ErrNoRows", err)
	}
	// It RE-REGISTERS: a restart rotates the nonce; now it is a fresh, safe incarnation.
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET snapshot_register_nonce='nonce-new', last_heartbeat_at=now() WHERE id=$1`, only)
	claimed, err := fx.claim(only, "nonce-new", false)
	if err != nil {
		t.Fatalf("re-registered reclaim: %v", err)
	}
	if claimed.ID != id {
		t.Fatalf("re-registered worker claimed %s, want %s", claimed.ID, id)
	}
}

// claim runs ClaimRun as the given worker with its current nonce (WorkerIdentity), the minimal params
// the wall-park fence needs. is not exhaustive of ClaimRun's spread/affinity knobs; the fixtures here
// have a single claimable run so placement is unambiguous.
func (fx *wpFixture) claim(workerID uuid.UUID, nonce string, docker bool) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:              fx.userID,
		HeartbeatCutoff:     wpAgo(45 * time.Second),
		AffinityCutoff:      wpAgo(2 * time.Minute),
		SpreadCutoff:        wpAgo(9 * time.Second),
		SnapshotFreshCutoff: wpAgo(45 * time.Second),
		IsDockerWorker:      docker,
		WorkerIdentity:      nonce,
	})
}

// --- Scenario 6: pair captured at park time -------------------------------------------------------

// TestWallParkPairCapturedAtParkLiveDB: incarnation A is server-parked; the worker re-registers as B
// (new nonce) WHILE the run stays paused; the owner extends; B claims at once; A stays excluded. The
// released pair is captured at PARK time (nonce-A) and never re-read, so a re-registered B is a fresh,
// claimable incarnation.
func TestWallParkPairCapturedAtParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("w", wpWorker{heartbeatAgo: time.Hour, nonce: "nonce-A"})
	id := fx.run(wpRun{worker: &w, budgetWall: p32(60), claimGen: 4, startedAgo: 2 * time.Hour})
	if _, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second), GraceSeconds: 600}); err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	// The worker re-registers as B (new nonce) while the run is still paused.
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET snapshot_register_nonce='nonce-B', last_heartbeat_at=now() WHERE id=$1`, w)
	// Owner extends (server-parked → worker_id NULL, released pair preserved at nonce-A).
	if _, err := fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
		ID: id, UserID: fx.userID, Secs: 3600, Cap: 57600, GlobalTimeoutSeconds: 7200,
		Body: pgtype.Text{String: "x", Valid: true}}); err != nil {
		t.Fatalf("ExtendAndResumeWallPark: %v", err)
	}
	r := fx.mustRun(id)
	if r.ReleasedWorkerNonce.String != "nonce-A" {
		t.Fatalf("released nonce = %q, want it captured at park time (nonce-A), never re-read", r.ReleasedWorkerNonce.String)
	}
	// B claims at once (its nonce differs from the excluded pair).
	claimed, err := fx.claim(w, "nonce-B", false)
	if err != nil {
		t.Fatalf("B ClaimRun: %v", err)
	}
	if claimed.ID != id {
		t.Fatalf("B claimed %s, want %s", claimed.ID, id)
	}
}

// --- Scenario 7 (store): CountOnlineWorkersClaimableForRun ----------------------------------------

// TestCountOnlineWorkersClaimableForRunLiveDB pins the new per-run availability count (PRD #1497
// D19): it excludes the released incarnation, counts a peer / re-registered incarnation, returns 0 on
// a split fleet where no single worker satisfies the whole conjunction (capabilities XOR Codex
// protocol), returns 0 for an ephemeral worker bound to another run, and CountOnlineEligibleWorkers
// ForRepo answers as before on the identical fixtures (only the new per-run query composes the whole
// conjunction).
func TestCountOnlineWorkersClaimableForRunLiveDB(t *testing.T) {
	// Each subtest builds its OWN fixture (fresh random user): CountOnlineWorkersClaimableForRun
	// filters w.user_id = run.user_id, so per-user isolation keeps one subtest's workers out of
	// another's count on the shared throwaway DB.
	claimableIn := func(fx *wpFixture, runID uuid.UUID, capAware bool) int64 {
		fx.t.Helper()
		n, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
			RunID: runID, HeartbeatCutoff: wpAgo(45 * time.Second), DockerRepoAllowlist: []uuid.UUID{}, CapabilityAware: capAware})
		if err != nil {
			fx.t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
		}
		return n
	}

	t.Run("excludes the released incarnation, counts a re-registered one", func(t *testing.T) {
		fx := newWPFixture(t)
		claimable := func(runID uuid.UUID, capAware bool) int64 { return claimableIn(fx, runID, capAware) }
		rel := uuid.New()
		id := fx.run(wpRun{status: "queued", holdReason: "budget_exhausted", claimReleased: true, releasedWorker: &rel, releasedNonce: "nonce-A"})
		// The excluded incarnation: online, still on nonce-A.
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
			VALUES ($1,$2,'excluded',$3,'online', now(), 'nonce-A')`, rel, fx.userID, tokenHash())
		if n := claimable(id, false); n != 0 {
			t.Fatalf("claimable with only the excluded incarnation = %d, want 0", n)
		}
		// The SAME worker re-registers (nonce rotates) → claimable.
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET snapshot_register_nonce='nonce-B' WHERE id=$1`, rel)
		if n := claimable(id, false); n != 1 {
			t.Fatalf("claimable after re-registration = %d, want 1", n)
		}
	})

	t.Run("counts a peer as claimable", func(t *testing.T) {
		fx := newWPFixture(t)
		claimable := func(runID uuid.UUID, capAware bool) int64 { return claimableIn(fx, runID, capAware) }
		rel := uuid.New()
		id := fx.run(wpRun{status: "queued", holdReason: "budget_exhausted", claimReleased: true, releasedWorker: &rel, releasedNonce: "nonce-A"})
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, snapshot_register_nonce)
			VALUES ($1,$2,'excluded2',$3,'online', now(), 'nonce-A')`, rel, fx.userID, tokenHash())
		fx.worker("peer", wpWorker{nonce: "nonce-P"})
		if n := claimable(id, false); n != 1 {
			t.Fatalf("claimable with the excluded incarnation + one peer = %d, want 1 (only the peer)", n)
		}
	})

	t.Run("split fleet: 0 claimable, eligibility unchanged (>0)", func(t *testing.T) {
		fx := newWPFixture(t)
		claimable := func(runID uuid.UUID, capAware bool) int64 { return claimableIn(fx, runID, capAware) }
		// A Codex run requiring {jvm}: worker A has {jvm} but no codex_harness_v1; worker B has
		// codex_harness_v1 but no {jvm}. No single worker satisfies the whole conjunction. harness
		// 'codex' alone marks the run codex (the claimable query reads harness OR codex_*), so no
		// codex_secret_id FK row is needed.
		id := fx.run(wpRun{status: "queued", harness: "codex", requiredCaps: []string{"jvm"}})
		fx.worker("hasCaps", wpWorker{caps: []string{"jvm"}, protocolCaps: []string{}})
		fx.worker("hasCodex", wpWorker{caps: []string{}, protocolCaps: []string{"codex_harness_v1"}})
		if n := claimable(id, true); n != 0 {
			t.Fatalf("split-fleet claimable = %d, want 0 (no single worker satisfies capability AND codex protocol)", n)
		}
		// Eligibility (docker-allowlist + capability fence, no codex-protocol composition) still counts
		// worker A: unchanged from before, proving only the new per-run query composes the conjunction.
		elig, err := fx.q.CountOnlineEligibleWorkersForRepo(fx.ctx, store.CountOnlineEligibleWorkersForRepoParams{
			UserID: fx.userID, DockerRepoAllowlist: []uuid.UUID{}, RepoID: fx.repoID, Kind: "issue",
			RequiredCapabilities: []string{"jvm"}, CapabilityAware: true})
		if err != nil {
			t.Fatalf("CountOnlineEligibleWorkersForRepo: %v", err)
		}
		if elig != 1 {
			t.Fatalf("eligibility on the split fleet = %d, want 1 (the jvm worker; the query ignores the codex composition)", elig)
		}
	})

	t.Run("ephemeral worker bound to another run is not claimable", func(t *testing.T) {
		fx := newWPFixture(t)
		claimable := func(runID uuid.UUID, capAware bool) int64 { return claimableIn(fx, runID, capAware) }
		other := fx.run(wpRun{status: "running"})
		id := fx.run(wpRun{status: "queued"})
		fx.worker("eph", wpWorker{ephemeral: true, ephemeralRun: &other})
		if n := claimable(id, false); n != 0 {
			t.Fatalf("ephemeral-bound-elsewhere claimable = %d, want 0", n)
		}
	})
}

// --- Scenario 8: wall-input settlement (D18) + D14 completion hold --------------------------------

// TestWallInputSettlementLiveDB pins D18: a server park AND a completion hold on a pause_mode='wall'
// row each stamp consumed_at on the unconsumed wall input, and after a resume ConsumeRunInputs
// returns no wall. And D14: SetRunCompletionHold on a pending-wall row clears the pause columns and
// yields hold_reason='completion_blocked' (not budget_exhausted).
func TestWallInputSettlementLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	armWall := func(id uuid.UUID) {
		fx.t.Helper()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
	}

	t.Run("a server park consumes the wall input", func(t *testing.T) {
		w := fx.worker("stale", wpWorker{heartbeatAgo: time.Hour, nonce: "n"})
		id := fx.run(wpRun{worker: &w, budgetWall: p32(60), claimGen: 1, startedAgo: 2 * time.Hour, pauseMode: "wall"})
		armWall(id)
		if _, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
			Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second), GraceSeconds: 600}); err != nil {
			t.Fatalf("ParkRunsAtWall: %v", err)
		}
		if fx.unconsumedWall(id) != 0 {
			t.Fatal("ParkRunsAtWall must consume the wall input (D18)")
		}
	})

	t.Run("a completion hold on a wall row consumes the input, clears pause, yields completion_blocked (D14)", func(t *testing.T) {
		w := fx.worker("live", wpWorker{nonce: "n2"})
		wid := pgtype.UUID{Bytes: w, Valid: true}
		id := fx.run(wpRun{worker: &w, completionAttempts: 1, completionContract: p32(1), pauseMode: "wall", claimGen: 1})
		armWall(id)
		r, err := fx.q.SetRunCompletionHold(fx.ctx, store.SetRunCompletionHoldParams{
			ID: id, WorkerID: wid, ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}})
		if err != nil {
			t.Fatalf("SetRunCompletionHold: %v", err)
		}
		if r.HoldReason.String != "completion_blocked" {
			t.Fatalf("hold_reason = %q, want completion_blocked (not budget_exhausted) [D14]", r.HoldReason.String)
		}
		if r.PauseMode.Valid || r.PauseRequestedAt.Valid {
			t.Fatal("the completion hold must clear the pending wall request (D14)")
		}
		if fx.unconsumedWall(id) != 0 {
			t.Fatal("the completion hold must consume the wall input (D18)")
		}
	})

	t.Run("after a resume, ConsumeRunInputs returns no wall", func(t *testing.T) {
		w := fx.worker("live2", wpWorker{nonce: "n3"})
		id := fx.run(wpRun{worker: &w, budgetWall: p32(3600), status: "paused", holdReason: "budget_exhausted", startedAgo: 2 * time.Hour, pauseMode: ""})
		armWall(id) // an unconsumed wall input left over
		// Settle it via a resume-style extend, then confirm the drain hands no wall.
		if _, err := fx.q.ExtendAndResumeWallPark(fx.ctx, store.ExtendAndResumeWallParkParams{
			ID: id, UserID: fx.userID, Secs: 3600, Cap: 57600, GlobalTimeoutSeconds: 7200, Body: pgtype.Text{String: "x", Valid: true}}); err != nil {
			t.Fatalf("ExtendAndResumeWallPark: %v", err)
		}
		rows, err := fx.q.ConsumeRunInputs(fx.ctx, id)
		if err != nil {
			t.Fatalf("ConsumeRunInputs: %v", err)
		}
		for _, row := range rows {
			if row.Kind == "pause" && row.Body.String == "wall" {
				t.Fatal("ConsumeRunInputs handed a stale wall input after a resume (D18)")
			}
		}
	})
}

// --- Scenario 9: owner-guard refusals -------------------------------------------------------------

// TestWallParkOwnerGuardRefusalsLiveDB pins that an owner cannot create/cancel/clear over, or
// SetRunPaused into, a wall request; and that CreateExtendInput on a running row with a pending wall
// request clears the columns and consumes the input.
func TestWallParkOwnerGuardRefusalsLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("w", wpWorker{nonce: "n"})
	wid := pgtype.UUID{Bytes: w, Valid: true}
	armWall := func(id uuid.UUID) {
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
	}

	t.Run("SetRunPaused refuses a wall request", func(t *testing.T) {
		id := fx.run(wpRun{worker: &w, pauseMode: "wall"})
		if rows, err := fx.q.SetRunPaused(fx.ctx, store.SetRunPausedParams{ID: id, WorkerID: wid}); err != nil || rows != 0 {
			t.Fatalf("SetRunPaused over a wall request = (%d,%v), want (0,nil)", rows, err)
		}
		if fx.status(id) != "running" {
			t.Fatal("an owner-pause report must not consume a wall request")
		}
	})

	t.Run("CreatePauseInput / CancelPauseInput / ClearPauseRequest refuse over a wall request", func(t *testing.T) {
		id := fx.run(wpRun{worker: &w, pauseMode: "wall"})
		if _, err := fx.q.CreatePauseInput(fx.ctx, store.CreatePauseInputParams{ID: id, Mode: pgconvText("now")}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("CreatePauseInput over wall = %v, want pgx.ErrNoRows", err)
		}
		if _, err := fx.q.CancelPauseInput(fx.ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("CancelPauseInput over wall = %v, want pgx.ErrNoRows (0 rows)", err)
		}
		if rows, err := fx.q.ClearPauseRequest(fx.ctx, store.ClearPauseRequestParams{ID: id, WorkerID: wid, ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}}); err != nil || rows != 0 {
			t.Fatalf("ClearPauseRequest over wall = (%d,%v), want (0,nil)", rows, err)
		}
		if pm := fx.mustRun(id).PauseMode; !pm.Valid || pm.String != "wall" {
			t.Fatalf("pause_mode = %+v, want it STILL 'wall' (owner cannot withdraw the system request)", pm)
		}
	})

	t.Run("CreateExtendInput on a running wall row clears the columns and consumes the input", func(t *testing.T) {
		id := fx.run(wpRun{worker: &w, budgetWall: p32(3600), pauseMode: "wall"})
		armWall(id)
		if _, err := fx.q.CreateExtendInput(fx.ctx, store.CreateExtendInputParams{ID: id, Secs: 3600, Cap: 57600, Body: pgconvText("+1h")}); err != nil {
			t.Fatalf("CreateExtendInput: %v", err)
		}
		r := fx.mustRun(id)
		if r.PauseMode.Valid || r.PauseRequestedAt.Valid {
			t.Fatal("Extend on a running wall row must clear the pause columns")
		}
		if fx.unconsumedWall(id) != 0 {
			t.Fatal("Extend on a running wall row must consume the wall input")
		}
	})
}

// --- Scenario 10 (store): ResumePausedRun guards --------------------------------------------------

// TestResumePausedRunWallGuardsLiveDB pins the resume guards (PRD #1497 D6/D7): refuses a
// completion_blocked hold unless opted in, refuses a budget_exhausted hold with no remaining budget,
// and resumes a budget_exhausted hold WITH budget (clearing hold_reason/hold_captured_head).
func TestResumePausedRunWallGuardsLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("w", wpWorker{nonce: "n"})
	resume := func(id uuid.UUID, allowCB bool) (store.ResumePausedRunRow, error) {
		return fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{
			ID: id, UserID: fx.userID, AllowCompletionBlockedHold: allowCB, GlobalTimeoutSeconds: 7200})
	}

	t.Run("refuses completion_blocked unless allowed", func(t *testing.T) {
		id := fx.run(wpRun{worker: &w, status: "paused", holdReason: "completion_blocked"})
		if _, err := resume(id, false); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("resume completion_blocked (no opt-in) = %v, want pgx.ErrNoRows", err)
		}
		if _, err := resume(id, true); err != nil {
			t.Fatalf("resume completion_blocked (opt-in) = %v, want nil", err)
		}
	})

	t.Run("refuses budget_exhausted with no remaining budget", func(t *testing.T) {
		// budget_wall 3600, started 3h ago, no banked pause → active elapsed >> total → remaining <= 0.
		id := fx.run(wpRun{worker: &w, status: "paused", holdReason: "budget_exhausted", budgetWall: p32(3600), startedAgo: 3 * time.Hour, sinceAgo: time.Minute})
		if _, err := resume(id, false); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("resume out-of-time budget_exhausted = %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("resumes budget_exhausted with budget, clearing the hold", func(t *testing.T) {
		// budget_wall 3600 but budget_paused banks the whole elapsed → remaining > 0.
		id := fx.run(wpRun{worker: &w, status: "paused", holdReason: "budget_exhausted", budgetWall: p32(3600),
			budgetPaused: 3 * 3600, startedAgo: 3 * time.Hour, sinceAgo: time.Minute, holdCapturedHead: "abc"})
		row, err := resume(id, false)
		if err != nil {
			t.Fatalf("resume budget_exhausted with budget: %v", err)
		}
		if row.Status != "queued" {
			t.Fatalf("status = %q, want queued", row.Status)
		}
		r := fx.mustRun(id)
		if r.HoldReason.Valid || r.HoldCapturedHead.Valid {
			t.Fatalf("resume must clear hold_reason/hold_captured_head (got %+v / %+v)", r.HoldReason, r.HoldCapturedHead)
		}
	})
}

// --- Scenario 11: StampCompletionBudgetExhausted extension term (pins the +extension term) ---------

// TestStampCompletionBudgetExhaustedExtendedLiveDB pins BLOCKING-B1: the stamp does NOT fire on an
// EXTENDED post-attempt row before its extended deadline, but DOES once past it. Mutation: removing
// the `+ budget_extension_seconds` term makes the extended-but-not-past row get stamped.
func TestStampCompletionBudgetExhaustedExtendedLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("live", wpWorker{nonce: "n"})
	stamp := func() int64 {
		fx.t.Helper()
		n, err := fx.q.StampCompletionBudgetExhausted(fx.ctx, store.StampCompletionBudgetExhaustedParams{
			Now: wpNow(), GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: wpAgo(45 * time.Second)})
		if err != nil {
			t.Fatalf("StampCompletionBudgetExhausted: %v", err)
		}
		return n
	}
	stampedAt := func(id uuid.UUID) bool {
		fx.t.Helper()
		var at pgtype.Timestamptz
		if err := fx.pool.QueryRow(fx.ctx, `SELECT completion_budget_exhausted_at FROM runs WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatalf("read stamp: %v", err)
		}
		return at.Valid
	}

	// Started 90m ago, wall 3600 (1h), extension 3600 (1h): original deadline (1h) passed, extended
	// deadline (2h) has NOT. Must NOT be stamped.
	notPast := fx.run(wpRun{worker: &w, completionAttempts: 1, completionContract: p32(1),
		budgetWall: p32(3600), budgetExtension: 3600, startedAgo: 90 * time.Minute})
	// Started 3h ago, same budgets: past the extended (2h) deadline. MUST be stamped.
	past := fx.run(wpRun{worker: &w, completionAttempts: 1, completionContract: p32(1),
		budgetWall: p32(3600), budgetExtension: 3600, startedAgo: 3 * time.Hour})

	// StampCompletionBudgetExhausted is a GLOBAL sweep (no user filter), so its return count can
	// include leftover rows from sibling tests on the shared DB — assert per-row, not on the count.
	if n := stamp(); n < 1 {
		t.Fatalf("stamped %d rows, want >= 1 (the past-deadline row)", n)
	}
	if stampedAt(notPast) {
		t.Fatal("an extended post-attempt row before its EXTENDED deadline must NOT be stamped (mutation: dropping +budget_extension_seconds stamps it)")
	}
	if !stampedAt(past) {
		t.Fatal("a post-attempt row past its extended deadline MUST be stamped (non-vacuity)")
	}
}

// --- Scenario 12: RequestWallParks idempotency ----------------------------------------------------

// TestRequestWallParksIdempotencyLiveDB pins that a second tick against an already-wall row inserts no
// duplicate wall input and does not reset pause_requested_at.
func TestRequestWallParksIdempotencyLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("live", wpWorker{nonce: "n", protocolCaps: []string{"wall_park_v1"}})
	id := fx.run(wpRun{worker: &w, budgetWall: p32(60), startedAgo: 2 * time.Hour})
	// RequestWallParks is a GLOBAL sweep; assert set MEMBERSHIP of this run, not the total count.
	req := func() map[uuid.UUID]bool {
		fx.t.Helper()
		rows, err := fx.q.RequestWallParks(fx.ctx, store.RequestWallParksParams{
			Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second)})
		if err != nil {
			t.Fatalf("RequestWallParks: %v", err)
		}
		set := map[uuid.UUID]bool{}
		for _, r := range rows {
			set[r.ID] = true
		}
		return set
	}
	if !req()[id] {
		t.Fatal("first RequestWallParks must request the past-deadline row")
	}
	first := fx.mustRun(id).PauseRequestedAt.Time
	var walls int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause' AND body='wall'`, id).Scan(&walls); err != nil {
		t.Fatalf("count wall inputs: %v", err)
	}
	if walls != 1 {
		t.Fatalf("wall inputs after first tick = %d, want 1", walls)
	}
	// Second tick: idempotent (pause_mode already 'wall') → this run is NOT re-requested.
	if req()[id] {
		t.Fatal("second RequestWallParks re-requested an already-wall row (not idempotent)")
	}
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause' AND body='wall'`, id).Scan(&walls); err != nil {
		t.Fatalf("count wall inputs: %v", err)
	}
	if walls != 1 {
		t.Fatalf("wall inputs after second tick = %d, want 1 (no duplicate)", walls)
	}
	if !fx.mustRun(id).PauseRequestedAt.Time.Equal(first) {
		t.Fatal("a second tick must not reset pause_requested_at")
	}
}

// --- Scenario 14: #1226 carve-out untouched -------------------------------------------------------

// TestWallParkCarveOutUntouchedLiveDB pins that a #1226 carve-out row (post-attempt with a LIVE
// worker) is untouched by both RequestWallParks and ParkRunsAtWall.
func TestWallParkCarveOutUntouchedLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	live := fx.worker("live", wpWorker{nonce: "n", protocolCaps: []string{"wall_park_v1"}})
	id := fx.run(wpRun{worker: &live, budgetWall: p32(60), startedAgo: 2 * time.Hour, completionAttempts: 1, completionContract: p32(1)})

	reqRows, err := fx.q.RequestWallParks(fx.ctx, store.RequestWallParksParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second)})
	if err != nil {
		t.Fatalf("RequestWallParks: %v", err)
	}
	for _, r := range reqRows {
		if r.ID == id {
			t.Fatal("RequestWallParks must skip a post-attempt live-worker carve-out row")
		}
	}
	parkRows, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second), GraceSeconds: 600})
	if err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	for _, r := range parkRows {
		if r.ID == id {
			t.Fatal("ParkRunsAtWall must skip a post-attempt live-worker carve-out row")
		}
	}
	if fx.status(id) != "running" {
		t.Fatal("the carve-out row must stay running")
	}
}

// --- Scenario 15: three-term agreement ------------------------------------------------------------

// TestWallParkThreeTermAgreementLiveDB pins that with all three budget terms non-zero the sweep park
// boundary, StampCompletionBudgetExhausted and RunDeadline agree on the same deadline. Rather than
// couple to the Go RunDeadline helper (a different package), it pins the SQL agreement: a row exactly
// 1s PAST the three-term deadline is both wall-requested and (as a post-attempt sibling) stamped,
// while a row 60s BEFORE it is neither.
func TestWallParkThreeTermAgreementLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	// total = wall(3600) + extension(1800) + finalize(900) + paused(600) = 6900s.
	const total = 3600 + 1800 + 900 + 600
	live := fx.worker("live", wpWorker{nonce: "n", protocolCaps: []string{"wall_park_v1"}})

	pastReq := fx.run(wpRun{worker: &live, budgetWall: p32(3600), budgetExtension: 1800, budgetFinalize: 900,
		budgetPaused: 600, startedAgo: time.Duration(total+1) * time.Second})
	beforeReq := fx.run(wpRun{worker: &live, budgetWall: p32(3600), budgetExtension: 1800, budgetFinalize: 900,
		budgetPaused: 600, startedAgo: time.Duration(total-60) * time.Second})
	pastStamp := fx.run(wpRun{worker: &live, budgetWall: p32(3600), budgetExtension: 1800, budgetFinalize: 900,
		budgetPaused: 600, startedAgo: time.Duration(total+1) * time.Second, completionAttempts: 1, completionContract: p32(1)})
	beforeStamp := fx.run(wpRun{worker: &live, budgetWall: p32(3600), budgetExtension: 1800, budgetFinalize: 900,
		budgetPaused: 600, startedAgo: time.Duration(total-60) * time.Second, completionAttempts: 1, completionContract: p32(1)})

	reqRows, err := fx.q.RequestWallParks(fx.ctx, store.RequestWallParksParams{
		Now: wpNow(), GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: wpAgo(45 * time.Second)})
	if err != nil {
		t.Fatalf("RequestWallParks: %v", err)
	}
	reqSet := map[uuid.UUID]bool{}
	for _, r := range reqRows {
		reqSet[r.ID] = true
	}
	if !reqSet[pastReq] {
		t.Fatal("a row 1s past the three-term deadline must be wall-requested")
	}
	if reqSet[beforeReq] {
		t.Fatal("a row 60s before the three-term deadline must NOT be wall-requested")
	}
	if _, err := fx.q.StampCompletionBudgetExhausted(fx.ctx, store.StampCompletionBudgetExhaustedParams{
		Now: wpNow(), GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: wpAgo(45 * time.Second)}); err != nil {
		t.Fatalf("StampCompletionBudgetExhausted: %v", err)
	}
	stampedAt := func(id uuid.UUID) bool {
		var at pgtype.Timestamptz
		if err := fx.pool.QueryRow(fx.ctx, `SELECT completion_budget_exhausted_at FROM runs WHERE id=$1`, id).Scan(&at); err != nil {
			t.Fatalf("read stamp: %v", err)
		}
		return at.Valid
	}
	if !stampedAt(pastStamp) {
		t.Fatal("a post-attempt row 1s past the three-term deadline must be stamped (stamp agrees with the sweep boundary)")
	}
	if stampedAt(beforeStamp) {
		t.Fatal("a post-attempt row 60s before the three-term deadline must NOT be stamped")
	}
}

// --- Scenario 16: kind coverage -------------------------------------------------------------------

// TestWallParkKindCoverageLiveDB pins that each of prompt/self_improve/mr_rework/ci_fix parks at the
// wall (in addition to issue/task), while chat/judge/interactive tasks are untouched.
func TestWallParkKindCoverageLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	stale := fx.worker("stale", wpWorker{heartbeatAgo: time.Hour, nonce: "n"})
	past := "now() - interval '2 hours'"
	since := "now() - interval '30 seconds'"
	insert := func(kind string, extraCols, extraVals string) uuid.UUID {
		fx.iid++
		id := uuid.New()
		sql := fmt.Sprintf(`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, worker_id, started_at, status_since, budget_wall_seconds %s)
			VALUES ($1,$2,$3,'t','d','running',$4,$5, %s, %s, 1 %s)`, extraCols, past, since, extraVals)
		mustExec(fx.ctx, t, fx.pool, sql, id, fx.userID, fx.repoID, kind, stale)
		return id
	}
	// The four kinds not already covered by issue/task in run_pause_livedb_test.go.
	prompt := insert("prompt", "", "")
	selfImprove := insert("self_improve", ", issue_iid", fmt.Sprintf(", %d", 90000+fx.iid))
	mrRework := func() uuid.UUID {
		fx.iid++
		id := uuid.New()
		// target_run_id has an FK to runs; use a real prior run as the rework target.
		tgt := fx.run(wpRun{status: "completed"})
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, worker_id, started_at, status_since, budget_wall_seconds, pipeline_ref, mr_iid, target_run_id)
			VALUES ($1,$2,$3,'t','d','running','mr_rework',$4, now()-interval '2 hours', now()-interval '30 seconds', 1, 'agent/mr', 42, $5)`, id, fx.userID, fx.repoID, stale, tgt)
		return id
	}()
	ciFix := func() uuid.UUID {
		fx.iid++
		id := uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, worker_id, started_at, status_since, budget_wall_seconds, pipeline_id, pipeline_ref)
			VALUES ($1,$2,$3,'t','d','running','ci_fix',$4, now()-interval '2 hours', now()-interval '30 seconds', 1, 777, 'agent/ci')`, id, fx.userID, fx.repoID, stale)
		return id
	}()
	// Untouched kinds.
	chat := func() uuid.UUID {
		fx.iid++
		id := uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, worker_id, started_at, status_since, budget_wall_seconds)
			VALUES ($1,$2,'t','d','running','chat',$3, now()-interval '2 hours', now()-interval '30 seconds', 1)`, id, fx.userID, stale)
		return id
	}()
	interactiveTask := func() uuid.UUID {
		fx.iid++
		id := uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, interactive, branch, worker_id, started_at, status_since, budget_wall_seconds)
			VALUES ($1,$2,$3,'t','d','running','task',true,'uzi/task/x',$4, now()-interval '2 hours', now()-interval '30 seconds', 1)`, id, fx.userID, fx.repoID, stale)
		return id
	}()

	parked, err := fx.q.ParkRunsAtWall(fx.ctx, store.ParkRunsAtWallParams{
		Now: wpNow(), GlobalTimeoutSeconds: 1, WorkerStaleCutoff: wpAgo(45 * time.Second), GraceSeconds: 600})
	if err != nil {
		t.Fatalf("ParkRunsAtWall: %v", err)
	}
	set := map[uuid.UUID]bool{}
	for _, r := range parked {
		set[r.ID] = true
	}
	for _, id := range []uuid.UUID{prompt, selfImprove, mrRework, ciFix} {
		if !set[id] {
			t.Fatalf("kind of run %s must park at the wall", id)
		}
	}
	for _, id := range []uuid.UUID{chat, interactiveTask} {
		if set[id] {
			t.Fatalf("run %s (chat/interactive) must NOT park", id)
		}
	}
}

// --- Scenario 17: cancel terminates a budget_exhausted row ----------------------------------------

// TestCancelBudgetExhaustedRowLiveDB pins that cancel (CancelRunServerSide) terminates a
// budget_exhausted (paused) wall park.
func TestCancelBudgetExhaustedRowLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("w", wpWorker{nonce: "n"})
	id := fx.run(wpRun{worker: &w, status: "paused", holdReason: "budget_exhausted"})
	rows, err := fx.q.CancelRunServerSide(fx.ctx, store.CancelRunServerSideParams{ID: id, UserID: fx.userID, StopReason: pgconvText("owner cancelled at the wall")})
	if err != nil || rows != 1 {
		t.Fatalf("CancelRunServerSide = (%d,%v), want (1,nil)", rows, err)
	}
	if fx.status(id) != "cancelled" {
		t.Fatalf("status = %q, want cancelled", fx.status(id))
	}
}

// --- Scenario 21: GetSlackRunContext selects hold_reason + budget_finalize_seconds ----------------

// TestGetSlackRunContextWallParkLiveDB pins that the Slack run-context query now selects hold_reason
// and budget_finalize_seconds for a parked row (so the Slack reply can branch on the wall park and
// omit Stop once the allowance is used).
func TestGetSlackRunContextWallParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	w := fx.worker("w", wpWorker{nonce: "n"})
	id := fx.run(wpRun{worker: &w, status: "paused", holdReason: "budget_exhausted", budgetFinalize: 1800})
	row, err := fx.q.GetSlackRunContext(fx.ctx, id)
	if err != nil {
		t.Fatalf("GetSlackRunContext: %v", err)
	}
	if !row.HoldReason.Valid || row.HoldReason.String != "budget_exhausted" {
		t.Fatalf("hold_reason = %+v, want budget_exhausted", row.HoldReason)
	}
	if row.BudgetFinalizeSeconds != 1800 {
		t.Fatalf("budget_finalize_seconds = %d, want 1800", row.BudgetFinalizeSeconds)
	}
}
