package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestChatRunsLiveDB exercises the PRD #39 chat schema + queries against a REAL
// Postgres — the invariants the fake-store unit tests cannot cover: the reworked
// runs_kind_shape CHECK (chat ⇒ repo_id/issue_iid/branch all NULL, issue/ci_fix ⇒
// repo_id NOT NULL), the disjoint claim lanes (ClaimRun excludes chat, ClaimChatRun
// only chat), the resume-of context, the idle-chat sweep, and the proposal
// confirm/dismiss guards.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store
// e2e runner (e2e/run-store-it.sh) provides one.
func TestChatRunsLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("chat-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// ── shape: a valid chat run has NULL repo_id/issue_iid/branch + kind='chat' ──
	chat, err := q.CreateChatRun(ctx, store.CreateChatRunParams{
		UserID: userID, IssueTitle: "How does the plan gate work?",
		IssueDescription: "how does the plan-approval gate work?",
		Title:            pgtype.Text{String: "How does the plan gate work?", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}
	if chat.Kind != "chat" || chat.RepoID.Valid || chat.IssueIid.Valid || chat.Branch.Valid {
		t.Fatalf("chat run must be kind=chat with NULL repo_id/issue_iid/branch, got %+v", chat)
	}

	// ── CHECK runs_kind_shape: chat with any of repo/issue/branch set → 23514 (use
	// the REAL repo id so the FK passes and the CHECK is what fires) ──
	if err := insertRunShape(ctx, pool, userID, "kind, repo_id", "'chat', '"+repoID.String()+"'"); !isCheckViolation(err) {
		t.Errorf("a chat run with a repo_id must violate runs_kind_shape, got %v", err)
	}
	if err := insertRunShape(ctx, pool, userID, "kind, issue_iid", "'chat', 7"); !isCheckViolation(err) {
		t.Errorf("a chat run with an issue_iid must violate runs_kind_shape, got %v", err)
	}
	if err := insertRunShape(ctx, pool, userID, "kind, branch", "'chat', 'agent/x'"); !isCheckViolation(err) {
		t.Errorf("a chat run with a branch must violate runs_kind_shape, got %v", err)
	}

	// ── CHECK runs_kind_shape: an issue/ci_fix run now REQUIRES repo_id (omit the
	// column so it is genuinely NULL — the zero UUID would trip the FK first) ──
	if err := insertRunShape(ctx, pool, userID, "kind, issue_iid", "'issue', 1"); !isCheckViolation(err) {
		t.Errorf("an issue run with NULL repo_id must violate runs_kind_shape, got %v", err)
	}
	if err := insertRunShape(ctx, pool, userID, "kind, pipeline_id, pipeline_ref", "'ci_fix', 1, 'main'"); !isCheckViolation(err) {
		t.Errorf("a ci_fix run with NULL repo_id must violate runs_kind_shape, got %v", err)
	}

	// ── claim lanes are disjoint: an issue run + the chat run above ──
	issueRun, err := q.CreateRun(ctx, store.CreateRunParams{
		UserID: userID, RepoID: repoID,
		IssueIid: pgtype.Int8{Int64: 42, Valid: true}, IssueTitle: "iss", IssueDescription: "d",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	worker := createWorker(ctx, t, pool, userID)
	claimParams := func() store.ClaimRunParams {
		return store.ClaimRunParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: userID, AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true}}
	}
	// The RUN lane claims the issue run, NEVER the chat run.
	claimedRun, err := q.ClaimRun(ctx, claimParams())
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimedRun.ID != issueRun.ID || claimedRun.Kind != "issue" {
		t.Fatalf("run lane claimed %s (kind %q), want the issue run", claimedRun.ID, claimedRun.Kind)
	}
	// A second run-lane claim is idle: the only remaining queued run is the chat.
	if _, err := q.ClaimRun(ctx, claimParams()); err != pgx.ErrNoRows {
		t.Fatalf("run lane must not claim a chat run; got %v", err)
	}
	// The CHAT lane claims the chat run.
	claimedChat, err := q.ClaimChatRun(ctx, store.ClaimChatRunParams{WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: userID, AffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true}})
	if err != nil {
		t.Fatalf("ClaimChatRun: %v", err)
	}
	if claimedChat.ID != chat.ID || claimedChat.Kind != "chat" {
		t.Fatalf("chat lane claimed %s (kind %q), want the chat run", claimedChat.ID, claimedChat.Kind)
	}

	// ── Continue + resume context: a Continue run carries resume_of + the prior
	// run's session, and worker_id for affinity ──
	mustExec(ctx, t, pool, `UPDATE runs SET session_id = 'sess-prior', status = 'completed', finished_at = now() WHERE id = $1`, chat.ID)
	cont, err := q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{
		UserID: userID, IssueTitle: chat.IssueTitle,
		Title:         pgtype.Text{String: "How does the plan gate work?", Valid: true},
		ResumeOfRunID: pgtype.UUID{Bytes: chat.ID, Valid: true},
		WorkerID:      pgtype.UUID{Bytes: worker, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateChatContinueRun: %v", err)
	}
	if cont.IssueDescription != "" {
		t.Errorf("a Continue run seeds no new prompt, got %q", cont.IssueDescription)
	}
	resumeSession, err := q.GetChatRunClaimContext(ctx, cont.ID)
	if err != nil {
		t.Fatalf("GetChatRunClaimContext: %v", err)
	}
	if !resumeSession.Valid || resumeSession.String != "sess-prior" {
		t.Fatalf("Continue claim must carry the prior run's session, got %+v", resumeSession)
	}

	// ── turn count: CountChatFollowUps counts only follow_up inputs ──
	freshChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{UserID: userID, IssueTitle: "t", IssueDescription: "hi", Title: pgtype.Text{String: "t", Valid: true}})
	for i := 0; i < 3; i++ {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{RunID: freshChat.ID, Kind: "follow_up", Body: pgtype.Text{String: "m", Valid: true}}); err != nil {
			t.Fatalf("CreateRunInput: %v", err)
		}
	}
	if n, err := q.CountChatFollowUps(ctx, freshChat.ID); err != nil || n != 3 {
		t.Fatalf("CountChatFollowUps = %d, %v; want 3", n, err)
	}

	// ── idle sweep: a claimed chat with an OLD message completes; a recent one and
	// a message-less queued chat do not ──
	idleChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{UserID: userID, IssueTitle: "idle", IssueDescription: "x", Title: pgtype.Text{String: "idle", Valid: true}})
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'running' WHERE id = $1`, idleChat.ID)
	mustExec(ctx, t, pool, `INSERT INTO run_messages (run_id, seq, kind, payload, created_at) VALUES ($1, 1, 'text', '{}', now() - interval '2 hours')`, idleChat.ID)

	activeChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{UserID: userID, IssueTitle: "active", IssueDescription: "x", Title: pgtype.Text{String: "active", Valid: true}})
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'running' WHERE id = $1`, activeChat.ID)
	mustExec(ctx, t, pool, `INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', '{}')`, activeChat.ID)

	swept, err := q.SweepIdleChatRuns(ctx, pgtype.Timestamptz{Time: time.Now().Add(-70 * time.Minute), Valid: true})
	if err != nil {
		t.Fatalf("SweepIdleChatRuns: %v", err)
	}
	if len(swept) != 1 || swept[0].ID != idleChat.ID {
		t.Fatalf("idle sweep must complete exactly the old-message chat, got %v", swept)
	}

	// ── proposals: user-scoped lookup + confirm/dismiss idempotency guards ──
	propID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO issue_proposals (id, run_id, repo_id, title, description) VALUES ($1, $2, $3, 'Add a job', 'desc')`,
		propID, freshChat.ID, repoID)

	// Owner sees it; a foreign user does not.
	if _, err := q.GetChatProposalForConfirm(ctx, store.GetChatProposalForConfirmParams{ID: propID, RunID: freshChat.ID, UserID: userID}); err != nil {
		t.Fatalf("owner GetChatProposalForConfirm: %v", err)
	}
	if _, err := q.GetChatProposalForConfirm(ctx, store.GetChatProposalForConfirmParams{ID: propID, RunID: freshChat.ID, UserID: uuid.New()}); err != pgx.ErrNoRows {
		t.Fatalf("a foreign user must not read another user's proposal, got %v", err)
	}

	// Confirm once stamps the iid; a second confirm touches nothing.
	if _, err := q.MarkProposalConfirmed(ctx, store.MarkProposalConfirmedParams{ID: propID, CreatedIssueIid: pgtype.Int8{Int64: 99, Valid: true}}); err != nil {
		t.Fatalf("MarkProposalConfirmed: %v", err)
	}
	if _, err := q.MarkProposalConfirmed(ctx, store.MarkProposalConfirmedParams{ID: propID, CreatedIssueIid: pgtype.Int8{Int64: 100, Valid: true}}); err != pgx.ErrNoRows {
		t.Fatalf("a second confirm must be a no-op (already resolved), got %v", err)
	}
	// Dismiss on an already-resolved proposal is a no-op.
	if _, err := q.MarkProposalDismissed(ctx, propID); err != pgx.ErrNoRows {
		t.Fatalf("dismiss of a confirmed proposal must be a no-op, got %v", err)
	}
}

// insertRunShape inserts a run with a caller-supplied set of extra columns/values
// (as SQL literals) to probe the runs_kind_shape CHECK, returning the DB error.
// repo_id is only present when the caller names it, so an issue/ci_fix probe that
// omits it inserts a genuinely NULL repo_id (the CHECK, not the FK, must fire).
func insertRunShape(ctx context.Context, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, userID uuid.UUID, extraCols, extraVals string) error {
	sql := fmt.Sprintf(
		`INSERT INTO runs (user_id, issue_title, issue_description, %s) VALUES ($1, 't', 'd', %s)`, extraCols, extraVals)
	_, err := pool.Exec(ctx, sql, userID)
	return err
}

// createWorker inserts an online worker for the user and returns its id.
func createWorker(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
		 VALUES ($1, $2, 'w', $3, 'online', now())`, id, userID, []byte(id.String()))
	return id
}
