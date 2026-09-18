package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUsageFoldDerivesHarnessLiveDB proves foldUsageFrames DERIVES the persisted run_usage
// row's harness/cost_status from runs.harness (PRD #1332 M5A / D2), never from a
// worker-supplied field, against a REAL Postgres:
//
//   - a Claude run's usage row is harness='claude', cost_status='metered', and keeps its
//     existing provider costUSD — old Claude accounting is UNCHANGED;
//   - a Codex run's usage row is harness='codex', cost_status='unreported', with the dollar
//     placeholder ZEROED even though the synthetic frame reported a positive costUSD (C4b
//     adds real Codex cost semantics later; C1 forces unreported).
//
// The Codex-harness frame is synthetic: real Codex agent emission arrives in C4a. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via ./e2e/run-store-it.sh).
func TestUsageFoldDerivesHarnessLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("foldharness-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// resultFrame builds a delivered SDK result frame (kind status) carrying one model's
	// usage — the exact shape foldUsageFrames folds.
	resultFrame := func(seq int32, model string, in, out int64, costUSD float64) IncomingMessage {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"event": "result",
			"modelUsage": map[string]any{
				model: map[string]any{
					"inputTokens":  in,
					"outputTokens": out,
					"costUSD":      costUSD,
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		return IncomingMessage{Seq: seq, Kind: "status", Agent: "lead", Payload: payload}
	}

	// readUsage returns the persisted (harness, cost_status, cost_usd-as-text) for a row.
	readUsage := func(runID uuid.UUID, model string) (harness, costStatus, costText string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT harness, cost_status, cost_usd::text FROM run_usage WHERE run_id=$1 AND model=$2`,
			runID, model).Scan(&harness, &costStatus, &costText); err != nil {
			t.Fatalf("read run_usage (%s): %v", runID, err)
		}
		return
	}

	// --- Claude run: harness='claude' by the CreateRun default; fold keeps metered + cost.
	claudeRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id)
	      VALUES ($1, $2, $3, 1, 't', 'd', 'running', 'issue', 'sess-claude')`, claudeRunID, userID, repoID)
	claudeRun, err := q.GetRunByID(ctx, claudeRunID)
	if err != nil {
		t.Fatalf("GetRunByID(claude): %v", err)
	}
	if claudeRun.Harness != "claude" {
		t.Fatalf("claude run harness = %q, want 'claude'", claudeRun.Harness)
	}
	if err := foldUsageFrames(ctx, q, claudeRun, []IncomingMessage{resultFrame(10, "claude-fable-5", 1000, 400, 0.0123)}); err != nil {
		t.Fatalf("foldUsageFrames(claude): %v", err)
	}
	if h, cs, cost := readUsage(claudeRunID, "claude-fable-5"); h != "claude" || cs != "metered" || cost != "0.012300" {
		t.Fatalf("claude usage row = harness %q / cost_status %q / cost_usd %q, want claude/metered/0.012300", h, cs, cost)
	}

	// --- Codex run: raw-flip harness='codex' (the coherence CHECK permits a Codex row with
	// no binding). The fold must DERIVE codex → unreported and ZERO the dollar placeholder
	// even though the frame reports a positive costUSD.
	codexRunID := uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, session_id)
	      VALUES ($1, $2, $3, 2, 't', 'd', 'running', 'issue', 'sess-codex')`, codexRunID, userID, repoID)
	exec(`UPDATE runs SET harness='codex' WHERE id=$1`, codexRunID)
	codexRun, err := q.GetRunByID(ctx, codexRunID)
	if err != nil {
		t.Fatalf("GetRunByID(codex): %v", err)
	}
	if codexRun.Harness != "codex" {
		t.Fatalf("codex run harness = %q, want 'codex'", codexRun.Harness)
	}
	if err := foldUsageFrames(ctx, q, codexRun, []IncomingMessage{resultFrame(10, "gpt-6-astra", 2000, 800, 5.55)}); err != nil {
		t.Fatalf("foldUsageFrames(codex): %v", err)
	}
	if h, cs, cost := readUsage(codexRunID, "gpt-6-astra"); h != "codex" || cs != "unreported" || cost != "0.000000" {
		t.Fatalf("codex usage row = harness %q / cost_status %q / cost_usd %q, want codex/unreported/0.000000", h, cs, cost)
	}
	// Tokens are preserved even though the dollar cost is unreported.
	var inTok, outTok int64
	if err := pool.QueryRow(ctx,
		`SELECT input_tokens, output_tokens FROM run_usage WHERE run_id=$1 AND model=$2`,
		codexRunID, "gpt-6-astra").Scan(&inTok, &outTok); err != nil {
		t.Fatalf("read codex tokens: %v", err)
	}
	if inTok != 2000 || outTok != 800 {
		t.Fatalf("codex usage tokens = in %d/out %d, want 2000/800 (tokens must survive an unreported cost)", inTok, outTok)
	}
}
