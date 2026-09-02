package slacksvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Chat-turn sentinels (PRD #191 M2). The adapter in main translates the workersvc
// turn-cap / terminal sentinels into these so the replier can say which happened
// without importing workersvc — the same pattern ErrReviseCapReached follows.
var (
	// ErrChatTurnCapReached: the conversation hit CHAT_MAX_TURNS.
	ErrChatTurnCapReached = errors.New("slacksvc: chat turn cap reached")
	// ErrChatEnded: the conversation is terminal (completed/failed/cancelled).
	ErrChatEnded = errors.New("slacksvc: chat has ended")
)

const (
	// chatStartedText is the bot's status root, posted in a thread on the user's
	// opening message; its ts becomes the anchor's status_ts (Decision 2), the line
	// M2b edits on a terminal transition. The agent's actual answers stream as
	// separate threaded messages (M3).
	chatStartedText = "💬 On it — starting a chat. I'll reply in this thread; send more here to keep the conversation going."
	// chatQueuedNoWorkerText is shown when the opener finds no worker online (M6): the
	// run is queued and will wait, so name the cause instead of leaving a silent thread.
	chatQueuedNoWorkerText = "💬 Chat queued — but no uzi worker is connected right now, so it's waiting. Start your worker and it'll pick this up; your messages here are saved."
	// chatStatusFallback is the notification/plain-text fallback for the status blocks.
	chatStatusFallback = "uzi chat"
	// chatBusyText refuses a second top-level DM while a chat is live (Decision 3).
	chatBusyText = "You already have a live chat in this DM — reply in its thread to continue it (or say you're done there first). One conversation at a time keeps them from queueing behind each other."
	// chatRateLimitedText is the shared-budget refusal (Decision 9): the chat spend
	// guard is one per-user pool across web and Slack, so it names the web page too.
	chatRateLimitedText = "You're starting chats faster than the limit allows — give it a moment. This budget is shared with the web Chat page, so heavy use on either side counts."
	// chatStartFailedText is a best-effort fallback when CreateChatRun itself fails.
	chatStartFailedText = "Couldn't start a chat just now — try again, or open the Chat page in uzi."
	// chatAnchorFailedText: the run was created but its DM anchor was not, so replies
	// here won't reach it — send the user to the web Chat page rather than ack a thread
	// that can't carry the conversation.
	chatAnchorFailedText = "Your chat started, but I couldn't wire it to this DM — continue it from the Chat page in uzi (replies here won't reach it)."
	// chatCapReachedText / chatEndedText answer a thread reply that can no longer
	// become a turn.
	chatCapReachedText = "This conversation has reached its turn limit — start a new chat with a fresh message to me."
	chatEndedText      = "This conversation has ended — start a new chat by sending me a new message (not in this thread)."
	chatTurnFailedText = "Couldn't record that message — try again, or open the chat in uzi."
)

// openChat handles a top-level DM: it opens a new chat conversation backed by a
// kind='chat' run and posts the bot's status root into a thread on the user's message
// (PRD #191 M2). The anchor stores the user's message ts as root_ts (what a later
// thread reply resolves against) and the bot status ts as status_ts (Decision 2).
//
// Two guards precede creation: Decision 3 refuses a second top-level DM while a chat
// is live (a pointer, not a clock-based continue), and Decision 9 draws the open from
// the shared per-user chat spend budget. An unlinked/deactivated author never reaches
// here — HandleMessage resolved the confirmed, active user first.
func (r *Replier) openChat(ctx context.Context, m MessageReply, user store.User) {
	// Decision 3: one live chat at a time. A pointer, not a clock — the user continues
	// by replying in the existing thread.
	if _, ok, err := r.svc.LiveChatForUser(ctx, user.ID); err != nil {
		r.logf("check live chat", err)
		return
	} else if ok {
		r.postThreadReply(ctx, m, chatBusyText)
		return
	}

	// Decision 9: the shared per-user chat spend budget. A nil guard (tests) is open.
	if r.chatAllow != nil && !r.chatAllow(user.ID) {
		r.postThreadReply(ctx, m, chatRateLimitedText)
		return
	}

	run, err := r.svc.CreateChatRun(ctx, user.ID, boundReply(m.Text))
	if err != nil {
		r.logf("create chat run", err)
		r.postThreadReply(ctx, m, chatStartFailedText)
		return
	}

	// Post the bot's status root (with an End-chat button) in a thread on the user's
	// message; its ts is the editable status_ts. The copy names the one cause a user
	// will actually hit (PRD #191 M6): no worker connected, so the run sits queued. A
	// post failure is non-fatal — the anchor records a NULL status_ts and later per-turn
	// answers still thread under root_ts (M3).
	statusTS, err := r.poster.PostBlocks(ctx, m.ChannelID, m.MessageTS, chatStatusFallback,
		chatLiveStatusBlocks(run.ID, r.openingStatusText(ctx, user.ID)))
	if err != nil {
		r.logf("post chat status root", err)
	}

	if _, err := r.store.InsertSlackChatAnchor(ctx, store.InsertSlackChatAnchorParams{
		RunID:     run.ID,
		ChannelID: m.ChannelID,
		RootTs:    m.MessageTS,
		StatusTs:  pgconv.TextOrNull(statusTS),
	}); err != nil {
		// Without the anchor a later thread reply resolves to no run and is silently
		// dropped — so DON'T ack (a ✅ would falsely signal "it landed"). Point the user
		// at the web Chat page, where the run (which does exist and is seeded) is visible.
		r.logf("insert chat anchor", err)
		r.postThreadReply(ctx, m, chatAnchorFailedText)
		return
	}

	// ✅ the opening message so the user sees it landed even before the worker replies.
	r.ack(ctx, m)
}

// openingStatusText picks the opener's status line: the normal "on it" copy, or —
// when the user has no worker connected (PRD #191 M6) — a line naming that cause, since
// the chat run then sits queued until a worker comes online. A worker-check error
// degrades to the neutral copy rather than a scary message.
func (r *Replier) openingStatusText(ctx context.Context, userID uuid.UUID) string {
	if online, err := r.svc.HasOnlineWorker(ctx, userID); err == nil && !online {
		return chatQueuedNoWorkerText
	}
	return chatStartedText
}

// submitChatTurn routes a thread reply on a chat run through SubmitChatMessage (PRD
// #191 Decision 5), turning it into the next conversation turn. Success acks; the two
// user-facing sentinels get a threaded explanation rather than a silent drop.
func (r *Replier) submitChatTurn(ctx context.Context, m MessageReply, userID uuid.UUID, anchor store.SlackRunMessage, text string) {
	err := r.svc.SubmitChatMessage(ctx, userID, anchor.RunID, text)
	switch {
	case err == nil:
		r.ack(ctx, m)
	case errors.Is(err, ErrChatTurnCapReached):
		r.postThreadReply(ctx, m, chatCapReachedText)
	case errors.Is(err, ErrChatEnded):
		r.postThreadReply(ctx, m, chatEndedText)
	default:
		r.logf("submit chat turn", err)
		r.postThreadReply(ctx, m, chatTurnFailedText)
	}
}

// postThreadReply posts a visible, scrubbed message in the conversation thread. For a
// thread reply it threads on the conversation root (m.ThreadTS); for a top-level DM it
// threads on the user's message (m.MessageTS), which IS the new root. Used for the
// opener's refusals/errors and the turn-cap/ended notices, which belong in the thread
// the user is looking at rather than as a vanishing ephemeral. Best-effort.
func (r *Replier) postThreadReply(ctx context.Context, m MessageReply, text string) {
	thread := m.ThreadTS
	if thread == "" {
		thread = m.MessageTS
	}
	if m.ChannelID == "" || thread == "" {
		return
	}
	if _, err := r.poster.Post(ctx, m.ChannelID, thread, ScrubSecrets(text)); err != nil {
		r.logf("post thread reply", err)
	}
}
