package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_fold_generation_livedb_test.go is the M9 (task b) live-DB gate for per-generation fold
// PROVENANCE: run_usage.claim_generation is attributed to the epoch of the FRAMES that produced the
// leg, the COALESCE guard never clobbers an established provenance, the incremental fold stamps the
// batch's fenced generation, and a full refold preserves each frame's persisted generation (old
// spend stays on the old token). Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

// seedFoldRun inserts a 'running' issue run owned by workerID, with a session id and the given
// claim_generation, for the fold-attribution tests.
func (e codexTestEnv) seedFoldRun(t *testing.T, userID, workerID, repoID uuid.UUID, gen int64, session string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, session_id, claim_generation, last_seq)
	        VALUES ($1, $2, $3, 'issue', 101, 't', 'd', 'running', $4, $5, $6, 0)`,
		runID, userID, repoID, workerID, session, gen)
	return runID
}

// usageClaimGeneration reads run_usage.claim_generation for one leg (by lineage_epoch), plus
// whether the column is non-null. A run has few legs, so the epoch is a stable key.
func (e codexTestEnv) usageClaimGeneration(t *testing.T, runID uuid.UUID, epoch int32) (int64, bool) {
	t.Helper()
	var val *int64
	if err := e.pool.QueryRow(e.ctx,
		`SELECT claim_generation FROM run_usage WHERE run_id = $1 AND lineage_epoch = $2`, runID, epoch).Scan(&val); err != nil {
		t.Fatalf("read run_usage.claim_generation (epoch %d): %v", epoch, err)
	}
	if val == nil {
		return 0, false
	}
	return *val, true
}

func initFrame(seq int32) IncomingMessage {
	return IncomingMessage{Seq: seq, Kind: "status", Payload: json.RawMessage(`{"event":"init"}`)}
}

func resultFrame(seq int32, model string, input int64) IncomingMessage {
	p := map[string]any{
		"event": "result",
		"modelUsage": map[string]any{
			model: map[string]any{"inputTokens": input, "outputTokens": 1, "costUSD": 0.0, "costStatus": "metered"},
		},
	}
	raw, _ := json.Marshal(p)
	return IncomingMessage{Seq: seq, Kind: "status", Payload: raw}
}

// The COALESCE guard (task b, a mutation seam): a leg's established non-null claim_generation is
// NEVER clobbered by a later same-leg frame carrying a DIFFERENT generation, while a NULL existing
// value adopts the incoming one. Exercised directly through UpsertRunUsage so the SQL is the subject.
func TestUpsertRunUsageClaimGenerationCoalesceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedFoldRun(t, userID, workerID, repoID, 1, "sess-coalesce")

	gen1 := int64(1)
	gen2 := int64(2)
	base := store.UpsertRunUsageParams{
		RunID: runID, SessionID: "sess-coalesce", Model: "claude-test", LineageEpoch: 0,
		InputTokens: 10, OutputTokens: 1, Harness: "claude", CostStatus: "metered", CostUsd: numericUSD(0), UsageBasis: "per_leg",
	}

	// First frame at generation 1 → the row adopts 1.
	first := base
	first.ClaimGeneration = pgconv.Int8Ptr(&gen1)
	if err := env.q.UpsertRunUsage(env.ctx, first); err != nil {
		t.Fatalf("upsert #1: %v", err)
	}
	// A later same-leg frame at generation 2 → COALESCE keeps the established 1 (NOT clobbered).
	second := base
	second.InputTokens = 99 // GREATEST still advances the token counts; provenance must not move
	second.ClaimGeneration = pgconv.Int8Ptr(&gen2)
	if err := env.q.UpsertRunUsage(env.ctx, second); err != nil {
		t.Fatalf("upsert #2: %v", err)
	}
	if g, ok := env.usageClaimGeneration(t, runID, 0); !ok || g != 1 {
		t.Fatalf("claim_generation = %d (ok=%v), want 1 (established provenance not clobbered)", g, ok)
	}

	// A NULL existing value adopts the incoming generation (COALESCE(NULL, EXCLUDED)).
	nullFirst := base
	nullFirst.LineageEpoch = 1
	nullFirst.ClaimGeneration = pgconv.Int8Ptr(nil) // NULL
	if err := env.q.UpsertRunUsage(env.ctx, nullFirst); err != nil {
		t.Fatalf("upsert null #1: %v", err)
	}
	adopt := nullFirst
	adopt.ClaimGeneration = pgconv.Int8Ptr(&gen2)
	if err := env.q.UpsertRunUsage(env.ctx, adopt); err != nil {
		t.Fatalf("upsert null #2: %v", err)
	}
	if g, ok := env.usageClaimGeneration(t, runID, 1); !ok || g != 2 {
		t.Fatalf("null-then-2 claim_generation = %d (ok=%v), want 2 (adopted)", g, ok)
	}
}

// The incremental fold stamps the batch's fenced generation onto both run_messages (task a) and
// run_usage (task b), and a FULL refold preserves each frame's persisted generation — so a leg
// produced under generation 1 (token A) stays attributed to 1 after an A→B switch and refold, never
// relabelled to the run's latest generation.
func TestUsageFoldPerGenerationAndRefoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	runID := env.seedFoldRun(t, userID, workerID, repoID, 1, "sess-fold")
	wkr := store.Worker{ID: workerID, UserID: userID, Status: "online"}

	svc := New(env.q, env.box, testParams())
	gen1, gen2 := int64(1), int64(2)

	// Generation-1 leg: init (seq 1) + result (seq 2). lineage_epoch(seq 2) = 1 init before it = 1.
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{initFrame(1), resultFrame(2, "claude-test", 10)}, &gen1); err != nil {
		t.Fatalf("append gen-1 batch: %v", err)
	}
	// Reclaim onto token B: the run advances to generation 2 (a new claim). claim_released_at stays
	// NULL (ClaimRun clears it), so the next fenced batch lands.
	env.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, runID)
	// Generation-2 leg: init (seq 3) + result (seq 4). lineage_epoch(seq 4) = 2 inits before it = 2.
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, runID, []IncomingMessage{initFrame(3), resultFrame(4, "claude-test", 20)}, &gen2); err != nil {
		t.Fatalf("append gen-2 batch: %v", err)
	}

	// Incremental attribution: epoch-1 leg on generation 1, epoch-2 leg on generation 2.
	if g, ok := env.usageClaimGeneration(t, runID, 1); !ok || g != 1 {
		t.Fatalf("epoch-1 leg claim_generation = %d (ok=%v), want 1", g, ok)
	}
	if g, ok := env.usageClaimGeneration(t, runID, 2); !ok || g != 2 {
		t.Fatalf("epoch-2 leg claim_generation = %d (ok=%v), want 2", g, ok)
	}
	// The frames themselves carry their originating generation (task a).
	var msgGen1, msgGen2 int64
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation FROM run_messages WHERE run_id = $1 AND seq = 2`, runID).Scan(&msgGen1); err != nil {
		t.Fatalf("read seq-2 generation: %v", err)
	}
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation FROM run_messages WHERE run_id = $1 AND seq = 4`, runID).Scan(&msgGen2); err != nil {
		t.Fatalf("read seq-4 generation: %v", err)
	}
	if msgGen1 != 1 || msgGen2 != 2 {
		t.Fatalf("frame generations = %d/%d, want 1/2", msgGen1, msgGen2)
	}

	// A FULL refold re-derives every leg from the persisted frames and their generations. Even
	// though the run's latest generation is now 2, the epoch-1 leg must STAY on generation 1.
	run, err := env.q.GetRunByID(env.ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if err := RefoldRunUsage(env.ctx, env.pool, env.q, run); err != nil {
		t.Fatalf("RefoldRunUsage: %v", err)
	}
	if g, ok := env.usageClaimGeneration(t, runID, 1); !ok || g != 1 {
		t.Fatalf("after refold, epoch-1 leg claim_generation = %d (ok=%v), want 1 (old spend stays on A)", g, ok)
	}
	if g, ok := env.usageClaimGeneration(t, runID, 2); !ok || g != 2 {
		t.Fatalf("after refold, epoch-2 leg claim_generation = %d (ok=%v), want 2", g, ok)
	}
}
