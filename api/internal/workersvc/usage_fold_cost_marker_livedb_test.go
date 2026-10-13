package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUsageFoldHonorsCodexCostMarkerLiveDB proves the C4b fold HONORS the agent's closed
// per-model costStatus marker for a Codex run and persists D5's conservative resolution to a
// REAL Postgres (through the run_usage CHECK constraints), while a Claude run still folds
// metered under its provider costUSD with the marker IGNORED:
//
//   - Codex + 'metered'      → cost_status='metered', cost_usd = the agent price-table amount;
//   - Codex + 'subscription' → cost_status='subscription', cost_usd=0 (even if a cost was emitted);
//   - Codex + 'unreported' / MISSING / UNKNOWN → cost_status='unreported', cost_usd=0;
//   - Claude + any marker    → cost_status='metered' under the provider costUSD (backward compat).
//
// The Codex-harness frames are synthetic (real Codex emission arrives via C4a): M5A is dark, so
// only tests reach a Codex row. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres (run via ./e2e/run-store-it.sh).
//
// Calibration (needs the DB): make deriveUsageCost's Codex 'metered' arm return unreported and
// the gpt-6-astra assertion reddens; make its default arm return metered and both the
// codex-missing and codex-unknown assertions redden.
func TestUsageFoldHonorsCodexCostMarkerLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("foldmarker-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// markedModelUsage builds one modelUsage entry, attaching the closed costStatus marker only
	// when non-empty — a pre-C4b / missing-marker frame omits the key entirely (lenient decode → "").
	markedModelUsage := func(marker string, costUSD float64) map[string]any {
		mu := map[string]any{"inputTokens": 2000, "outputTokens": 800, "costUSD": costUSD}
		if marker != "" {
			mu["costStatus"] = marker
		}
		return mu
	}

	// readUsage returns the persisted (harness, cost_status, cost_usd-as-text) for a row.
	readUsage := func(runID uuid.UUID, model string) (harness, costStatus, costText string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT harness, cost_status, cost_usd::text FROM run_usage WHERE run_id=$1 AND model=$2`,
			runID, model).Scan(&harness, &costStatus, &costText); err != nil {
			t.Fatalf("read run_usage (%s / %s): %v", runID, model, err)
		}
		return
	}

	// --- Codex run: raw-flip harness='codex' (the coherence CHECK permits a Codex row with no
	// binding). One frame carries every marker on its OWN model — the PK includes model, so the
	// rows never collide — and the fold must resolve each per D5.
	codexRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id)
	      VALUES ($1, $2, $3, 1, 't', 'd', 'running', 'issue', 'sess-marker-codex')`, codexRunID, userID, repoID)
	exec(`UPDATE runs SET harness='codex' WHERE id=$1`, codexRunID)
	codexRun, err := q.GetRunByID(ctx, codexRunID)
	if err != nil {
		t.Fatalf("GetRunByID(codex): %v", err)
	}
	if codexRun.Harness != "codex" {
		t.Fatalf("codex run harness = %q, want 'codex'", codexRun.Harness)
	}

	codexCases := []struct {
		model, marker        string
		costUSD              float64
		wantStatus, wantCost string
	}{
		{"gpt-6-astra", "metered", 5.55, "metered", "5.550000"},                 // metered marker → agent price-table amount
		{"gpt-5.6-sol", "subscription", 0, "subscription", "0.000000"},          // subscription → neutral zero
		{"codex-sub-nonzero", "subscription", 9.99, "subscription", "0.000000"}, // subscription zeroes even an emitted cost
		{"codex-explicit-unrep", "unreported", 3.33, "unreported", "0.000000"},  // explicit unreported
		{"codex-missing", "", 3.33, "unreported", "0.000000"},                   // MISSING marker → unreported
		{"codex-unknown", "flex-tier", 3.33, "unreported", "0.000000"},          // UNKNOWN marker → unreported
		{"codex-metered-neg", "metered", -5.5, "unreported", "0.000000"},        // m5: metered + NEGATIVE cost → unreported
	}
	modelUsage := map[string]any{}
	for _, c := range codexCases {
		modelUsage[c.model] = markedModelUsage(c.marker, c.costUSD)
	}
	// Three malformed/absent-field models that markedModelUsage's typed (string marker, float64
	// cost) shape cannot express, folded in the SAME frame as the valid rows above so they double
	// as the sibling-survival proof — before m4 decoded costStatus/costUSD as json.RawMessage, a
	// malformed field would fail the frame's json.Unmarshal and drop EVERY row:
	//   - codex-absent-cost: a 'metered' marker with NO costUSD key → m5 → unreported/0;
	//   - codex-badstatus:   a non-string costStatus (bool) → invalid marker → unreported/0;
	//   - codex-null-cost:   a 'metered' marker with a JSON `null` costUSD (nil marshals to the
	//     `null` literal, distinct from an omitted key) → Fix 1 treats null as ABSENT → unreported/0
	//     (before Fix 1 it resolved to a metered $0.000000 row).
	modelUsage["codex-absent-cost"] = map[string]any{"inputTokens": 2000, "outputTokens": 800, "costStatus": "metered"}
	modelUsage["codex-badstatus"] = map[string]any{"inputTokens": 2000, "outputTokens": 800, "costUSD": 1.11, "costStatus": false}
	modelUsage["codex-null-cost"] = map[string]any{"inputTokens": 2000, "outputTokens": 800, "costUSD": nil, "costStatus": "metered"}
	codexPayload, err := json.Marshal(map[string]any{"event": "result", "modelUsage": modelUsage})
	if err != nil {
		t.Fatalf("marshal codex frame: %v", err)
	}
	if err := foldUsageFrames(ctx, q, codexRun, []IncomingMessage{{Seq: 10, Kind: "status", Agent: "lead", Payload: codexPayload}}); err != nil {
		t.Fatalf("foldUsageFrames(codex): %v", err)
	}
	for _, c := range codexCases {
		if h, cs, cost := readUsage(codexRunID, c.model); h != "codex" || cs != c.wantStatus || cost != c.wantCost {
			t.Fatalf("codex model %q row = harness %q / cost_status %q / cost_usd %q, want codex/%s/%s",
				c.model, h, cs, cost, c.wantStatus, c.wantCost)
		}
	}
	// The malformed/absent-field siblings: each persists (frame not dropped) and resolves to unreported/0.
	for _, model := range []string{"codex-absent-cost", "codex-badstatus", "codex-null-cost"} {
		if h, cs, cost := readUsage(codexRunID, model); h != "codex" || cs != "unreported" || cost != "0.000000" {
			t.Fatalf("codex malformed model %q row = harness %q / cost_status %q / cost_usd %q, want codex/unreported/0.000000",
				model, h, cs, cost)
		}
	}
	// Tokens survive an unreported cost — a zero dollar placeholder is not lost token data.
	var inTok, outTok int64
	if err := pool.QueryRow(ctx,
		`SELECT input_tokens, output_tokens FROM run_usage WHERE run_id=$1 AND model=$2`,
		codexRunID, "codex-missing").Scan(&inTok, &outTok); err != nil {
		t.Fatalf("read codex tokens: %v", err)
	}
	if inTok != 2000 || outTok != 800 {
		t.Fatalf("codex tokens = in %d/out %d, want 2000/800 (tokens survive an unreported cost)", inTok, outTok)
	}

	// --- Claude run: metered under the provider costUSD, and the marker is IGNORED even though
	// the frame carries a 'subscription' marker (D5 backward compat — Claude never consults it).
	claudeRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id)
	      VALUES ($1, $2, $3, 2, 't', 'd', 'running', 'issue', 'sess-marker-claude')`, claudeRunID, userID, repoID)
	claudeRun, err := q.GetRunByID(ctx, claudeRunID)
	if err != nil {
		t.Fatalf("GetRunByID(claude): %v", err)
	}
	if claudeRun.Harness != "claude" {
		t.Fatalf("claude run harness = %q, want 'claude'", claudeRun.Harness)
	}
	claudePayload, err := json.Marshal(map[string]any{
		"event":      "result",
		"modelUsage": map[string]any{"claude-fable-5": markedModelUsage("subscription", 0.0123)},
	})
	if err != nil {
		t.Fatalf("marshal claude frame: %v", err)
	}
	if err := foldUsageFrames(ctx, q, claudeRun, []IncomingMessage{{Seq: 10, Kind: "status", Agent: "lead", Payload: claudePayload}}); err != nil {
		t.Fatalf("foldUsageFrames(claude): %v", err)
	}
	if h, cs, cost := readUsage(claudeRunID, "claude-fable-5"); h != "claude" || cs != "metered" || cost != "0.012300" {
		t.Fatalf("claude usage row = harness %q / cost_status %q / cost_usd %q, want claude/metered/0.012300 (marker ignored)", h, cs, cost)
	}
}

// Example 4 composes agent-shaped result frames with the existing persisted fold and
// aggregate readers. It does not exercise a provider or infer a credential's billing.
func TestUsageFoldPricedSubscriptionAndLegacyLegsLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("foldmarker-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	before, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	makeRun := func(iid int64, age int) store.Run {
		t.Helper()
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, harness, session_id, created_at)
        VALUES ($1,$2,$3,$4,'t','d','running','issue','codex',$5,now()-make_interval(days => $6))`, id, userID, repoID, iid, uuid.NewString(), age)
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	fold := func(run store.Run, seq int32, model string, usage map[string]any) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"event": "result", "modelUsage": map[string]any{model: usage}})
		if err != nil {
			t.Fatal(err)
		}
		if err := foldUsageFrames(ctx, q, run, []IncomingMessage{{Seq: seq, Kind: "status", Agent: "lead", Payload: payload}}); err != nil {
			t.Fatal(err)
		}
	}
	// Exact priced subscription-shaped agent checker fixture: reasoning is included
	// in outputTokens, not charged again. The server honors the emitted marker/amount.
	priced := map[string]any{"inputTokens": 300, "cacheReadInputTokens": 600, "cacheCreationInputTokens": 100, "outputTokens": 200, "reasoningOutputTokens": 50, "costStatus": "metered", "costUSD": 0.00291}
	legacy := map[string]any{"inputTokens": 120, "outputTokens": 40, "costStatus": "subscription", "costUSD": 0}
	newRun := makeRun(41, 0)
	fold(newRun, 10, "gpt-6.1-sol", priced)
	mixed := makeRun(42, 0)
	fold(mixed, 10, "gpt-6.1-sol", legacy)
	// Persist an init between results so the second leg has BOTH a distinct session
	// and a distinct epoch, rather than exercising same-key conflict suppression.
	exec(`INSERT INTO run_messages (run_id,seq,kind,agent,payload) VALUES ($1,20,'status','lead','{"event":"init"}'::jsonb)`, mixed.ID)
	exec(`UPDATE runs SET session_id=$2 WHERE id=$1`, mixed.ID, uuid.NewString())
	mixed, err = q.GetRunByID(ctx, mixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	fold(mixed, 30, "gpt-6.1-sol", priced)
	oldRun := makeRun(43, 10)
	fold(oldRun, 10, "legacy-model", legacy)
	oldWorker := makeRun(44, 0)
	fold(oldWorker, 10, "old-worker", map[string]any{"inputTokens": 70, "outputTokens": 30})

	for _, run := range []store.Run{newRun, mixed} {
		var marker, amount, model string
		var in, read, create, out int64
		if err := pool.QueryRow(ctx, `SELECT model,cost_status,cost_usd::text,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens FROM run_usage WHERE run_id=$1 AND cost_status='metered'`, run.ID).Scan(&model, &marker, &amount, &in, &read, &create, &out); err != nil {
			t.Fatal(err)
		}
		if model != "gpt-6.1-sol" || marker != "metered" || amount != "0.002910" || in != 300 || read != 600 || create != 100 || out != 200 {
			t.Fatalf("persisted priced row: %s %s %s tokens %d/%d/%d/%d", model, marker, amount, in, read, create, out)
		}
	}
	var legs, sessions, epochs int64
	if err := pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT session_id),count(DISTINCT lineage_epoch) FROM run_usage WHERE run_id=$1`, mixed.ID).Scan(&legs, &sessions, &epochs); err != nil {
		t.Fatal(err)
	}
	if legs != 2 || sessions != 2 || epochs != 2 {
		t.Fatalf("mixed identities: legs=%d sessions=%d epochs=%d, want 2/2/2", legs, sessions, epochs)
	}
	number := func(n pgtype.Numeric) float64 {
		t.Helper()
		v, err := n.Float64Value()
		if err != nil || !v.Valid {
			t.Fatalf("numeric %v: %v", n, err)
		}
		return v.Float64
	}
	for _, tc := range []struct {
		run                   store.Run
		status                string
		in, read, create, out int64
		cost                  float64
	}{
		{newRun, "metered", 300, 600, 100, 200, 0.00291},
		{mixed, "unreported", 420, 600, 100, 240, 0.00291},
		{oldRun, "subscription", 120, 0, 0, 40, 0},
		{oldWorker, "unreported", 70, 0, 0, 30, 0},
	} {
		total, err := q.GetRunUsageTotal(ctx, tc.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if total.CostStatus != tc.status || total.InputTokens != tc.in || total.CacheReadTokens != tc.read || total.CacheCreationTokens != tc.create || total.OutputTokens != tc.out || number(total.CostUsd) != tc.cost {
			t.Fatalf("run total %s: %+v", tc.status, total)
		}
	}
	self, err := q.SelfUsage(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if number(self.LifetimeCostUsd) != 0.00582 || number(self.Last7CostUsd) != 0.00582 || self.LifetimeSubscriptionRunCount != 1 || self.LifetimeUnreportedRunCount != 2 || self.Last7SubscriptionRunCount != 0 || self.Last7UnreportedRunCount != 2 || self.RunCount != 4 {
		t.Fatalf("self windows: %+v", self)
	}
	if self.LifetimeInputTokens != 910 || self.LifetimeCacheReadTokens != 1200 || self.LifetimeCacheCreationTokens != 200 || self.LifetimeOutputTokens != 510 || self.Last7InputTokens != 790 || self.Last7CacheReadTokens != 1200 || self.Last7CacheCreationTokens != 200 || self.Last7OutputTokens != 470 {
		t.Fatalf("self tokens: %+v", self)
	}
	rows, err := q.AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.UserID != userID {
			continue
		}
		found = true
		if number(row.CostUsd) != 0.00582 || number(row.Last7CostUsd) != 0.00582 || row.SubscriptionRunCount != 1 || row.UnreportedRunCount != 2 || row.Last7SubscriptionRunCount != 0 || row.Last7UnreportedRunCount != 2 || row.RunCount != 4 || row.Last7RunCount != 3 {
			t.Fatalf("admin user windows: %+v", row)
		}
		if row.InputTokens != 910 || row.CacheReadTokens != 1200 || row.CacheCreationTokens != 200 || row.OutputTokens != 510 || row.Last7InputTokens != 790 || row.Last7CacheReadTokens != 1200 || row.Last7CacheCreationTokens != 200 || row.Last7OutputTokens != 470 {
			t.Fatalf("admin user tokens: %+v", row)
		}
	}
	if !found {
		t.Fatal("missing admin user")
	}
	after, err := q.AdminUsageTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Factory readers share the test DB. Compare this fixture's literal contribution
	// against the snapshot taken before seeding; no truncate of sibling fixtures.
	if math.Abs(number(after.LifetimeCostUsd)-number(before.LifetimeCostUsd)-0.00582) > 1e-9 || math.Abs(number(after.Last7CostUsd)-number(before.Last7CostUsd)-0.00582) > 1e-9 || after.LifetimeSubscriptionRunCount-before.LifetimeSubscriptionRunCount != 1 || after.LifetimeUnreportedRunCount-before.LifetimeUnreportedRunCount != 2 || after.Last7SubscriptionRunCount-before.Last7SubscriptionRunCount != 0 || after.Last7UnreportedRunCount-before.Last7UnreportedRunCount != 2 {
		t.Fatalf("admin factory windows: before=%+v after=%+v", before, after)
	}
	if after.LifetimeInputTokens-before.LifetimeInputTokens != 910 || after.LifetimeCacheReadTokens-before.LifetimeCacheReadTokens != 1200 || after.LifetimeCacheCreationTokens-before.LifetimeCacheCreationTokens != 200 || after.LifetimeOutputTokens-before.LifetimeOutputTokens != 510 || after.Last7InputTokens-before.Last7InputTokens != 790 || after.Last7CacheReadTokens-before.Last7CacheReadTokens != 1200 || after.Last7CacheCreationTokens-before.Last7CacheCreationTokens != 200 || after.Last7OutputTokens-before.Last7OutputTokens != 470 {
		t.Fatalf("admin factory tokens: before=%+v after=%+v", before, after)
	}
}
