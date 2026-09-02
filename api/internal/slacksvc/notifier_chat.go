package slacksvc

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The chat status-line seam (PRD #191 M2b): handleChat edits a Slack-anchored chat
// run's status message on a transition, distinct from chatpost.go's handleChatMsg turn stream.

// handleChat renders a Slack-anchored chat run's status line on a state transition
// (PRD #191 M2b). It is a PARALLEL path to handle's repo-ful renderer: a chat has no
// repo, forge, issue identity or deep link, so nothing downstream of GetSlackRunContext
// applies, and the anchor already knows the DM channel (no delivery-target resolution —
// a user who opened a chat in Slack is confirmed and wants the reply).
//
// It edits the bot's OWN status message (status_ts), NEVER the user's root_ts message
// (Decision 2: a bot cannot edit a user's message). A chat with no anchor is
// web-originated and keeps skipping exactly as before. Only terminal transitions render
// a line today (renderChatStatus returns "" otherwise) — the opener posted the initial
// status and M3 streams the turns. All best-effort: a failure is logged and never
// affects the run.
func (n *Notifier) handleChat(ctx context.Context, ev stateEvent) {
	cc, err := n.store.GetSlackChatContext(ctx, ev.runID)
	if err != nil {
		// Not a chat (a judge/self_improve run, or one deleted under us) → the original
		// silent skip; a real DB error is logged.
		if !errors.Is(err, pgx.ErrNoRows) {
			n.logf("load chat context", err)
		}
		return
	}
	line := renderChatStatus(cc)
	if line == "" {
		return // non-terminal chat state: the opener + M3 own the live view
	}
	// (The turn-streaming state is freed by handle's deferred evictChatConvo, which
	// fires for every terminal transition regardless of kind.)
	anchor, err := n.store.GetSlackRunMessage(ctx, ev.runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // web-originated chat, no Slack DM → skip exactly as before
	}
	if err != nil {
		n.logf("load chat anchor", err)
		return
	}
	if !anchor.StatusTs.Valid || anchor.StatusTs.String == "" {
		// No bot status message to edit (the open-time post failed, a rare case). We do
		// NOT post a fresh line here: a new Post would bypass the opt-out/deactivation
		// gate the issue path honours (a status edit only finishes a message already in
		// the DM the user opened) and would have no dedup key, so a redelivered terminal
		// transition would duplicate it. The run stays visible in the web Chat page.
		return
	}
	// Edit the bot status message in place, swapping the End button for a Continue
	// button (PRD #191 M6): a terminal chat offers Continue, which mints a fresh chat
	// resuming this one. NEVER Update root_ts (the user's message). A redelivered
	// terminal transition is an idempotent edit-to-same-blocks.
	if err := n.poster.UpdateBlocks(ctx, anchor.ChannelID, anchor.StatusTs.String,
		ScrubSecrets(line), chatEndedStatusBlocks(ev.runID, ScrubSecrets(line))); err != nil {
		n.logf("update chat status", err)
	}
}

// renderChatStatus is the chat status line for a transition, or "" for a non-terminal
// state the notifier does not narrate (PRD #191 M2b). Terminal states get a short,
// content-minimized line; a failure reason is included but scrubbed by the caller.
func renderChatStatus(cc store.GetSlackChatContextRow) string {
	switch cc.Status {
	case "completed":
		// Idle backstop (CHAT_IDLE_TIMEOUT) or the turn cap (CHAT_MAX_TURNS): both land
		// the run in completed. Continue picks it back up.
		return "✅ This conversation ended (it went quiet, or hit its turn limit). Continue it to pick up where it left off."
	case "cancelled":
		return "🛑 You ended this conversation. Continue it to pick up where it left off."
	case "failed":
		msg := "⚠️ This chat run failed."
		if cc.FailureReason.Valid {
			if reason := strings.TrimSpace(cc.FailureReason.String); reason != "" {
				msg += " " + reason
			}
		}
		return msg + " You can Continue it to try again."
	default:
		return ""
	}
}
