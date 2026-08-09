package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// topLevelDM is a top-level DM (no thread_ts): MessageTS is the user's opening
// message ts, which the opener stores as the anchor root_ts.
func topLevelDM(text string) MessageReply {
	return MessageReply{SlackUserID: "Uauth", ChannelID: "D1", ThreadTS: "", MessageTS: "open1", Text: text}
}

// A top-level DM from a confirmed user opens exactly one chat run and one anchor whose
// status_ts is the BOT's status message, and acks the opening message.
func TestOpenChatCreatesRunAndAnchor(t *testing.T) {
	user := store.User{ID: uuid.New()}
	runID := uuid.New()
	fs := &fakeReplierStore{user: user}
	sub := &fakeSubmitter{createdChat: store.Run{ID: runID, Kind: runKindChat}} // liveChatOK defaults false
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), topLevelDM("  what's running?  "))

	if len(sub.createdChats) != 1 {
		t.Fatalf("want exactly one chat created, got %d", len(sub.createdChats))
	}
	if got := sub.createdChats[0].message; got != "what's running?" {
		t.Errorf("chat seeded with %q, want trimmed/scrubbed opening message", got)
	}
	if len(fs.chatAnchors) != 1 {
		t.Fatalf("want exactly one anchor row, got %d", len(fs.chatAnchors))
	}
	a := fs.chatAnchors[0]
	if a.RunID != runID || a.ChannelID != "D1" || a.RootTs != "open1" {
		t.Errorf("anchor mismapped: %+v (root_ts must be the user's message ts)", a)
	}
	// The status root is a Block Kit message (with an End button, M6) threaded on the
	// user's message; its ts is status_ts — a BOT message, never the user's.
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "open1" {
		t.Fatalf("want one status root threaded on the user message, got %+v", fp.blocks)
	}
	if strings.Join(fp.blocks[0].actionIDs, ",") != ActionChatEnd {
		t.Errorf("live status must carry the End button, got %v", fp.blocks[0].actionIDs)
	}
	if !a.StatusTs.Valid || a.StatusTs.String != "ts1" {
		t.Errorf("status_ts = %+v, want the bot post ts (ts1)", a.StatusTs)
	}
	if a.StatusTs.String == a.RootTs {
		t.Error("status_ts must be the bot message, never equal to the user's root_ts")
	}
	if len(fp.reactions) != 1 || fp.reactions[0].ts != "open1" {
		t.Errorf("want a ✅ ack on the opening message, got %+v", fp.reactions)
	}
}

// With no worker online, the opener's status names that cause (M6).
func TestOpenChatNoWorkerNamesTheCause(t *testing.T) {
	user := store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user}
	sub := &fakeSubmitter{createdChat: store.Run{ID: uuid.New(), Kind: runKindChat}} // workerOnline defaults false
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), topLevelDM("hi"))

	if len(fp.blocks) != 1 || !strings.Contains(strings.ToLower(fp.blocks[0].sectionText), "no uzi worker") {
		t.Fatalf("status should name the no-worker cause, got %+v", fp.blocks)
	}
}

// If the anchor insert fails, the opener does NOT ack (a ✅ would falsely say "it
// landed" for a thread that can't carry the conversation) and points the user at the
// web Chat page instead.
func TestOpenChatAnchorFailureDoesNotAck(t *testing.T) {
	user := store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user, chatAnchorErr: errors.New("db down")}
	sub := &fakeSubmitter{createdChat: store.Run{ID: uuid.New(), Kind: runKindChat}}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), topLevelDM("hello"))

	if len(fp.reactions) != 0 {
		t.Errorf("an anchor failure must not ack, got %+v", fp.reactions)
	}
	// One status root (blocks) + one anchor-failure notice (a plain thread post).
	if len(fp.blocks) != 1 {
		t.Fatalf("want the status root block, got %+v", fp.blocks)
	}
	if len(fp.posts) != 1 {
		t.Fatalf("want one anchor-failure notice, got %+v", fp.posts)
	}
}

// A top-level DM from an unlinked Slack user creates nothing (HandleMessage refuses at
// the confirmed-user lookup, before the opener).
func TestOpenChatUnlinkedUserCreatesNothing(t *testing.T) {
	fs := &fakeReplierStore{userErr: pgx.ErrNoRows}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), topLevelDM("hello"))

	if len(sub.createdChats) != 0 {
		t.Errorf("an unlinked user must create no chat, got %d", len(sub.createdChats))
	}
	if len(fs.chatAnchors) != 0 {
		t.Errorf("an unlinked user must write no anchor, got %d", len(fs.chatAnchors))
	}
}

