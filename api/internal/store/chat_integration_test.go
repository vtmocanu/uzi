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

	"github.com/vtmocanu/uzi/api/internal/store"
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
		RunID: uuid.New(), UserID: userID, IssueTitle: "How does the plan gate work?",
		IssueDescription: "how does the plan-approval gate work?",
		Title:            pgtype.Text{String: "How does the plan gate work?", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}
	if chat.Kind != "chat" || chat.RepoID.Valid || chat.IssueIid.Valid || chat.Branch.Valid {
		t.Fatalf("chat run must be kind=chat with NULL repo_id/issue_iid/branch, got %+v", chat)
	}
	// The create atomically seeds the first message as a follow_up input (pinned M4
	// contract) — so a fresh chat already has exactly one persisted turn.
	if n, err := q.CountChatFollowUps(ctx, chat.ID); err != nil || n != 1 {
		t.Fatalf("CreateChatRun must seed the first message as a follow_up; CountChatFollowUps = %d, %v; want 1", n, err)
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
		IssueIid: pgtype.Int8{Int64: 42, Valid: true}, IssueTitle: "iss", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
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

	// ── turn count: CountChatFollowUps counts every follow_up input, including the
	// seeded initial prompt (1) plus 3 later turns → 4 ──
	freshChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{RunID: uuid.New(), UserID: userID, IssueTitle: "t", IssueDescription: "hi", Title: pgtype.Text{String: "t", Valid: true}})
	for i := 0; i < 3; i++ {
		if _, err := q.CreateRunInput(ctx, store.CreateRunInputParams{RunID: freshChat.ID, Kind: "follow_up", Body: pgtype.Text{String: "m", Valid: true}}); err != nil {
			t.Fatalf("CreateRunInput: %v", err)
		}
	}
	if n, err := q.CountChatFollowUps(ctx, freshChat.ID); err != nil || n != 4 {
		t.Fatalf("CountChatFollowUps = %d, %v; want 4 (seeded initial + 3)", n, err)
	}

	// ── idle sweep: a claimed chat with an OLD message completes; a recent one and
	// a message-less queued chat do not ──
	idleChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{RunID: uuid.New(), UserID: userID, IssueTitle: "idle", IssueDescription: "x", Title: pgtype.Text{String: "idle", Valid: true}})
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'running' WHERE id = $1`, idleChat.ID)
	mustExec(ctx, t, pool, `INSERT INTO run_messages (run_id, seq, kind, payload, created_at) VALUES ($1, 1, 'text', '{}', now() - interval '2 hours')`, idleChat.ID)

	activeChat, _ := q.CreateChatRun(ctx, store.CreateChatRunParams{RunID: uuid.New(), UserID: userID, IssueTitle: "active", IssueDescription: "x", Title: pgtype.Text{String: "active", Valid: true}})
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'running' WHERE id = $1`, activeChat.ID)
	mustExec(ctx, t, pool, `INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'text', '{}')`, activeChat.ID)

	swept, err := q.SweepIdleChatRuns(ctx, pgtype.Timestamptz{Time: time.Now().Add(-70 * time.Minute), Valid: true})
	if err != nil {
		t.Fatalf("SweepIdleChatRuns: %v", err)
	}
	if len(swept) != 1 || swept[0].ID != idleChat.ID {
		t.Fatalf("idle sweep must complete exactly the old-message chat, got %v", swept)
	}

	// ── list DTO: turn_count, last_message_at, and last-activity ordering ──
	list, err := q.ListChatRunsForUser(ctx, userID)
	if err != nil {
		t.Fatalf("ListChatRunsForUser: %v", err)
	}
	byID := map[uuid.UUID]store.ListChatRunsForUserRow{}
	for _, row := range list {
		byID[row.ID] = row
	}
	if got := byID[freshChat.ID].TurnCount; got != 4 {
		t.Fatalf("freshChat turn_count = %d, want 4 (seeded + 3)", got)
	}
	if !byID[activeChat.ID].LastMessageAt.Valid {
		t.Fatalf("activeChat has a message, so last_message_at must be set")
	}
	if byID[freshChat.ID].LastMessageAt.Valid {
		t.Fatalf("freshChat has no run_messages, so last_message_at must be NULL")
	}
	// activeChat's message is the newest activity across all the user's chats, so it
	// sorts first (last-activity DESC, message-less chats falling back to created_at).
	if len(list) == 0 || list[0].ID != activeChat.ID {
		t.Fatalf("chat list must sort by last activity (activeChat first), got first=%v", list[0].ID)
	}

	// ── proposals (M3): creation + per-run count + claim-first confirm + revert ──
	p1, err := q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{
		RunID: freshChat.ID, RepoID: repoID, Title: "Add a job", Description: "desc", Labels: []byte(`["enhancement"]`),
	})
	if err != nil {
		t.Fatalf("CreateIssueProposal: %v", err)
	}
	if p1.Status != "pending" {
		t.Fatalf("a new proposal must be pending, got %q", p1.Status)
	}
	if n, err := q.CountPendingProposalsForRun(ctx, freshChat.ID); err != nil || n != 1 {
		t.Fatalf("CountPendingProposalsForRun = %d, %v; want 1", n, err)
	}

	// A foreign user cannot claim it (user-scoped through the chat run).
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p1.ID, RunID: freshChat.ID, UserID: uuid.New()}); err != pgx.ErrNoRows {
		t.Fatalf("a foreign user must not claim another user's proposal, got %v", err)
	}

	// Claim-first: the FIRST claim wins (pending -> confirming) and returns the draft;
	// a second (concurrent) claim matches no pending row -> the confirm-side race guard.
	claim, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p1.ID, RunID: freshChat.ID, UserID: userID})
	if err != nil {
		t.Fatalf("ClaimProposalForConfirm: %v", err)
	}
	if claim.Title != "Add a job" || claim.RepoID != repoID {
		t.Fatalf("claim must return the draft + repo, got %+v", claim)
	}
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p1.ID, RunID: freshChat.ID, UserID: userID}); err != pgx.ErrNoRows {
		t.Fatalf("a second claim must match no pending row (claim-first race guard), got %v", err)
	}
	// A claimed (confirming) proposal no longer counts as pending.
	if n, _ := q.CountPendingProposalsForRun(ctx, freshChat.ID); n != 0 {
		t.Fatalf("a claimed (confirming) proposal must not count as pending, got %d", n)
	}
	// Settle confirming -> confirmed; then it can't be claimed, reverted, or dismissed.
	if _, err := q.MarkProposalConfirmed(ctx, store.MarkProposalConfirmedParams{ID: p1.ID, CreatedIssueIid: pgtype.Int8{Int64: 99, Valid: true}}); err != nil {
		t.Fatalf("MarkProposalConfirmed from confirming: %v", err)
	}
	if n, err := q.RevertProposalToPending(ctx, p1.ID); err != nil || n != 0 {
		t.Fatalf("revert of a confirmed proposal must touch 0 rows, got n=%d err=%v", n, err)
	}
	if _, err := q.MarkProposalDismissed(ctx, p1.ID); err != pgx.ErrNoRows {
		t.Fatalf("dismiss of a confirmed proposal must be a no-op, got %v", err)
	}

	// Revert path: a claimed proposal reverts to pending and is claimable again (the
	// forge-failure retry path).
	p2, _ := q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{RunID: freshChat.ID, RepoID: repoID, Title: "t2", Description: "d2", Labels: []byte(`[]`)})
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p2.ID, RunID: freshChat.ID, UserID: userID}); err != nil {
		t.Fatalf("claim p2: %v", err)
	}
	if n, err := q.RevertProposalToPending(ctx, p2.ID); err != nil || n != 1 {
		t.Fatalf("revert of a confirming proposal must touch 1 row, got n=%d err=%v", n, err)
	}
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p2.ID, RunID: freshChat.ID, UserID: userID}); err != nil {
		t.Fatalf("a reverted proposal must be claimable again: %v", err)
	}

	// ── stuck-confirming recovery: a proposal stranded in 'confirming' past the
	// cutoff is reverted to pending; a freshly-claimed one (p2, confirming_since=now)
	// is not. Simulate a handler killed mid-flight by backdating confirming_since. ──
	p3, _ := q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{RunID: freshChat.ID, RepoID: repoID, Title: "t3", Description: "d3", Labels: []byte(`[]`)})
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p3.ID, RunID: freshChat.ID, UserID: userID}); err != nil {
		t.Fatalf("claim p3: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE issue_proposals SET confirming_since = now() - interval '10 minutes' WHERE id = $1`, p3.ID)
	recovered, err := q.SweepStuckConfirmingProposals(ctx, pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true})
	if err != nil {
		t.Fatalf("SweepStuckConfirmingProposals: %v", err)
	}
	if len(recovered) != 1 || recovered[0] != p3.ID {
		t.Fatalf("stuck sweep must recover exactly the backdated proposal (not the fresh p2), got %v", recovered)
	}
	// The recovered proposal is pending again → claimable.
	if _, err := q.ClaimProposalForConfirm(ctx, store.ClaimProposalForConfirmParams{ID: p3.ID, RunID: freshChat.ID, UserID: userID}); err != nil {
		t.Fatalf("a recovered proposal must be claimable again: %v", err)
	}

	// ── worker chat reads (M3): user_id-scoped, foreign id -> no row ──
	wruns, err := q.ListRunsForWorkerUser(ctx, store.ListRunsForWorkerUserParams{UserID: userID, Lim: 50})
	if err != nil {
		t.Fatalf("ListRunsForWorkerUser: %v", err)
	}
	if len(wruns) == 0 {
		t.Fatalf("worker run list must include the user's runs")
	}
	if foreign, _ := q.ListRunsForWorkerUser(ctx, store.ListRunsForWorkerUserParams{UserID: uuid.New(), Lim: 50}); len(foreign) != 0 {
		t.Fatalf("a foreign user's worker run list must be empty, got %d", len(foreign))
	}
	if _, err := q.GetRunForWorkerUser(ctx, store.GetRunForWorkerUserParams{ID: issueRun.ID, UserID: userID}); err != nil {
		t.Fatalf("GetRunForWorkerUser(own issue run): %v", err)
	}
	if _, err := q.GetRunForWorkerUser(ctx, store.GetRunForWorkerUserParams{ID: issueRun.ID, UserID: uuid.New()}); err != pgx.ErrNoRows {
		t.Fatalf("GetRunForWorkerUser for a foreign user must be no row, got %v", err)
	}
	// activeChat has one run_message; the bounded page returns it.
	page, err := q.ListRunMessagesForWorkerPage(ctx, store.ListRunMessagesForWorkerPageParams{RunID: activeChat.ID, AfterSeq: 0, Lim: 200})
	if err != nil || len(page) != 1 {
		t.Fatalf("ListRunMessagesForWorkerPage = %d msgs, %v; want 1", len(page), err)
	}

	// ── repo path resolution (M3): the proposal endpoint resolves repo_path -> id,
	// user-scoped (the agent never sees internal UUIDs) ──
	if gotID, err := q.GetRepoIDByPathForUser(ctx, store.GetRepoIDByPathForUserParams{Path: "g/r", UserID: userID}); err != nil || gotID != repoID {
		t.Fatalf("GetRepoIDByPathForUser(own repo) = %v, %v; want %v", gotID, err, repoID)
	}
	if _, err := q.GetRepoIDByPathForUser(ctx, store.GetRepoIDByPathForUserParams{Path: "g/r", UserID: uuid.New()}); err != pgx.ErrNoRows {
		t.Fatalf("a foreign user must not resolve another user's repo path, got %v", err)
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
