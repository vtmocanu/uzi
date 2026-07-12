package store_test

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
	fold := func(session string, in, cacheR, cacheC, out int64, cost float64) {
		t.Helper()
		if err := q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
			RunID: runID, SessionID: session, Model: model,
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
	fold("sess-1", 1000, 200, 50, 400, 0.010)
	if in, out := tokensOf("sess-1"); in != 1000 || out != 400 {
		t.Fatalf("phase-1 row wrong: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.010000") {
		t.Fatal("phase-1 cost should be 0.010000")
	}

	// Phase 2 resumes sess-1: under verdict (b) the frame is cumulative, so a HIGHER
	// snapshot lands on the SAME key. GREATEST advances the row.
	fold("sess-1", 1800, 500, 90, 700, 0.019)
	if in, out := tokensOf("sess-1"); in != 1800 || out != 700 {
		t.Fatalf("phase-2 merge should advance to the higher cumulative: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.019000") {
		t.Fatal("phase-2 cost should advance to 0.019000")
	}

	// Crash-retry re-delivers the phase-1 (LOWER) frame after phase-2. GREATEST must
	// NOT regress the row — the "re-delivery changes nothing" guarantee.
	fold("sess-1", 1000, 200, 50, 400, 0.010)
	if in, out := tokensOf("sess-1"); in != 1800 || out != 700 {
		t.Fatalf("re-delivered earlier frame must not regress the row: in=%d out=%d", in, out)
	}
	if !costMatches("sess-1", "0.019000") {
		t.Fatal("re-delivered lower cost must not regress the row")
	}

	// A DISTINCT evolved session id carries the same model's cumulative forward (the
	// SDK accumulator is restored on resume). Two session rows now exist for the model.
	fold("sess-2", 2500, 800, 120, 1100, 0.033)

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
}
