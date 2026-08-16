package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// chatAnchor is a Slack-anchored chat's DM row: root_ts is the user's message, and
// status_ts (when valid) is the bot's editable status message.
func chatAnchor(runID uuid.UUID, statusTS string) store.SlackRunMessage {
	m := store.SlackRunMessage{RunID: runID, ChannelID: "D1", RootTs: "user1"}
	if statusTS != "" {
		m.StatusTs = pgtype.Text{String: statusTS, Valid: true}
	}
	return m
}

// chatCtx is a repo-less chat context (what GetSlackChatContext returns).
func chatCtx(runID uuid.UUID, status string) store.GetSlackChatContextRow {
	return store.GetSlackChatContextRow{ID: runID, UserID: uuid.New(), Status: status, Kind: "chat"}
}

// A chat run reaching completed edits the bot's status_ts message exactly once, and
// never touches the user's root_ts (Decision 2). The run-context lookup returns
// ErrNoRows (a chat has no repo), so the chat fallback path runs.
func TestNotifierChatCompletedEditsStatusTs(t *testing.T) {
	runID := uuid.New()
	fs := &fakeNotifStore{
		rcErr:      pgx.ErrNoRows,
		chatCtxSet: true,
		chatCtx:    chatCtx(runID, "completed"),
		msg:        chatAnchor(runID, "bot1"),
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: runID, status: "completed"})

	// Terminal edit swaps the End button for a Continue button (M6) via UpdateBlocks.
	if len(fp.updateBlocks) != 1 {
		t.Fatalf("want exactly one status edit, got %+v", fp.updateBlocks)
	}
	if fp.updateBlocks[0].ts != "bot1" {
		t.Errorf("status edit must target status_ts (bot1), got %q", fp.updateBlocks[0].ts)
	}
	if strings.Join(fp.updateBlocks[0].actionIDs, ",") != ActionChatContinue {
		t.Errorf("terminal status must carry the Continue button, got %v", fp.updateBlocks[0].actionIDs)
	}
	for _, u := range append(fp.updates, updateCallsFromBlocks(fp.updateBlocks)...) {
		if u.ts == "user1" {
			t.Errorf("a chat transition must NEVER edit root_ts (the user's message): %+v", u)
		}
	}
	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Errorf("a status_ts blocks-edit must not also plain-post/update: posts=%v updates=%v", fp.posts, fp.updates)
	}
}

// updateCallsFromBlocks projects updateBlockCall to (channel, ts) for the root_ts guard.
func updateCallsFromBlocks(bs []updateBlockCall) []updateCall {
	out := make([]updateCall, len(bs))
	for i, b := range bs {
		out[i] = updateCall{channel: b.channel, ts: b.ts}
	}
	return out
}

// A failed chat includes the (scrubbed) failure reason on the status line.
func TestNotifierChatFailedShowsReason(t *testing.T) {
	runID := uuid.New()
	cc := chatCtx(runID, "failed")
	cc.FailureReason = pgtype.Text{String: "worker disappeared mid-turn", Valid: true}
	fs := &fakeNotifStore{rcErr: pgx.ErrNoRows, chatCtxSet: true, chatCtx: cc, msg: chatAnchor(runID, "bot1")}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: runID, status: "failed"})

	if len(fp.updateBlocks) != 1 || fp.updateBlocks[0].ts != "bot1" {
		t.Fatalf("want one status edit on status_ts, got %+v", fp.updateBlocks)
	}
	if !strings.Contains(fp.updateBlocks[0].sectionText, "worker disappeared mid-turn") {
		t.Errorf("failed status should name the reason: %q", fp.updateBlocks[0].sectionText)
	}
}

// A chat whose open-time status post failed (NULL status_ts) has no bot message to
// edit, so the terminal transition is silent — it does NOT post a fresh line (that
// would bypass the opt-out gate and have no dedup key). The run stays visible in web.
func TestNotifierChatNullStatusTsIsSilent(t *testing.T) {
	runID := uuid.New()
	fs := &fakeNotifStore{
		rcErr:      pgx.ErrNoRows,
		chatCtxSet: true,
		chatCtx:    chatCtx(runID, "completed"),
		msg:        chatAnchor(runID, ""), // NULL status_ts
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: runID, status: "completed"})

	if len(fp.updates) != 0 || len(fp.updateBlocks) != 0 || len(fp.posts) != 0 {
		t.Fatalf("with no status_ts to edit the transition must be silent: updates=%v blocks=%v posts=%v", fp.updates, fp.updateBlocks, fp.posts)
	}
}

// A non-terminal chat transition (running) is not narrated: nothing is posted or edited.
func TestNotifierChatRunningIsSilent(t *testing.T) {
	runID := uuid.New()
	fs := &fakeNotifStore{
		rcErr:      pgx.ErrNoRows,
		chatCtxSet: true,
		chatCtx:    chatCtx(runID, "running"),
		msg:        chatAnchor(runID, "bot1"),
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: runID, status: "running"})

	if len(fp.updates) != 0 || len(fp.updateBlocks) != 0 || len(fp.posts) != 0 {
		t.Fatalf("a non-terminal chat state must be silent: updates=%v blocks=%v posts=%v", fp.updates, fp.updateBlocks, fp.posts)
	}
}

// A repo-less run that is NOT a chat (a judge run: GetSlackChatContext returns
// ErrNoRows) stays dropped exactly as before — no Slack call.
func TestNotifierNonChatRepolessRunStillDropped(t *testing.T) {
	runID := uuid.New()
	fs := &fakeNotifStore{rcErr: pgx.ErrNoRows} // chatCtxSet false → GetSlackChatContext ErrNoRows
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: runID, status: "completed"})

	if len(fp.updates) != 0 || len(fp.updateBlocks) != 0 || len(fp.posts) != 0 {
		t.Fatalf("a repo-less non-chat run must not reach Slack: updates=%v blocks=%v posts=%v", fp.updates, fp.updateBlocks, fp.posts)
	}
}
