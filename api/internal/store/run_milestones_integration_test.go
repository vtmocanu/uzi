package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestRunMilestonesLifecycleLiveDB pins PRD #122 M1's milestone columns against a
// REAL Postgres — the candidate/frozen write semantics the fake-store unit tests
// cannot exercise, because they live entirely in the SQL:
//
//   - SetRunAwaitingApproval writes milestones_candidate DIRECTLY (replaced each
//     round), leaving milestones_frozen NULL.
//   - CreateApprovePlanInput copies candidate → frozen at approve, idempotently.
//   - SetRunRunning writes milestones_frozen only via COALESCE, so a later report
//     can never overwrite a frozen list (autopilot immutability).
//   - A report with no milestones leaves the columns NULL (back-compat).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one); mirrors the other *_integration_test.go here.
func TestRunMilestonesLifecycleLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("milestones-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("milestones-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	// Column readers. NULL comes back as a nil []byte; a stored array as its jsonb text.
	milestones := func(id uuid.UUID, col string) []byte {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM runs WHERE id = $1`, col), id).Scan(&raw); err != nil {
			t.Fatalf("read %s of %s: %v", col, id, err)
		}
		return raw
	}

	candidateJSON := []byte(`[{"id": "m1", "title": "First"}, {"id": "m2", "title": "Second"}]`)

	// ── The human-gated freeze: awaiting_approval writes the candidate, approve copies
	//    it into the immutable frozen list. ──
	t.Run("candidate then approve freezes", func(t *testing.T) {
		gateRun := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, 1, 't', 'd', 'running', $4, 'issue')`, gateRun, userID, repoID, wkr.ID)

		rows, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: candidateJSON, ID: gateRun, WorkerID: workerID,
		})
		if err != nil || rows != 1 {
			t.Fatalf("SetRunAwaitingApproval: rows=%d err=%v", rows, err)
		}
		if got := milestones(gateRun, "milestones_candidate"); string(got) != `[{"id": "m1", "title": "First"}, {"id": "m2", "title": "Second"}]` {
			t.Fatalf("candidate stored = %s, want the exact list", got)
		}
		if got := milestones(gateRun, "milestones_frozen"); got != nil {
			t.Fatalf("frozen must be NULL before approve, got %s", got)
		}

		// A revision round REPLACES the candidate (direct assignment, not COALESCE).
		revised := []byte(`[{"id": "m1", "title": "Only one now"}]`)
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: revised, ID: gateRun, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunAwaitingApproval (revise): %v", err)
		}
		if got := milestones(gateRun, "milestones_candidate"); string(got) != `[{"id": "m1", "title": "Only one now"}]` {
			t.Fatalf("candidate after revise = %s, want the replaced list", got)
		}

		// Approve copies candidate → frozen.
		if _, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: gateRun, Body: pgtype.Text{String: "{}", Valid: true},
			AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
		}); err != nil {
			t.Fatalf("CreateApprovePlanInput: %v", err)
		}
		frozen := milestones(gateRun, "milestones_frozen")
		if string(frozen) != `[{"id": "m1", "title": "Only one now"}]` {
			t.Fatalf("frozen after approve = %s, want a copy of the candidate", frozen)
		}

		// A DOUBLE-approve does not change an already-frozen list (idempotent freeze),
		// even after the candidate moves again.
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			MilestonesCandidate: candidateJSON, ID: gateRun, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunAwaitingApproval (post-freeze revise): %v", err)
		}
		if _, err := q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
			RunID: gateRun, Body: pgtype.Text{String: "{}", Valid: true},
			AgentSource: pgtype.Text{String: "own", Valid: true}, AgentExclusions: []byte("[]"),
		}); err != nil {
			t.Fatalf("CreateApprovePlanInput (double approve): %v", err)
		}
		if got := milestones(gateRun, "milestones_frozen"); string(got) != string(frozen) {
			t.Fatalf("frozen changed on double-approve: got %s, want unchanged %s", got, frozen)
		}
	})

	// ── The autopilot freeze: SetRunRunning writes frozen via COALESCE, so the first
	//    report wins and a later one with a different list cannot overwrite it. ──
	t.Run("autopilot frozen is immutable", func(t *testing.T) {
		autoRun := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, 2, 't', 'd', 'claimed', $4, 'issue')`, autoRun, userID, repoID, wkr.ID)

		first := []byte(`[{"id": "a", "title": "Alpha"}]`)
		if rows, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			MilestonesFrozen: first, ID: autoRun, WorkerID: workerID,
		}); err != nil || rows != 1 {
			t.Fatalf("SetRunRunning (first): rows=%d err=%v", rows, err)
		}
		if got := milestones(autoRun, "milestones_frozen"); string(got) != `[{"id": "a", "title": "Alpha"}]` {
			t.Fatalf("frozen after first running report = %s", got)
		}

		// A second running report with a DIFFERENT list must not overwrite the frozen one.
		second := []byte(`[{"id": "b", "title": "Beta"}]`)
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{
			MilestonesFrozen: second, ID: autoRun, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunRunning (second): %v", err)
		}
		if got := milestones(autoRun, "milestones_frozen"); string(got) != `[{"id": "a", "title": "Alpha"}]` {
			t.Fatalf("frozen was overwritten by a later report: %s — COALESCE must keep the first", got)
		}

		// A heartbeat that carries no milestones (NULL param) leaves it untouched too.
		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: autoRun, WorkerID: workerID}); err != nil {
			t.Fatalf("SetRunRunning (heartbeat): %v", err)
		}
		if got := milestones(autoRun, "milestones_frozen"); string(got) != `[{"id": "a", "title": "Alpha"}]` {
			t.Fatalf("frozen disturbed by a milestone-less heartbeat: %s", got)
		}
	})

	// ── Back-compat: a run whose reports never carry milestones keeps both columns
	//    NULL and behaves exactly as a pre-feature run. ──
	t.Run("no milestones leaves both columns NULL", func(t *testing.T) {
		plainRun := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, 3, 't', 'd', 'claimed', $4, 'issue')`, plainRun, userID, repoID, wkr.ID)

		if _, err := q.SetRunRunning(ctx, store.SetRunRunningParams{ID: plainRun, WorkerID: workerID}); err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
		if _, err := q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{ID: plainRun, WorkerID: workerID}); err != nil {
			t.Fatalf("SetRunAwaitingApproval: %v", err)
		}
		if got := milestones(plainRun, "milestones_candidate"); got != nil {
			t.Fatalf("candidate = %s, want NULL for a milestone-less run", got)
		}
		if got := milestones(plainRun, "milestones_frozen"); got != nil {
			t.Fatalf("frozen = %s, want NULL for a milestone-less run", got)
		}
	})
}
