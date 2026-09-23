package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestUpsertRunUsageMergeLiveDB proves the run_usage fold's SQL contract against a
// REAL Postgres — the GREATEST monotonic merge and the latest/MAX-per-model rollup
// that M1's cumulative-across-resume verdict (PRD #40 Decision 3b) requires. The
// fake-store unit tests prove the fold RUNS on every delivery; only this proves the
// SQL MERGE converges — a crash-retry re-delivering an EARLIER cumulative frame
// after a LATER one must never regress a row ("re-delivering the whole batch
// changes nothing") — and that a SUM across session rows would over-count.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one); mirrors the other *_integration_test.go here.
func TestUpsertRunUsageMergeLiveDB(t *testing.T) {
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

	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("runusage-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'running')`, runID, userID, repoID)

	const model = "claude-fable-5"

	// numericFromMicros mirrors the fold's numericUSD: quantize a dollar float to
	// numeric(12,6) via microdollars, so the stored value is exact for comparison.
	numericFromMicros := func(usd float64) pgtype.Numeric {
		return pgtype.Numeric{Int: big.NewInt(int64(usd*1e6 + 0.5)), Exp: -6, Valid: true}
	}
	fold := func(session string, epoch int32, in, cacheR, cacheC, out int64, cost float64) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID, SessionID: session, Model: model, LineageEpoch: epoch,
			InputTokens: in, CacheReadTokens: cacheR, CacheCreationTokens: cacheC, OutputTokens: out,
			// PRD #1332 M5A: these Claude rows are metered; harness/cost_status are now
			// NOT NULL and CHECK-closed (migration 00226), so the fold must supply them.
			// ADR-1562: usage_basis is NOT NULL + CHECK-closed too (migration 00244).
			CostUsd: numericFromMicros(cost), Harness: "claude", CostStatus: "metered", UsageBasis: "per_leg",
		}); err != nil {
			t.Fatalf("UpsertRunUsage(%s): %v", session, err)
		}
	}
	tokensOf := func(session string) (in, out int64) {
		if err := pool.QueryRow(ctx,
			`SELECT input_tokens, output_tokens FROM run_usage WHERE run_id=$1 AND session_id=$2 AND model=$3`,
			runID, session, model).Scan(&in, &out); err != nil {
			t.Fatalf("read run_usage (%s): %v", session, err)
		}
		return
	}
	// costMatches compares via SQL (numeric = numeric from a text literal) to avoid
	// any float→numeric scan ambiguity on the Go side.
	costMatches := func(session, want string) bool {
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT cost_usd = $4::numeric FROM run_usage WHERE run_id=$1 AND session_id=$2 AND model=$3`,
			runID, session, model, want).Scan(&ok); err != nil {
			t.Fatalf("read cost (%s): %v", session, err)
		}
		return ok
	}

	// Phase 1 result frame (cumulative-to-phase-1).
	fold("sess-1", 0, 1000, 200, 50, 400, 0.010)
	if in, out := tokensOf("sess-1"); in != 1000 || out != 400 {
		t.Fatalf("phase-1 row wrong: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.010000") {
		t.Fatal("phase-1 cost should be 0.010000")
	}

	// Phase 2 resumes sess-1: under verdict (b) the frame is cumulative, so a HIGHER
	// snapshot lands on the SAME key. GREATEST advances the row.
	fold("sess-1", 0, 1800, 500, 90, 700, 0.019)
	if in, out := tokensOf("sess-1"); in != 1800 || out != 700 {
		t.Fatalf("phase-2 merge should advance to the higher cumulative: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.019000") {
		t.Fatal("phase-2 cost should advance to 0.019000")
	}

	// Crash-retry re-delivers the phase-1 (LOWER) frame after phase-2. GREATEST must
	// NOT regress the row — the "re-delivery changes nothing" guarantee.
	fold("sess-1", 0, 1000, 200, 50, 400, 0.010)
	if in, out := tokensOf("sess-1"); in != 1800 || out != 700 {
		t.Fatalf("re-delivered earlier frame must not regress the row: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.019000") {
		t.Fatal("re-delivered lower cost must not regress the row")
	}

	// --- PRD #1079: two LEGS under the SAME session_id and model, keyed apart by epoch.
	//
	// The pre-#1079 defect: the worker reports session_id once and every query() leg
	// shares it, so two legs collapsed at the (run_id, session_id, model) key and
	// GREATEST kept only the largest. With lineage_epoch in the primary key each leg is
	// its own row, and the run_usage_totals view (MAX within (run, model, epoch), SUM
	// across) returns their SUM. A SECOND run isolates this from the sess-1 rows above.
	runID2 := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 2, 't', 'd', 'running')`, runID2, userID, repoID)

	const legModel = "claude-fable-legs"
	const legSession = "sess-shared" // SAME session for both legs — the collapse trap.
	foldLeg := func(epoch int32, in, cacheR, cacheC, out int64, cost float64) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID2, SessionID: legSession, Model: legModel, LineageEpoch: epoch,
			InputTokens: in, CacheReadTokens: cacheR, CacheCreationTokens: cacheC, OutputTokens: out,
			// PRD #1332 M5A: Claude/metered rows; harness/cost_status are NOT NULL + CHECK-closed.
			// ADR-1562: usage_basis is NOT NULL + CHECK-closed too (migration 00244).
			CostUsd: numericFromMicros(cost), Harness: "claude", CostStatus: "metered", UsageBasis: "per_leg",
		}); err != nil {
			t.Fatalf("UpsertRunUsage(leg %d): %v", epoch, err)
		}
	}
	// Leg 1 (epoch 1) is LARGER than leg 2 (epoch 2) in every column, exactly the
	// 02854d5e shape where a later leg reports less. Under the OLD three-column key the
	// smaller leg 2 is absorbed by GREATEST and the run total is leg 1 alone; the fix
	// keeps both, so the total is the SUM.
	foldLeg(1, 1000, 400, 60, 700, 0.030)
	foldLeg(2, 200, 90, 15, 150, 0.007)

	legTotal, err := q.GetRunUsageTotal(ctx, runID2)
	if err != nil {
		t.Fatalf("GetRunUsageTotal(runID2): %v", err)
	}
	// SUM of the two legs, per column. On c415789 (old three-column key) leg 2 is
	// absorbed and this reads leg 1 alone (1000/700) — the assertion is RED there.
	if legTotal.InputTokens != 1200 || legTotal.OutputTokens != 850 ||
		legTotal.CacheReadTokens != 490 || legTotal.CacheCreationTokens != 75 {
		t.Fatalf("two-leg run total = in %d/out %d/cr %d/cw %d, want the SUM 1200/850/490/75 "+
			"(the old (run,session,model) key would absorb leg 2 by GREATEST and read leg 1 alone 1000/700)",
			legTotal.InputTokens, legTotal.OutputTokens, legTotal.CacheReadTokens, legTotal.CacheCreationTokens)
	}
	if legTotal.InputTokens == 1000 {
		t.Fatal("two-leg input == 1000 means leg 2 was absorbed at the key — lineage_epoch is not splitting the rows")
	}

	// Two distinct run_usage rows now exist for this (run, session, model), one per epoch.
	var legRows int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM run_usage WHERE run_id=$1 AND session_id=$2 AND model=$3`,
		runID2, legSession, legModel).Scan(&legRows); err != nil {
		t.Fatalf("count leg rows: %v", err)
	}
	if legRows != 2 {
		t.Fatalf("two legs under one session must persist as 2 rows keyed by epoch, got %d", legRows)
	}

	// Re-delivering EITHER frame verbatim changes nothing — GREATEST within a leg key is
	// idempotent, so at-least-once delivery converges.
	foldLeg(1, 1000, 400, 60, 700, 0.030)
	foldLeg(2, 200, 90, 15, 150, 0.007)
	legTotal2, err := q.GetRunUsageTotal(ctx, runID2)
	if err != nil {
		t.Fatalf("GetRunUsageTotal(runID2) after replay: %v", err)
	}
	if legTotal2.InputTokens != 1200 || legTotal2.OutputTokens != 850 {
		t.Fatalf("re-delivering both legs must not change the total: got in %d/out %d, want 1200/850", legTotal2.InputTokens, legTotal2.OutputTokens)
	}
}

// TestRunUsageTotalsSessionCumulativeLiveDB proves the run_usage_totals view (migration
// 00244, issue #1562) against a REAL Postgres for EVERY scenario in the cumulative fixture:
// insert the fixture's per-leg rows via UpsertRunUsage, then assert GetRunUsageTotal equals
// the fixture `totals` EXACTLY (tokens + cost by SQL numeric equality) and DIFFERS from the
// pre-#1562 per-leg SUM (`sum_fold_totals`) — so a fold that regressed to summing resumed
// legs reddens on a named value. It also asserts an old-data parity case (per_leg rows,
// lineage 0, a leg split across two session_ids equal to the 00228 answer), replay
// idempotency, and the one-way usage_basis upgrade on conflict.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunUsageTotalsSessionCumulativeLiveDB(t *testing.T) {
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

	// Read the cumulative rollup fixture (rows + totals + sum_fold_totals per scenario).
	type cumRow struct {
		Model               string  `json:"model"`
		LineageEpoch        int32   `json:"lineage_epoch"`
		LineageIndex        int32   `json:"lineage_index"`
		UsageBasis          string  `json:"usage_basis"`
		InputTokens         int64   `json:"input_tokens"`
		CacheReadTokens     int64   `json:"cache_read_tokens"`
		CacheCreationTokens int64   `json:"cache_creation_tokens"`
		OutputTokens        int64   `json:"output_tokens"`
		CostUSD             float64 `json:"cost_usd"`
	}
	type cumTotals struct {
		InputTokens         int64   `json:"input_tokens"`
		CacheReadTokens     int64   `json:"cache_read_tokens"`
		CacheCreationTokens int64   `json:"cache_creation_tokens"`
		OutputTokens        int64   `json:"output_tokens"`
		CostUSD             float64 `json:"cost_usd"`
	}
	type cumScenario struct {
		Name          string    `json:"name"`
		Rows          []cumRow  `json:"rows"`
		Totals        cumTotals `json:"totals"`
		SumFoldTotals cumTotals `json:"sum_fold_totals"`
	}
	var rollup struct {
		Scenarios []cumScenario `json:"scenarios"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "run-usage", "run-usage-cumulative.json"))
	if err != nil {
		t.Fatalf("read cumulative fixture: %v", err)
	}
	if err := json.Unmarshal(b, &rollup); err != nil {
		t.Fatalf("parse cumulative fixture: %v", err)
	}
	if len(rollup.Scenarios) == 0 {
		t.Fatal("cumulative fixture broken: no scenarios")
	}

	// Seed one user/connection/repo the scenarios' runs hang off.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("cumusage-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r-cum', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	iid := int64(5_000_000)
	seedRun := func() uuid.UUID {
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running')`, id, userID, repoID, iid)
		return id
	}
	// numericFromMicros mirrors the fold's numericUSD (round to microdollars) so the stored
	// numeric(12,6) is exact for the SQL equality below.
	numericFromMicros := func(usd float64) pgtype.Numeric {
		return pgtype.Numeric{Int: big.NewInt(int64(usd*1e6 + 0.5)), Exp: -6, Valid: true}
	}
	// upsert each fixture row. session_id is fixed per run — the leg key is (epoch), which is
	// unique per row here, so one session suffices; the view ignores session_id.
	insertRow := func(runID uuid.UUID, session string, r cumRow) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID, SessionID: session, Model: r.Model,
			LineageEpoch: r.LineageEpoch, LineageIndex: r.LineageIndex, UsageBasis: r.UsageBasis,
			InputTokens: r.InputTokens, CacheReadTokens: r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens, OutputTokens: r.OutputTokens,
			CostUsd: numericFromMicros(r.CostUSD), Harness: "claude", CostStatus: "metered",
		}); err != nil {
			t.Fatalf("UpsertRunUsage(%s epoch %d): %v", r.Model, r.LineageEpoch, err)
		}
	}
	// costEquals compares the view's cost_usd to a want via SQL numeric equality.
	costEquals := func(runID uuid.UUID, want float64) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx,
			`SELECT cost_usd = $2::numeric FROM run_usage_totals WHERE run_id = $1`,
			runID, fmt.Sprintf("%.6f", want)).Scan(&ok); err != nil {
			t.Fatalf("read view cost (run %s): %v", runID, err)
		}
		return ok
	}

	for _, sc := range rollup.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			runID := seedRun()
			for _, r := range sc.Rows {
				insertRow(runID, "sess-cum", r)
			}
			total, err := q.GetRunUsageTotal(ctx, runID)
			if err != nil {
				t.Fatalf("GetRunUsageTotal: %v", err)
			}
			if total.InputTokens != sc.Totals.InputTokens ||
				total.CacheReadTokens != sc.Totals.CacheReadTokens ||
				total.CacheCreationTokens != sc.Totals.CacheCreationTokens ||
				total.OutputTokens != sc.Totals.OutputTokens {
				t.Fatalf("run total = in %d/cr %d/cw %d/out %d, want %d/%d/%d/%d",
					total.InputTokens, total.CacheReadTokens, total.CacheCreationTokens, total.OutputTokens,
					sc.Totals.InputTokens, sc.Totals.CacheReadTokens, sc.Totals.CacheCreationTokens, sc.Totals.OutputTokens)
			}
			if !costEquals(runID, sc.Totals.CostUSD) {
				t.Fatalf("run total cost must be the session-cumulative fold %.6f", sc.Totals.CostUSD)
			}
			// The pre-#1562 per-leg SUM must DIFFER (unless the scenario is all-per_leg, in
			// which case the two coincide by construction — none of the fixture scenarios are).
			if total.InputTokens == sc.SumFoldTotals.InputTokens &&
				total.CacheReadTokens == sc.SumFoldTotals.CacheReadTokens &&
				total.OutputTokens == sc.SumFoldTotals.OutputTokens &&
				costEquals(runID, sc.SumFoldTotals.CostUSD) {
				t.Fatalf("run total collapsed to the pre-#1562 per-leg SUM (in %d/out %d, cost %.6f) -- the view is summing resumed legs",
					sc.SumFoldTotals.InputTokens, sc.SumFoldTotals.OutputTokens, sc.SumFoldTotals.CostUSD)
			}

			// Replay idempotency: re-upsert every row unchanged -> the total is unchanged.
			for _, r := range sc.Rows {
				insertRow(runID, "sess-cum", r)
			}
			total2, err := q.GetRunUsageTotal(ctx, runID)
			if err != nil {
				t.Fatalf("GetRunUsageTotal after replay: %v", err)
			}
			if total2.InputTokens != sc.Totals.InputTokens || total2.OutputTokens != sc.Totals.OutputTokens ||
				!costEquals(runID, sc.Totals.CostUSD) {
				t.Fatalf("replay changed the total: in %d/out %d", total2.InputTokens, total2.OutputTokens)
			}
		})
	}

	// --- old-data parity: per_leg rows (the DEFAULT), lineage 0, a single leg split across
	// TWO session_ids. This is the exact pre-#1562 shape, and the view must answer 00228's
	// number: MAX within (run, model, epoch) across the two sessions, then SUM across epochs.
	t.Run("old-data-parity-per-leg-two-sessions", func(t *testing.T) {
		runID := seedRun()
		insLegacy := func(session, model string, epoch int32, in, out int64) {
			mustExec(ctx, t, pool,
				`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, output_tokens, cost_usd)
				 VALUES ($1, $2, $3, $4, $5, $6, 0)`, runID, session, model, epoch, in, out)
		}
		// Leg (epoch 0) split across two sessions: MAX per (run, model, epoch) = 1500/700.
		insLegacy("sA", "modelX", 0, 1000, 400)
		insLegacy("sB", "modelX", 0, 1500, 700)
		// A second leg (epoch 1) sums onto it: total = 1500 + 300 = 1800 in, 700 + 100 = 800.
		insLegacy("sB", "modelX", 1, 300, 100)
		total, err := q.GetRunUsageTotal(ctx, runID)
		if err != nil {
			t.Fatalf("GetRunUsageTotal(parity): %v", err)
		}
		if total.InputTokens != 1800 || total.OutputTokens != 800 {
			t.Fatalf("old-data parity total = in %d/out %d, want 1800/800 (MAX across sessions per epoch, SUM across epochs)",
				total.InputTokens, total.OutputTokens)
		}
		// The DEFAULT usage_basis must be per_leg and lineage_index 0 (migration defaults),
		// so a legacy INSERT with neither column named still folds as before.
		var basis string
		var lineage int32
		if err := pool.QueryRow(ctx,
			`SELECT usage_basis, lineage_index FROM run_usage WHERE run_id=$1 AND session_id='sA' AND model='modelX' AND lineage_epoch=0`,
			runID).Scan(&basis, &lineage); err != nil {
			t.Fatalf("read legacy row defaults: %v", err)
		}
		if basis != "per_leg" || lineage != 0 {
			t.Fatalf("legacy row defaults = basis %q / lineage %d, want per_leg / 0", basis, lineage)
		}
	})

	// --- one-way usage_basis upgrade on conflict (the in-flight-across-deploy case): a leg
	// first folded per_leg, then re-delivered as session_cumulative, latches cumulative and
	// never downgrades back.
	t.Run("one-way-basis-upgrade-on-conflict", func(t *testing.T) {
		runID := seedRun()
		readBasis := func() string {
			var basis string
			if err := pool.QueryRow(ctx,
				`SELECT usage_basis FROM run_usage WHERE run_id=$1 AND session_id='sX' AND model='modelX' AND lineage_epoch=0`,
				runID).Scan(&basis); err != nil {
				t.Fatalf("read basis: %v", err)
			}
			return basis
		}
		upsert := func(basis string, in int64) {
			if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
				RunID: runID, SessionID: "sX", Model: "modelX", LineageEpoch: 0, LineageIndex: 0,
				UsageBasis: basis, InputTokens: in, OutputTokens: 10,
				CostUsd: numericFromMicros(0.001), Harness: "claude", CostStatus: "metered",
			}); err != nil {
				t.Fatalf("UpsertRunUsage(%s): %v", basis, err)
			}
		}
		upsert("per_leg", 100)
		if got := readBasis(); got != "per_leg" {
			t.Fatalf("after first per_leg upsert, basis = %q, want per_leg", got)
		}
		upsert("session_cumulative", 200)
		if got := readBasis(); got != "session_cumulative" {
			t.Fatalf("a session_cumulative frame must upgrade the leg, basis = %q", got)
		}
		// A later per_leg frame for the same leg must NOT downgrade it.
		upsert("per_leg", 300)
		if got := readBasis(); got != "session_cumulative" {
			t.Fatalf("a later per_leg frame downgraded the leg to %q -- the upgrade must be one-way", got)
		}
	})
}

