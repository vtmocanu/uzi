package slacksvc

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
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
	// InsertSlackChatAnchor records the DM anchor for a Slack-originated chat (PRD
	// #191 M2): root_ts is the user's opening message, status_ts the bot's status
	// message (Decision 2).
	InsertSlackChatAnchor(ctx context.Context, arg store.InsertSlackChatAnchorParams) (store.SlackRunMessage, error)
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

	// chatAllow draws the opener from the SHARED per-user chat spend budget (PRD #191
	// Decision 9): main wires it to chatLimiter.Allow keyed identically to the web
	// CreateChat route, so a heavy Slack day rate-limits the web Chat page and vice
	// versa. Nil (tests, or a deployment that never wires it) means unlimited — the
	// opener never blocks on a missing guard.
	chatAllow func(userID uuid.UUID) bool

	// steer is the shared pending-steer registry (PRD #322 M4). A reply in a chat thread
	// with a live pending armed by the SAME user is submitted as a follow_up to the
	// pending's TARGET issue run — NOT treated as a chat turn — so a steer card posted in
	// the chat thread routes its reply to a different run. Nil (tests, or a deployment
	// that never wires it) means no steer interception; replies fall through unchanged.
	steer *SteerPendings
}

// SetChatSpendGuard wires the per-user chat spend guard for the top-level-DM opener
// (PRD #191 M2, Decision 9). Call once at startup, before the socket manager serves;
// pass a closure over the SAME chatLimiter the web /chats route mounts, keyed
// identically, so web and Slack share one budget.
func (r *Replier) SetChatSpendGuard(allow func(userID uuid.UUID) bool) { r.chatAllow = allow }

// SetSteerPending wires the shared pending-steer registry for the steer-reply
// interception (PRD #322 M4). Pass the SAME *SteerPendings the ChatActions Steer button
// arms, so a reply here consumes a pending that press created.
func (r *Replier) SetSteerPending(s *SteerPendings) { r.steer = s }

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

	// A top-level DM (no thread_ts) is NOT a reply to a run — it opens a new chat
	// conversation (PRD #191 M2, Decision 1/3). Continuing an existing chat happens by
	// replying IN its thread, which carries a thread_ts and falls through to the
	// anchor-resolution path below.
	if strings.TrimSpace(m.ThreadTS) == "" {
		r.openChat(ctx, m, user)
		return
	}

	// Steer interception (PRD #322 M4): a Steer button pressed on a chat card arms a
	// one-shot pending keyed by this chat thread. This MUST run before the chat-turn arm
	// below — the steer card lives in the CHAT thread, so its reply would otherwise become
	// a chat turn and the target run_id would be lost. Take is user-scoped and one-shot, so
	// only the requester's reply consumes it; a wrong-user reply leaves it armed. Guarding
	// on text != "" means a scrub-to-empty reply does NOT consume the pending (it stays
	// armed for the real instruction), satisfying "an empty reply is a no-op". An empty
	// ev.Text never reaches here — routeMessage drops it (socket.go).
	if r.steer != nil {
		if text := boundReply(m.Text); text != "" {
			if targetRunID, ok := r.steer.Take(m.ChannelID, m.ThreadTS, user.ID); ok {
				if err := r.svc.SteerRunFromCard(ctx, user.ID, targetRunID, text); err != nil {
					// The adapter built a user-safe message (chat-run target, foreign/terminal run).
					r.ephemeral(ctx, m, err.Error())
					return
				}
				r.ack(ctx, m)
				return
			}
		}
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

	// Chat runs branch BEFORE the status switch (PRD #191 Decision 5). A chat's status
	// is queued/claimed/running, which would land in the default: arm and submit a raw
	// follow_up — which the service now REJECTS for a chat run (ErrChatInputNotAllowed).
	// Route the reply through SubmitChatMessage so it becomes a turn, not a 409.
	if run.Kind == runKindChat {
		r.submitChatTurn(ctx, m, user.ID, anchor, text)
		return
	}

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
			// A transient submit failure AFTER the CAS already cleared revise_pending: the
			// feedback did not persist and the anchor is no longer revise-pending, so a
			// re-reply would only hit the open-gate nudge. Surface the failure instead of
			// dropping it silently — the user can press Request changes again or use uzi.
			r.logf("submit revise", err)
			r.ephemeral(ctx, m, "Couldn't record your feedback — press Request changes again, or open the run in uzi.")
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

	case run.Status == "awaiting_input":
		// The run is parked on a clarification question and the reply IS the answer (PRD
		// #88 M3). This case MUST precede the default arm: the default submits a follow_up,
		// which the worker queues for the NEXT implement turn — a turn that never arrives,
		// because the run is parked waiting for the answer that just became a follow-up.
		// The run would then sit until its deadline having visibly ignored the user.
		//
		// Unlike both awaiting_approval cases above this is a BARE status check: those are
		// compound because a plan revision keeps the run at awaiting_approval, so the
		// status alone cannot tell an open gate from a reject/revise-pending one. A
		// question has no such sub-state — awaiting_input means exactly one thing.
		r.answer(ctx, m, user.ID, anchor, text)

	default:
		// Live run, no gate → follow_up for the next implement turn.
		if err := r.svc.SubmitInput(ctx, user.ID, anchor.RunID, "follow_up", text); err != nil {
			r.logf("submit follow_up", err)
			return
		}
		r.ack(ctx, m)
	}
}

