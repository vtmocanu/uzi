package slacksvc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// GateStore is the slice of generated queries the gatekeeper reads/writes.
// *store.Queries satisfies it.
type GateStore interface {
	// GetConfirmedUserBySlackID resolves the Slack-authenticated actor to their
	// EXACTLY-ONE confirmed uzi user (the unique partial index makes it at most one;
	// the confirmed filter is what makes it an authz join). pgx.ErrNoRows = the
	// account is not linked, and the action is refused rather than guessed.
	GetConfirmedUserBySlackID(ctx context.Context, slackResolvedID pgtype.Text) (store.User, error)
	// GetSlackRunMessage reads the run's DM anchor so a gate click can be verified
	// against the LIVE gate_ts (PRD #41 Decision 10b): a click whose message ts no
	// longer matches the anchor is superseded and refused server-side, independent of
	// the best-effort message edit.
	GetSlackRunMessage(ctx context.Context, runID uuid.UUID) (store.SlackRunMessage, error)
	// SetSlackRunGate clears the gate anchor (a terminal resolve). The reject/revise-pending
	// TRANSITION writes use the compare-and-swap SetSlackRunGateIf instead.
	SetSlackRunGate(ctx context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error)
	// SetSlackRunGateIf is the compare-and-swap transition write (PRD #41 Decision 10d):
	// enter reject/revise-pending ONLY if the anchor still shows the gate the click acted
	// on (gate_ts == the clicked message, still open). A concurrent cross-surface re-gate
	// (a web-driven or timed-out revise) can advance the anchor to a newer plan generation
	// between the supersede read below and this write; the CAS makes such a stale click
	// LOSE (no row) rather than revert gate_ts to the superseded message and orphan the
	// live gate — so every gate-anchor write is generation-guarded, not just the read.
	SetSlackRunGateIf(ctx context.Context, arg store.SetSlackRunGateIfParams) (store.SlackRunMessage, error)
}

// PlanGateSubmitter is the slice of the run service the gatekeeper drives: read a
// run (ownership-scoped, for the stale-click status check), submit a reject
// steering input, and submit an approve carrying the agent SOURCE. *workersvc.Service
// satisfies it through a thin adapter in main, keeping slacksvc free of a workersvc
// import — SubmitApproval takes the source as a plain string, and the adapter builds
// the workersvc.AgentSelection.
type PlanGateSubmitter interface {
	GetRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error)
	SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string) error
	// SubmitApproval enqueues an approve_plan for `source` ("repo"|"own"); the server
	// validates the source against the run's real roster and rejects with the
	// ErrSelectionRejected sentinel when it no longer holds.
	SubmitApproval(ctx context.Context, userID, runID uuid.UUID, source string) error
	// SubmitAnswer enqueues an `answer` to the clarification question named by
	// questionID, carrying the user's reply text (PRD #88 M3).
	//
	// It takes the id and the text as plain strings for the same reason SubmitApproval
	// takes a source string: the JSON body is workersvc's own AnswerBody, built by the
	// adapter in main, so the wire shape is declared EXACTLY ONCE and Slack cannot drift
	// into being a second contract. The server re-validates questionID against the run's
	// open_question_id and re-encodes from its own scrubbed values, so what is passed
	// here is a request, never a trusted record.
	//
	// A run that has moved off that question is rejected with the ErrAnswerStale /
	// ErrNotAwaitingInput sentinels rather than a generic error, so the replier can say
	// which happened.
	SubmitAnswer(ctx context.Context, userID, runID uuid.UUID, questionID, text string) error

	// LiveChatForUser reports the user's newest non-terminal chat run, if any (PRD
	// #191 M2, Decision 3). The Slack opener refuses a second top-level DM while a
	// chat is live rather than minting a second run.
	LiveChatForUser(ctx context.Context, userID uuid.UUID) (store.Run, bool, error)
	// HasOnlineWorker reports whether the user has a worker online (PRD #191 M6): the
	// opener names "no worker connected" as the reason a fresh chat sits queued.
	HasOnlineWorker(ctx context.Context, userID uuid.UUID) (bool, error)
	// CreateChatRun queues a new chat run seeded with the opening message, returning
	// the run (its id anchors the DM). It rides the same ownership-scoped service the
	// web Chat page uses; the Slack path draws from the shared per-user chat spend
	// budget BEFORE calling it (Decision 9).
	CreateChatRun(ctx context.Context, userID uuid.UUID, message string) (store.Run, error)
	// SubmitChatMessage records a thread reply as the next chat turn (Decision 5),
	// enforcing the turn cap and the terminal 409 at the service boundary. It
	// translates the two user-facing sentinels so the replier can say which happened:
	// ErrChatTurnCapReached (the cap) and ErrChatEnded (a terminal conversation).
	SubmitChatMessage(ctx context.Context, userID, runID uuid.UUID, message string) error
}

