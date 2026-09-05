package slacksvc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// NotifierStore is the slice of generated queries the notifier reads/writes.
// *store.Queries satisfies it.
type NotifierStore interface {
	GetSlackRunContext(ctx context.Context, runID uuid.UUID) (store.GetSlackRunContextRow, error)
	// GetSlackChatContext is the repo-less context for a CHAT run (PRD #191 M2b): the
	// notifier falls back to it on the run-context ErrNoRows so a Slack-anchored chat's
	// terminal transitions reach the DM. A non-chat id returns ErrNoRows.
	GetSlackChatContext(ctx context.Context, runID uuid.UUID) (store.GetSlackChatContextRow, error)
	GetSlackDeliveryForUser(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	GetSlackRunMessage(ctx context.Context, runID uuid.UUID) (store.SlackRunMessage, error)
	UpsertSlackRunMessage(ctx context.Context, arg store.UpsertSlackRunMessageParams) (store.SlackRunMessage, error)
	// SetSlackRunGate records/clears the open-gate anchor (PRD #25 M4): the notifier
	// sets it when a run enters awaiting_approval and clears it when the run is
	// resolved from EITHER surface (cross-surface idempotency).
	SetSlackRunGate(ctx context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error)
	// SetSlackRunGateGen records a FRESH gate anchor together with its plan generation
	// (PRD #41 Decision 10d), generation-guarded so a slow drain can't clobber a newer
	// gate. No row = the write was refused (a newer generation already posted).
	SetSlackRunGateGen(ctx context.Context, arg store.SetSlackRunGateGenParams) (store.SlackRunMessage, error)
	// CountRunPlanMessages returns the number of kind='plan' run_messages for a run —
	// the monotonic plan generation the notifier uses to tell a new plan version from a
	// redundant awaiting_approval re-broadcast (PRD #41 Decision 10a/e).
	CountRunPlanMessages(ctx context.Context, runID uuid.UUID) (int64, error)
	// GetLatestRunQuestion returns the newest kind='question' run_message payload —
	// the clarification question a parked run is waiting on (PRD #88 M3). No row = the
	// question is not flushed yet; the notifier waits rather than posting a park it
	// cannot explain.
	GetLatestRunQuestion(ctx context.Context, runID uuid.UUID) ([]byte, error)
	// SetSlackRunQuestion records which question the run's thread already carries, so a
	// re-park on the SAME question after a worker death does not post it twice.
	SetSlackRunQuestion(ctx context.Context, arg store.SetSlackRunQuestionParams) (store.SlackRunMessage, error)
	// SetSlackRunMilestoneNotified records the last completed-milestone COUNT the notifier
	// posted a `🧩 N/M` thread line for (PRD #122 M4), generation-guarded so a redelivered
	// `running` report cannot re-post a line the thread already carries. Distinct from
	// gate_generation (the plan gate's own counter).
	SetSlackRunMilestoneNotified(ctx context.Context, arg store.SetSlackRunMilestoneNotifiedParams) (store.SlackRunMessage, error)
	// SetSlackRunLimitPause stamps the pending usage-limit park's start (the run's
	// status_since) on the anchor (PRD #1116). It is its own column, so a park can
	// never clear a gate/question and vice versa; overwrite is correct because a
	// re-park always follows a consumed resume.
	SetSlackRunLimitPause(ctx context.Context, arg store.SetSlackRunLimitPauseParams) (store.SlackRunMessage, error)
	// ClearSlackRunLimitPause consumes the park marker on the next transition via a
	// compare-and-swap on @at (PRD #1116): no row = a newer park or an already-cleared
	// marker did not match, so a stale clear can never wipe a newer park's marker.
	ClearSlackRunLimitPause(ctx context.Context, arg store.ClearSlackRunLimitPauseParams) (store.SlackRunMessage, error)
}

// Poster is the outbound Slack surface the notifier drives: open a DM channel and
// post / edit messages with the bot token. The production implementation
// (slackPoster) reads the current bot token from settings per call so a hot
// token rotation is picked up; tests inject a recording fake.
type Poster interface {
	// OpenDM returns the DM channel id for a Slack user id.
	OpenDM(ctx context.Context, slackUserID string) (string, error)
	// Post posts text to a channel (threadTS "" = top level) and returns its ts.
	Post(ctx context.Context, channelID, threadTS, text string) (string, error)
	// Update edits a message in place.
	Update(ctx context.Context, channelID, ts, text string) error
	// PostBlocks posts a Block Kit message (threadTS "" = top level); fallbackText
	// is the notification/plain-text fallback. Used for the link-confirmation DM
	// (Confirm / Not me) and the approval gate (Approve / Reject).
	PostBlocks(ctx context.Context, channelID, threadTS, fallbackText string, blocks []slack.Block) (string, error)
	// UpdateBlocks edits a message in place to a new Block Kit body (fallbackText is
	// the plain-text fallback). The gate flow swaps its buttons for a reject-pending
	// or resolved state, which a text-only Update could not do.
	UpdateBlocks(ctx context.Context, channelID, ts, fallbackText string, blocks []slack.Block) error
	// PostEphemeral sends a message only the given user sees in the channel (a
	// stale-click or unlinked-user notice), so those transient replies don't persist
	// in the DM history.
	PostEphemeral(ctx context.Context, channelID, userID, text string) error
	// AddReaction adds an emoji reaction to a message — the ✅ ack on an accepted
	// inbound thread reply (PRD #25 M5). Best-effort.
	AddReaction(ctx context.Context, channelID, ts, emoji string) error
	// LookupUserByEmail resolves a workspace member's email to their Slack user id
	// (users.lookupByEmail), for the email auto-match pass. A not-found or any other
	// Slack error is returned to the caller, which treats it as "no match".
	LookupUserByEmail(ctx context.Context, email string) (string, error)
}

// notifierQueue bounds the in-memory event backlog. PublishState drops when full
// rather than block the request path (Slack is strictly best-effort).
const notifierQueue = 256

// notifierMsgQueue bounds the chat message-frame backlog (PRD #191 M3). Larger than
// notifierQueue because a chat turn emits many text frames; still bounded and
// drop-when-full (Slack streaming is best-effort — a dropped frame at worst strands a
// placeholder the deep link and the terminal status line still cover).
const notifierMsgQueue = 1024

// Notifier turns run state transitions into per-owner Slack DMs (PRD #25 M3). It
// implements workersvc.Broadcaster: PublishState enqueues and returns
// immediately (never blocks the run lifecycle), and a drain goroutine does the
// run/owner loads and the Slack calls. A Slack failure is logged (redacted) and
// never affects the run. Messages are content-minimized: status + issue title +
// links only, never plan/diff content, and every outbound string is scrubbed of
// secrets.
type Notifier struct {
	store   NotifierStore
	poster  Poster
	baseURL func(context.Context) (string, error)
	logger  *slog.Logger
	ch      chan stateEvent
	// notifyCh carries generic inbox notifications (PRD #46 M2) — judge reviews,
	// self-improvement MRs, anything the notifysvc seam publishes. It is a SEPARATE
	// queue from ch on purpose: these events do NOT go through GetSlackRunContext
	// (they are not run-state transitions and a judge run is repo-less), so they
	// share none of the state path's run/repo rendering.
	notifyCh chan notifyEvent
	healthCh chan healthEvent
	// msgCh carries chat run message frames for turn streaming (PRD #191 M3). A
	// SEPARATE queue from ch: message frames are higher-volume than state transitions
	// and their consumer is a parallel path (chatpost.go) that never touches the
	// repo-ful renderer.
	msgCh chan chatMsgEvent
	// chatConvos holds per-chat-run turn-coalescing state, OWNED BY the drain goroutine
	// (single-threaded, no lock). A present-but-nil value is the "known non-chat / no
	// anchor — skip" marker, set once so later frames for that run drop in O(1).
	chatConvos map[uuid.UUID]*chatConvo
	// chatDecided mirrors the skip decision for PublishMessage's hot path (called from
	// worker request goroutines), so a busy issue run stops enqueuing message frames
	// after the drain has classified it. sync.Map because it is written by the drain and
	// read by many publishers.
	chatDecided sync.Map
}

type stateEvent struct {
	runID  uuid.UUID
	status string
}

// notifyEvent is a generic inbox notification bound for a user's DM (PRD #46 M2).
// title is a caller-set fixed label; body is dynamic, potentially untrusted free
// text; link is an in-app deep-link URL. emoji is a caller-set leading glyph and
// facts are caller-built TRUSTED short strings carrying intentional markup, built
// from closed enums/ints (PRD #268 M3). The untrusted fields (title, body) are
// escaped/scrubbed at render; facts are scrubbed but NOT escaped (see
// notificationBlocks).
type notifyEvent struct {
	userID uuid.UUID
	title  string
	body   string
	link   string
	emoji  string
	facts  []string
}

// healthEvent is a run-health flag change (PRD #47 M4). nudge is set only when the
// sweeper judged the event nudge-worthy and already stamped health_notified_at, so
// the notifier threads exactly one DM; otherwise it only re-renders the root.
type healthEvent struct {
	runID  uuid.UUID
	health string
	reason string
	nudge  bool
}

// NewNotifier builds a Notifier. baseURL supplies the public base URL for deep
// links (settings.PublicBaseURL). Call Run in a goroutine.
func NewNotifier(s NotifierStore, poster Poster, baseURL func(context.Context) (string, error), logger *slog.Logger) *Notifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notifier{
		store:      s,
		poster:     poster,
		baseURL:    baseURL,
		logger:     logger,
		ch:         make(chan stateEvent, notifierQueue),
		notifyCh:   make(chan notifyEvent, notifierQueue),
		healthCh:   make(chan healthEvent, notifierQueue),
		msgCh:      make(chan chatMsgEvent, notifierMsgQueue),
		chatConvos: make(map[uuid.UUID]*chatConvo),
	}
}

