package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestClaimChatSucceedsWithoutForgeConnection: a chat claim assembles from the
// Anthropic token alone (Decision 12) — no repo, no forge connection, no bot PAT —
// so a user with a token but no forge configured can still chat. assembleChatClaim
// must never touch the repo/forge join (GetRunClaimContext), only openAnthropic.
func TestClaimChatSucceedsWithoutForgeConnection(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-chat-token-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{
		chatClaimRun: store.Run{
			ID: uuid.New(), UserID: uid, Kind: RunKindChat, Status: "claimed",
			Title:            pgtype.Text{String: "How does the plan gate work?", Valid: true},
			IssueDescription: "how does the plan-approval gate work?",
		},
		anthropic: sealedTok,
	}
	svc := New(fs, box, testParams())

	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("ClaimChat: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a chat claim payload for a user with an Anthropic token and no forge connection")
	}
	if payload.Kind != RunKindChat {
		t.Errorf("Kind = %q, want chat", payload.Kind)
	}
	if payload.Secrets.AnthropicOAuthToken != "anthropic-chat-token-abcdef1234567890" {
		t.Errorf("chat claim must carry the decrypted Anthropic token")
	}
	if payload.Prompt != "how does the plan-approval gate work?" {
		t.Errorf("Prompt = %q, want the first message", payload.Prompt)
	}
	if payload.Config.MaxTurns != 50 {
		t.Errorf("Config.MaxTurns = %d, want 50 (delivered so the worker enforces the same cap)", payload.Config.MaxTurns)
	}
}

// TestClaimChatWireOmitsForgePAT: the chat claim JSON must contain NO
// forge_pat/forge_username key (Decision 9). The guarantee is structural — the
// ChatClaimSecrets type has no such field — so this marshals a real assembled
// payload and asserts the keys are absent while the Anthropic token IS present.
func TestClaimChatWireOmitsForgePAT(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-nopat-token-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{
		chatClaimRun: store.Run{ID: uuid.New(), UserID: uid, Kind: RunKindChat, Status: "claimed", Title: pgtype.Text{String: "t", Valid: true}},
		anthropic:    sealedTok,
	}
	svc := New(fs, box, testParams())
	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("ClaimChat: payload=%v err=%v", payload, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	if strings.Contains(js, "forge_pat") || strings.Contains(js, "forge_username") {
		t.Fatalf("chat claim wire must contain no forge credential key, got: %s", js)
	}
	if !strings.Contains(js, "anthropic_oauth_token") {
		t.Fatalf("chat claim wire must carry the anthropic token, got: %s", js)
	}
}

// TestClaimChatUsesChatLane: ClaimChat claims via ClaimChatRun (the chat lane),
// never ClaimRun (the run lane) — the service half of the lane isolation the SQL
// predicates enforce (the DB-level isolation is covered by the LiveDB test).
func TestClaimChatUsesChatLane(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-lane-token-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{
		chatClaimRun: store.Run{ID: uuid.New(), UserID: uid, Kind: RunKindChat, Status: "claimed", Title: pgtype.Text{String: "t", Valid: true}},
		anthropic:    sealedTok,
	}
	svc := New(fs, box, testParams())
	if _, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uid}); err != nil {
		t.Fatalf("ClaimChat: %v", err)
	}
	if fs.chatClaimParams == nil {
		t.Fatal("ClaimChat must call ClaimChatRun (the chat lane)")
	}
	if fs.claimParams != nil {
		t.Fatal("ClaimChat must NOT call ClaimRun (the run lane)")
	}
}

// TestClaimChatIdleWhenQueueEmpty: no queued chat → nil payload, nil error (idle).
func TestClaimChatIdleWhenQueueEmpty(t *testing.T) {
	fs := &fakeStore{chatClaimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uuid.New()})
	if err != nil || payload != nil {
		t.Fatalf("empty chat queue must be idle, got payload=%v err=%v", payload, err)
	}
}

// TestClaimChatFailsWithoutAnthropicToken: a chat claim for a user with no
// Anthropic token is failed (terminal, not retryable) and reported idle, so a
// tokenless chat never wedges the worker's chat loop.
func TestClaimChatFailsWithoutAnthropicToken(t *testing.T) {
	runID := uuid.New()
	fs := &fakeStore{
		chatClaimRun: store.Run{ID: runID, UserID: uuid.New(), Kind: RunKindChat, Status: "claimed"},
		anthropicErr: pgx.ErrNoRows, // no token configured
	}
	svc := New(fs, newBox(t), testParams())
	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uuid.New()})
	if err != nil {
		t.Fatalf("ClaimChat should report idle (nil err) after failing the run, got %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle (nil payload) after failing a tokenless chat")
	}
	if fs.markedFailed == nil || fs.markedFailed.ID != runID {
		t.Fatalf("a tokenless chat run must be failed, markedFailed=%v", fs.markedFailed)
	}
}

