package store_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the C1 store-level proof for PRD #1332 M5A (the dark Codex routing
// foundation): every production run-creation origin writes harness='claude', the
// preference columns default NULL, FreezeRunCodexBinding atomically marks its row Codex
// (deletion-proof and idempotent), the binding-coherence CHECK rejects an incoherent row,
// and the conservative cost_status conflict/rollup rules hold. All tests are LiveDB-suffixed
// so ./e2e/run-store-it.sh runs them against a real Postgres.

// hText builds a valid (non-NULL) pgtype.Text.
func hText(s string) pgtype.Text { return pgconv.Text(s) }

// hInt8 builds a valid pgtype.Int8.
func hInt8(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }

// micro builds a numeric(12,6) cost from an exact microdollar count (no float rounding).
func micro(n int64) pgtype.Numeric { return pgtype.Numeric{Int: big.NewInt(n), Exp: -6, Valid: true} }

// microsOf reads a numeric cost back as an exact microdollar integer (numeric(12,6) carries
// six fractional digits), independent of how pgx normalizes the scanned value's Exp. It fails
// the test on a NULL/NaN numeric or on sub-microdollar precision, so a value comparison can
// never silently pass on a mangled read.
func microsOf(t *testing.T, n pgtype.Numeric) int64 {
	t.Helper()
	if !n.Valid || n.NaN {
		t.Fatalf("cost_usd is not a valid finite numeric: %+v", n)
	}
	if n.Int == nil {
		return 0
	}
	// value = Int * 10^Exp; microdollars = value * 10^6 = Int * 10^(Exp+6).
	shift := int(n.Exp) + 6
	v := new(big.Int).Set(n.Int)
	ten := big.NewInt(10)
	if shift >= 0 {
		v.Mul(v, new(big.Int).Exp(ten, big.NewInt(int64(shift)), nil))
	} else {
		div := new(big.Int).Exp(ten, big.NewInt(int64(-shift)), nil)
		var rem big.Int
		v.QuoRem(v, div, &rem)
		if rem.Sign() != 0 {
			t.Fatalf("cost_usd %+v carries sub-microdollar precision", n)
		}
	}
	return v.Int64()
}

// harnessEnv holds the live-DB handles + a fresh user/connection/repo for one test.
type harnessEnv struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
}

func setupHarnessEnv(t *testing.T) *harnessEnv {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("harness-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID,
		fmt.Sprintf("g/r-%s", repoID))
	return &harnessEnv{ctx: ctx, pool: pool, q: store.New(pool), userID: userID, repoID: repoID}
}

