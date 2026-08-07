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

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
