package slacksvc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// MessageReply is one inbound message.im thread reply, resolved from a Socket Mode
// Events-API envelope. SlackUserID is the AUTHENTICATED envelope author (ev.User)
// — never a client-controlled value — which the confirmed-user + ownership join
// relies on. The shape is intentionally minimal (the content is worker-bound).
type MessageReply struct {
	SlackUserID string // ev.User — the Slack-authenticated author
	ChannelID   string // the DM channel
	ThreadTS    string // the parent thread ts == the run's root message ts
	MessageTS   string // this reply's ts (for the ✅ ack reaction)
	Text        string // the reply body
}

// MessageHandler routes an inbound thread reply from the Socket Mode receive loop.
// *Replier is the production implementation.
type MessageHandler interface {
	HandleMessage(ctx context.Context, m MessageReply)
}

// ReplierStore is the slice of generated queries the replier reads/writes.
// *store.Queries satisfies it.
type ReplierStore interface {
	GetConfirmedUserBySlackID(ctx context.Context, slackResolvedID pgtype.Text) (store.User, error)
	GetSlackRunMessageByRoot(ctx context.Context, arg store.GetSlackRunMessageByRootParams) (store.SlackRunMessage, error)
	SetSlackRunGate(ctx context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error)
	// SetSlackRunGateIf is the compare-and-swap accept for a revise-feedback reply
	// (PRD #41 Decision 10c): a revision keeps the run awaiting_approval, so two
	// replies both pass the status guard — the CAS clears revise_pending only if the
	// anchor still shows it at the same gate_ts, so exactly one reply wins.
	SetSlackRunGateIf(ctx context.Context, arg store.SetSlackRunGateIfParams) (store.SlackRunMessage, error)
}

// gateNudgeText is the bare-reply nudge shown while the gate is open but takes no
// threaded verdict directly — it names all THREE actions (PRD #41 Decision 10).
const gateNudgeText = "The plan gate takes Approve, Request changes, or Reject — a bare reply isn't a verdict. " +
	"Press Request changes then reply with what to change, or Reject then reply with the reason, or open the run in uzi."

const (
	// maxReplyRunes bounds an accepted reply before it becomes a reject_plan reason
	// or a follow_up (worker-bound input hygiene — a Slack reply is untrusted free
	// text with no length bound of its own). The reply never goes back out to Slack.
	maxReplyRunes = 2000
	// ackReaction is the emoji added to an accepted reply as its ack.
	ackReaction = "white_check_mark"
	// inboundWindow/inboundMax bound how many message.im events one Slack user can
	// drive per window (flood protection); excess is dropped silently.
	inboundWindow = time.Minute
	inboundMax    = 20
	// coalesceTTL bounds how often the same "not linked" / "run finished" ephemeral
	// is re-sent to one Slack user, so a burst of replies is answered once, not N×.
	coalesceTTL = 10 * time.Minute
)

// Replier handles inbound Slack thread replies (PRD #25 M5). For each reply it
// re-resolves the authenticated author to their confirmed, active uzi user,
// verifies they OWN the run anchored at the thread (never trusting the anchor or
// gate_state alone), and routes: a reasoned reject_plan when the gate is
// reject-pending, a follow_up on a live run, a nudge during an open gate, or a
// coalesced ephemeral for an unlinked author or a finished run. Every accepted
// reply gets a ✅ ack. Best-effort on the Slack side; the run-affecting submit
// rides workersvc's ownership-checked SubmitInput.
type Replier struct {
	store  ReplierStore
	svc    PlanGateSubmitter
	poster Poster
	logger *slog.Logger

	mu      sync.Mutex
	inbound map[string]*floodWindow // per-Slack-user flood window
	told    map[string]time.Time    // per (user, notice-kind) coalesce memory
}

type floodWindow struct {
	count int
	reset time.Time
}

// NewReplier builds a Replier. svc is the ownership-checked run service (the same
// adapter the gatekeeper uses); poster is the shared bot-token Slack surface.
func NewReplier(s ReplierStore, svc PlanGateSubmitter, poster Poster, logger *slog.Logger) *Replier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Replier{
		store: s, svc: svc, poster: poster, logger: logger,
		inbound: make(map[string]*floodWindow), told: make(map[string]time.Time),
	}
}