// PublishNotification enqueues a generic inbox notification for delivery to the
// user's Slack DM (PRD #46 M2). It implements notifysvc.Slacker. Like PublishState
// it MUST NOT block: it enqueues and returns, dropping the event if the queue is
// full (Slack is strictly best-effort — the inbox row is already durable).
func (n *Notifier) PublishNotification(userID uuid.UUID, r notifysvc.SlackRender) {
	select {
	case n.notifyCh <- notifyEvent{userID: userID, title: r.Title, body: r.Body, link: r.Link, emoji: r.Emoji, facts: r.Facts}:
	default:
		n.logger.Warn("slack: notifier queue full, dropping notification", "user", userID.String())
	}
}

// PublishState implements workersvc.Broadcaster. It MUST NOT block: it enqueues
// and returns, dropping the event if the queue is full.
func (n *Notifier) PublishState(runID uuid.UUID, status string) {
	select {
	case n.ch <- stateEvent{runID: runID, status: status}:
	default:
		n.logger.Warn("slack: notifier queue full, dropping transition", "run", runID.String(), "status", status)
	}
}

// PublishMessage streams a CHAT run's turns into its Slack thread (PRD #191 M3), and
// stays a no-op for every other run kind (content minimization: an issue/ci_fix run's
// message content never goes to Slack). It MUST NOT block the worker's message-append
// path: it filters to the frames a chat turn is built from, skips runs the drain has
// already classified non-chat, and enqueues (dropping if full). The drain
// (handleChatMsg) resolves runs.kind and coalesces the turn — kind here is the MESSAGE
// kind, not the run kind, so the run-kind decision cannot be made on this hot path.
func (n *Notifier) PublishMessage(runID uuid.UUID, _ int32, kind, _, _, _ string, payload []byte, _ time.Time) {
	if !chatRelevantKind(kind) {
		return
	}
	// A run already classified non-chat stops enqueuing here, so a busy issue run does
	// not flood the queue with frames the drain would only drop.
	if v, ok := n.chatDecided.Load(runID); ok && !v.(bool) {
		return
	}
	select {
	case n.msgCh <- chatMsgEvent{runID: runID, kind: kind, payload: payload}:
	default:
		n.logger.Warn("slack: notifier msg queue full, dropping chat frame", "run", runID.String())
	}
}