// TestUsageRollupsLiveDB proves the M3 read rollups against a REAL Postgres — the
// verdict-(b) rule (greatest-wins per model, summed across models) lives in the
// run_usage_totals view, and every read query reads it. Asserts the M3 validation
// criteria: a run total collapses session snapshots by MAX (not SUM); SelfUsage sums
// exactly the caller's runs and honours the 7-day window + run count; a pre-feature
// run (no usage rows) is absent (never a fake 0); and the admin factory total equals
// the sum of the per-user rows.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestUsageRollupsLiveDB(t *testing.T) {
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

	// Two users, each with their own connection + repo.
	seedUserRepo := func(tag string) (userID, repoID uuid.UUID) {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("rollup-%s-%s@e2e", tag, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, 1, 'g/r-'||$3, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, tag)
		return userID, repoID
	}
	userA, repoA := seedUserRepo("a")
	userB, repoB := seedUserRepo("b")
	// A third user isolates the broken-lineage run so it cannot perturb the absolute
	// SelfUsage / AdminUsagePerUser assertions for userA/userB below (PRD #632 M5).
	userC, repoC := seedUserRepo("c")

	iid := int64(0)
	seedRun := func(userID, repoID uuid.UUID, daysAgo int) uuid.UUID {
		iid++
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, created_at)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', now() - make_interval(days => $5))`,
			id, userID, repoID, iid, daysAgo)
		return id
	}
	// insUsageEpoch inserts a run_usage row including its lineage_epoch (PRD #632).
	insUsageEpoch := func(runID uuid.UUID, session, model string, epoch int32, in, out int64) {
		mustExec(ctx, t, pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, input_tokens, output_tokens, cost_usd)
			 VALUES ($1, $2, $3, $4, $5, $6, 0)`, runID, session, model, epoch, in, out)
	}
	// insUsage keeps the epoch-0 call sites (and their exact totals) byte-identical —
	// the no-restatement / byte-identity regression guard (Success Criterion 3).
	insUsage := func(runID uuid.UUID, session, model string, in, out int64) {
		insUsageEpoch(runID, session, model, 0, in, out)
	}

	// Run A1: model X across two sessions (cumulative snapshots 1000 → 1500; MAX=1500)
	// plus model Y (200). Run total input = 1500 + 200 = 1700 (SUM of per-model MAX).
	a1 := seedRun(userA, repoA, 0)
	insUsage(a1, "s1", "modelX", 1000, 400)
	insUsage(a1, "s2", "modelX", 1500, 700) // resumed session, higher cumulative
	insUsage(a1, "s1", "modelY", 200, 50)
	// Run A2: single model, backdated 10 days (outside the 7-day window).
	a2 := seedRun(userA, repoA, 10)
	insUsage(a2, "s1", "modelX", 500, 300)
	// Run A3: NO usage rows — a pre-feature run.
	a3 := seedRun(userA, repoA, 0)
	// Run B1 (other user): single model.
	b1 := seedRun(userB, repoB, 0)
	insUsage(b1, "s1", "modelX", 300, 100)

	// Run BL (userC): a genuine BROKEN lineage — a dropped resume latched a fresh SDK
	// session, so leg 2 is a DISTINCT session_id AND a DISTINCT epoch, accumulating
	// from 0 (its 500/200 sits BELOW leg 1's 2000/800). The rewritten view MAXes
	// within (run, model, epoch) then SUMs across epochs, so the run total is the SUM
	// of the two per-epoch maxima: 2000+500 = 2500 input, 800+200 = 1000 output.
	bl := seedRun(userC, repoC, 0)
	insUsageEpoch(bl, "s1", "modelX", 0, 2000, 800) // leg 1: earlier lineage, epoch 0
	insUsageEpoch(bl, "s2", "modelX", 1, 500, 200)  // leg 2: dropped-resume fresh leg, epoch 1, below leg 1

	// --- per-run total: MAX per model, summed across models (NOT a SUM of snapshots).
	rt, err := q.GetRunUsageTotal(ctx, a1)
	if err != nil {
		t.Fatalf("GetRunUsageTotal(a1): %v", err)
	}
	if rt.InputTokens != 1700 || rt.OutputTokens != 750 {
		t.Fatalf("run A1 total = in %d/out %d, want 1700/750 (greatest-wins per model)", rt.InputTokens, rt.OutputTokens)
	}
	// A pre-feature run has no view row → ErrNoRows (the handler renders "no usage").
	if _, err := q.GetRunUsageTotal(ctx, a3); err == nil {
		t.Fatal("GetRunUsageTotal(a3) must return no row for a run with no usage")
	}

	// --- broken-lineage run total, read THROUGH the view (PRD #632, the fix proof).
	// The rewritten view groups MAX within (run, model, epoch) then SUMs across
	// epochs, so BL = (MAX epoch0=2000) + (MAX epoch1=500) = 2500 input, 800+200 =
	// 1000 output. The OLD view (MAX per (run, model), no epoch) would have returned
	// 2000/800 — MAX-masking the smaller leg 2 entirely — so this assertion is exactly
	// what proves the epoch SUM landed.
	blt, err := q.GetRunUsageTotal(ctx, bl)
	if err != nil {
		t.Fatalf("GetRunUsageTotal(bl): %v", err)
	}
	if blt.InputTokens != 2500 || blt.OutputTokens != 1000 {
		t.Fatalf("broken-lineage BL total = in %d/out %d, want 2500/1000 (SUM of per-epoch maxima 2000+500, 800+200); the old MAX-masking view would have returned 2000/800", blt.InputTokens, blt.OutputTokens)
	}
	if blt.InputTokens == 2000 {
		t.Fatal("BL input == 2000 means the view MAX-masked leg 2 — the epoch SUM did not land")
	}

	// --- SelfUsage: sums exactly the caller's runs, honours the window + run count.
	selfA, err := q.SelfUsage(ctx, userA)
	if err != nil {
		t.Fatalf("SelfUsage(A): %v", err)
	}
	if selfA.LifetimeInputTokens != 2200 { // 1700 (A1) + 500 (A2); A3 contributes nothing
		t.Fatalf("A lifetime input = %d, want 2200", selfA.LifetimeInputTokens)
	}
	if selfA.Last7InputTokens != 1700 { // A2 backdated out of the window
		t.Fatalf("A last-7-days input = %d, want 1700 (A2 is 10 days old)", selfA.Last7InputTokens)
	}
	if selfA.RunCount != 2 { // A1 + A2 carry usage; A3 does not
		t.Fatalf("A run_count = %d, want 2 (pre-feature A3 excluded)", selfA.RunCount)
	}
	// SelfUsage is scoped: user B never sees user A's spend.
	selfB, err := q.SelfUsage(ctx, userB)
	if err != nil {
		t.Fatalf("SelfUsage(B): %v", err)
	}
	if selfB.LifetimeInputTokens != 300 || selfB.RunCount != 1 {
		t.Fatalf("B self usage = in %d / runs %d, want 300 / 1", selfB.LifetimeInputTokens, selfB.RunCount)
	}

	// --- admin factory total == sum of the per-user rows. The store IT runner shares
	// one DB across all *LiveDB tests, so the factory-wide aggregates also see other
	// tests' seeded runs; assert the INVARIANT (per-user rows sum to the factory total,
	// which holds regardless of that shared data) plus this test's own user rows,
	// rather than an absolute factory number.
	totals, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatalf("AdminUsageTotals: %v", err)
	}
	perUser, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatalf("AdminUsagePerUser: %v", err)
	}
	var sumIn, sumOut int64
	perUserByID := map[uuid.UUID]store.AdminUsagePerUserRow{}
	for _, u := range perUser {
		sumIn += u.InputTokens
		sumOut += u.OutputTokens
		perUserByID[u.UserID] = u
	}
	if sumIn != totals.LifetimeInputTokens || sumOut != totals.LifetimeOutputTokens {
		t.Fatalf("per-user rows (in %d/out %d) must sum to factory total (in %d/out %d)",
			sumIn, sumOut, totals.LifetimeInputTokens, totals.LifetimeOutputTokens)
	}
	if got := perUserByID[userA].InputTokens; got != 2200 {
		t.Fatalf("user A per-user input = %d, want 2200", got)
	}
	if got := perUserByID[userB].InputTokens; got != 300 {
		t.Fatalf("user B per-user input = %d, want 300", got)
	}

	// --- run list: usage columns populated for a run with usage, NULL for one without.
	rows, err := q.ListRunsForUser(ctx, store.ListRunsForUserParams{UserID: userA})
	if err != nil {
		t.Fatalf("ListRunsForUser(A): %v", err)
	}
	byID := map[uuid.UUID]store.ListRunsForUserRow{}
	for _, row := range rows {
		byID[row.Run.ID] = row
	}
	if r := byID[a1]; !r.UsageInputTokens.Valid || r.UsageInputTokens.Int64 != 1700 {
		t.Fatalf("A1 list usage = %+v, want valid 1700", r.UsageInputTokens)
	}
	if r := byID[a3]; r.UsageInputTokens.Valid {
		t.Fatalf("A3 (no usage) must have NULL usage columns, got %+v", r.UsageInputTokens)
	}
}

// TestRunOutcomesRollupsLiveDB proves the PRD #1293 M1 failed-run rate aggregates against
// a REAL Postgres: the outcome counts are a straight scan over `runs` (NOT the usage join,
// D1 — a run that fails before spending has no usage row and must still count), restricted
// to terminal non-chat/judge runs and windowed on created_at (D3). It asserts the
// exclusions (chat/judge never counted), both contract invariants (finished == sum of the
// four outcomes; sum(fail_origins) == failed), that a NULL fail_origin buckets as
// "unknown", the plan_rejected split (out of failed, in finished, D4), the 7-day window,
// and that the per-user and factory aggregates agree (per-user rows sum to the factory).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunOutcomesRollupsLiveDB(t *testing.T) {
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

	// Fresh users/repos so the SCOPED SelfRunOutcomes assertions are absolute; the shared
	// store-IT DB means the FACTORY aggregates also see other tests' runs, so those are
	// asserted only by invariant + per-user/factory agreement (mirrors TestUsageRollupsLiveDB).
	seedUserRepo := func(tag string) (userID, repoID uuid.UUID) {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("outcomes-%s-%s@e2e", tag, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, 1, 'g/r-outcomes-'||$3, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, tag+"-"+userID.String())
		return userID, repoID
	}
	userA, repoA := seedUserRepo("a")
	userB, repoB := seedUserRepo("b")

	oiid := int64(1_000_000) // high base so this test's issue_iids never collide with siblings'
	// seedOutcomeRun inserts a terminal run with an explicit status/kind/fail_origin/created_at.
	// An issue-shaped run carries repo_id + issue_iid (the kind-shape CHECK); a chat/judge run
	// carries neither (nor a branch), and a judge run additionally needs a target_run_id.
	seedOutcomeRun := func(userID, repoID uuid.UUID, status, kind string, failOrigin *string, daysAgo int, targetRunID *uuid.UUID) uuid.UUID {
		oiid++
		id := uuid.New()
		if kind == "chat" || kind == "judge" {
			var target any
			if targetRunID != nil {
				target = *targetRunID
			}
			mustExec(ctx, t, pool,
				`INSERT INTO runs (id, user_id, kind, target_run_id, issue_title, issue_description, status, fail_origin, created_at)
				 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, now() - make_interval(days => $7))`,
				id, userID, kind, target, status, failOrigin, daysAgo)
			return id
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, fail_origin, created_at)
			 VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, now() - make_interval(days => $8))`,
			id, userID, repoID, kind, oiid, status, failOrigin, daysAgo)
		return id
	}
	planRejected := "plan_rejected"
	agentFailure := "agent_failure"

	// userA: one run per terminal status + a plan_rejected + a real-origin failure + a
	// NULL-origin failure, plus one chat and one judge failure that MUST be excluded. The
	// NULL-origin failure is backdated 10 days (outside the 7-day window) to prove windowing.
	aCompleted := seedOutcomeRun(userA, repoA, "completed", "issue", nil, 0, nil)
	seedOutcomeRun(userA, repoA, "cancelled", "issue", nil, 0, nil)
	seedOutcomeRun(userA, repoA, "failed", "issue", &planRejected, 0, nil)
	seedOutcomeRun(userA, repoA, "failed", "issue", &agentFailure, 0, nil)
	seedOutcomeRun(userA, repoA, "failed", "issue", nil, 10, nil) // NULL origin -> "unknown", out of the 7-day window
	seedOutcomeRun(userA, repoA, "failed", "chat", &agentFailure, 0, nil)
	seedOutcomeRun(userA, repoA, "failed", "judge", &agentFailure, 0, &aCompleted)

	// userB: a smaller set so per-user grouping is meaningful (its runs must not leak into
	// userA's counts): one completed + one real-origin failure.
	seedOutcomeRun(userB, repoB, "completed", "issue", nil, 0, nil)
	seedOutcomeRun(userB, repoB, "failed", "issue", &agentFailure, 0, nil)

	// --- SelfRunOutcomes(userA): the exclusions and the two windows. The needs_landing
	// sub-cut (issue #1418) takes the Go-owned human-landable set as its bind array; this
	// test seeds no landable+recoverable failures, so the value is 0 either way.
	selfA, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: userA, LandableOrigins: workersvc.AllHumanLandableFailOrigins()})
	if err != nil {
		t.Fatalf("SelfRunOutcomes(A): %v", err)
	}
	// finished == 5 (completed, cancelled, plan_rejected, failed-agent, failed-null); the
	// chat and judge failures are EXCLUDED (a 7 here would mean the kind filter is broken).
	if selfA.LifetimeFinished != 5 {
		t.Fatalf("A lifetime finished = %d, want 5 (chat+judge excluded)", selfA.LifetimeFinished)
	}
	if selfA.LifetimeCompleted != 1 || selfA.LifetimeCancelled != 1 || selfA.LifetimePlanRejected != 1 {
		t.Fatalf("A lifetime completed/cancelled/plan_rejected = %d/%d/%d, want 1/1/1",
			selfA.LifetimeCompleted, selfA.LifetimeCancelled, selfA.LifetimePlanRejected)
	}
	// failed == 2 (agent_failure + NULL origin); the plan_rejected run is split OUT of failed.
	if selfA.LifetimeFailed != 2 {
		t.Fatalf("A lifetime failed = %d, want 2 (plan_rejected split out)", selfA.LifetimeFailed)
	}
	// Invariant 1: finished == completed + cancelled + plan_rejected + failed.
	if got := selfA.LifetimeCompleted + selfA.LifetimeCancelled + selfA.LifetimePlanRejected + selfA.LifetimeFailed; got != selfA.LifetimeFinished {
		t.Fatalf("A lifetime invariant: finished %d != sum %d", selfA.LifetimeFinished, got)
	}
	// Windowing: the NULL-origin failure is 10 days old, so last7 sees 4 finished / 1 failed.
	if selfA.Last7Finished != 4 || selfA.Last7Failed != 1 {
		t.Fatalf("A last7 finished/failed = %d/%d, want 4/1 (the 10-day-old NULL failure is out of window)",
			selfA.Last7Finished, selfA.Last7Failed)
	}
	if got := selfA.Last7Completed + selfA.Last7Cancelled + selfA.Last7PlanRejected + selfA.Last7Failed; got != selfA.Last7Finished {
		t.Fatalf("A last7 invariant: finished %d != sum %d", selfA.Last7Finished, got)
	}

	// --- SelfRunOutcomeOrigins(userA): NULL buckets as "unknown"; sum == failed per window.
	lifeOrigins, last7Origins := map[string]int64{}, map[string]int64{}
	originRows, err := q.SelfRunOutcomeOrigins(ctx, userA)
	if err != nil {
		t.Fatalf("SelfRunOutcomeOrigins(A): %v", err)
	}
	for _, o := range originRows {
		switch o.WindowTag {
		case "lifetime":
			lifeOrigins[o.Origin] = o.Cnt
		case "last7":
			last7Origins[o.Origin] = o.Cnt
		}
	}
	if lifeOrigins["agent_failure"] != 1 || lifeOrigins["unknown"] != 1 {
		t.Fatalf("A lifetime origins = %v, want agent_failure:1 unknown:1", lifeOrigins)
	}
	// Invariant 2: sum(fail_origins) == failed, per window.
	if s := lifeOrigins["agent_failure"] + lifeOrigins["unknown"]; s != selfA.LifetimeFailed {
		t.Fatalf("A lifetime sum(origins)=%d != failed=%d", s, selfA.LifetimeFailed)
	}
	if last7Origins["agent_failure"] != 1 || last7Origins["unknown"] != 0 {
		t.Fatalf("A last7 origins = %v, want agent_failure:1 (the NULL failure is out of window)", last7Origins)
	}
	if last7Origins["agent_failure"] != selfA.Last7Failed {
		t.Fatalf("A last7 sum(origins)=%d != failed=%d", last7Origins["agent_failure"], selfA.Last7Failed)
	}

	// --- SelfRunOutcomes(userB): isolation — userA's runs never leak in.
	selfB, err := q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{UserID: userB, LandableOrigins: workersvc.AllHumanLandableFailOrigins()})
	if err != nil {
		t.Fatalf("SelfRunOutcomes(B): %v", err)
	}
	if selfB.LifetimeFinished != 2 || selfB.LifetimeFailed != 1 || selfB.LifetimeCompleted != 1 {
		t.Fatalf("B lifetime finished/failed/completed = %d/%d/%d, want 2/1/1",
			selfB.LifetimeFinished, selfB.LifetimeFailed, selfB.LifetimeCompleted)
	}

	// --- Per-user aggregate == self, and the per-user rows sum to the factory (agreement).
	perUser, err := q.AdminRunOutcomesPerUser(ctx, workersvc.AllHumanLandableFailOrigins())
	if err != nil {
		t.Fatalf("AdminRunOutcomesPerUser: %v", err)
	}
	perUserByID := map[uuid.UUID]store.AdminRunOutcomesPerUserRow{}
	var sumFinished, sumCompleted, sumCancelled, sumPlanRejected, sumFailed int64
	for _, u := range perUser {
		perUserByID[u.UserID] = u
		sumFinished += u.Finished
		sumCompleted += u.Completed
		sumCancelled += u.Cancelled
		sumPlanRejected += u.PlanRejected
		sumFailed += u.Failed
	}
	if a := perUserByID[userA]; a.Finished != selfA.LifetimeFinished || a.Failed != selfA.LifetimeFailed ||
		a.Completed != selfA.LifetimeCompleted || a.Cancelled != selfA.LifetimeCancelled || a.PlanRejected != selfA.LifetimePlanRejected {
		t.Fatalf("per-user row for A (%+v) must equal SelfRunOutcomes lifetime (finished %d failed %d)",
			a, selfA.LifetimeFinished, selfA.LifetimeFailed)
	}
	if b := perUserByID[userB]; b.Finished != 2 || b.Failed != 1 {
		t.Fatalf("per-user row for B = finished %d/failed %d, want 2/1", b.Finished, b.Failed)
	}

	factory, err := q.AdminRunOutcomes(ctx, workersvc.AllHumanLandableFailOrigins())
	if err != nil {
		t.Fatalf("AdminRunOutcomes: %v", err)
	}
	if sumFinished != factory.LifetimeFinished || sumCompleted != factory.LifetimeCompleted ||
		sumCancelled != factory.LifetimeCancelled || sumPlanRejected != factory.LifetimePlanRejected ||
		sumFailed != factory.LifetimeFailed {
		t.Fatalf("per-user rows must sum to the factory: sum(finished %d/completed %d/cancelled %d/plan_rejected %d/failed %d) "+
			"vs factory(finished %d/completed %d/cancelled %d/plan_rejected %d/failed %d)",
			sumFinished, sumCompleted, sumCancelled, sumPlanRejected, sumFailed,
			factory.LifetimeFinished, factory.LifetimeCompleted, factory.LifetimeCancelled, factory.LifetimePlanRejected, factory.LifetimeFailed)
	}
	// The factory invariant holds over the whole shared DB too.
	if got := factory.LifetimeCompleted + factory.LifetimeCancelled + factory.LifetimePlanRejected + factory.LifetimeFailed; got != factory.LifetimeFinished {
		t.Fatalf("factory lifetime invariant: finished %d != sum %d", factory.LifetimeFinished, got)
	}

	// --- Per-user origins agree with the self origins for userA (NULL -> "unknown").
	puOriginsA := map[string]int64{}
	puOriginRows, err := q.AdminRunOutcomeOriginsPerUser(ctx)
	if err != nil {
		t.Fatalf("AdminRunOutcomeOriginsPerUser: %v", err)
	}
	for _, o := range puOriginRows {
		if o.UserID == userA {
			puOriginsA[o.Origin] = o.Cnt
		}
	}
	if puOriginsA["agent_failure"] != 1 || puOriginsA["unknown"] != 1 {
		t.Fatalf("per-user origins for A = %v, want agent_failure:1 unknown:1", puOriginsA)
	}
}

