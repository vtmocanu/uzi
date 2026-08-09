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
// regardless of N (PRD #268 M4: both are Block Kit). The edit's section carries the
// joined body and the deep link rides its OWN context block, not the section text.
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

	if len(fp.blocks) != 1 {
		t.Fatalf("want exactly one placeholder Block Kit post, got %d: %+v", len(fp.blocks), fp.blocks)
	}
	if fp.blocks[0].thread != "user1" {
		t.Errorf("placeholder must thread on root_ts, got %q", fp.blocks[0].thread)
	}
	if len(fp.updateBlocks) != 1 {
		t.Fatalf("want exactly one Block Kit edit regardless of frame count, got %d: %+v", len(fp.updateBlocks), fp.updateBlocks)
	}
	edit := fp.updateBlocks[0]
	if edit.ts != "ts1" { // the placeholder's ts
		t.Errorf("edit must target the placeholder, got %q", edit.ts)
	}
	if !strings.Contains(edit.sectionText, "The plan gate pauses the run for approval.") {
		t.Errorf("edit section must carry the joined text-frame body: %q", edit.sectionText)
	}
	// The deep link lives in the context block, NOT inside the section text.
	if strings.Contains(edit.sectionText, "Open in uzi") {
		t.Errorf("deep link must be moved OUT of the section body: %q", edit.sectionText)
	}
	if !strings.Contains(edit.contextText, "Open in uzi") {
		t.Errorf("resolved turn should keep the deep link in its own context block: %q", edit.contextText)
	}
}

// The placeholder is a context block that carries the deep link so an orphan (api
// restart mid-turn) is useful (PRD #268 M4).
func TestChatPlaceholderCarriesDeepLink(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID, frame("user_message", `{"text":"hi"}`))

	if len(fp.blocks) != 1 {
		t.Fatalf("want exactly one placeholder Block Kit post, got %+v", fp.blocks)
	}
	// The placeholder is a context block (no section), carrying the thinking glyph and
	// the deep link.
	if fp.blocks[0].sectionText != "" {
		t.Errorf("placeholder must be a context block, not a section: %q", fp.blocks[0].sectionText)
	}
	if !strings.Contains(fp.blocks[0].contextText, "uzi is thinking") {
		t.Errorf("placeholder must carry the thinking text: %q", fp.blocks[0].contextText)
	}
	if !strings.Contains(fp.blocks[0].contextText, "Open in uzi") {
		t.Errorf("placeholder must carry the deep link: %q", fp.blocks[0].contextText)
	}
	if fp.blocks[0].fallback != "uzi is thinking…" {
		t.Errorf("placeholder fallback = %q, want the fixed thinking text", fp.blocks[0].fallback)
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

			if len(fp.blocks) != 1 {
				t.Fatalf("want one placeholder, got %+v", fp.blocks)
			}
			if len(fp.updateBlocks) != 1 {
				t.Fatalf("%s must resolve the placeholder with exactly one edit, got %+v", tc.name, fp.updateBlocks)
			}
			if !strings.Contains(fp.updateBlocks[0].sectionText, tc.wantText) {
				t.Errorf("%s edit section missing %q: %q", tc.name, tc.wantText, fp.updateBlocks[0].sectionText)
			}
		})
	}
}

// A text-less status{event:"init"} heartbeat (the real Claude Agent SDK emits one at
// the start of every query) must NOT flush the open turn before its answer buffers —
// reproduces the PRD #268 init-heartbeat drop, where the answer was replaced by
// chatNoAnswerText in Slack.
func TestChatInitStatusDoesNotDropAnswer(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"what runs do we have now?"}`),
		frame("status", `{"event":"init","model":"claude-opus-4-8"}`),
		frame("text", `{"text":"the real answer"}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.blocks) != 1 {
		t.Fatalf("want exactly one placeholder post, got %d: %+v", len(fp.blocks), fp.blocks)
	}
	if len(fp.updateBlocks) != 1 {
		t.Fatalf("want exactly one edit resolving the turn, got %d: %+v", len(fp.updateBlocks), fp.updateBlocks)
	}
	edit := fp.updateBlocks[0]
	// The answer must reach the edit — in the section and the fallback — never replaced
	// by the no-answer degrade (the M1 init-heartbeat regression).
	if !strings.Contains(edit.sectionText, "the real answer") {
		t.Errorf("edit section must carry the answer, not drop it: %q", edit.sectionText)
	}
	if !strings.Contains(edit.fallback, "the real answer") {
		t.Errorf("edit fallback must carry the answer: %q", edit.fallback)
	}
	if strings.Contains(edit.sectionText, chatNoAnswerText) || strings.Contains(edit.contextText, chatNoAnswerText) {
		t.Errorf("init heartbeat must not flush the turn to the no-answer placeholder: section=%q context=%q", edit.sectionText, edit.contextText)
	}
}

