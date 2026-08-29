package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestRunMilestoneProgressLiveDB pins PRD #122 M2's progress + budget SQL against a
// REAL Postgres — the derived budget (a server-side CASE over jsonb_array_length),
// the union-on-write for milestones_completed, the overwrite for milestones_in_progress,
// and the per-run SweepRunningTimeout cutoff. None of these live in Go, so a fake store
// cannot exercise them.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one).
func TestRunMilestoneProgressLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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
		userID, fmt.Sprintf("mprogress-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("mprogress-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	// The budget config a real SetState/approve passes (workersvc constants + testParams
	// defaults): RUN_MAX_ITERATIONS=5, RUN_TIMEOUT=7200s, cap=12, wall ceiling=28800s.
	const (
		runMaxIter    = 5
		runTimeout    = 7200
		budgetCap     = 12
		wallCeiling   = 28800
		issueIIDStart = 1
	)

	// milestoneList builds a candidate/frozen list of n {id,title} entries.
	milestoneList := func(n int) []byte {
		type m struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		out := make([]m, n)
		for i := range out {
			out[i] = m{ID: fmt.Sprintf("m%d", i+1), Title: fmt.Sprintf("Milestone %d", i+1)}
		}
		b, _ := json.Marshal(out)
		return b
	}

	// readBudget returns the two int budget columns as *int32 (nil when NULL).
	readBudget := func(id uuid.UUID) (maxIter, wall *int32) {
		t.Helper()
		var mi, w pgtype.Int4
		if err := pool.QueryRow(ctx,
			`SELECT budget_max_iterations, budget_wall_seconds FROM runs WHERE id = $1`, id).Scan(&mi, &w); err != nil {
			t.Fatalf("read budget of %s: %v", id, err)
		}
		if mi.Valid {
			maxIter = &mi.Int32
		}
		if w.Valid {
			wall = &w.Int32
		}
		return
	}

	// readPaused returns a run's budget_paused_seconds (issue #783), which is NOT NULL.
	readPaused := func(id uuid.UUID) int32 {
		t.Helper()
		var p int32
		if err := pool.QueryRow(ctx,
			`SELECT budget_paused_seconds FROM runs WHERE id = $1`, id).Scan(&p); err != nil {
			t.Fatalf("read budget_paused_seconds of %s: %v", id, err)
		}
		return p
	}

	// readIDs decodes a jsonb id-array column into a sorted []string (nil when NULL), so
	// set assertions are order-independent (jsonb_agg order is not guaranteed).
	readIDs := func(id uuid.UUID, col string) []string {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM runs WHERE id = $1`, col), id).Scan(&raw); err != nil {
			t.Fatalf("read %s of %s: %v", col, id, err)
		}
		if raw == nil {
			return nil
		}
		var ids []string
		if err := json.Unmarshal(raw, &ids); err != nil {
			t.Fatalf("decode %s of %s: %v (%s)", col, id, err, raw)
		}
		sort.Strings(ids)
		return ids
	}

	nextIID := int64(issueIIDStart)
	newRun := func(status string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, 'issue')`, id, userID, repoID, nextIID, status, wkr.ID)
		nextIID++
		return id
	}

	// ── Freeze budget on the human-gated approve: 7 milestones → 5*7=35 turns and
	//    min(7200*7, 28800)=28800s wall; 1 milestone → both NULL (global default). ──
	t.Run("approve freezes a scaled budget", func(t *testing.T) {
		run := newRun("running")
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: milestoneList(7), ID: run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunAwaitingApproval: %v", err)
		}
		if _, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: run, Body: pgtype.Text{String: "{}", Valid: true},
			AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("CreateApprovePlanInput: %v", err)
		}
		mi, w := readBudget(run)
		if mi == nil || *mi != 35 || w == nil || *w != 28800 {
			t.Fatalf("scaled budget = %v/%v, want 35/28800", ptrStr(mi), ptrStr(w))
		}
	})

	t.Run("approve of a single-milestone plan leaves the budget NULL", func(t *testing.T) {
		run := newRun("running")
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: milestoneList(1), ID: run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunAwaitingApproval: %v", err)
		}
		if _, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: run, Body: pgtype.Text{String: "{}", Valid: true},
			AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("CreateApprovePlanInput: %v", err)
		}
		if mi, w := readBudget(run); mi != nil || w != nil {
			t.Fatalf("single-milestone budget = %v/%v, want NULL/NULL", ptrStr(mi), ptrStr(w))
		}
	})

	// ── The count cap: 40 frozen milestones is under the 50 STORAGE cap but the budget
	//    scales by only min(40,12)=12, so max_iter=5*12=60 and wall=min(7200*12,28800)
	//    =28800. This is the positive control for LEAST(n, @milestone_budget_cap) — without
	//    it a regression that raised or removed the cap would ship green. ──
	t.Run("approve caps the budget scaling at 12 while storing all 40 milestones", func(t *testing.T) {
		run := newRun("running")
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: milestoneList(40), ID: run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunAwaitingApproval: %v", err)
		}
		if _, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: run, Body: pgtype.Text{String: "{}", Valid: true},
			AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("CreateApprovePlanInput: %v", err)
		}
		mi, w := readBudget(run)
		if mi == nil || *mi != 60 || w == nil || *w != 28800 {
			t.Fatalf("capped budget = %v/%v, want 60/28800 (5*min(40,12), min(7200*12,28800))", ptrStr(mi), ptrStr(w))
		}
		// The full 40-entry list is still frozen: the 50 storage cap, not the 12 budget
		// cap, bounds what is stored.
		var frozenLen int
		if err := pool.QueryRow(ctx,
			`SELECT jsonb_array_length(milestones_frozen) FROM runs WHERE id = $1`, run).Scan(&frozenLen); err != nil {
			t.Fatalf("read frozen length: %v", err)
		}
		if frozenLen != 40 {
			t.Fatalf("frozen list length = %d, want all 40 stored (budget cap is not a storage cap)", frozenLen)
		}
	})

	// ── Autopilot: SetRunRunning with a 7-milestone frozen list derives the SAME budget,
	//    written immutably (COALESCE). ──
	t.Run("autopilot running report freezes the same budget", func(t *testing.T) {
		run := newRun("claimed")
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			MilestonesFrozen: milestoneList(7), ID: run, WorkerID: workerID,
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
		mi, w := readBudget(run)
		if mi == nil || *mi != 35 || w == nil || *w != 28800 {
			t.Fatalf("autopilot budget = %v/%v, want 35/28800", ptrStr(mi), ptrStr(w))
		}
		// A later heartbeat cannot change the frozen budget (COALESCE keeps it).
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (heartbeat): %v", err)
		}
		if mi, w := readBudget(run); mi == nil || *mi != 35 || w == nil || *w != 28800 {
			t.Fatalf("budget changed on heartbeat = %v/%v, want a stable 35/28800", ptrStr(mi), ptrStr(w))
		}
	})

	// ── Progress: completed is UNIONED (monotone, idempotent); in_progress is OVERWRITTEN. ──
	t.Run("completed unions and in_progress overwrites", func(t *testing.T) {
		run := newRun("running")
		report := func(completed, inProgress []byte) {
			t.Helper()
			if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
				ID: run, WorkerID: workerID,
				MilestonesCompleted: completed, MilestonesInProgress: inProgress,
				RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
				MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
			}); err != nil {
				t.Fatalf("SetRunRunning (progress): %v", err)
			}
		}

		report([]byte(`["m1"]`), []byte(`["m4"]`))
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1"}) {
			t.Fatalf("completed after [m1] = %v", got)
		}
		if got := readIDs(run, "milestones_in_progress"); !eqIDs(got, []string{"m4"}) {
			t.Fatalf("in_progress after [m4] = %v", got)
		}

		// completed unions to {m1,m3}; in_progress overwrites to {m5}.
		report([]byte(`["m3"]`), []byte(`["m5"]`))
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1", "m3"}) {
			t.Fatalf("completed after union [m3] = %v, want {m1,m3}", got)
		}
		if got := readIDs(run, "milestones_in_progress"); !eqIDs(got, []string{"m5"}) {
			t.Fatalf("in_progress after overwrite [m5] = %v, want {m5}", got)
		}

		// Re-reporting m1 is idempotent (DISTINCT): the set stays {m1,m3}.
		report([]byte(`["m1"]`), nil)
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1", "m3"}) {
			t.Fatalf("completed after re-report [m1] = %v, want a stable {m1,m3}", got)
		}
		// A nil in_progress param leaves the column untouched (still {m5}).
		if got := readIDs(run, "milestones_in_progress"); !eqIDs(got, []string{"m5"}) {
			t.Fatalf("in_progress disturbed by a nil param = %v, want {m5}", got)
		}
	})

	// ── PRD #265 M1: SetRunCompleted reconciles the tracker. The lead's signal_done
	//    declaration is UNIONED into milestones_completed (never overwritten), and the
	//    in_progress snapshot is cleared. This is the R2 regression: a run that reported
	//    {m1,m2} via a mid-run report and then declares {m3} on completion must end at
	//    {m1,m2,m3}, proving the completion path copied SetRunRunning's UNION and not the
	//    plain assignment mr_iid/prd_done_path use. ──
	t.Run("completed unions the signal_done declaration and clears in_progress", func(t *testing.T) {
		run := newRun("running")
		// Mid-run: report {m1,m2} complete and {m3} in progress.
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			MilestonesCompleted: []byte(`["m1","m2"]`), MilestonesInProgress: []byte(`["m3"]`),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (progress): %v", err)
		}
		// Completion declares {m3}: the union must yield {m1,m2,m3}, NOT {m3}.
		if _, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:              pgtype.Text{String: "agent/issue-1", Valid: true},
			MilestonesCompleted: []byte(`["m3"]`),
			ID:                  run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1", "m2", "m3"}) {
			t.Fatalf("completed after signal_done union = %v, want {m1,m2,m3} (UNION, not overwrite)", got)
		}
		if got := readIDs(run, "milestones_in_progress"); got != nil {
			t.Fatalf("in_progress must be cleared on completion, got %v", got)
		}
	})

	// ── PRD #265 M1: a completion that declares NOTHING (nil param) leaves
	//    milestones_completed untouched — additive-absent, byte-identical to pre-#265. The
	//    in_progress snapshot is still cleared (the clear is unconditional, not tied to the
	//    declaration). ──
	t.Run("completed with no declaration leaves completed untouched and still clears in_progress", func(t *testing.T) {
		run := newRun("running")
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: []byte(`["m2"]`),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (progress): %v", err)
		}
		if _, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:              pgtype.Text{String: "agent/issue-1", Valid: true},
			MilestonesCompleted: nil, // nothing declared
			ID:                  run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1"}) {
			t.Fatalf("completed with no declaration = %v, want an untouched {m1}", got)
		}
		if got := readIDs(run, "milestones_in_progress"); got != nil {
			t.Fatalf("in_progress must be cleared on completion, got %v", got)
		}
	})

	// ── PRD #265 D4: a FAILED terminal transition also clears the in_progress snapshot
	//    (but never back-fills completed). ──
	t.Run("failed clears in_progress and does not touch completed", func(t *testing.T) {
		run := newRun("running")
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: []byte(`["m2"]`),
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (progress): %v", err)
		}
		if _, err := q.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: pgtype.Text{String: "boom", Valid: true},
			ID:            run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunFailed: %v", err)
		}
		if got := readIDs(run, "milestones_completed"); !eqIDs(got, []string{"m1"}) {
			t.Fatalf("failed must not back-fill completed = %v, want {m1}", got)
		}
		if got := readIDs(run, "milestones_in_progress"); got != nil {
			t.Fatalf("in_progress must be cleared on failure, got %v", got)
		}
	})

	// ── PRD #390 M4/D5: the SERVER CONTRACT the neutral (`–/N`) render depends on. A
	//    running heartbeat that carries NO progress (MilestonesCompleted AND
	//    MilestonesInProgress both nil — exactly what the agent sends post-M1 when nothing
	//    real was reported) must leave milestones_completed as SQL NULL, so
	//    DecodeMilestoneIDs yields a NIL slice (not a non-nil empty `[]`). That NULL is what
	//    the DTO marshals as `null`, which the web badge and CLI both render neutral rather
	//    than as `0/N`. The complement to M1's agent-side no-signal proof. ──
	t.Run("a no-progress running heartbeat leaves milestones_completed NULL", func(t *testing.T) {
		run := newRun("claimed")
		// Freeze a milestone breakdown and go running — but report NO progress.
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			MilestonesFrozen: milestoneList(3), ID: run, WorkerID: workerID,
			MilestonesCompleted: nil, MilestonesInProgress: nil,
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (freeze, no progress): %v", err)
		}
		// A second plain heartbeat, still with no progress, must not conjure a `[]` either.
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			MilestonesCompleted: nil, MilestonesInProgress: nil,
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (heartbeat, no progress): %v", err)
		}

		// Read the raw column and decode via the production path: NULL ⇒ nil slice.
		raw := rawMilestoneCol(ctx, t, pool, run, "milestones_completed")
		if raw != nil {
			t.Fatalf("milestones_completed must be SQL NULL after a no-progress heartbeat, got raw jsonb %q", raw)
		}
		ids, err := workersvc.DecodeMilestoneIDs(raw)
		if err != nil {
			t.Fatalf("DecodeMilestoneIDs(NULL): %v", err)
		}
		if ids != nil {
			t.Fatalf("a NULL milestones_completed must decode to a NIL slice (never-reported), got %#v (len %d)", ids, len(ids))
		}

		// Discriminating / non-vacuity guard: a heartbeat WITH a real completed report
		// makes the same column non-NULL — so this test would fail if the column were
		// always NULL (or always non-NULL) regardless of the param.
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			ID: run, WorkerID: workerID,
			MilestonesCompleted: []byte(`["m1"]`), MilestonesInProgress: nil,
			RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
			MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
		}); err != nil {
			t.Fatalf("SetRunRunning (real progress): %v", err)
		}
		raw = rawMilestoneCol(ctx, t, pool, run, "milestones_completed")
		if raw == nil {
			t.Fatalf("milestones_completed must be non-NULL after a real report, got SQL NULL")
		}
		ids, err = workersvc.DecodeMilestoneIDs(raw)
		if err != nil {
			t.Fatalf("DecodeMilestoneIDs(reported): %v", err)
		}
		if !eqIDs(ids, []string{"m1"}) {
			t.Fatalf("a reported milestones_completed must decode to {m1}, got %#v", ids)
		}
	})

	// ── SweepRunningTimeout honours the PER-RUN wall clock (Decision 5b). ──
	t.Run("sweep honours the per-run wall clock", func(t *testing.T) {
		base := time.Now().UTC()
		// budget_wall=28800s (8h), started 3h ago → NOT swept.
		survives := newRun("running")
		mustExec(ctx, t, pool, `UPDATE runs SET started_at = $2, budget_wall_seconds = 28800 WHERE id = $1`,
			survives, base.Add(-3*time.Hour))
		// budget_wall=28800s (8h), started 9h ago → swept.
		scaledOver := newRun("running")
		mustExec(ctx, t, pool, `UPDATE runs SET started_at = $2, budget_wall_seconds = 28800 WHERE id = $1`,
			scaledOver, base.Add(-9*time.Hour))
		// NULL budget, started 3h ago → swept at the global 2h.
		globalOver := newRun("running")
		mustExec(ctx, t, pool, `UPDATE runs SET started_at = $2 WHERE id = $1`,
			globalOver, base.Add(-3*time.Hour))

		swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason:        pgtype.Text{String: "run exceeded RUN_TIMEOUT", Valid: true},
			Now:                  pgtype.Timestamptz{Time: base, Valid: true},
			GlobalTimeoutSeconds: runTimeout,
		})
		if err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		got := map[uuid.UUID]bool{}
		for _, r := range swept {
			got[r.ID] = true
		}
		if got[survives] {
			t.Fatalf("a scaled run 3h into an 8h budget must NOT be swept")
		}
		if !got[scaledOver] {
			t.Fatalf("a scaled run 9h into an 8h budget MUST be swept")
		}
		if !got[globalOver] {
			t.Fatalf("a NULL-budget run 3h in MUST be swept at the global 2h")
		}
	})

	// ── Issue #783: SweepRunningTimeout EXCLUDES budget_paused_seconds (time parked at a
	//    human gate) from the deadline, so gate-wait does not consume the wall budget. ──
	t.Run("sweep excludes banked parked time from the wall deadline", func(t *testing.T) {
		base := time.Now().UTC()
		// 8h budget + 2h banked park = 10h deadline; started 9h ago → 9h < 10h → NOT swept.
		underParked := newRun("running")
		mustExec(ctx, t, pool,
			`UPDATE runs SET started_at = $2, budget_wall_seconds = 28800, budget_paused_seconds = 7200 WHERE id = $1`,
			underParked, base.Add(-9*time.Hour))
		// Same shape but started 11h ago → 11h > 10h → swept.
		overParked := newRun("running")
		mustExec(ctx, t, pool,
			`UPDATE runs SET started_at = $2, budget_wall_seconds = 28800, budget_paused_seconds = 7200 WHERE id = $1`,
			overParked, base.Add(-11*time.Hour))

		swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason:        pgtype.Text{String: "run exceeded RUN_TIMEOUT", Valid: true},
			Now:                  pgtype.Timestamptz{Time: base, Valid: true},
			GlobalTimeoutSeconds: runTimeout,
		})
		if err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		got := map[uuid.UUID]bool{}
		for _, r := range swept {
			got[r.ID] = true
		}
		if got[underParked] {
			t.Fatalf("a run 9h into an 8h budget + 2h banked park (10h deadline) must NOT be swept")
		}
		if !got[overParked] {
			t.Fatalf("a run 11h into an 8h budget + 2h banked park (10h deadline) MUST be swept")
		}
	})

	// ── Issue #783: the same exclusion holds on the COALESCE fallback branch — a run with
	//    NULL budget_wall_seconds falls back to GlobalTimeoutSeconds, and banked park time
	//    must still be added to THAT deadline (the seeded / non-gated run path). ──
	t.Run("sweep excludes banked parked time on the global-timeout fallback", func(t *testing.T) {
		base := time.Now().UTC()
		// runTimeout=7200s (2h) global + 1h banked park = 3h deadline; NULL budget_wall_seconds.
		// Started 2h30m ago: 2.5h < 3h → NOT swept — but 2.5h > the bare 2h global, so WITHOUT
		// the pause bank this run WOULD be swept. Sitting inside the pause-credit window is what
		// makes this assertion actually require budget_paused_seconds (a boundary-exact 2h would
		// pass even if the bank were dropped, since the deadline test is a strict `<`).
		underGlobal := newRun("running")
		mustExec(ctx, t, pool,
			`UPDATE runs SET started_at = $2, budget_wall_seconds = NULL, budget_paused_seconds = 3600 WHERE id = $1`,
			underGlobal, base.Add(-150*time.Minute))
		// Same shape but started 4h ago → 4h > 3h → swept.
		overGlobal := newRun("running")
		mustExec(ctx, t, pool,
			`UPDATE runs SET started_at = $2, budget_wall_seconds = NULL, budget_paused_seconds = 3600 WHERE id = $1`,
			overGlobal, base.Add(-4*time.Hour))

		swept, err := q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
			FailureReason:        pgtype.Text{String: "run exceeded RUN_TIMEOUT", Valid: true},
			Now:                  pgtype.Timestamptz{Time: base, Valid: true},
			GlobalTimeoutSeconds: runTimeout,
		})
		if err != nil {
			t.Fatalf("SweepRunningTimeout: %v", err)
		}
		got := map[uuid.UUID]bool{}
		for _, r := range swept {
			got[r.ID] = true
		}
		if got[underGlobal] {
			t.Fatalf("a NULL-budget run 2h30m into a 2h global + 1h banked park (3h deadline) must NOT be swept")
		}
		if !got[overGlobal] {
			t.Fatalf("a NULL-budget run 4h into a 2h global + 1h banked park (3h deadline) MUST be swept")
		}
	})

	// ── Issue #783: SetRunRunning banks the park duration on a real park→running resume,
	//    and never double-counts on the running→running heartbeat. ──
	t.Run("running resume from awaiting_approval banks the park duration once", func(t *testing.T) {
		run := newRun("claimed")
		// Park it at awaiting_approval with status_since (and started_at) 600s in the past.
		mustExec(ctx, t, pool,
			`UPDATE runs SET status = 'awaiting_approval', status_since = now() - interval '600 seconds',
			     started_at = now() - interval '600 seconds' WHERE id = $1`, run)
		// Satisfy SetRunRunning's awaiting_approval resume guard with a CONSUMED approve_plan.
		mustExec(ctx, t, pool,
			`INSERT INTO run_user_inputs (run_id, kind, body, consumed_at) VALUES ($1, 'approve_plan', '{}', now())`, run)

		setRunning := func(what string) {
			t.Helper()
			if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
				ID: run, WorkerID: workerID,
				RunMaxIterations: runMaxIter, RunTimeoutSeconds: runTimeout,
				MilestoneBudgetCap: budgetCap, BudgetWallCeilingSeconds: wallCeiling,
			}); err != nil {
				t.Fatalf("SetRunRunning (%s): %v", what, err)
			}
		}

		setRunning("resume")
		if got := readPaused(run); got < 595 || got > 610 {
			t.Fatalf("banked park time after resume = %ds, want ~600 (595..610)", got)
		}
		// A running→running heartbeat must NOT double-count (old status is 'running' → 0).
		setRunning("heartbeat")
		if got := readPaused(run); got < 595 || got > 610 {
			t.Fatalf("banked park time changed on heartbeat = %ds, want an unchanged ~600", got)
		}
	})

	// ── Issue #783: RequeueRunsOfStaleWorkers banks park time before the worker-death
	//    requeue → queued, and leaves started_at untouched. ──
	t.Run("requeue of a stale worker's parked run banks the park duration", func(t *testing.T) {
		w2, err := q.CreateWorker(ctx, store.CreateWorkerParams{
			UserID: userID, Name: "stale", TokenHash: append([]byte("mprog-stale-"), userID[:]...),
			AnthropicBindMode: "default",
		})
		if err != nil {
			t.Fatalf("CreateWorker: %v", err)
		}
		run := newRun("claimed")
		startedAt := time.Now().UTC().Add(-30 * time.Minute)
		mustExec(ctx, t, pool,
			`UPDATE runs SET status = 'awaiting_approval', status_since = now() - interval '600 seconds',
			     started_at = $2, worker_id = $3 WHERE id = $1`, run, startedAt, w2.ID)
		// A sibling parked at awaiting_input on the same stale worker: the requeue banks
		// BOTH statuses (its CASE keys on status IN ('awaiting_approval','awaiting_input')),
		// so covering only awaiting_approval would miss a regression that drops awaiting_input.
		// It also starts with a NON-zero prior bank (120s): the requeue ADDS the new park
		// (budget_paused_seconds = budget_paused_seconds + …), so asserting the accumulated
		// ~720s catches a regression that overwrote the bank instead of adding to it.
		runAI := newRun("claimed")
		mustExec(ctx, t, pool,
			`UPDATE runs SET status = 'awaiting_input', status_since = now() - interval '600 seconds',
			     started_at = $2, worker_id = $3, budget_paused_seconds = 120 WHERE id = $1`, runAI, startedAt, w2.ID)
		// Make the worker's heartbeat stale so the requeue picks it up.
		mustExec(ctx, t, pool,
			`UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, w2.ID)

		requeued, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
			MaxRequeues: 5,
			Cutoff:      pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		})
		if err != nil {
			t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
		}
		foundApproval, foundInput := false, false
		for _, r := range requeued {
			switch r.ID {
			case run:
				foundApproval = true
			case runAI:
				foundInput = true
			}
		}
		if !foundApproval || !foundInput {
			t.Fatalf("both parked runs must be requeued (awaiting_approval=%v awaiting_input=%v)", foundApproval, foundInput)
		}
		if got := readPaused(run); got < 595 || got > 610 {
			t.Fatalf("awaiting_approval park time on requeue = %ds, want ~600 (595..610)", got)
		}
		// runAI seeded a 120s prior bank, so the accumulated value is ~720 (120 + ~600):
		// a range that a bank-overwrite regression (~600) would fail.
		if got := readPaused(runAI); got < 715 || got > 730 {
			t.Fatalf("awaiting_input accumulated park time on requeue = %ds, want ~720 (715..730)", got)
		}
		// started_at must be untouched by the requeue — assert for BOTH parked runs, since a
		// regression could reset it for only one status.
		assertStartedUnchanged := func(id uuid.UUID, label string) {
			t.Helper()
			var gotStarted time.Time
			if err := pool.QueryRow(ctx, `SELECT started_at FROM runs WHERE id = $1`, id).Scan(&gotStarted); err != nil {
				t.Fatalf("read started_at (%s): %v", label, err)
			}
			if d := gotStarted.Sub(startedAt); d < -2*time.Second || d > 2*time.Second {
				t.Fatalf("started_at (%s) moved by %v on requeue, want unchanged", label, d)
			}
		}
		assertStartedUnchanged(run, "awaiting_approval")
		assertStartedUnchanged(runAI, "awaiting_input")
	})

	// ── Issue #783: PromoteLimitWaitRuns gives the resumed run a FRESH wall (started_at =
	//    NULL), so the pause banked against the discarded baseline must be cleared too —
	//    otherwise stale gate-wait credit inflates the new deadline (defeats RUN_TIMEOUT). ──
	t.Run("limit_wait promotion clears the banked pause and nulls started_at", func(t *testing.T) {
		run := newRun("claimed")
		// Park it at limit_wait with a banked pause of 7200s and a started_at 1h in the past;
		// retry_not_before in the past makes it eligible for promotion.
		mustExec(ctx, t, pool,
			`UPDATE runs SET status = 'limit_wait', budget_paused_seconds = 7200,
			     started_at = now() - interval '1 hour', retry_not_before = now() - interval '1 minute'
			 WHERE id = $1`, run)

		promoted, err := q.PromoteLimitWaitRuns(ctx, pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true})
		if err != nil {
			t.Fatalf("PromoteLimitWaitRuns: %v", err)
		}
		found := false
		for _, r := range promoted {
			if r.ID == run {
				found = true
			}
		}
		if !found {
			t.Fatalf("the parked limit_wait run must be promoted")
		}
		// The fresh wall discards started_at, so the banked pause must be cleared.
		if got := readPaused(run); got != 0 {
			t.Fatalf("budget_paused_seconds after limit_wait promotion = %d, want 0", got)
		}
		var startedAt pgtype.Timestamptz
		if err := pool.QueryRow(ctx, `SELECT started_at FROM runs WHERE id = $1`, run).Scan(&startedAt); err != nil {
			t.Fatalf("read started_at: %v", err)
		}
		if startedAt.Valid {
			t.Fatalf("started_at after limit_wait promotion is %v, want NULL (fresh wall)", startedAt.Time)
		}
	})
}

// rawMilestoneCol returns a run's raw jsonb id-array column bytes, nil when SQL NULL. It
// is the discriminating read for PRD #390 D5: the neutral render turns on the column being
// NULL vs a non-nil `[]`, a distinction readIDs (which folds both to nil) deliberately drops.
func rawMilestoneCol(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID, col string) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM runs WHERE id = $1`, col), id).Scan(&raw); err != nil {
		t.Fatalf("read raw %s of %s: %v", col, id, err)
	}
	return raw
}

func ptrStr(v *int32) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%d", *v)
}

func eqIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
