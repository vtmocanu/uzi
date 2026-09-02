package slacksvc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The run-health thread seam (PRD #47): handleHealth re-renders the run root on a
// health flip and threads a cooldown-gated nudge; ensureRoot posts/edits its anchor.

// handleHealth processes one run-health event (PRD #47 M4): re-render the run's root
// (⚠ flip on a flag, back on clear) and, when the sweeper marked the event
// nudge-worthy, thread exactly one DM. Delivery is re-resolved per event
// (GetSlackDeliveryForUser), so an owner who opted out mid-run gets nothing — the
// persisted anchor is never sufficient on its own (Decision 7). Every failure path
// logs redacted and returns; a run is never affected.
func (n *Notifier) handleHealth(ctx context.Context, ev healthEvent) {
	rc, err := n.store.GetSlackRunContext(ctx, ev.runID)
	if err != nil {
		// A chat run has no repo → ErrNoRows (as in handle); chat runs are never
		// health-flagged anyway. Skip silently; only a real error is logged.
		if !errors.Is(err, pgx.ErrNoRows) {
			n.logf("load run context (health)", err)
		}
		return
	}
	target, err := n.store.GetSlackDeliveryForUser(ctx, rc.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // unlinked, opted out mid-run, or unconfirmed → drop silently
	}
	if err != nil {
		n.logf("resolve delivery target (health)", err)
		return
	}
	if !target.Valid || target.String == "" {
		return
	}

	channel, err := n.poster.OpenDM(ctx, target.String)
	if err != nil {
		n.logf("open dm (health)", err)
		return
	}
	base, _ := n.baseURL(ctx)

	// Re-render the root so the ⚠️ flag reflects the current health (create it if a
	// stuck queued run never got a state DM). rootBlocks carries the flag as a
	// context element, keyed off the run context's current health.
	anchor, ok := n.ensureRoot(ctx, rc, channel, base)
	if !ok {
		return
	}

	if !ev.nudge {
		return
	}
	// One threaded nudge. approval_idle threads under the open gate message so the
	// Approve / Reject buttons are one scroll away (Decision 7); every other flag
	// threads under the root. The reason is server-authored; the whole message still
	// passes ScrubSecrets as a last line of defense.
	threadTS := anchor.RootTs
	if ev.health == healthApprovalIdle && anchor.GateTs.Valid && anchor.GateTs.String != "" {
		threadTS = anchor.GateTs.String
	}
	hblocks, hfallback := healthNudgeBlocks(ev.health, ev.reason, base, rc.ID)
	if _, perr := n.poster.PostBlocks(ctx, channel, threadTS, hfallback, hblocks); perr != nil {
		n.logf("post health nudge", perr)
	}
}

// ensureRoot returns the run's DM anchor, posting the root message when none exists
// yet (a stuck queued run that never sent a state DM still reaches its owner) or
// editing the existing one to the current health-aware render. ok is false on an
// unrecoverable error. It is the health path's counterpart to handle's inline anchor
// flow (which also threads terminal outcome events, so the two are not merged).
func (n *Notifier) ensureRoot(ctx context.Context, rc store.GetSlackRunContextRow, channel, base string) (store.SlackRunMessage, bool) {
	blocks, fallback := rootBlocks(rc, base)
	existing, err := n.store.GetSlackRunMessage(ctx, rc.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		ts, perr := n.poster.PostBlocks(ctx, channel, "", fallback, blocks)
		if perr != nil {
			n.logf("post root (health)", perr)
			return store.SlackRunMessage{}, false
		}
		saved, uerr := n.store.UpsertSlackRunMessage(ctx, store.UpsertSlackRunMessageParams{
			RunID: rc.ID, ChannelID: channel, RootTs: ts,
		})
		if uerr != nil {
			n.logf("record anchor (health)", uerr)
			return store.SlackRunMessage{RunID: rc.ID, ChannelID: channel, RootTs: ts}, true
		}
		return saved, true
	case err != nil:
		n.logf("load anchor (health)", err)
		return store.SlackRunMessage{}, false
	default:
		if uerr := n.poster.UpdateBlocks(ctx, existing.ChannelID, existing.RootTs, fallback, blocks); uerr != nil {
			n.logf("update root (health)", uerr)
		}
		return existing, true
	}
}