// TestSubmitChatMessageEnforcesTurnCap: at CHAT_MAX_TURNS persisted follow-ups the
// server rejects a further message (Decision 3), and below the cap it enqueues a
// follow_up on the steering wire.
func TestSubmitChatMessageEnforcesTurnCap(t *testing.T) {
	uid, runID := uuid.New(), uuid.New()
	chatRun := store.Run{ID: runID, UserID: uid, Kind: RunKindChat, Status: "running"}

	// At the cap → rejected, nothing enqueued.
	atCap := &fakeStore{runByID: chatRun, followUpCount: 50}
	svc := New(atCap, newBox(t), testParams())
	if _, err := svc.SubmitChatMessage(context.Background(), uid, runID, "one more?"); !errors.Is(err, ErrChatTurnCapReached) {
		t.Fatalf("at the turn cap err = %v, want ErrChatTurnCapReached", err)
	}
	if atCap.createdInput != nil {
		t.Fatal("a capped chat must not enqueue the follow-up")
	}

	// Below the cap → enqueued as a follow_up.
	below := &fakeStore{runByID: chatRun, followUpCount: 3}
	svc = New(below, newBox(t), testParams())
	if _, err := svc.SubmitChatMessage(context.Background(), uid, runID, "keep going"); err != nil {
		t.Fatalf("below the cap SubmitChatMessage: %v", err)
	}
	if below.createdInput == nil || below.createdInput.Kind != "follow_up" {
		t.Fatalf("below the cap must enqueue a follow_up, got %v", below.createdInput)
	}
}

// TestSubmitChatMessageRejectsTerminal: a terminal chat refuses new messages
// (ErrRunTerminal), which the client surfaces as Continue (Decision 11).
func TestSubmitChatMessageRejectsTerminal(t *testing.T) {
	uid, runID := uuid.New(), uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindChat, Status: "completed"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.SubmitChatMessage(context.Background(), uid, runID, "hi"); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("terminal chat message err = %v, want ErrRunTerminal", err)
	}
}

// TestGetChatRunRejectsNonChat: a non-chat run is not addressable through the chat
// surface (ErrRunNotFound), so /api/chats can never steer an issue/ci_fix run.
func TestGetChatRunRejectsNonChat(t *testing.T) {
	uid, runID := uuid.New(), uuid.New()
	fs := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindIssue, Status: "running"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.GetChatRun(context.Background(), uid, runID); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("GetChatRun on an issue run err = %v, want ErrRunNotFound", err)
	}
}

// TestDismissProposalNeverWritesForge: DismissProposal only flips status
// (MarkProposalDismissed); it holds no forge dependency at all (Decision 8 —
// dismissing provably writes nothing to the forge).
func TestDismissProposalNeverWritesForge(t *testing.T) {
	propID := uuid.New()
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	if err := svc.DismissProposal(context.Background(), propID); err != nil {
		t.Fatalf("DismissProposal: %v", err)
	}
	if fs.markedProposalDismiss == nil || *fs.markedProposalDismiss != propID {
		t.Fatalf("DismissProposal must call MarkProposalDismissed, got %v", fs.markedProposalDismiss)
	}
	if fs.markedProposalConfirm != nil {
		t.Fatal("DismissProposal must never confirm")
	}
}

// TestConfirmProposalRaceIsNotPending: a lost race (the guarded UPDATE touches 0
// rows → pgx.ErrNoRows) surfaces as ErrProposalNotPending, never re-recording.
func TestConfirmProposalRaceIsNotPending(t *testing.T) {
	fs := &fakeStore{proposalConfirmErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	if err := svc.ConfirmProposal(context.Background(), uuid.New(), 42); !errors.Is(err, ErrProposalNotPending) {
		t.Fatalf("raced confirm err = %v, want ErrProposalNotPending", err)
	}
}

// TestSweepCompletesIdleChats: the sweep completes chat runs the idle-chat query
// returns, fanning each transition out (the not-trusting-the-worker backstop).
func TestSweepCompletesIdleChats(t *testing.T) {
	fs := &fakeStore{
		sweptIdleChats: []store.SweepIdleChatRunsRow{
			{ID: uuid.New(), UserID: uuid.New(), Status: "completed"},
		},
	}
	svc := New(fs, newBox(t), testParams())
	res, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ChatIdleCompleted != 1 {
		t.Fatalf("ChatIdleCompleted = %d, want 1", res.ChatIdleCompleted)
	}
}