// TestRunCreationOriginsWriteClaudeHarnessLiveDB reaches EVERY one of the twelve production
// INSERT INTO runs origins and asserts the resulting runs.harness='claude' — pinned by
// query/path, not a count (PRD #1332 M5A / D2). What this proves is narrow and exact: every
// origin yields a run with harness='claude'. It does NOT prove each origin writes the literal
// 'claude' explicitly — because runs.harness DEFAULTs to 'claude' and the literal each origin
// writes equals that default, an explicit write and a silently-omitted harness column are
// indistinguishable here (both surface 'claude'). That literal-vs-default distinction only
// becomes observable once an origin can resolve a non-'claude' harness — a flag for M5B, when
// origins must resolve Codex; until the literal can diverge from the default, this test cannot
// catch a dropped harness column.
func TestRunCreationOriginsWriteClaudeHarnessLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	// An anchor run for the *_run_id foreign keys (target/resume/then_fix/review targets).
	anchorID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 900, 't', 'd', 'completed', 'issue')`, anchorID, e.userID, e.repoID)

	// A schedule for CreatePromptRun's NOT-NULL schedule_id.
	scheduleID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr)
		 VALUES ($1, $2, $3, 'prompt', 'do a thing', 'recurring', '0 0 * * *')`, scheduleID, e.userID, e.repoID)

	cases := []struct {
		name   string
		create func() (store.Run, error)
	}{
		{"CreateRun", func() (store.Run, error) {
			return q.CreateRun(ctx, store.CreateRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueIid: hInt8(1), IssueTitle: "t", IssueDescription: "d",
				AutoApprove: false, WaitOnLimit: false, PlanSource: "agent", RequireBaseMatch: false,
				OverrideSubagentModel: false, TriggerSource: "manual",
			})
		}},
		{"CreatePromptRun", func() (store.Run, error) {
			return q.CreatePromptRun(ctx, store.CreatePromptRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueTitle: "t", IssueDescription: "d",
				ScheduleID: scheduleID, AutoApprove: true, WaitOnLimit: false, OverrideSubagentModel: false,
			})
		}},
		{"CreateSelfImproveRun", func() (store.Run, error) {
			return q.CreateSelfImproveRun(ctx, store.CreateSelfImproveRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueIid: hInt8(2), IssueTitle: "t", IssueDescription: "d",
				WaitOnLimit: false, OverrideSubagentModel: false,
			})
		}},
		{"CreateTaskRun", func() (store.Run, error) {
			return q.CreateTaskRun(ctx, store.CreateTaskRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				RunID:   uuid.New(), UserID: e.userID, RepoID: e.repoID, Branch: hText("uzi/task/a"),
				IssueTitle: "t", IssueDescription: "d", WaitOnLimit: false,
			})
		}},
		{"CreateThenFixRun", func() (store.Run, error) {
			return q.CreateThenFixRun(ctx, store.CreateThenFixRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				RunID:   uuid.New(), UserID: e.userID, RepoID: e.repoID, Branch: hText("uzi/task/b"),
				ThenFixOfRunID: pgconv.UUID(anchorID), WaitOnLimit: false, IssueTitle: "t", IssueDescription: "d",
			})
		}},
		{"CreateTaskReviewRun", func() (store.Run, error) {
			return q.CreateTaskReviewRun(ctx, store.CreateTaskReviewRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				RunID:   uuid.New(), UserID: e.userID, RepoID: e.repoID, Branch: hText("uzi/task/c"),
				TargetRunID: pgconv.UUID(anchorID), IssueTitle: "t",
			})
		}},
		{"CreateChatRun", func() (store.Run, error) {
			return q.CreateChatRun(ctx, store.CreateChatRunParams{
				RunID: uuid.New(), UserID: e.userID, IssueTitle: "t", IssueDescription: "hi", Title: hText("chat"),
			})
		}},
		{"CreateChatContinueRun", func() (store.Run, error) {
			return q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{
				UserID: e.userID, IssueTitle: "t", Title: hText("chat2"), ResumeOfRunID: pgconv.UUID(anchorID),
			})
		}},
		{"CreateCIFixRun", func() (store.Run, error) {
			return q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueTitle: "t", IssueDescription: "d",
				PipelineID: hInt8(11), PipelineRef: hText("ci-ref-1"), FailureSnapshot: []byte(`{}`),
				CiConfigPaths: []string{}, WaitOnLimit: false, AutoApprove: true,
			})
		}},
		{"CreateAutoMRReworkRun", func() (store.Run, error) {
			return q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueTitle: "t", IssueDescription: "d",
				PipelineRef: hText("mr-ref-1"), MrIid: hInt8(21), TargetRunID: pgconv.UUID(anchorID),
				WaitOnLimit: false, TriggerSource: "mr_rework",
			})
		}},
		{"CreateManualMRReworkRunAndAdvance", func() (store.Run, error) {
			return q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, RepoID: e.repoID, IssueTitle: "t", IssueDescription: "d",
				PipelineRef: hText("mr-ref-2"), MrIid: hInt8(22), TargetRunID: pgconv.UUID(anchorID),
				WaitOnLimit: false, HighWater: 1,
			})
		}},
		{"CreateJudgeRun", func() (store.Run, error) {
			return q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
				Harness: "claude", // PRD #1429 M1: harness is now a required @harness param.
				UserID:  e.userID, TargetRunID: pgconv.UUID(anchorID), IssueTitle: "t", IssueDescription: "d",
				TriggerSource: "judge",
			})
		}},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		run, err := tc.create()
		if err != nil {
			t.Fatalf("%s: create failed: %v", tc.name, err)
		}
		if run.Harness != "claude" {
			t.Fatalf("%s: runs.harness = %q, want 'claude'", tc.name, run.Harness)
		}
		seen[tc.name] = true
	}
	// Exactly the twelve production origins are reached (guards against a silently-dropped
	// case, which a bare length assertion could also catch, but the names make the gap legible).
	for _, want := range []string{
		"CreateRun", "CreatePromptRun", "CreateSelfImproveRun", "CreateTaskRun", "CreateThenFixRun",
		"CreateTaskReviewRun", "CreateChatRun", "CreateChatContinueRun", "CreateCIFixRun",
		"CreateAutoMRReworkRun", "CreateManualMRReworkRunAndAdvance", "CreateJudgeRun",
	} {
		if !seen[want] {
			t.Fatalf("creation origin %s was not exercised", want)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("exercised %d creation origins, want exactly 12", len(seen))
	}
}