// Gatekeeper handles the Slack approval-gate buttons (PRD #25 M4): Approve,
// Reject, and Reject-without-reason. It is an InboundHandler wired into the
// Manager alongside the Linker (via InboundMux); it ignores non-gate actions. It
// is best-effort on the Slack side (edits/ephemerals are logged, not fatal) but
// the run-affecting submit rides workersvc's ownership-checked SubmitInput, so no
// button can act on a run whose owner isn't the confirmed-linked presser.
type Gatekeeper struct {
	store  GateStore
	svc    PlanGateSubmitter
	poster Poster
	logger *slog.Logger
}

// NewGatekeeper builds a Gatekeeper. poster is the shared bot-token Slack surface.
func NewGatekeeper(s GateStore, svc PlanGateSubmitter, poster Poster, logger *slog.Logger) *Gatekeeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gatekeeper{store: s, svc: svc, poster: poster, logger: logger}
}

// HandleBlockAction routes a gate button press. It ACK-relies on the socket loop
// (the envelope was ACKed before this ran) and uses the Slack-authenticated actor
// id ONLY — never the value blob, which carries just the run id. The run id is
// authorized through SubmitInput's ownership scope, so a forged id can only ever
// act on a run the presser owns. Non-gate actions (the linker's, the Open-in-uzi
// url button) are ignored.
func (g *Gatekeeper) HandleBlockAction(ctx context.Context, a BlockAction) {
	if !isGateAction(a.ActionID) {
		return
	}
	slackID := strings.TrimSpace(a.SlackUserID)
	if slackID == "" {
		return // no authenticated actor
	}

	// Resolve the actor to their exactly-one confirmed uzi user. Unlinked → refuse
	// with an ephemeral notice, never guess.
	user, err := g.store.GetConfirmedUserBySlackID(ctx, pgText(slackID))
	if errors.Is(err, pgx.ErrNoRows) {
		g.ephemeral(ctx, a, "This Slack account isn't linked to uzi — open uzi → Settings → Notifications to link it.")
		return
	}
	if err != nil {
		g.logf("resolve confirmed user", err)
		return
	}

	runID, err := uuid.Parse(strings.TrimSpace(a.Value))
	if err != nil {
		g.logf("parse run id", err)
		return
	}

	// Ownership-scoped read: a run the presser doesn't own resolves to no row.
	run, err := g.svc.GetRun(ctx, user.ID, runID)
	if err != nil {
		g.ephemeral(ctx, a, "That run isn't yours, or it no longer exists.")
		return
	}

	// Stale-click: SubmitInput does NOT verify awaiting_approval for approves, so
	// the gatekeeper checks it. Anything else means the gate was resolved from
	// another surface (web UI, timeout, sweeper) already.
	if run.Status != "awaiting_approval" {
		g.ephemeral(ctx, a, "This plan was already handled (the run is now "+EscapeMrkdwn(run.Status)+").")
		return
	}

	// Server-enforced supersede (PRD #41 Decision 10b): the run can be parked at
	// awaiting_approval across MULTIPLE plan generations (a revision re-gates without
	// leaving the status), so the status guard alone can't tell a live gate from a
	// stale one. Refuse any click whose message ts no longer matches the anchor's
	// live gate_ts — the plan it was showing has been superseded by a newer one. This
	// makes the no-unseen-plan guarantee independent of the best-effort message edit
	// and stops a stale Reject/Request-changes from overwriting the live anchor.
	anchor, err := g.store.GetSlackRunMessage(ctx, runID)
	if err != nil || !anchor.GateTs.Valid || anchor.GateTs.String != a.MessageTS {
		g.ephemeral(ctx, a, "This gate was superseded — scroll down to the latest plan message.")
		return
	}

	switch a.ActionID {
	// The approve id encodes the agent source (PRD #37 M7), from a CLOSED set — the
	// server never receives a client-supplied source string. The legacy/no-roster
	// id maps to "own".
	case ActionGateApprove, ActionGateApproveOwn:
		g.approve(ctx, a, user.ID, runID, "own")

	case ActionGateApproveRepo:
		g.approve(ctx, a, user.ID, runID, "repo")

	case ActionGateReject:
		// Enter reject-pending via compare-and-swap: keep the run parked (still
		// awaiting_approval) and swap the buttons for the reasoned-reject affordance, but
		// ONLY if the anchor still points at THIS open gate. The supersede read above is at
		// read time; a concurrent cross-surface re-gate can advance the anchor in the window
		// before this write, so the CAS (expected gate_ts == the clicked message, state
		// still open) makes a stale click LOSE rather than revert gate_ts to a superseded
		// message and orphan the live gate (PRD #41 Decision 10d). The threaded reply that
		// carries the reason is handled by the replier.
		if _, err := g.store.SetSlackRunGateIf(ctx, store.SetSlackRunGateIfParams{
			RunID: runID, GateTs: pgText(a.MessageTS), GateState: pgText(gateStateRejectPending),
			ExpectedGateTs: pgText(a.MessageTS), ExpectedGateState: pgText(gateStateOpen),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				g.ephemeral(ctx, a, "This gate was superseded — scroll down to the latest plan message.")
				return
			}
			g.logf("set reject-pending", err)
			return
		}
		if err := g.poster.UpdateBlocks(ctx, a.ChannelID, a.MessageTS,
			"Reply with a rejection reason", rejectPendingBlocks(runID)); err != nil {
			g.logf("edit gate to reject-pending", err)
		}

	case ActionGateRequestChanges:
		// Enter revise-pending (PRD #41) via the same compare-and-swap as reject-pending:
		// keep the run parked (still awaiting_approval) and swap the buttons for the "reply
		// with what should change" affordance, ONLY if the anchor still shows this open gate.
		// gate_ts is kept (== a.MessageTS) and gate_generation is preserved by the CAS, so
		// the notifier still sees the current generation and re-gates the NEXT plan version;
		// a stale click that races a cross-surface re-gate loses the CAS instead of orphaning
		// the live gate (PRD #41 Decision 10d). The threaded feedback reply is accepted by
		// the replier's own compare-and-swap.
		if _, err := g.store.SetSlackRunGateIf(ctx, store.SetSlackRunGateIfParams{
			RunID: runID, GateTs: pgText(a.MessageTS), GateState: pgText(gateStateRevisePending),
			ExpectedGateTs: pgText(a.MessageTS), ExpectedGateState: pgText(gateStateOpen),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				g.ephemeral(ctx, a, "This gate was superseded — scroll down to the latest plan message.")
				return
			}
			g.logf("set revise-pending", err)
			return
		}
		if err := g.poster.UpdateBlocks(ctx, a.ChannelID, a.MessageTS,
			"Reply with what should change", revisePendingBlocks(runID)); err != nil {
			g.logf("edit gate to revise-pending", err)
		}

	case ActionGateRejectNoReason:
		if err := g.svc.SubmitInput(ctx, user.ID, runID, "reject_plan", ""); err != nil {
			g.logf("submit reject", err)
			g.ephemeral(ctx, a, "Couldn't record the rejection — try again from uzi.")
			return
		}
		g.resolveGate(ctx, a, runID, "❌ Plan rejected.")
	}
}