// PublishInput is a deliberate no-op (PRD #95): the steer-queue delivery ack is a
// web/CLI concern (browsers re-read the owner-gated queue). Steer text never goes to
// Slack — same content-minimization reason PublishMessage no-ops.
func (n *Notifier) PublishInput(uuid.UUID) {}

// PublishHealth implements workersvc.Broadcaster for the run-health flag (PRD #47).
// Like PublishState it MUST NOT block: it enqueues and returns, dropping the event
// if the queue is full (Slack is strictly best-effort). The drain goroutine
// re-renders the run's root (⚠ flip on flag, back on clear) and, when nudge is set,
// threads one cooldown-gated DM.
func (n *Notifier) PublishHealth(runID uuid.UUID, health, reason string, nudge bool) {
	select {
	case n.healthCh <- healthEvent{runID: runID, health: health, reason: reason, nudge: nudge}:
	default:
		n.logger.Warn("slack: notifier queue full, dropping health event", "run", runID.String(), "health", health)
	}
}

// Run drains the queue until ctx is cancelled. Wire it into the background
// WaitGroup alongside the socket manager.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-n.ch:
			n.handle(ctx, ev)
		case ev := <-n.notifyCh:
			n.handleNotify(ctx, ev)
		case hev := <-n.healthCh:
			n.handleHealth(ctx, hev)
		case mev := <-n.msgCh:
			n.handleChatMsg(ctx, mev)
		}
	}
}

func (n *Notifier) logf(what string, err error) {
	n.logger.Warn("slack notifier: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// threadSectionBlock / threadMrkdwnElem are the tiny mrkdwn constructors the thread
// event builder reuses; the text is already scrubbed/escaped by the caller.
func threadSectionBlock(text string) *slack.SectionBlock {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
}

func threadMrkdwnElem(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, text, false, false)
}

// maxFailureReason bounds the worker-originated failure reason before it reaches
// Slack. Free text with no source-side limit, so cap it defensively (full
// source-side sanitization is a separate follow-up).
const maxFailureReason = 500

// boundReason truncates an over-long failure reason on a rune boundary, appending
// an ellipsis so the DM stays readable and can't be flooded.
func boundReason(s string) string {
	r := []rune(s)
	if len(r) <= maxFailureReason {
		return s
	}
	return string(r[:maxFailureReason]) + "…"
}

// runURL is the plain webui deep-link URL to the run view (empty when no base URL
// resolves). Used raw as a Block Kit button url; runLink wraps it as mrkdwn. The
// base is operator-set and http(s)-validated server-side and the id is a uuid, so
// there is nothing hostile to escape.
func runURL(base string, runID uuid.UUID) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/runs/%s", base, runID.String())
}

// runLink is the webui deep link to the run view, as a Slack mrkdwn link. Empty
// when no base URL resolves.
func runLink(base string, runID uuid.UUID) string {
	if u := runURL(base, runID); u != "" {
		return fmt.Sprintf("<%s|Open in uzi>", u)
	}
	return ""
}