// TestHarnessPreferenceColumnsDefaultNullLiveDB proves the two nullable preference columns
// (users.default_harness, run_schedules.harness) get no writer in M5A and default NULL.
func TestHarnessPreferenceColumnsDefaultNullLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	dh, err := q.GetUserDefaultHarness(ctx, e.userID)
	if err != nil {
		t.Fatalf("GetUserDefaultHarness: %v", err)
	}
	if dh.Valid {
		t.Fatalf("users.default_harness = %q, want NULL for a fresh user", dh.String)
	}

	scheduleID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr)
		 VALUES ($1, $2, $3, 'prompt', 'p', 'recurring', '0 0 * * *')`, scheduleID, e.userID, e.repoID)
	sched, err := q.GetRunSchedule(ctx, scheduleID)
	if err != nil {
		t.Fatalf("GetRunSchedule: %v", err)
	}
	if sched.Harness.Valid {
		t.Fatalf("run_schedules.harness = %q, want NULL", sched.Harness.String)
	}
}

// TestFreezeRunCodexBindingMarksCodexLiveDB proves the C1 half of D2: FreezeRunCodexBinding
// atomically flips its row to harness='codex' in the same guarded UPDATE that pins the M1
// binding; an idempotent replay still matches the original snapshot (harness is NOT in the
// exact-unchanged retry comparison) and keeps the row Codex; the harness survives the alias
// deletion that nulls codex_secret_id (the deletion-proof property); and the
// binding-coherence CHECK rejects a row indicated Codex but left harness='claude'.
func TestFreezeRunCodexBindingMarksCodexLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	secretID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext) VALUES ($1, $2, 'codex_auth', 'default', $3)`,
		secretID, e.userID, []byte{0x1})

	runID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 5, 't', 'd', 'claimed', 'issue')`, runID, e.userID, e.repoID)

	harnessOf := func() string {
		t.Helper()
		var h string
		if err := e.pool.QueryRow(ctx, `SELECT harness FROM runs WHERE id=$1`, runID).Scan(&h); err != nil {
			t.Fatalf("read harness: %v", err)
		}
		return h
	}
	if h := harnessOf(); h != "claude" {
		t.Fatalf("pre-freeze harness = %q, want 'claude'", h)
	}

	freezeParams := store.FreezeRunCodexBindingParams{
		SecretID: secretID, AuthMode: "subscription", SecretLabel: "default", MaterialRevision: 7,
		AccountKey: pgtype.Text{}, AccountRevision: pgtype.Int8{}, ID: runID, UserID: e.userID,
	}
	n, err := q.FreezeRunCodexBinding(ctx, freezeParams)
	if err != nil {
		t.Fatalf("FreezeRunCodexBinding (first): %v", err)
	}
	if n != 1 {
		t.Fatalf("first freeze affected %d rows, want 1", n)
	}
	if h := harnessOf(); h != "codex" {
		t.Fatalf("post-freeze harness = %q, want 'codex'", h)
	}

	// Idempotent replay: the EXACT same snapshot still matches (harness is absent from the
	// retry comparison) and reasserts Codex.
	n, err = q.FreezeRunCodexBinding(ctx, freezeParams)
	if err != nil {
		t.Fatalf("FreezeRunCodexBinding (replay): %v", err)
	}
	if n != 1 {
		t.Fatalf("idempotent replay affected %d rows, want 1 (the M1 snapshot must still match)", n)
	}
	if h := harnessOf(); h != "codex" {
		t.Fatalf("post-replay harness = %q, want 'codex'", h)
	}

	// A changed material_revision is a DIFFERENT snapshot → immutable, 0 rows, no reassert.
	changed := freezeParams
	changed.MaterialRevision = 8
	n, err = q.FreezeRunCodexBinding(ctx, changed)
	if err != nil {
		t.Fatalf("FreezeRunCodexBinding (changed): %v", err)
	}
	if n != 0 {
		t.Fatalf("changed-snapshot freeze affected %d rows, want 0 (immutable binding)", n)
	}

	// Deletion-proof: deleting the alias nulls codex_secret_id via the FK ON DELETE SET NULL
	// (codex_secret_id), but harness stays 'codex' and codex_material_revision remains.
	mustExec(ctx, t, e.pool, `DELETE FROM user_secrets WHERE id=$1`, secretID)
	var secretNull bool
	var matRev pgtype.Int8
	if err := e.pool.QueryRow(ctx,
		`SELECT codex_secret_id IS NULL, codex_material_revision FROM runs WHERE id=$1`, runID).
		Scan(&secretNull, &matRev); err != nil {
		t.Fatalf("read post-delete row: %v", err)
	}
	if !secretNull {
		t.Fatal("codex_secret_id should be NULL after the alias was deleted")
	}
	if !matRev.Valid {
		t.Fatal("codex_material_revision should survive the alias deletion (the deletion-proof sentinel)")
	}
	if h := harnessOf(); h != "codex" {
		t.Fatalf("post-alias-delete harness = %q, want 'codex' (a Codex row stays Codex)", h)
	}

	// Coherence CHECK: a fresh Claude run cannot be given a codex binding sentinel while
	// staying harness='claude'.
	badRunID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 6, 't', 'd', 'claimed', 'issue')`, badRunID, e.userID, e.repoID)
	_, err = e.pool.Exec(ctx, `UPDATE runs SET codex_material_revision = 3 WHERE id=$1`, badRunID)
	if err == nil {
		t.Fatal("setting codex_material_revision on a harness='claude' run must violate the coherence CHECK")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "runs_codex_harness_coherence_check" {
		t.Fatalf("want a runs_codex_harness_coherence_check violation, got %v", err)
	}
}