// A second top-level DM while a chat is live is refused with a threaded pointer, and
// creates no second run (Decision 3).
func TestOpenChatRefusesSecondWhileLive(t *testing.T) {
	user := store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user}
	sub := &fakeSubmitter{liveChat: store.Run{ID: uuid.New(), Kind: runKindChat}, liveChatOK: true}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), topLevelDM("another one"))

	if len(sub.createdChats) != 0 {
		t.Errorf("must not mint a second chat while one is live, got %d", len(sub.createdChats))
	}
	if len(fp.posts) != 1 || fp.posts[0].thread != "open1" {
		t.Fatalf("want one threaded pointer to the live chat, got %+v", fp.posts)
	}
}

// The shared per-user chat spend guard refuses the open when over budget (Decision 9);
// no run is created and the refusal names the shared budget.
func TestOpenChatSpendGuardRefuses(t *testing.T) {
	user := store.User{ID: uuid.New()}
	fs := &fakeReplierStore{user: user}
	sub := &fakeSubmitter{}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)
	r.SetChatSpendGuard(func(uuid.UUID) bool { return false }) // over budget

	r.HandleMessage(context.Background(), topLevelDM("hello"))

	if len(sub.createdChats) != 0 {
		t.Errorf("a rate-limited open must create no chat, got %d", len(sub.createdChats))
	}
	if len(fp.posts) != 1 {
		t.Fatalf("want one threaded rate-limit notice, got %+v", fp.posts)
	}
}

// A thread reply on a CHAT run routes through SubmitChatMessage as a turn — NOT a raw
// follow_up (the injection the service now rejects). Assert on the persisted turn and
// that no follow_up SubmitInput was made.
func TestChatThreadReplySubmitsTurnNotFollowUp(t *testing.T) {
	user := store.User{ID: uuid.New()}
	runID := uuid.New()
	fs := &fakeReplierStore{
		user:   user,
		anchor: store.SlackRunMessage{RunID: runID, ChannelID: "D1", RootTs: "root1"},
	}
	sub := &fakeSubmitter{run: store.Run{ID: runID, UserID: user.ID, Kind: runKindChat, Status: "running"}}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	// reply() carries ThreadTS "root1" → a thread reply, not a top-level DM.
	r.HandleMessage(context.Background(), reply("tell me more"))

	if len(sub.chatTurns) != 1 {
		t.Fatalf("want exactly one chat turn, got %d", len(sub.chatTurns))
	}
	if sub.chatTurns[0].runID != runID || sub.chatTurns[0].message != "tell me more" {
		t.Errorf("chat turn mismapped: %+v", sub.chatTurns[0])
	}
	if len(sub.submitted) != 0 {
		t.Errorf("a chat reply must NOT submit a raw follow_up, got %+v", sub.submitted)
	}
	if len(fp.reactions) != 1 {
		t.Errorf("want a ✅ ack on the accepted turn, got %+v", fp.reactions)
	}
}

// A thread reply on a chat that hit the turn cap is answered in-thread, not acked.
func TestChatThreadReplyTurnCapReached(t *testing.T) {
	user := store.User{ID: uuid.New()}
	runID := uuid.New()
	fs := &fakeReplierStore{
		user:   user,
		anchor: store.SlackRunMessage{RunID: runID, ChannelID: "D1", RootTs: "root1"},
	}
	sub := &fakeSubmitter{
		run:         store.Run{ID: runID, UserID: user.ID, Kind: runKindChat, Status: "running"},
		chatTurnErr: ErrChatTurnCapReached,
	}
	fp := &fakePoster{}
	r := NewReplier(fs, sub, fp, nil)

	r.HandleMessage(context.Background(), reply("more please"))

	if len(fp.reactions) != 0 {
		t.Errorf("a rejected turn must not ack, got %+v", fp.reactions)
	}
	if len(fp.posts) != 1 || fp.posts[0].thread != "root1" {
		t.Fatalf("want one cap notice in the conversation thread, got %+v", fp.posts)
	}
}
