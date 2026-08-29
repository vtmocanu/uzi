package store_test

// PRD #517 M1 — the parts of interactive, long-lived task runs that only a real Postgres
// can answer: whether migration 00146's four CHECK/column changes actually took, and
// whether runs.interactive round-trips create → row.
//
// (a) The widened CHECKs must ACCEPT the new values and still accept every pre-existing
//     one — a DROP+ADD that silently forgot a value would reject a status/kind that used
//     to be legal, so re-asserting the old values is what makes the widening honest.
// (b) interactive set at create must read back true on the run row (the "silently false"
//     trap: a bool omitted from the params literal builds green but persists false).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestInteractiveTaskRunConstraintsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("interactive-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// ── (b) create → row: interactive=true persists and reads back true. ──
	id := uuid.New()
	branch := "uzi/task/" + id.String()
	run, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            id,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: branch, Valid: true},
		OpenMr:           false,
		Interactive:      true,
		IssueTitle:       "interactive handoff",
		IssueDescription: "iterate with me",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (interactive): %v", err)
	}
	if !run.Interactive {
		t.Error("interactive = false on the RETURNING row, want true (the column or the params binding is not wired)")
	}
	// Read it back from the row independently — the returned struct and the persisted
	// column must agree.
	got, err := q.GetRunByID(ctx, id)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !got.Interactive {
		t.Error("interactive did not persist: GetRunByID read false, want true")
	}

	// A plain (non-interactive) task defaults interactive=false — the DEFAULT false half
	// of the column, and the "did we accidentally hard-code true" backstop.
	plainID := uuid.New()
	plain, err := q.CreateTaskRun(ctx, store.CreateTaskRunParams{
		RunID:            plainID,
		UserID:           userID,
		RepoID:           repoID,
		Branch:           pgtype.Text{String: "uzi/task/" + plainID.String(), Valid: true},
		Interactive:      false,
		IssueTitle:       "plain",
		IssueDescription: "one-shot",
	})
	if err != nil {
		t.Fatalf("CreateTaskRun (plain): %v", err)
	}
	if plain.Interactive {
		t.Error("interactive = true for a plain handoff, want false by default")
	}

	// ── (a) runs_status_check accepts 'awaiting_followup' AND every pre-existing value. ──
	statuses := []string{
		"queued", "claimed", "running", "awaiting_approval", "awaiting_input",
		"limit_wait", "completed", "failed", "cancelled", "awaiting_followup",
	}
	for _, s := range statuses {
		if _, err := pool.Exec(ctx, `UPDATE runs SET status = $1 WHERE id = $2`, s, id); err != nil {
			t.Errorf("runs_status_check rejected status=%q, want accepted (widening dropped a value?): %v", s, err)
		}
	}
	// And a bogus status is still rejected — the CHECK is not vacuous.
	if _, err := pool.Exec(ctx, `UPDATE runs SET status = 'not_a_status' WHERE id = $1`, id); err == nil {
		t.Error("runs_status_check ACCEPTED a bogus status: the CHECK is vacuous")
	}

	// ── (a) runs_stop_kind_check accepts 'stopped' AND every pre-existing value. ──
	// stop_kind is display-only, so it can be set independently of status.
	for _, k := range []string{"cancelled", "plan_rejected", "auto_stopped", "stopped"} {
		if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = $1 WHERE id = $2`, k, id); err != nil {
			t.Errorf("runs_stop_kind_check rejected stop_kind=%q, want accepted: %v", k, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = 'bogus' WHERE id = $1`, id); err == nil {
		t.Error("runs_stop_kind_check ACCEPTED a bogus stop_kind: the CHECK is vacuous")
	}

	// ── (a) run_user_inputs_kind_check accepts 'stop' AND every pre-existing value. ──
	for _, k := range []string{"follow_up", "approve_plan", "reject_plan", "cancel", "revise_plan", "answer", "stop"} {
		if _, err := pool.Exec(ctx, `INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, $2)`, id, k); err != nil {
			t.Errorf("run_user_inputs_kind_check rejected kind=%q, want accepted: %v", k, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_user_inputs (run_id, kind) VALUES ($1, 'bogus')`, id); err == nil {
		t.Error("run_user_inputs_kind_check ACCEPTED a bogus kind: the CHECK is vacuous")
	}
}

// followupWatermarkFixture bundles the shared seeding + query closures the issue #559 M1
// watermark tests below all need: a fresh online worker, a running interactive task run,
// and helpers to append a follow_up, consume the queue, park with a worker-provided
// watermark, and read the stamped open_followup_id back. It reuses the followupFixture
// helpers from interactive_task_runs_recovery_livedb_test.go (same package).
type followupWatermarkFixture struct {
	f   *followupFixture
	ctx context.Context
	wkr uuid.UUID
	run uuid.UUID
}

func newFollowupWatermarkFixture(ctx context.Context, t *testing.T, dsn string) (*followupWatermarkFixture, func()) {
	t.Helper()
	f, done := setupFollowup(ctx, t, dsn)
	wkr := f.seedWorker(ctx, t, "online", pgtype.Int4{}, false)
	run := f.seedInteractiveTaskRun(ctx, t, "running", &wkr)
	return &followupWatermarkFixture{f: f, ctx: ctx, wkr: wkr, run: run}, done
}

// createConsumedFollowup appends a follow_up steering input and consumes the queue,
// returning the new input's id (a member of the run's max-consumed set thereafter).
func (w *followupWatermarkFixture) createConsumedFollowup(t *testing.T) int64 {
	t.Helper()
	in, err := w.f.q.CreateRunInput(w.ctx, store.CreateRunInputParams{
		RunID: w.run, Kind: "follow_up", Body: pgT(`{"body":"steer"}`),
	})
	if err != nil {
		t.Fatalf("CreateRunInput(follow_up): %v", err)
	}
	if _, err := w.f.q.ConsumeRunInputs(w.ctx, w.run); err != nil {
		t.Fatalf("ConsumeRunInputs: %v", err)
	}
	return in.ID
}

// park reports awaiting_followup with the given worker-provided watermark. provided==nil
// omits it (old worker / first park), so the server's COALESCE fallback fires.
func (w *followupWatermarkFixture) park(t *testing.T, provided *int64) {
	t.Helper()
	p := pgtype.Int8{}
	if provided != nil {
		p = pgtype.Int8{Int64: *provided, Valid: true}
	}
	rows, err := w.f.q.SetRunAwaitingFollowup(w.ctx, store.SetRunAwaitingFollowupParams{
		OpenFollowupID: p, ID: w.run, WorkerID: pgU(w.wkr),
	})
	if err != nil || rows != 1 {
		t.Fatalf("SetRunAwaitingFollowup(provided=%v): rows=%d err=%v", provided, rows, err)
	}
}

// watermark reads the stamped open_followup_id (Int64 is 0 when NULL, matching the guard).
func (w *followupWatermarkFixture) watermark(t *testing.T) int64 {
	t.Helper()
	got, err := w.f.q.GetRunByID(w.ctx, w.run)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	return got.OpenFollowupID.Int64
}

// ptr is a tiny helper so a test can pass a *int64 literal to park().
func ptrInt64(v int64) *int64 { return &v }

// Issue #559 M1: a WORKER-PROVIDED watermark ≤ the run's max-consumed is stamped VERBATIM.
// Two follow_ups are consumed (id1 < id2, so max-consumed == id2); the worker reports id1
// (it only applied id1), and the LEAST(provided, max-consumed) clamp leaves id1 untouched
// because id1 < id2. Proves the provided value flows through, not the server subquery.
func TestInteractiveTaskRunsFollowupWatermarkWorkerProvidedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	w, done := newFollowupWatermarkFixture(ctx, t, dsn)
	defer done()

	id1 := w.createConsumedFollowup(t)
	id2 := w.createConsumedFollowup(t)
	if id2 <= id1 {
		t.Fatalf("run_user_inputs.id not monotone: id2=%d id1=%d", id2, id1)
	}
	w.park(t, ptrInt64(id1))
	if got := w.watermark(t); got != id1 {
		t.Fatalf("open_followup_id = %d, want the worker-provided id1=%d (max-consumed was id2=%d) — "+
			"the provided value did not flow through, or the clamp over-clamped", got, id1, id2)
	}
}

// Issue #559 M1: THE RACE. Two follow_ups are consumed (M < N, so N was consumed DURING
// what would be this park's DB round-trip), but the worker only APPLIED M and reports M.
// The pure-server watermark would fold N in and permanently strand the run (the wake guard
// would then never see a follow_up NEWER than the watermark). With the worker-provided
// value, the watermark is M, so N (> M, consumed) still admits awaiting_followup → running.
func TestInteractiveTaskRunsFollowupWatermarkRaceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	w, done := newFollowupWatermarkFixture(ctx, t, dsn)
	defer done()

	m := w.createConsumedFollowup(t)
	n := w.createConsumedFollowup(t) // consumed during the park's round-trip; not yet applied by the worker
	if n <= m {
		t.Fatalf("run_user_inputs.id not monotone: n=%d m=%d", n, m)
	}
	w.park(t, ptrInt64(m))
	if got := w.watermark(t); got != m {
		t.Fatalf("open_followup_id = %d, want M=%d (NOT N=%d) — the server folded the raced-in "+
			"follow_up into the watermark and would strand the run", got, m, n)
	}
	// N (> M) is consumed, so the wake guard admits the resume: the run is NOT stranded.
	rows, err := w.f.q.SetRunRunning(ctx, store.SetRunRunningParams{
		ID: w.run, WorkerID: pgU(w.wkr), IterationCount: 1,
	})
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunRunning matched %d rows, want 1 — N=%d (> watermark M=%d, consumed) should admit "+
			"the wake, proving the run is not stranded", rows, n, m)
	}
	if got := w.f.status(ctx, t, w.run); got != "running" {
		t.Fatalf("status = %q after wake, want running", got)
	}
}

// Issue #559 M1: the CLAMP. A buggy worker reports a watermark FAR above any consumed
// follow_up. LEAST(provided, max-consumed) clamps it down to max-consumed, so it cannot
// permanently strand the run (an unclamped huge value would exceed every future follow_up
// id and the wake guard would never admit a resume).
func TestInteractiveTaskRunsFollowupWatermarkClampLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	w, done := newFollowupWatermarkFixture(ctx, t, dsn)
	defer done()

	maxConsumed := w.createConsumedFollowup(t)
	w.park(t, ptrInt64(maxConsumed+1_000_000))
	if got := w.watermark(t); got != maxConsumed {
		t.Fatalf("open_followup_id = %d, want it clamped down to max-consumed=%d — a buggy huge "+
			"worker value was NOT neutralized and would strand the run", got, maxConsumed)
	}
}

// Issue #559 M1: BACKWARD-COMPAT. An old worker omits open_followup_id (NULL param), so the
// COALESCE fallback recomputes the server-derived max-consumed — byte-identical to the
// pre-#559 behavior. Also covers the first-park case (nothing consumed → watermark 0).
func TestInteractiveTaskRunsFollowupWatermarkFallbackLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	w, done := newFollowupWatermarkFixture(ctx, t, dsn)
	defer done()

	// First park, nothing consumed, no provided value → server-derived MAX over no rows = 0.
	w.park(t, nil)
	if got := w.watermark(t); got != 0 {
		t.Fatalf("open_followup_id on first old-worker park = %d, want 0 (server-derived floor)", got)
	}
	// Two consumed follow_ups, still omitting the provided value → falls back to max-consumed.
	_ = w.createConsumedFollowup(t)
	id2 := w.createConsumedFollowup(t)
	w.park(t, nil)
	if got := w.watermark(t); got != id2 {
		t.Fatalf("open_followup_id on old-worker re-park = %d, want the server-derived max-consumed=%d — "+
			"the NULL-param fallback is not byte-identical to the pre-#559 behavior", got, id2)
	}
}
