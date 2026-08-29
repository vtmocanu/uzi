package store_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
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
			CostUsd: numericFromMicros(cost),
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

	// A DISTINCT evolved session id, and a DISTINCT epoch (leg 2 of a broken lineage):
	// the dropped-resume fresh leg lands under its own (run, session) row. Two session
	// rows now exist for the model, carrying epochs 0 and 1.
	fold("sess-2", 1, 2500, 800, 120, 1100, 0.033)

	// The (b) rollup is latest/MAX per (run_id, model) — NEVER a SUM across session
	// rows, which would over-count the cumulative snapshots. This is the rule M3 must
	// use for run/user/factory totals.
	var maxIn, sumIn int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(input_tokens),0), COALESCE(SUM(input_tokens),0) FROM run_usage WHERE run_id=$1 AND model=$2`,
		runID, model).Scan(&maxIn, &sumIn); err != nil {
		t.Fatalf("rollup read: %v", err)
	}
	if maxIn != 2500 {
		t.Fatalf("latest/MAX-per-model rollup should be the final cumulative 2500, got %d", maxIn)
	}
	if sumIn <= maxIn {
		t.Fatalf("test setup bug: SUM (%d) should exceed MAX (%d) so the over-count is demonstrable", sumIn, maxIn)
	}

	// Row-splitter precondition (PRD #632): the two legs MUST persist as SEPARATE
	// run_usage rows under distinct session_ids — that separation comes from
	// session_id (the conflict key is (run_id, session_id, model), NOT lineage_epoch),
	// and it is what lets the view SUM the legs instead of GREATEST-collapsing them at
	// the row level. Assert exactly two rows for this (run, model), keyed {sess-1,
	// sess-2}, and that each carries its expected epoch (sess-1 → 0, sess-2 → 1). This
	// guards the row-splitter, not merely the epoch stamp: a future change to the
	// onSessionId latch or the SetRunRunning COALESCE that collapsed the legs onto one
	// session_id would fail HERE even if the epoch stamp still worked.
	epochBySession := map[string]int32{}
	rows2, err := pool.Query(ctx,
		`SELECT session_id, lineage_epoch FROM run_usage WHERE run_id=$1 AND model=$2`, runID, model)
	if err != nil {
		t.Fatalf("read legs: %v", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var sess string
		var epoch int32
		if err := rows2.Scan(&sess, &epoch); err != nil {
			t.Fatalf("scan leg: %v", err)
		}
		epochBySession[sess] = epoch
	}
	if err := rows2.Err(); err != nil {
		t.Fatalf("iterate legs: %v", err)
	}
	if len(epochBySession) != 2 {
		t.Fatalf("the two legs must persist as 2 distinct-session rows, got %d: %+v", len(epochBySession), epochBySession)
	}
	if e, ok := epochBySession["sess-1"]; !ok || e != 0 {
		t.Fatalf("leg 1 must be session sess-1 at epoch 0, got %+v", epochBySession)
	}
	if e, ok := epochBySession["sess-2"]; !ok || e != 1 {
		t.Fatalf("leg 2 must be session sess-2 at epoch 1, got %+v", epochBySession)
	}
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
