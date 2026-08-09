package notifysvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// runFailureQueue bounds the in-memory backlog of failed-run ids awaiting an
// inbox notification. PublishState drops when full rather than block the run
// lifecycle path (the notification is strictly best-effort — the run's status is
// already durable, and the Slack ❌ DM still fires for opted-in users).
const runFailureQueue = 256

// runReader is the slice of the generated queries this adapter needs: a single
// run load by id. *store.Queries satisfies it. It is deliberately narrower than
// notifysvc.Store so the adapter states exactly what it reads.
type runReader interface {
	GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error)
}

// RunFailureNotifier is a workersvc.Broadcaster adapter (PRD #284 M5) that lands
// an in-app inbox notification whenever a run transitions to "failed". It closes
// the gap that a failed run reached no inbox notification, so a user WITHOUT
// Slack still gets an inbox badge on failure instead of only the conditional
// Slack ❌ DM.
//
// INBOX-ONLY BY DESIGN. It builds the Notification with Slack == nil, so Notify
// persists the inbox row and skips Slack entirely. The existing slacksvc failed
// DM (notifier.go) already covers Slack for opted-in users; passing a SlackRender
// here would double-DM them. Inbox and Slack are therefore split cleanly: this
// adapter owns the inbox, slacksvc owns the DM.
//
// It sits in workersvc.MultiBroadcaster next to the WS hub and the Slack
// notifier, so it fires uniformly for BOTH worker-reported failures and
// sweep-driven ones — every path that writes a "failed" status fans through
// PublishState.
//
// NON-BLOCKING CONTRACT. Like the other Broadcasters, PublishState MUST NOT block
// the run lifecycle: it enqueues onto a buffered channel and returns, dropping
// the event if the queue is full. A drain goroutine (Run) does the run load and
// the Notify call.
type RunFailureNotifier struct {
	q      runReader
	svc    *Service
	ch     chan uuid.UUID
	logger *slog.Logger
}

// NewRunFailureNotifier builds the adapter. svc is the notify seam it writes
// through; logger defaults to slog.Default() when nil. Call Run in a goroutine.
func NewRunFailureNotifier(q runReader, svc *Service, logger *slog.Logger) *RunFailureNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &RunFailureNotifier{
		q:      q,
		svc:    svc,
		ch:     make(chan uuid.UUID, runFailureQueue),
		logger: logger,
	}
}

// PublishMessage is a no-op: run message frames are not a failure signal. The
// signature mirrors workersvc.Broadcaster exactly so this type satisfies it
// structurally at the MultiBroadcaster literal in main.go.
func (n *RunFailureNotifier) PublishMessage(runID uuid.UUID, seq int32, kind, agent, agentInstance, agentLabel string, payload []byte, createdAt time.Time) {
}

// PublishHealth is a no-op: health flags are handled server-side (PRD #47), not
// through this adapter.
func (n *RunFailureNotifier) PublishHealth(uuid.UUID, string, string, bool) {}

// PublishInput is a no-op: the steer-queue ack is a web/CLI concern.
func (n *RunFailureNotifier) PublishInput(uuid.UUID) {}

// PublishState implements the failure-relevant half of workersvc.Broadcaster. It
// only cares about the "failed" transition; every other status is ignored. It
// MUST NOT block: it enqueues the run id and returns, dropping (with a Warn) if
// the queue is full.
func (n *RunFailureNotifier) PublishState(runID uuid.UUID, status string) {
	if status != "failed" {
		return
	}
	select {
	case n.ch <- runID:
	default:
		n.logger.Warn("notify: run-failure queue full, dropping notification", "run", runID.String())
	}
}

// Run drains the queue until ctx is cancelled. Wire it into the background
// WaitGroup alongside the other Broadcaster drains.
func (n *RunFailureNotifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case runID := <-n.ch:
			n.handle(ctx, runID)
		}
	}
}

// handle loads the run, re-checks it is failed, applies the stop_kind gate, and
// writes the inbox notification. Best-effort throughout: any error is logged and
// swallowed (the run's status is already durable).
func (n *RunFailureNotifier) handle(ctx context.Context, runID uuid.UUID) {
	run, err := n.q.GetRunByID(ctx, runID)
	if err != nil {
		// Includes pgx.ErrNoRows (run deleted between the transition and the drain).
		n.logger.Warn("notify: run-failure load failed", "run", runID.String(), "error", err)
		return
	}

	// Defensive: the status may have moved between enqueue and drain. Only notify a
	// still-failed run.
	if run.Status != "failed" {
		return
	}

	// Gate on stop_kind, NEVER on failure_reason. A deliberate human stop
	// ('cancelled' or 'plan_rejected') is not a surprise worth an inbox badge — the
	// user did it. Everything else (an 'auto_stopped' park or a bare NULL stop_kind,
	// which is the transient-forge-failure case this PRD exists to surface) IS a
	// notification. failure_reason is free text and must not be parsed for this
	// decision.
	if run.StopKind.Valid && (run.StopKind.String == "cancelled" || run.StopKind.String == "plan_rejected") {
		return
	}

	uid := run.UserID
	rid := run.ID
	payload := map[string]any{
		"run_id": rid.String(),
		"kind":   run.Kind,
	}
	if run.IssueIid.Valid {
		payload["issue_iid"] = run.IssueIid.Int64
	}

	// Slack: nil ⇒ inbox only. The slacksvc failed-DM covers Slack already.
	if _, err := n.svc.Notify(ctx, Notification{
		UserID:  uid,
		Kind:    "run_failed",
		Payload: payload,
		RunID:   &rid,
		Slack:   nil,
	}); err != nil {
		n.logger.Warn("notify: run-failure notification failed", "run", rid.String(), "error", err)
	}
}