// A tool-only turn (a user_message and a result with no text frames) degrades to a
// de-emphasized context block carrying chatNoAnswerText — NOT a section — plus the deep
// link, and never strands the placeholder (PRD #268 M4).
func TestChatEmptyTurnDegradesToContextBlock(t *testing.T) {
	runID := uuid.New()
	fp := &fakePoster{}
	n := NewNotifier(chatMsgStore(runID), fp, fixedBase, nil)

	feed(n, runID,
		frame("user_message", `{"text":"just run a tool"}`),
		frame("status", `{"event":"result"}`),
	)

	if len(fp.updateBlocks) != 1 {
		t.Fatalf("want exactly one edit resolving the empty turn, got %+v", fp.updateBlocks)
	}
	edit := fp.updateBlocks[0]
	if edit.sectionText != "" {
		t.Errorf("empty-turn degrade must be a context block, not a section: %q", edit.sectionText)
	}
	if !strings.Contains(edit.contextText, chatNoAnswerText) {
		t.Errorf("empty-turn degrade must carry the no-answer text in a context block: %q", edit.contextText)
	}
	if !strings.Contains(edit.contextText, "Open in uzi") {
		t.Errorf("empty-turn degrade must still carry the deep link: %q", edit.contextText)
	}
	if edit.fallback != chatNoAnswerText {
		t.Errorf("empty-turn fallback = %q, want the no-answer text", edit.fallback)
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
	if len(fp.blocks) != 0 || len(fp.updateBlocks) != 0 {
		t.Fatalf("a text frame with no open turn must not post: posts=%v updates=%v", fp.blocks, fp.updateBlocks)
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
		frame("text", `{"text":"ping <@U123> see <https://evil|Open> token glpat-ABCDEF1234567890abcd"}`), //gitleaks:allow // fake PAT fixture: asserts a credential-shaped string is scrubbed, never a real secret
		frame("status", `{"event":"result"}`),
	)

	if len(fp.updateBlocks) != 1 {
		t.Fatalf("want one resolved turn, got %+v", fp.updateBlocks)
	}
	// The untrusted answer sits inside the section block, whole-blob escaped/scrubbed.
	body := fp.updateBlocks[0].sectionText
	if strings.Contains(body, "<@U123>") {
		t.Errorf("a raw Slack mention must be neutralized in the section: %q", body)
	}
	if strings.Contains(body, "<https://evil|Open>") {
		t.Errorf("a masquerading link must be neutralized in the section: %q", body)
	}
	if strings.Contains(body, "glpat-ABCDEF1234567890abcd") { //gitleaks:allow // fake PAT fixture: asserts the credential-shaped string was scrubbed, never a real secret
		t.Errorf("a credential-shaped string must be scrubbed: %q", body)
	}
	// The fallback is built from the same escaped body, so it is inert there too.
	if strings.Contains(fp.updateBlocks[0].fallback, "<@U123>") {
		t.Errorf("the fallback must carry the escaped (inert) body: %q", fp.updateBlocks[0].fallback)
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

	if len(fp.blocks) != 0 || len(fp.updateBlocks) != 0 {
		t.Fatalf("frames for an already-terminal chat must not stream: posts=%v updates=%v", fp.blocks, fp.updateBlocks)
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

	if len(fp.blocks) != 0 || len(fp.updateBlocks) != 0 {
		t.Fatalf("an issue run must produce no chat posts: posts=%v updates=%v", fp.blocks, fp.updateBlocks)
	}
	if v, ok := n.chatDecided.Load(runID); !ok || v.(bool) {
		t.Errorf("the run must be classified non-chat (decided=false), got ok=%v v=%v", ok, v)
	}
}