// TestUpsertRunUsageCostStatusConflictLiveDB proves UpsertRunUsage's conservative
// conflict rule (PRD #1332 M5A / D2/D5): equal statuses retain; any disagreement or
// unreported dominates → unreported; cost_usd is GREATEST only when the RESOLVED status is
// metered, else 0, so the result always satisfies the non-metered→zero CHECK.
func TestUpsertRunUsageCostStatusConflictLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	runID := uuid.New()
	mustExec(ctx, t, e.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 7, 't', 'd', 'running', 'issue')`, runID, e.userID, e.repoID)

	upsert := func(model, harness, status string, costMicros int64) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID, SessionID: "s", Model: model, LineageEpoch: 0,
			InputTokens: 100, CacheReadTokens: 0, CacheCreationTokens: 0, OutputTokens: 50,
			CostUsd: micro(costMicros), Harness: harness, CostStatus: status, UsageBasis: "per_leg",
		}); err != nil {
			t.Fatalf("UpsertRunUsage(%s,%s): %v", model, status, err)
		}
	}
	read := func(model string) (status, costText string) {
		t.Helper()
		if err := e.pool.QueryRow(ctx,
			`SELECT cost_status, cost_usd::text FROM run_usage WHERE run_id=$1 AND model=$2`,
			runID, model).Scan(&status, &costText); err != nil {
			t.Fatalf("read run_usage(%s): %v", model, err)
		}
		return
	}

	// A: metered + metered → metered, cost GREATEST.
	upsert("mA", "claude", "metered", 20000)
	upsert("mA", "claude", "metered", 50000)
	if s, c := read("mA"); s != "metered" || c != "0.050000" {
		t.Fatalf("case A = %s/%s, want metered/0.050000", s, c)
	}

	// B: metered + subscription → unreported, cost 0 (disagreement).
	upsert("mB", "codex", "metered", 30000)
	upsert("mB", "codex", "subscription", 0)
	if s, c := read("mB"); s != "unreported" || c != "0.000000" {
		t.Fatalf("case B = %s/%s, want unreported/0.000000", s, c)
	}

	// C: unreported (existing) + metered (incoming) → unreported dominates, cost stays 0
	// even though a positive metered cost arrived.
	upsert("mC", "codex", "unreported", 0)
	upsert("mC", "codex", "metered", 90000)
	if s, c := read("mC"); s != "unreported" || c != "0.000000" {
		t.Fatalf("case C = %s/%s, want unreported/0.000000 (unreported dominance)", s, c)
	}

	// D: subscription + subscription → subscription, cost 0.
	upsert("mD", "codex", "subscription", 0)
	upsert("mD", "codex", "subscription", 0)
	if s, c := read("mD"); s != "subscription" || c != "0.000000" {
		t.Fatalf("case D = %s/%s, want subscription/0.000000", s, c)
	}
}

// TestCrossRunCostStatusCountsLiveDB proves the widened cross-run cost queries surface
// subscription/unreported RUN COUNTS beside every dollar sum (PRD #1332 M5A / D2), and that
// GetRunUsageTotal returns the per-run folded cost_status. Uses a dedicated user so the
// user-scoped SelfUsage / AdminUsagePerUser assertions are exact; AdminUsageTotals is
// factory-wide (shared DB) so it is asserted >= this test's contributions.
func TestCrossRunCostStatusCountsLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	// insertRunWithUsage makes an issue run at a given age and one run_usage row of the
	// given folded cost_status. Non-metered rows carry cost_usd=0 (the CHECK).
	insertRunWithUsage := func(iid int64, ageDays int, harness, status string, costMicros int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(ctx, t, e.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', 'issue', now() - make_interval(days => $5))`,
			id, e.userID, e.repoID, iid, ageDays)
		mustExec(ctx, t, e.pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, output_tokens, cost_usd, harness, cost_status)
			 VALUES ($1, 's', 'm', 0, 100, 50, $2::numeric / 1000000, $3, $4)`,
			id, costMicros, harness, status)
		return id
	}

	meteredID := insertRunWithUsage(101, 0, "claude", "metered", 50000) // recent, metered $0.05
	subID := insertRunWithUsage(102, 0, "codex", "subscription", 0)     // recent, subscription
	unrepID := insertRunWithUsage(103, 0, "codex", "unreported", 0)     // recent, unreported
	_ = insertRunWithUsage(104, 10, "codex", "subscription", 0)         // 10 days ago, subscription

	// Per-run folded cost_status via GetRunUsageTotal.
	for id, want := range map[uuid.UUID]string{meteredID: "metered", subID: "subscription", unrepID: "unreported"} {
		got, err := q.GetRunUsageTotal(ctx, id)
		if err != nil {
			t.Fatalf("GetRunUsageTotal(%s): %v", id, err)
		}
		if got.CostStatus != want {
			t.Fatalf("GetRunUsageTotal(%s).CostStatus = %q, want %q", id, got.CostStatus, want)
		}
	}

	// SelfUsage: lifetime counts both subscription runs and the one unreported; the last-7
	// window excludes the 10-day-old subscription run.
	self, err := q.SelfUsage(ctx, e.userID)
	if err != nil {
		t.Fatalf("SelfUsage: %v", err)
	}
	if self.LifetimeSubscriptionRunCount != 2 || self.LifetimeUnreportedRunCount != 1 {
		t.Fatalf("SelfUsage lifetime counts = sub %d/unrep %d, want 2/1",
			self.LifetimeSubscriptionRunCount, self.LifetimeUnreportedRunCount)
	}
	if self.Last7SubscriptionRunCount != 1 || self.Last7UnreportedRunCount != 1 {
		t.Fatalf("SelfUsage last-7 counts = sub %d/unrep %d, want 1/1 (the 10-day-old sub run is excluded)",
			self.Last7SubscriptionRunCount, self.Last7UnreportedRunCount)
	}
	if self.RunCount != 4 {
		t.Fatalf("SelfUsage run_count = %d, want 4", self.RunCount)
	}

	// AdminUsagePerUser: this user's exact lifetime counts.
	rows, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatalf("AdminUsagePerUser: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.UserID == e.userID {
			found = true
			if r.SubscriptionRunCount != 2 || r.UnreportedRunCount != 1 {
				t.Fatalf("AdminUsagePerUser[%s] counts = sub %d/unrep %d, want 2/1",
					e.userID, r.SubscriptionRunCount, r.UnreportedRunCount)
			}
		}
	}
	if !found {
		t.Fatalf("AdminUsagePerUser has no row for user %s", e.userID)
	}

	// AdminUsageTotals is factory-wide over a shared DB, so assert it covers at least this
	// test's contributions (a bug returning 0 would still fail this).
	totals, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatalf("AdminUsageTotals: %v", err)
	}
	if totals.LifetimeSubscriptionRunCount < 2 || totals.LifetimeUnreportedRunCount < 1 {
		t.Fatalf("AdminUsageTotals lifetime counts = sub %d/unrep %d, want >= 2/1",
			totals.LifetimeSubscriptionRunCount, totals.LifetimeUnreportedRunCount)
	}
	if totals.Last7SubscriptionRunCount < 1 || totals.Last7UnreportedRunCount < 1 {
		t.Fatalf("AdminUsageTotals last-7 counts = sub %d/unrep %d, want >= 1/1",
			totals.Last7SubscriptionRunCount, totals.Last7UnreportedRunCount)
	}
}

// TestRunUsageTotalsCrossModelCostStatusFoldLiveDB closes the CROSS-MODEL gap in the
// run_usage_totals fold (PRD #1332 M5A / C1, D2/D5). The existing fold tests each use ONE
// run_usage row per run, so they exercise only the trivial single-status fold; this builds
// runs with TWO run_usage rows for DIFFERENT models carrying DIFFERENT cost_status, which is
// the only shape that reaches the OUTER per-run fold's mixed-dominance branch
// (bool_or(metered) AND bool_or(subscription) → unreported). Each model's single row folds to
// its own status at the inner (run_id, model, lineage_epoch) level, so the cross-model
// combination happens at the outer level under test.
//
// The two rows share the run's harness ('claude'): the view folds on cost_status, not harness,
// and run_usage carries no harness↔cost_status coherence CHECK (only runs does), so a
// 'claude' row with cost_status 'subscription'/'unreported' is legal as long as it carries
// cost_usd=0 (00226's run_usage_nonmetered_zero_check) — which every non-metered row here does.
//
// Mutation sensitivity, so a cross-model regression cannot ride in green:
//   - reverting the mixed-dominance AND to OR misfolds the all-subscription run (run3) and the
//     all-metered run (run4) to 'unreported' (and moves the SelfUsage counts);
//   - dropping the mixed-dominance branch misfolds the metered+subscription run (run1) to
//     'subscription' (and moves the SelfUsage counts).
func TestRunUsageTotalsCrossModelCostStatusFoldLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx, q := e.ctx, e.q

	// newRun creates a fresh 'claude' issue run (harness defaults to 'claude').
	newRun := func(iid int64) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(ctx, t, e.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', 'issue')`, id, e.userID, e.repoID, iid)
		return id
	}
	// addModel writes one run_usage row for a DISTINCT model of the run via the production
	// UpsertRunUsage. harness stays 'claude' across the run's rows; non-metered rows MUST carry
	// cost_usd=0 (the non-metered→zero CHECK), so callers pass 0 for subscription/unreported.
	addModel := func(runID uuid.UUID, model, status string, costMicros int64) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID, SessionID: "s", Model: model, LineageEpoch: 0,
			InputTokens: 100, CacheReadTokens: 0, CacheCreationTokens: 0, OutputTokens: 50,
			CostUsd: micro(costMicros), Harness: "claude", CostStatus: status, UsageBasis: "per_leg",
		}); err != nil {
			t.Fatalf("UpsertRunUsage(run=%s model=%s status=%s): %v", runID, model, status, err)
		}
	}
	fold := func(runID uuid.UUID) store.GetRunUsageTotalRow {
		t.Helper()
		got, err := q.GetRunUsageTotal(ctx, runID)
		if err != nil {
			t.Fatalf("GetRunUsageTotal(%s): %v", runID, err)
		}
		return got
	}

	// Run 1 — metered($0.07) + subscription($0): the mixed-dominance branch. Folds to
	// 'unreported', and cost_usd is the metered row's cost ALONE (the subscription row is 0),
	// proving SUM(cost_usd) is the metered-only dollar total even for a mixed run.
	run1 := newRun(201)
	addModel(run1, "m1-metered", "metered", 70000)
	addModel(run1, "m1-subscription", "subscription", 0)
	if r := fold(run1); r.CostStatus != "unreported" {
		t.Fatalf("run1 (metered+subscription) cost_status = %q, want unreported", r.CostStatus)
	} else if got := microsOf(t, r.CostUsd); got != 70000 {
		t.Fatalf("run1 cost_usd = %d micros, want 70000 (only the metered row)", got)
	}

	// Run 2 — metered($0.04) + unreported($0): folds to 'unreported'.
	run2 := newRun(202)
	addModel(run2, "m2-metered", "metered", 40000)
	addModel(run2, "m2-unreported", "unreported", 0)
	if r := fold(run2); r.CostStatus != "unreported" {
		t.Fatalf("run2 (metered+unreported) cost_status = %q, want unreported", r.CostStatus)
	}

	// Run 3 — subscription($0) + subscription($0) across two models: folds to 'subscription'.
	// (Reverting the mixed-dominance AND to OR would misfold this to 'unreported'.)
	run3 := newRun(203)
	addModel(run3, "m3-a", "subscription", 0)
	addModel(run3, "m3-b", "subscription", 0)
	if r := fold(run3); r.CostStatus != "subscription" {
		t.Fatalf("run3 (subscription+subscription) cost_status = %q, want subscription", r.CostStatus)
	} else if got := microsOf(t, r.CostUsd); got != 0 {
		t.Fatalf("run3 cost_usd = %d micros, want 0", got)
	}

	// Run 4 — metered($0.03) + metered($0.025) across two models: folds to 'metered', cost_usd
	// is the SUM. (Reverting the mixed-dominance AND to OR would misfold this to 'unreported'.)
	run4 := newRun(204)
	addModel(run4, "m4-a", "metered", 30000)
	addModel(run4, "m4-b", "metered", 25000)
	if r := fold(run4); r.CostStatus != "metered" {
		t.Fatalf("run4 (metered+metered) cost_status = %q, want metered", r.CostStatus)
	} else if got := microsOf(t, r.CostUsd); got != 55000 {
		t.Fatalf("run4 cost_usd = %d micros, want 55000 (the sum of both metered rows)", got)
	}

	// The cross-run counts must see each run by its FOLDED per-run cost_status, so a cross-model
	// mislabel would move them: run1+run2 fold to 'unreported', run3 to 'subscription', run4 to
	// 'metered'. The user is fresh (setupHarnessEnv), so these four are its only usage-bearing,
	// non-chat runs and the counts are exact.
	self, err := q.SelfUsage(ctx, e.userID)
	if err != nil {
		t.Fatalf("SelfUsage: %v", err)
	}
	if self.LifetimeUnreportedRunCount != 2 {
		t.Fatalf("SelfUsage lifetime_unreported_run_count = %d, want 2 (run1+run2 fold to unreported)",
			self.LifetimeUnreportedRunCount)
	}
	if self.LifetimeSubscriptionRunCount != 1 {
		t.Fatalf("SelfUsage lifetime_subscription_run_count = %d, want 1 (only run3; the mixed run1 must NOT count as subscription)",
			self.LifetimeSubscriptionRunCount)
	}
	if self.RunCount != 4 {
		t.Fatalf("SelfUsage run_count = %d, want 4", self.RunCount)
	}
}
