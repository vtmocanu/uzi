package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
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

// TestClaimProposalForConfirmDistinguishesNotFoundVsNotPending: the claim's 0-row
// path does one follow-up read to tell a genuinely absent/foreign proposal (404)
// from one that exists but is no longer pending (409).
func TestClaimProposalForConfirmDistinguishesNotFoundVsNotPending(t *testing.T) {
	uid, runID, propID := uuid.New(), uuid.New(), uuid.New()

	// Claim wins → returns the row.
	won := &fakeStore{claimProposalRow: store.ClaimProposalForConfirmRow{ID: propID, Title: "t"}}
	if _, err := New(won, newBox(t), testParams()).ClaimProposalForConfirm(context.Background(), uid, runID, propID); err != nil {
		t.Fatalf("winning claim: %v", err)
	}

	// Claim 0-row + follow-up read finds nothing → ErrProposalNotFound (404).
	gone := &fakeStore{claimProposalErr: pgx.ErrNoRows, getChatProposalErr: pgx.ErrNoRows}
	if _, err := New(gone, newBox(t), testParams()).ClaimProposalForConfirm(context.Background(), uid, runID, propID); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("absent proposal err = %v, want ErrProposalNotFound", err)
	}

	// Claim 0-row + follow-up read finds a (non-pending) row → ErrProposalNotPending (409).
	resolved := &fakeStore{claimProposalErr: pgx.ErrNoRows, getChatProposalRow: store.GetChatProposalForConfirmRow{ID: propID, Status: "confirmed"}}
	if _, err := New(resolved, newBox(t), testParams()).ClaimProposalForConfirm(context.Background(), uid, runID, propID); !errors.Is(err, ErrProposalNotPending) {
		t.Fatalf("resolved proposal err = %v, want ErrProposalNotPending", err)
	}
}

// TestCreateProposalGuards: proposal creation rejects a non-chat target, a terminal
// chat, and a run already at the per-run pending cap.
func TestCreateProposalGuards(t *testing.T) {
	uid, runID, repoID := uuid.New(), uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	labels := []byte(`[]`)

	// Non-chat target → ErrProposalRunNotChat.
	nonChat := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindIssue, Status: "running"}}
	if _, err := New(nonChat, newBox(t), testParams()).CreateProposal(context.Background(), wkr, runID, repoID, "t", "d", labels); !errors.Is(err, ErrProposalRunNotChat) {
		t.Fatalf("non-chat target err = %v, want ErrProposalRunNotChat", err)
	}

	// Terminal chat → ErrRunTerminal.
	terminal := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindChat, Status: "completed"}}
	if _, err := New(terminal, newBox(t), testParams()).CreateProposal(context.Background(), wkr, runID, repoID, "t", "d", labels); !errors.Is(err, ErrRunTerminal) {
		t.Fatalf("terminal chat err = %v, want ErrRunTerminal", err)
	}

	// At the per-run pending cap → ErrProposalCapReached.
	capped := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindChat, Status: "running"}, pendingProposalCount: MaxPendingProposalsPerRun}
	if _, err := New(capped, newBox(t), testParams()).CreateProposal(context.Background(), wkr, runID, repoID, "t", "d", labels); !errors.Is(err, ErrProposalCapReached) {
		t.Fatalf("capped err = %v, want ErrProposalCapReached", err)
	}

	// Happy path → a proposal is created.
	ok := &fakeStore{runByID: store.Run{ID: runID, UserID: uid, Kind: RunKindChat, Status: "running"}, pendingProposalCount: 2}
	if _, err := New(ok, newBox(t), testParams()).CreateProposal(context.Background(), wkr, runID, repoID, "t", "d", labels); err != nil {
		t.Fatalf("valid CreateProposal: %v", err)
	}
	if ok.createdProposal == nil || ok.createdProposal.RunID != runID {
		t.Fatalf("a valid proposal must be created, got %v", ok.createdProposal)
	}
}

// TestSweepRecoversStuckConfirmingProposals: the sweep reverts proposals stranded
// in 'confirming' (a confirm handler killed mid-flight) back to pending.
func TestSweepRecoversStuckConfirmingProposals(t *testing.T) {
	fs := &fakeStore{sweptStuckProposals: []uuid.UUID{uuid.New(), uuid.New()}}
	p := testParams()
	p.ProposalConfirmStuckTimeout = 2 * time.Minute
	res, err := New(fs, newBox(t), p).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ProposalsRecovered != 2 {
		t.Fatalf("ProposalsRecovered = %d, want 2", res.ProposalsRecovered)
	}
}

// TestDeriveChatTitle pins the title derivation, including the #213 terminal-safety
// strip: control/bidi runes are NOT unicode.IsSpace, so strings.Fields alone left
// them in runs.title, which renders in the cross-tenant `uzi admin runs` table.
// termsafe.CellText removes them at derivation time. Cases also lock the existing
// behaviour (first-line cut, whitespace collapse, fallback, truncation) so the fix
// does not regress it.
func TestDeriveChatTitle(t *testing.T) {
	// A first line of 100 'a' runes exceeds maxChatTitleRunes (80); after truncation
	// the (space-free) 80-rune prefix keeps all 80 runes and the ellipsis is appended,
	// so the result is 81 runes ending in "…".
	longInput := strings.Repeat("a", 100)

	tests := []struct {
		name  string
		in    string
		want  string // exact expected result; "" means use the assert func instead
		check func(t *testing.T, got string)
	}{
		{
			name: "strips ESC terminal-control sequence",
			in:   "hi\x1b[2Jthere",
			check: func(t *testing.T, got string) {
				if strings.ContainsRune(got, '\x1b') {
					t.Errorf("title %q must not contain ESC (\\x1b)", got)
				}
				// The visible characters survive (the '[' is ordinary text after the
				// control rune is stripped).
				if !strings.Contains(got, "hi") || !strings.Contains(got, "there") {
					t.Errorf("title %q lost visible text", got)
				}
			},
		},
		{
			name: "strips bidi RLO override",
			in:   "safe\u202eevil",
			check: func(t *testing.T, got string) {
				if strings.ContainsRune(got, '\u202e') {
					t.Errorf("title %q must not contain bidi RLO (U+202E)", got)
				}
				if !strings.Contains(got, "safe") || !strings.Contains(got, "evil") {
					t.Errorf("title %q lost visible text", got)
				}
			},
		},
		{name: "cuts at first newline", in: "first\nsecond", want: "first"},
		{name: "collapses whitespace runs", in: "a   b", want: "a b"},
		{name: "whitespace-only first line falls back", in: "   \n rest", want: "New chat"},
		{name: "plain text unchanged", in: "Hello world", want: "Hello world"},
		{
			name: "truncates past maxChatTitleRunes with ellipsis",
			in:   longInput,
			check: func(t *testing.T, got string) {
				if !strings.HasSuffix(got, "…") {
					t.Errorf("truncated title %q must end with an ellipsis", got)
				}
				if n := len([]rune(got)); n != maxChatTitleRunes+1 {
					t.Errorf("truncated title rune count = %d, want %d", n, maxChatTitleRunes+1)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveChatTitle(tc.in)
			if tc.check != nil {
				tc.check(t, got)
				return
			}
			if got != tc.want {
				t.Errorf("deriveChatTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
