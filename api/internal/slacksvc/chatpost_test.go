package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// chatMsgStore builds a fake wired so setupChatConvo classifies runID as an anchored
// chat: GetSlackChatContext returns a chat row and GetSlackRunMessage returns the DM
// anchor.
func chatMsgStore(runID uuid.UUID) *fakeNotifStore {
	return &fakeNotifStore{
		chatCtxSet: true,
		chatCtx:    store.GetSlackChatContextRow{ID: runID, Status: "running", Kind: "chat"},
		msg:        chatAnchor(runID, "bot1"),
	}
}

func frame(kind, payload string) chatMsgEvent {
	return chatMsgEvent{kind: kind, payload: []byte(payload)}
}

func feed(n *Notifier, runID uuid.UUID, frames ...chatMsgEvent) {
	for _, f := range frames {
		f.runID = runID
		n.handleChatMsg(context.Background(), f)
	}
}

// A turn emitting N text frames produces exactly ONE placeholder post and ONE edit,
// regardless of N, and the edit carries the joined body.
func TestChatTurnCoalescesToOnePostOneEdit(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"how does the gate work?"}`),
		frame("text", `{"text":"The plan gate "}`),
		frame("text", `{"text":"pauses the run "}`),
		frame("text", `{"text":"for approval."}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.posts) != 1 {
		t.Fatalf("want exactly one placeholder post, got %d: %+v", len(fp.posts), fp.posts)
	}
	if fp.posts[0].thread != "user1" {
		t.Errorf("placeholder must thread on root_ts, got %q", fp.posts[0].thread)
	}
	if len(fp.updates) != 1 {
		t.Fatalf("want exactly one edit regardless of frame count, got %d: %+v", len(fp.updates), fp.updates)
	}
	if fp.updates[0].ts != "ts1" { // the placeholder's ts
		t.Errorf("edit must target the placeholder, got %q", fp.updates[0].ts)
	}
	body := fp.updates[0].text
	if !strings.Contains(body, "The plan gate pauses the run for approval.") {
		t.Errorf("edit must carry the joined text-frame body: %q", body)
	}
	if !strings.Contains(body, "Open in uzi") {
		t.Errorf("resolved turn should keep the deep link: %q", body)
	}
}

// The placeholder carries the deep link so an orphan (api restart mid-turn) is useful.
func TestChatPlaceholderCarriesDeepLink(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("user_message", `{"text":"hi"}`))

	if len(fp.posts) != 1 || !strings.Contains(fp.posts[0].text, "Open in uzi") {
		t.Fatalf("placeholder must carry the deep link: %+v", fp.posts)
	}
}

// Each non-result turn end resolves the placeholder rather than stranding it — one
// test per path (a consumer keyed only on the result frame passes the happy path but
// strands these).
func TestChatNonResultTurnEndsResolvePlaceholder(t *testing.T) {
	cases := []struct {
		name     string
		end      chatMsgEvent
		wantText string
	}{
		// Timeout: a prose status with no result event.
		{"timeout", frame("status", `{"text":"the previous turn took too long and was stopped"}`), "too long"},
		// Error: an error frame.
		{"error", frame("error", `{"text":"model overloaded"}`), "model overloaded"},
		// Cancel: the turn itself emits nothing; the conversation-end line (a prose
		// status) lands while the turn is still open and resolves it.
		{"cancel via conv-end line", frame("status", `{"text":"chat ended after 3 turns"}`), "chat ended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := uuid.New()
			fp := &fakePoster{}
			n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

			feed(n, runID,
				frame("user_message", `{"text":"do the thing"}`),
				frame("text", `{"text":"working on it"}`),
				tc.end,
			)

			if len(fp.posts) != 1 {
				t.Fatalf("want one placeholder, got %+v", fp.posts)
			}
			if len(fp.updates) != 1 {
				t.Fatalf("%s must resolve the placeholder with exactly one edit, got %+v", tc.name, fp.updates)
			}
			if !strings.Contains(fp.updates[0].text, tc.wantText) {
				t.Errorf("%s edit missing %q: %q", tc.name, tc.wantText, fp.updates[0].text)
			}
		})
	}
}