// approve submits an approve for the chosen agent source (PRD #37 M7) and resolves
// the gate. A source the server rejects (ErrSelectionRejected — the roster changed
// under the button, e.g. a requeue re-detected an empty roster) leaves the gate
// OPEN with an ephemeral, so the presser can retry from a fresh state rather than
// being stuck on a stale button.
func (g *Gatekeeper) approve(ctx context.Context, a BlockAction, userID, runID uuid.UUID, source string) {
	if err := g.svc.SubmitApproval(ctx, userID, runID, source); err != nil {
		if errors.Is(err, ErrSelectionRejected) {
			g.ephemeral(ctx, a, "That agent choice is no longer valid for this run (the roster may have changed) — reopen the plan in uzi.")
			return
		}
		g.logf("submit approve", err)
		g.ephemeral(ctx, a, "Couldn't record the approval — try again from uzi.")
		return
	}
	g.resolveGate(ctx, a, runID, "✅ Plan approved — the run is continuing.")
}

// resolveGate edits the gate message to a terminal (button-free) state and clears
// the gate anchor, so a later stale click (or the notifier consuming the run's
// transition) is a no-op. Best-effort.
func (g *Gatekeeper) resolveGate(ctx context.Context, a BlockAction, runID uuid.UUID, text string) {
	if err := g.poster.UpdateBlocks(ctx, a.ChannelID, a.MessageTS, text, gateResolvedBlocks(text)); err != nil {
		g.logf("edit gate to resolved", err)
	}
	if _, err := g.store.SetSlackRunGate(ctx, store.SetSlackRunGateParams{RunID: runID}); err != nil {
		g.logf("clear gate", err)
	}
}

// ephemeral posts a notice only the presser sees, for stale clicks and unlinked
// actors. Best-effort and secret-scrubbed.
func (g *Gatekeeper) ephemeral(ctx context.Context, a BlockAction, text string) {
	if a.ChannelID == "" || a.SlackUserID == "" {
		return
	}
	if err := g.poster.PostEphemeral(ctx, a.ChannelID, a.SlackUserID, ScrubSecrets(text)); err != nil {
		g.logf("post ephemeral", err)
	}
}

func (g *Gatekeeper) logf(what string, err error) {
	g.logger.Warn("slack gatekeeper: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// ensure *Gatekeeper satisfies InboundHandler at compile time.
var _ InboundHandler = (*Gatekeeper)(nil)