// answer submits a threaded reply as the answer to a clarification question (PRD #88
// M3). Everything here turns on ONE decision: which question a piece of free text
// answers.
//
// Web and CLI do not have this problem — they echo back the question id they were
// shown, so the answer names itself. A Slack reply carries no id, so the id must be
// DERIVED, and the obvious derivation is wrong. "Whichever question is open when the
// reply arrives" is an ARRIVAL-TIME key wearing identity's clothes: a reply written
// against Q1 that lands after Q2 opened would be stamped with Q2's id by the server,
// and would then satisfy every equality check downstream precisely because the server
// is what supplied the id. That is the race identity keying exists to close, re-opened
// from the one direction the guard cannot see.
//
// The correct derivation is the question the reply FOLLOWS, ordered by ts against the
// card the notifier posted:
//
//   - reply after the current question's message ⇒ it answers that question;
//   - reply before it ⇒ it answers a superseded one ⇒ refuse, and say so.
//
// It survives a requeue for free, which is the property every clock-based scheme
// failed: the re-park re-uses the question id, so the notifier's identity dedupe does
// NOT re-post, so the recorded ts still points at the ORIGINAL card — and an answer
// written before a worker death is still after it, and is still honoured.
//
// The id submitted is the ANCHOR's, i.e. the question the user actually saw, not the
// run's currently-open one. The server then checks it against runs.open_question_id,
// so the two facts stay independent: slacksvc says what was answered, workersvc says
// whether that is still the open question. A reply to a card the run has already moved
// past is rejected by the server rather than silently retargeted.
//
// Two replies racing before the worker consumes the first are BOTH accepted (same
// question, run still parked) and the worker takes ONE of them, discarding the rest —
// deliberately not a compare-and-swap: unlike a plan revision the loser is harmless,
// being one more consumed input the worker drops.
//
// WHICH one it takes depends on whether they arrive in the same poll, and the precise
// answer is worth having because "first-wins" (what this comment used to say) is only
// half true. SteeringChannel.route assigns a SINGLE buffer slot per routed input, and
// pollLoop routes an entire /inputs batch before servicing the waiter once. So within
// one batch the LAST reply overwrites the earlier ones and wins; across batches the
// first wins, because it has already resolved the park and the later reply arrives
// with nothing waiting on it and no matching open question.
//
// Not a security property either way — same authenticated owner, same question, and
// the server has already bound the reply to the anchor's question id. It is recorded
// because it is a present-tense claim about a concurrency path, and the previous
// wording would send someone debugging a "lost" reply looking for a bug that is
// actually the documented behaviour.
func (r *Replier) answer(ctx context.Context, m MessageReply, userID uuid.UUID, anchor store.SlackRunMessage, text string) {
	qid := strings.TrimSpace(anchor.QuestionID.String)
	qts := strings.TrimSpace(anchor.QuestionTs.String)
	if qid == "" || qts == "" {
		// The run is parked but no question card is recorded on this thread: the post
		// failed, or the state report outran the question message and the notifier is still
		// waiting. There is nothing to bind the reply to, and guessing is exactly what this
		// function refuses to do.
		r.ephemeral(ctx, m, "I haven't posted that question here yet — open the run in uzi to answer it.")
		return
	}
	if !replyFollows(m.MessageTS, qts) {
		// Written against an earlier question, or unorderable. Refusing is the whole point:
		// this is the case a correct implementation must DISCARD, and the one the arrival-
		// time derivation would have silently applied to the live question instead.
		r.coalescedEphemeral(ctx, m, "stale-answer",
			"That reply came before the question above it — scroll down to the latest question and answer there.")
		return
	}
	if text == "" {
		// Empty after trim/scrub (a file-only or emoji-only message). Submitting it would
		// resolve the question with no content and let the run continue on nothing.
		r.coalescedEphemeral(ctx, m, "empty-answer",
			"That reply had no text — answer in words and I'll pass it to the run.")
		return
	}
	err := r.svc.SubmitAnswer(ctx, userID, anchor.RunID, qid, text)
	switch {
	case err == nil:
		r.ack(ctx, m)
	case errors.Is(err, ErrAnswerStale), errors.Is(err, ErrNotAwaitingInput):
		// Lost a race to another surface between the status read and this submit. Surfaced
		// rather than dropped: no ✅, and a notice that says which way it went.
		r.ephemeral(ctx, m, "The run already moved on from that question — open it in uzi to see where it is.")
	default:
		r.logf("submit answer", err)
		r.ephemeral(ctx, m, "Couldn't record your answer — try again, or answer from uzi.")
	}
}

// replyFollows reports whether a reply's ts comes strictly after the question card's.
//
// Slack ts values are "<epoch-seconds>.<microseconds>" and are compared NUMERICALLY,
// not lexicographically: string ordering happens to agree only while the seconds part
// has a fixed digit count, which is a property of the current decade rather than of the
// format. Anything that cannot be ordered is treated as NOT following — the ordering is
// what binds an answer to a question, so an ordering that cannot be established must
// refuse rather than default to accepting.
//
// "Cannot be ordered" means unparseable OR non-finite, and the second half is the one
// that is not decorative. Measured: a parse failure yields 0, which is already before
// any real card, so the error check alone changes no outcome — but `strconv.ParseFloat`
// accepts "Inf"/"+Inf"/"Infinity" with a NIL error, and +Inf is after every card ts, so
// an error-only guard would ACCEPT it. A check that reads like a safety guard while
// deciding nothing is exactly the shape D-R struck elsewhere in this PRD; finiteness is
// what makes this one live.
func replyFollows(replyTS, questionTS string) bool {
	r, rok := parseSlackTS(replyTS)
	q, qok := parseSlackTS(questionTS)
	return rok && qok && r > q
}

// parseSlackTS parses a Slack message ts to an orderable number, reporting false for
// anything that cannot participate in an ordering.
func parseSlackTS(ts string) (float64, bool) {
	v, err := strconv.ParseFloat(ts, 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
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