// thinking / tool frames never post: they are filtered before the queue, and even if
// one reached the drain it carries no turn body.
func TestChatThinkingAndToolFramesNeverPost(t *testing.T) {
	if chatRelevantKind("thinking") || chatRelevantKind("tool_use") || chatRelevantKind("tool_result") {
		t.Fatal("thinking/tool frames must not be chat-relevant (they would post)")
	}
	// A text frame with no open turn (no preceding user_message) buffers nothing and
	// posts nothing.
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)
	feed(n, runID, frame("text", `{"text":"orphan text"}`))
	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("a text frame with no open turn must not post: posts=%v updates=%v", fp.posts, fp.updates)
	}
}

// A model answer containing a Slack mention, a masquerading link, and a
// credential-shaped string renders inert and scrubbed.
func TestChatAnswerIsScrubbedAndInert(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"echo this"}`),
		frame("text", `{"text":"ping <@U123> see <https://evil|Open> token glpat-ABCDEF1234567890abcd"}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.updates) != 1 {
		t.Fatalf("want one resolved turn, got %+v", fp.updates)
	}
	body := fp.updates[0].text
	if strings.Contains(body, "<@U123>") {
		t.Errorf("a raw Slack mention must be neutralized: %q", body)
	}
	if strings.Contains(body, "<https://evil|Open>") {
		t.Errorf("a masquerading link must be neutralized: %q", body)
	}
	if strings.Contains(body, "glpat-ABCDEF1234567890abcd") {
		t.Errorf("a credential-shaped string must be scrubbed: %q", body)
	}
}

// A terminal transition frees the run's turn-streaming state (no per-run leak), for
// every run kind — the defer in handle fires on any terminal ev.status.
func TestChatTerminalTransitionFreesConvo(t *testing.T) {
	runID := uuid.New()
	fs := chatMsgStore(runID)
	fs.rcErr = pgx.ErrNoRows // repo-less → handle takes the chat path
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	feed(n, runID, frame("user_message", `{"text":"hi"}`))
	if _, ok := n.chatConvos[runID]; !ok {
		t.Fatal("expected a live convo after user_message")
	}

	fs.chatCtx.Status = "completed"
	n.handle(context.Background(), stateEvent{runID: runID, status: "completed"})

	if _, ok := n.chatConvos[runID]; ok {
		t.Error("terminal transition must free the convo entry (no per-run leak)")
	}
	if _, ok := n.chatDecided.Load(runID); ok {
		t.Error("terminal transition must clear the decided marker")
	}
}

// A frame that trails the terminal transition (maps already freed) does NOT re-create
// a live conversation into an ended chat — it would strand a placeholder nothing cleans.
func TestChatFrameAfterTerminalDoesNotStream(t *testing.T) {
	runID := uuid.New()
	fs := chatMsgStore(runID)
	fs.chatCtx.Status = "completed" // the chat has already ended
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"late"}`),
		frame("text", `{"text":"stray"}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("frames for an already-terminal chat must not stream: posts=%v updates=%v", fp.posts, fp.updates)
	}
}

// A NON-chat run (an issue run: GetSlackChatContext returns ErrNoRows) produces ZERO
// chat posts — the frames are classified skip on the first one. A bare zero would also
// be what an unwired fake produces, so also assert the run was classified (decided).
func TestChatMsgIssueRunProducesNoChatPosts(t *testing.T) {
	runID := uuid.New()
	fs := &fakeNotifStore{} // chatCtxSet false → GetSlackChatContext ErrNoRows (not a chat)
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"x"}`),
		frame("text", `{"text":"y"}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("an issue run must produce no chat posts: posts=%v updates=%v", fp.posts, fp.updates)
	}
	if v, ok := n.chatDecided.Load(runID); !ok || v.(bool) {
		t.Errorf("the run must be classified non-chat (decided=false), got ok=%v v=%v", ok, v)
	}
}
