package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCodexChatClaimFailsClosedLiveDB proves the PRD #1332 M5A (D3) UNCONDITIONAL fail-closed on
// the chat claim lane: ClaimChatRun must NEVER claim a chat run carrying ANY Codex indicator
// (harness='codex', codex_material_revision, or codex_secret_id). Chat does NOT implement Codex
// until M5B and — UNLIKE the run lane's D3 clause — there is no worker capability that authorizes
// it, so the guard is a plain, capability-less predicate. An ordinary Claude chat run is still
// claimed normally.
//
// The runs_codex_harness_coherence_check (migration 00226, enforced on write) forces each Codex
// sentinel to co-occur with harness='codex', so every Codex-indicating row seeded here is also
// harness='codex' by construction — a codex_material_revision / codex_secret_id row cannot exist
// with harness='claude'.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via e2e/run-store-it.sh).
//
// Calibration: drop the "AND NOT (r.harness = 'codex' OR ...)" clause from ClaimChatRun and each
// Codex sub-case's pgx.ErrNoRows / stays-queued assertion reddens (the run would be claimed).
func TestCodexChatClaimFailsClosedLiveDB(t *testing.T) {
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

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("codexchat-%s@e2e", userID))
	worker := createWorker(ctx, t, pool, userID)

	claim := func() (store.Run, error) {
		return q.ClaimChatRun(ctx, store.ClaimChatRunParams{
			WorkerID:       pgtype.UUID{Bytes: worker, Valid: true},
			UserID:         userID,
			AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		})
	}
	statusOf := func(runID uuid.UUID) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&s); err != nil {
			t.Fatalf("read status (%s): %v", runID, err)
		}
		return s
	}

	// (c) below needs a real user_secrets FK row for codex_secret_id (runs_codex_secret_fk).
	secretID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext) VALUES ($1, $2, 'codex_auth', 'default', $3)`,
		secretID, userID, []byte{0x1})

	// Three Codex-indicating queued chat rows, one per binding fact. Each carries harness='codex'
	// (the coherence CHECK forces it) with NULL repo_id/issue_iid/branch (runs_kind_shape for chat):
	//   (a) harness='codex' alone; (b) + codex_material_revision; (c) + codex_secret_id.
	harnessOnly := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, harness)
		 VALUES ($1, $2, 'chat', 't', 'd', 'queued', 'codex')`, harnessOnly, userID)
	withMaterial := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, harness, codex_material_revision)
		 VALUES ($1, $2, 'chat', 't', 'd', 'queued', 'codex', 7)`, withMaterial, userID)
	withSecret := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, harness, codex_secret_id)
		 VALUES ($1, $2, 'chat', 't', 'd', 'queued', 'codex', $3)`, withSecret, userID, secretID)

	codexRuns := map[string]uuid.UUID{
		"harness=codex":           harnessOnly,
		"codex_material_revision": withMaterial,
		"codex_secret_id":         withSecret,
	}

	// With ONLY Codex-indicating chats queued, the claim lane must find nothing claimable: if ANY
	// were claimable the claim would return it rather than pgx.ErrNoRows.
	if _, err := claim(); err != pgx.ErrNoRows {
		t.Fatalf("ClaimChatRun over only Codex-indicating chats must be idle (pgx.ErrNoRows), got %v", err)
	}
	// And every Codex row must remain queued — asserted per binding fact.
	for name, runID := range codexRuns {
		if s := statusOf(runID); s != "queued" {
			t.Fatalf("Codex-indicating chat (%s) status = %q after ClaimChatRun, want 'queued' (must never be claimed)", name, s)
		}
	}

	// Control: an ordinary Claude chat run (CreateChatRun writes harness='claude') IS claimed, and
	// being the only claimable chat it is returned exactly, while the Codex rows stay untouched.
	claude, err := q.CreateChatRun(ctx, store.CreateChatRunParams{
		RunID: uuid.New(), UserID: userID, IssueTitle: "ordinary chat", IssueDescription: "hi",
		Title: pgtype.Text{String: "ordinary chat", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateChatRun (Claude control): %v", err)
	}
	claimed, err := claim()
	if err != nil {
		t.Fatalf("ClaimChatRun (Claude control) must claim the Claude chat, got err %v", err)
	}
	if claimed.ID != claude.ID || claimed.Kind != "chat" {
		t.Fatalf("claim returned %s (kind %q), want the Claude chat run %s", claimed.ID, claimed.Kind, claude.ID)
	}
	// The Codex rows are STILL queued after a successful Claude claim.
	for name, runID := range codexRuns {
		if s := statusOf(runID); s != "queued" {
			t.Fatalf("Codex-indicating chat (%s) status = %q after the Claude claim, want 'queued'", name, s)
		}
	}
}

// TestClaimChatRunCodexGuardNamesAllIndicators closes the discrimination gap the live schema
// necessarily has: runs_codex_harness_coherence_check forces either binding sentinel to coexist
// with harness='codex', so a LiveDB fixture cannot isolate those two SQL arms. Pin the source query's
// complete defense-in-depth clause; sqlc's generated-code no-drift gate carries it to execution.
func TestClaimChatRunCodexGuardNamesAllIndicators(t *testing.T) {
	src, err := os.ReadFile("queries/chat.sql")
	if err != nil {
		t.Fatalf("read chat query source: %v", err)
	}
	const want = "AND NOT (r.harness = 'codex' OR r.codex_material_revision IS NOT NULL OR r.codex_secret_id IS NOT NULL)"
	if !strings.Contains(string(src), want) {
		t.Fatalf("ClaimChatRun must gate all three Codex indicators with the exact fail-closed clause %q", want)
	}
}