// HandleMessage routes one inbound thread reply. The envelope author is the only
// identity input; the run id comes from the thread anchor, authorized through the
// ownership-scoped GetRun, so a reply can only ever act on a run the author owns.
func (r *Replier) HandleMessage(ctx context.Context, m MessageReply) {
	slackID := strings.TrimSpace(m.SlackUserID)
	if slackID == "" {
		return
	}
	// Flood protection: drop events beyond the per-user budget.
	if !r.allowInbound(slackID) {
		return
	}

	// Re-resolve the author to their confirmed, ACTIVE uzi user. This single
	// guarded lookup also enforces deactivation (a deactivated account resolves to
	// no row, so it cannot act on a run from Slack). Unlinked → coalesced notice.
	user, err := r.store.GetConfirmedUserBySlackID(ctx, pgText(slackID))
	if errors.Is(err, pgx.ErrNoRows) {
		r.coalescedEphemeral(ctx, m, "unlinked",
			"This Slack account isn't linked to uzi — open uzi → Settings → Notifications to link it.")
		return
	}
	if err != nil {
		r.logf("resolve confirmed user", err)
		return
	}

	// Resolve the run anchored at this thread (thread_ts == root_ts). No anchor →
	// the reply isn't under a run DM; ignore silently. (channel_id, root_ts) is
	// effectively unique: each run posts its OWN root message, so its ts is distinct
	// within the DM channel — hence the :one lookup can never straddle two runs.
	anchor, err := r.store.GetSlackRunMessageByRoot(ctx, store.GetSlackRunMessageByRootParams{
		ChannelID: m.ChannelID, RootTs: m.ThreadTS,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		r.logf("resolve run by thread", err)
		return
	}

	// Ownership: the confirmed author must OWN the anchored run — never trust the
	// anchor (or gate_state) alone. GetRun is user-scoped, so a foreign run (e.g.
	// after a mid-run re-link) resolves to nothing and the reply is refused.
	run, err := r.svc.GetRun(ctx, user.ID, anchor.RunID)
	if err != nil {
		r.coalescedEphemeral(ctx, m, "notyours", "That run isn't yours.")
		return
	}

	text := boundReply(m.Text)

	switch {
	case run.Status == "awaiting_approval" && anchor.GateState.Valid && anchor.GateState.String == gateStateRevisePending:
		// Revise-pending AND the run is still parked: the reply IS the revision feedback
		// (PRD #41 Decision 10c). Because a revision KEEPS the run awaiting_approval,
		// two replies would both reach here — so accept via compare-and-swap: atomically
		// clear revise_pending only if the anchor still shows it at the same gate_ts.
		// Exactly one reply wins; the loser (no row) falls through to the nudge. On a win
		// we do NOT resolveGate — the run stays parked and the gate re-opens with the
		// next plan version (a higher generation); the gate_ts is cleared so the
		// notifier's cross-surface close can't fire during the revise turn.
		if _, err := r.store.SetSlackRunGateIf(ctx, store.SetSlackRunGateIfParams{
			RunID: anchor.RunID, GateTs: pgtype.Text{}, GateState: pgtype.Text{},
			ExpectedGateTs: anchor.GateTs, ExpectedGateState: anchor.GateState,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Lost the CAS — another reply already took the revision; nudge, don't submit.
				r.coalescedEphemeral(ctx, m, "gate-open", gateNudgeText)
				return
			}
			r.logf("cas revise accept", err)
			return
		}
		if err := r.svc.SubmitInput(ctx, user.ID, anchor.RunID, "revise_plan", text); err != nil {
			if errors.Is(err, ErrReviseCapReached) {
				r.editGateMessage(ctx, anchor, "🔁 Revision limit reached — approve or reject this plan in uzi.")
				r.ephemeral(ctx, m, "You've hit the plan-revision limit for this run — approve or reject the current plan instead.")
				return
			}
			r.logf("submit revise", err)
			return
		}
		r.editGateMessage(ctx, anchor, "🔁 Revising the plan with your feedback…")
		r.ack(ctx, m)

	case run.Status == "awaiting_approval" && anchor.GateState.Valid && anchor.GateState.String == gateStateRejectPending:
		// Reject-pending AND the run is still parked: the reply IS the reasoned
		// rejection. Submit it, resolve the gate, ack; the reason goes only to the
		// worker, never echoed back to Slack. The run.Status guard mirrors the
		// gatekeeper's stale-check: if the gate was resolved from another surface
		// while reject-pending and the notifier hasn't yet cleared the anchor, this
		// stale reply must NOT submit reject_plan (which could wrongly fail a run that
		// already left the gate) — it falls through to the branches below instead.
		if err := r.svc.SubmitInput(ctx, user.ID, anchor.RunID, "reject_plan", text); err != nil {
			r.logf("submit reasoned reject", err)
			return
		}
		r.resolveGate(ctx, anchor, "❌ Plan rejected.")
		r.ack(ctx, m)

	case run.Status == "awaiting_approval":
		// Open gate (not reject-pending): a bare reply is NOT a plan verdict — the
		// worker queues a follow_up submitted during a gate rather than consuming it
		// as feedback (verified in agent/src/steering.ts), so nudge, don't submit.
		// Coalesced like the other notices so a burst is nudged once.
		r.coalescedEphemeral(ctx, m, "gate-open", gateNudgeText)

	case isTerminalStatus(run.Status):
		r.coalescedEphemeral(ctx, m, "finished", "That run has already finished.")

	default:
		// Live run, no gate → follow_up for the next implement turn.
		if err := r.svc.SubmitInput(ctx, user.ID, anchor.RunID, "follow_up", text); err != nil {
			r.logf("submit follow_up", err)
			return
		}
		r.ack(ctx, m)
	}
}

// resolveGate edits the gate message button-free and clears the anchor, so a later
// stale click / the notifier's transition consumer is a no-op. Best-effort.
func (r *Replier) resolveGate(ctx context.Context, anchor store.SlackRunMessage, text string) {
	if anchor.GateTs.Valid && anchor.GateTs.String != "" {
		if err := r.poster.UpdateBlocks(ctx, anchor.ChannelID, anchor.GateTs.String, text, gateResolvedBlocks(text)); err != nil {
			r.logf("edit gate to resolved", err)
		}
	}
	if _, err := r.store.SetSlackRunGate(ctx, store.SetSlackRunGateParams{RunID: anchor.RunID}); err != nil {
		r.logf("clear gate", err)
	}
}

// editGateMessage edits the gate message to a neutral, button-free state WITHOUT
// clearing the anchor — used by the revise-accept path, where the CAS already
// transitioned the anchor and the run stays parked for the next plan version (PRD
// #41). Best-effort; a NULL gate_ts is a no-op.
func (r *Replier) editGateMessage(ctx context.Context, anchor store.SlackRunMessage, text string) {
	if anchor.GateTs.Valid && anchor.GateTs.String != "" {
		if err := r.poster.UpdateBlocks(ctx, anchor.ChannelID, anchor.GateTs.String, text, gateResolvedBlocks(text)); err != nil {
			r.logf("edit gate message", err)
		}
	}
}

// ack adds the ✅ reaction to an accepted reply. Best-effort.
func (r *Replier) ack(ctx context.Context, m MessageReply) {
	if m.ChannelID == "" || m.MessageTS == "" {
		return
	}
	if err := r.poster.AddReaction(ctx, m.ChannelID, m.MessageTS, ackReaction); err != nil {
		r.logf("add ack reaction", err)
	}
}

// ephemeral posts a notice only the author sees (fixed text, secret-scrubbed).
func (r *Replier) ephemeral(ctx context.Context, m MessageReply, text string) {
	if m.ChannelID == "" || m.SlackUserID == "" {
		return
	}
	if err := r.poster.PostEphemeral(ctx, m.ChannelID, m.SlackUserID, ScrubSecrets(text)); err != nil {
		r.logf("post ephemeral", err)
	}
}

// coalescedEphemeral sends an ephemeral at most once per coalesceTTL per (author,
// kind), so a burst of replies from an unlinked author (or on a finished run) is
// answered once rather than per event.
func (r *Replier) coalescedEphemeral(ctx context.Context, m MessageReply, kind, text string) {
	if r.shouldTell(m.SlackUserID, kind) {
		r.ephemeral(ctx, m, text)
	}
}

// allowInbound reports whether slackID is within its per-window event budget,
// recording the hit. Pruning on each call bounds both state maps to the live rate.
func (r *Replier) allowInbound(slackID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.pruneLocked(now)
	w := r.inbound[slackID]
	if w == nil || now.After(w.reset) {
		r.inbound[slackID] = &floodWindow{count: 1, reset: now.Add(inboundWindow)}
		return true
	}
	w.count++
	return w.count <= inboundMax
}

// shouldTell reports whether a coalesced notice of kind for slackID is outside its
// TTL, recording the send when it is.
func (r *Replier) shouldTell(slackID, kind string) bool {
	if slackID == "" {
		return false
	}
	key := slackID + ":" + kind
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if at, ok := r.told[key]; ok && now.Sub(at) < coalesceTTL {
		return false
	}
	r.told[key] = now
	return true
}

// pruneLocked evicts expired flood windows and coalesce entries. Caller holds mu.
func (r *Replier) pruneLocked(now time.Time) {
	for k, w := range r.inbound {
		if now.After(w.reset) {
			delete(r.inbound, k)
		}
	}
	for k, at := range r.told {
		if now.Sub(at) >= coalesceTTL {
			delete(r.told, k)
		}
	}
}

func (r *Replier) logf(what string, err error) {
	r.logger.Warn("slack replier: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// boundReply sanitizes an accepted reply before it becomes a worker-bound reject
// reason / follow_up: trim, scrub credential patterns (defense in depth — a secret
// a user pastes into a Slack reply must not propagate into run inputs or agent
// context), then length-cap. Untrusted free text; it is never echoed back to Slack.
func boundReply(s string) string {
	s = ScrubSecrets(strings.TrimSpace(s))
	rn := []rune(s)
	if len(rn) > maxReplyRunes {
		return string(rn[:maxReplyRunes])
	}
	return s
}

// isTerminalStatus reports whether a run status accepts no further steering input.
func isTerminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// ensure *Replier satisfies MessageHandler at compile time.
var _ MessageHandler = (*Replier)(nil)
