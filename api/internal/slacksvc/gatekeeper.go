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
	// SetSlackRunGate records the reject-pending state or clears the gate anchor.
	SetSlackRunGate(ctx context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error)
}

// PlanGateSubmitter is the slice of the run service the gatekeeper drives: read a
// run (ownership-scoped, for the stale-click status check) and submit the
// approve/reject steering input (ownership-checked). *workersvc.Service satisfies
// it through a thin adapter in main, keeping slacksvc free of a workersvc import.
type PlanGateSubmitter interface {
	GetRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error)
	SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string) error
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

	switch a.ActionID {
	case ActionGateApprove:
		if err := g.svc.SubmitInput(ctx, user.ID, runID, "approve_plan", ""); err != nil {
			g.logf("submit approve", err)
			g.ephemeral(ctx, a, "Couldn't record the approval — try again from uzi.")
			return
		}
		g.resolveGate(ctx, a, runID, "✅ Plan approved — the run is continuing.")

	case ActionGateReject:
		// Enter reject-pending: the run stays parked (still awaiting_approval); swap
		// the buttons for the reasoned-reject affordance. The threaded reply that
		// carries the reason is wired in M5; the escape-hatch button rejects now.
		if _, err := g.store.SetSlackRunGate(ctx, store.SetSlackRunGateParams{
			RunID: runID, GateTs: pgText(a.MessageTS), GateState: pgText(gateStateRejectPending),
		}); err != nil {
			g.logf("set reject-pending", err)
		}
		if err := g.poster.UpdateBlocks(ctx, a.ChannelID, a.MessageTS,
			"Reply with a rejection reason", rejectPendingBlocks(runID)); err != nil {
			g.logf("edit gate to reject-pending", err)
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