// TestRunUsagePerLegFoldEndToEndLiveDB drives the PRODUCTION fold (workersvc.AppendMessages)
// over the eight recorded frames of run 02854d5e — four init frames interleaved with four
// result-frame legs — through a REAL Postgres, and proves the run total is the SUM of the
// four legs (PRD #1079 Success Criterion 5). Feeding one frame per batch exercises the
// cross-batch epoch read (a result frame's epoch is counted off the init frames already
// committed to run_messages); replaying every batch in a SCRAMBLED order proves the
// position-absolute epoch is order-independent — the total is unchanged after replay.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestRunUsagePerLegFoldEndToEndLiveDB(t *testing.T) {
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

	// Read the fixture (the SAME file the Go and TS contract halves read; it sits above
	// the api module, so this reads it by relative path).
	type fixtureFrame struct {
		Seq     int32           `json:"seq"`
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	var fixture struct {
		Frames []fixtureFrame `json:"frames"`
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "run-usage", "result-frames-02854d5e.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fixture.Frames) != 8 {
		t.Fatalf("fixture must carry 8 frames (4 init + 4 result), got %d", len(fixture.Frames))
	}

	// Seed a user, connection, repo, worker and a running run the worker owns.
	userID, connID, repoID, runID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("perleg-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, max_concurrent_runs)
		 VALUES ($1, $2, 'w', $3, 4)`, workerID, userID, fmt.Sprintf("hash-%s", workerID))
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, worker_id, issue_iid, issue_title, issue_description, status, session_id)
		 VALUES ($1, $2, $3, $4, 1, 't', 'd', 'running', 'sess-02854d5e')`, runID, userID, repoID, workerID)

	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	svc := workersvc.New(q, box, workersvc.Params{})
	wkr := store.Worker{ID: workerID}

	batchOf := func(f fixtureFrame) []workersvc.IncomingMessage {
		return []workersvc.IncomingMessage{{Seq: f.Seq, Kind: f.Kind, Payload: f.Payload}}
	}
	// Pass 1: one frame per batch, in seq order (init before its result).
	for _, f := range fixture.Frames {
		if err := svc.AppendMessages(ctx, wkr, runID, batchOf(f)); err != nil {
			t.Fatalf("AppendMessages(seq %d): %v", f.Seq, err)
		}
	}

	// The authored per-leg SUM (fixtures/run-usage/run-usage-02854d5e.json totals).
	assertTotal := func(when string) {
		t.Helper()
		total, err := q.GetRunUsageTotal(ctx, runID)
		if err != nil {
			t.Fatalf("GetRunUsageTotal (%s): %v", when, err)
		}
		if total.InputTokens != 12785 || total.CacheReadTokens != 187880173 ||
			total.CacheCreationTokens != 5500712 || total.OutputTokens != 1021240 {
			t.Fatalf("run total (%s) = in %d/cr %d/cw %d/out %d, want the per-leg SUM 12785/187880173/5500712/1021240",
				when, total.InputTokens, total.CacheReadTokens, total.CacheCreationTokens, total.OutputTokens)
		}
		// cost_usd compared via SQL numeric equality to avoid float→numeric ambiguity.
		var costOK bool
		if err := pool.QueryRow(ctx,
			`SELECT cost_usd = 153.582776::numeric FROM run_usage_totals WHERE run_id = $1`, runID).Scan(&costOK); err != nil {
			t.Fatalf("read view cost (%s): %v", when, err)
		}
		if !costOK {
			t.Fatalf("run total cost (%s) must be the per-leg SUM 153.582776 USD", when)
		}
		// Guard the discriminator: 77.185539 is the pre-#1079 MAX-per-model under-count.
		var isUndercount bool
		if err := pool.QueryRow(ctx,
			`SELECT cost_usd = 77.185539::numeric FROM run_usage_totals WHERE run_id = $1`, runID).Scan(&isUndercount); err != nil {
			t.Fatalf("read view cost undercount (%s): %v", when, err)
		}
		if isUndercount {
			t.Fatalf("run total (%s) collapsed to the MAX-per-model under-count 77.185539 — legs were not kept apart", when)
		}
		// Six per-(model, epoch) rows: haiku@1, opus@1..4, sonnet@4.
		var rows int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_usage WHERE run_id = $1`, runID).Scan(&rows); err != nil {
			t.Fatalf("count rows (%s): %v", when, err)
		}
		if rows != 6 {
			t.Fatalf("expected 6 per-(model, epoch) rows (%s), got %d", when, rows)
		}
	}
	assertTotal("first pass")

	// Pass 2: replay every batch in a SCRAMBLED order. All frames are already persisted,
	// so each result frame's epoch is recomputed off the same committed init frames,
	// independent of delivery order — the total must be identical.
	scramble := []int{7, 3, 5, 1, 6, 0, 4, 2}
	if len(scramble) != len(fixture.Frames) {
		t.Fatalf("scramble permutation must cover all %d frames", len(fixture.Frames))
	}
	for _, idx := range scramble {
		f := fixture.Frames[idx]
		if err := svc.AppendMessages(ctx, wkr, runID, batchOf(f)); err != nil {
			t.Fatalf("replay AppendMessages(seq %d): %v", f.Seq, err)
		}
	}
	assertTotal("after scrambled replay")

	// A tautology guard so the assertions above cannot be vacuously green: the true SUM
	// and the under-count must differ (they do: 153.58 vs 77.19).
	if math.Abs(153.582776-77.185539) < 1 {
		t.Fatal("the per-leg SUM and the MAX-per-model under-count must differ for this test to discriminate")
	}
}
