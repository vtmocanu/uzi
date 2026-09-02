package slacksvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
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

// handleNotify delivers one generic inbox notification to the user's DM (PRD #46
// M2). It reuses the run-state path's delivery resolution + per-user opt-in gating
// (GetSlackDeliveryForUser) but NOT its rendering: there is no run/repo context, so
// it never calls GetSlackRunContext. Unlinked / opted-out / unconfirmed users drop
// silently; every failure logs redacted and returns (a Slack problem never affects
// the caller — the inbox row is already persisted).
func (n *Notifier) handleNotify(ctx context.Context, ev notifyEvent) {
	target, err := n.store.GetSlackDeliveryForUser(ctx, ev.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // unlinked, opted out, or unconfirmed → drop silently
	}
	if err != nil {
		n.logf("resolve delivery target", err)
		return
	}
	if !target.Valid || target.String == "" {
		return
	}

	channel, err := n.poster.OpenDM(ctx, target.String)
	if err != nil {
		n.logf("open dm", err)
		return
	}

	blocks, fallback := notificationBlocks(ev)
	if _, err := n.poster.PostBlocks(ctx, channel, "", fallback, blocks); err != nil {
		n.logf("post notification", err)
	}
}

// notifyFactMarkupStripper removes the intentional mrkdwn markup from a fact so it can
// go into the plain-text notification fallback (the OS-notification text, which Slack
// parses for mrkdwn). Facts carry `*bold*`, “ `code` “ chips and verdict emoji built
// from closed enums; the emoji is harmless plain text, but the *, `, _ control chars
// must go so the fallback reads cleanly and can never leave a dangling mrkdwn token.
var notifyFactMarkupStripper = strings.NewReplacer("*", "", "`", "", "_", "")

// notificationBlocks builds the content-minimized DM for a generic notification as
// Block Kit (message family D, PRD #268 M3). Shape, in order and only for the parts
// that apply: a section with the caller emoji + bold title; a context block of the
// trusted facts; a blockquote section for the untrusted body; a context block for the
// deep link.
//
// The field-by-field trust discipline is the load-bearing part:
//
//   - title is a FIXED caller label, EscapeMrkdwn'd anyway (defense in depth) before it
//     becomes the bold head; the emoji is a fixed caller glyph rendered raw.
//   - facts are caller-built TRUSTED strings that INTENTIONALLY carry markup (bold, code
//     chips, verdict emoji) built from closed enums/ints, so they are ScrubSecrets'd but
//     NOT EscapeMrkdwn'd — escaping would break the intended markup.
//   - body is UNTRUSTED model/forge free text carrying no trusted markup of its own, so it
//     is whole-blob rendered by SlackMrkdwn (CommonMark→mrkdwn, PRD #292; SlackMrkdwn OWNS
//     its &<>-escaping and REPLACES EscapeMrkdwn — never nested) after ScrubSecrets, then
//     bounded to Slack's section limit. It is blockquote-prefixed line-by-line UNLESS the
//     render emitted a fenced/indented code block (reported by SlackMrkdwnBlock from the AST
//     walk, not a substring scan): a fenced body is emitted as a PLAIN section instead (PRD
//     #292 Decision 6), because a `> ` prefix injected into a fence's interior lines corrupts
//     Slack's code rendering.
//   - the deep link keeps its raw <url|label> markup (operator-set base, http(s)-validated),
//     ScrubSecrets'd as a no-op-on-clean last line of defense.
//
// The fallback is built from FIXED/escaped fields only — never a raw model summary alone:
// the escaped title, then the facts with their markup stripped (so the verdict/count still
// appear as the OS-notification text), then the escaped, length-bounded body.
func notificationBlocks(ev notifyEvent) (blocks []slack.Block, fallback string) {
	title := EscapeMrkdwn(strings.TrimSpace(ev.title))
	head := "*" + title + "*"
	if ev.emoji != "" {
		head = ev.emoji + " " + head
	}
	blocks = []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, head, false, false), nil, nil)}

	if len(ev.facts) > 0 {
		blocks = append(blocks, slack.NewContextBlock("slack_notify_facts",
			slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets(strings.Join(ev.facts, "  ·  ")), false, false)))
	}

	if body := strings.TrimSpace(ev.body); body != "" {
		markdown, hasCodeBlock := SlackMrkdwnBlock(ScrubSecrets(body))
		rendered := truncateForSlackSection(markdown)
		text := rendered
		if !hasCodeBlock {
			// No fenced code: blockquote every line, not just the first — Slack's `>`
			// quotes only the line it leads, so lines 2+ of a multi-line body would
			// otherwise escape the blockquote. A fenced body is emitted as a plain
			// section instead (PRD #292 Decision 6) — the `> ` prefix would corrupt the
			// fence's interior lines and break Slack's code rendering. hasCodeBlock comes
			// from the AST walk, so a literal ``` in PROSE (a Text node, not a fence) is
			// correctly still blockquoted.
			text = "> " + strings.ReplaceAll(rendered, "\n", "\n> ")
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil))
	}

	if url := strings.TrimSpace(ev.link); url != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_notify_link",
			slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets(fmt.Sprintf("🔗 <%s|Open in uzi>", url)), false, false)))
	}

	return blocks, notificationFallback(ev)
}

// notificationFallback is the OS-notification text for a generic notification, built
// from fixed/escaped fields ONLY (never a raw model summary alone): the escaped title,
// the facts with their trusted markup stripped, and the escaped, length-bounded body.
func notificationFallback(ev notifyEvent) string {
	s := EscapeMrkdwn(strings.TrimSpace(ev.title))
	if len(ev.facts) > 0 {
		s += " — " + EscapeMrkdwn(notifyFactMarkupStripper.Replace(strings.Join(ev.facts, ", ")))
	}
	if body := strings.TrimSpace(ev.body); body != "" {
		// ev.body can now be MULTI-LINE (the judge summary preserves newlines for the
		// Slack blockquote). Collapse its whitespace/newlines to single spaces so the
		// OS-notification preview stays a clean one-liner, then bound + escape it exactly
		// as before (still escaped, still from the same field — just flattened).
		s += " — " + EscapeMrkdwn(boundReason(strings.Join(strings.Fields(body), " ")))
	}
	return ScrubSecrets(s)
}

// handle processes one transition: resolve the owner's delivery target, then
// post the root DM (first time) or edit it and thread the outcome. Every failure
// path logs redacted and returns — a run is never affected.
func (n *Notifier) handle(ctx context.Context, ev stateEvent) {
	// On a terminal transition — for EVERY run kind — free any chat turn-streaming
	// state so the maps track only in-flight runs (PRD #191 M3). Deferred so it runs
	// after the render/handleChat below on all paths; a frame that trails this is caught
	// by setupChatConvo's terminal check + the evict-cap.
	if isTerminalStatus(ev.status) {
		defer n.evictChatConvo(ev.runID)
	}
	rc, err := n.store.GetSlackRunContext(ctx, ev.runID)
	if err != nil {
		// No row: GetSlackRunContext INNER-JOINs repos, so a repo-less run yields
		// ErrNoRows here. A CHAT run (PRD #191 M2b) may still have a Slack-anchored DM
		// to update — fall back to the repo-less chat path. A judge/self_improve run,
		// or a run deleted out from under us, is not a chat and skips silently there.
		if errors.Is(err, pgx.ErrNoRows) {
			n.handleChat(ctx, ev)
			return
		}
		n.logf("load run context", err)
		return
	}
	// PRD #46 Decision 6: a judge or self_improve run's OWN state transitions are not
	// user-facing run events — suppress them from the run-state DM path. A judge run is
	// repo-less and already yields ErrNoRows above; a self_improve run is repo-ful, so
	// it needs this explicit skip. The judge "review ready" / self-improve "MR opened"
	// notifications are a SEPARATE notifier event, not this run-state rendering.
	if rc.Kind == "judge" || rc.Kind == "self_improve" {
		return
	}
	target, err := n.store.GetSlackDeliveryForUser(ctx, rc.UserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // unlinked, opted out, or unconfirmed → drop silently
	}
	if err != nil {
		n.logf("resolve delivery target", err)
		return
	}
	if !target.Valid || target.String == "" {
		return
	}
	slackID := target.String

	channel, err := n.poster.OpenDM(ctx, slackID)
	if err != nil {
		n.logf("open dm", err)
		return
	}

	base, _ := n.baseURL(ctx)
	blocks, fallback := rootBlocks(rc, base)

	existing, err := n.store.GetSlackRunMessage(ctx, ev.runID)
	var anchor store.SlackRunMessage
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		ts, perr := n.poster.PostBlocks(ctx, channel, "", fallback, blocks)
		if perr != nil {
			n.logf("post root", perr)
			return
		}
		saved, uerr := n.store.UpsertSlackRunMessage(ctx, store.UpsertSlackRunMessageParams{
			RunID: rc.ID, ChannelID: channel, RootTs: ts,
		})
		if uerr != nil {
			n.logf("record anchor", uerr)
			// Fall back to a synthetic anchor so the gate step can still run.
			saved = store.SlackRunMessage{RunID: rc.ID, ChannelID: channel, RootTs: ts}
		}
		anchor = saved
	case err != nil:
		n.logf("load anchor", err)
		return
	default:
		anchor = existing
		if uerr := n.poster.UpdateBlocks(ctx, existing.ChannelID, existing.RootTs, fallback, blocks); uerr != nil {
			n.logf("update root", uerr)
		}
		if tblocks, tfallback, tok := renderThreadBlocks(rc, base); tok {
			if _, perr := n.poster.PostBlocks(ctx, existing.ChannelID, existing.RootTs, tfallback, tblocks); perr != nil {
				n.logf("post thread event", perr)
			}
		}
		n.handleMilestone(ctx, rc, existing)
	}

	n.handleGate(ctx, rc, anchor, base)
	n.handleQuestion(ctx, rc, anchor, base)
}

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

// handleQuestion posts a clarification question into the run's DM thread when the run
// parks at awaiting_input (PRD #88 M3). It is the question's counterpart to
// handleGate, and deliberately much smaller: there is no button, no anchor state
// machine and no compare-and-swap, because the distinct awaiting_input status is
// itself the routing signal the replier reads (D5) — the plan gate needed gate_state
// only because a revision keeps the run at awaiting_approval.
//
// Dedupe is by question IDENTITY, not by a count and not by "is the run parked":
// awaiting_input is re-broadcast for the SAME question after a worker death (the run
// re-queues, the resumed worker re-parks re-using the question id), so a count-based
// key would post the card a second time while an identity comparison is a no-op across
// the requeue by construction. A genuinely new question carries a new id and posts.
//
// All best-effort: a failure is logged (redacted) and never affects the run.
func (n *Notifier) handleQuestion(ctx context.Context, rc store.GetSlackRunContextRow, anchor store.SlackRunMessage, base string) {
	if rc.Status != "awaiting_input" {
		return
	}
	raw, err := n.store.GetLatestRunQuestion(ctx, rc.ID)
	if err != nil {
		// No row: the state report reached us before the question message was durable.
		// Waiting is correct — a later event re-drives this with the question present, and
		// posting "the run needs your answer" with no question would be worse than late.
		if !errors.Is(err, pgx.ErrNoRows) {
			n.logf("load question", err)
		}
		return
	}
	q, ok := parseQuestionPayload(raw)
	if !ok {
		n.logf("parse question payload", fmt.Errorf("run %s: unusable question payload", rc.ID))
		return
	}
	if anchor.QuestionID.Valid && anchor.QuestionID.String == q.QuestionID {
		return // already on screen in this thread — a re-park, not a new question
	}
	ts, err := n.poster.PostBlocks(ctx, anchor.ChannelID, anchor.RootTs,
		"The run needs your answer", questionThreadBlocks(rc.ID, q, base))
	if err != nil {
		n.logf("post question in thread", err)
		return // do not record it as posted; a later event retries
	}
	// The ts is recorded with the id because the replier orders inbound replies against
	// it — a reply before this card answers a superseded question. A post whose ts came
	// back empty is therefore worse than not recording at all: it would satisfy the
	// notifier's dedupe (never re-posting) while leaving every reply unbindable.
	if ts == "" {
		n.logf("record question", fmt.Errorf("run %s: question posted with no ts", rc.ID))
		return
	}
	if _, err := n.store.SetSlackRunQuestion(ctx, store.SetSlackRunQuestionParams{
		RunID: rc.ID, QuestionID: pgconv.Text(q.QuestionID), QuestionTs: pgconv.Text(ts),
	}); err != nil {
		n.logf("record question", err)
	}
}

// handleGate manages the approval-gate message's lifecycle on the notifier's
// (state-transition) side, now generation-aware for plan revision (PRD #41 Decision
// 10a/e). The run can sit at awaiting_approval across MULTIPLE plan generations (a
// revision re-gates without leaving the status), so "a gate is already open" is NOT a
// safe dedupe key — a redundant re-broadcast and a genuinely new plan version look
// identical by status alone. The authority is the plan generation: the count of
// kind='plan' run_messages, compared against the anchor's stored gate_generation.
//
//   - awaiting_approval, currentGen > storedGen → a NEW plan version: supersede the
//     prior gate (button-free edit) if one is open, post a FRESH gate + the plan into
//     the thread, and record gate_ts + gate_generation (generation-guarded).
//   - awaiting_approval, currentGen <= storedGen → a redundant re-broadcast of the
//     same version: return without posting (never spam, never re-post the plan).
//   - left awaiting_approval while a gate is open → resolved from another surface (web
//     UI, timeout, sweeper): close the gate message and clear the anchor
//     (cross-surface idempotency).
//
// All best-effort: a failure is logged (redacted) and never affects the run.
func (n *Notifier) handleGate(ctx context.Context, rc store.GetSlackRunContextRow, anchor store.SlackRunMessage, base string) {
	gateOpen := anchor.GateTs.Valid && anchor.GateTs.String != ""

	if rc.Status == "awaiting_approval" {
		storedGen := int64(0)
		if anchor.GateGeneration.Valid {
			storedGen = int64(anchor.GateGeneration.Int32)
		}
		currentGen, err := n.store.CountRunPlanMessages(ctx, rc.ID)
		if err != nil {
			// Without a reliable plan-generation count we cannot tell a genuinely new plan
			// version from a redundant re-broadcast. The old fallback guessed storedGen+1 on
			// a closed gate, but that BURNS the next generation: it posts the current
			// (possibly still pre-revision) plan and records the guessed generation, so the
			// genuine v2 re-gate later reads currentGen == storedGen and is silently swallowed
			// — a gate showing the wrong plan version. Skip this event instead; the run is
			// unaffected (Slack is best-effort, the web gate is canonical) and a subsequent
			// state event re-drives handleGate with a working count.
			n.logf("count plan messages", err)
			return
		}
		if currentGen <= storedGen {
			// Redundant re-broadcast of a plan version already gated — never spam. This also
			// covers currentGen==0: the worker flushes the `plan` run_message BEFORE it
			// re-reports awaiting_approval (§343), so a correctly-ordered gate always has
			// currentGen>=1; a 0 means the plan isn't flushed yet, so waiting (no gate with no
			// plan) is correct, not a drop.
			return
		}

		// A genuinely new plan version. Supersede a still-open prior gate button-free so
		// no stale card lingers (the pure-Slack revise flow already cleared gate_ts, so
		// this fires mainly for a web-UI-driven or timed-out revise).
		if gateOpen {
			if err := n.poster.UpdateBlocks(ctx, anchor.ChannelID, anchor.GateTs.String, "Plan superseded by a newer version",
				gateResolvedBlocks("Superseded by a newer plan version below.")); err != nil {
				n.logf("supersede prior gate", err)
			}
		}

		// Slack gate parity (Decision 10): the plan itself now rides the thread, posted
		// FIRST so it reads above the gate buttons. Bound to THIS fresh-gate branch and
		// keyed by the same generation, so it is never posted on a redundant broadcast.
		// Skipped when the plan is empty (nothing to show).
		if plan := strings.TrimSpace(rc.PlanMd.String); rc.PlanMd.Valid && plan != "" {
			if _, err := n.poster.PostBlocks(ctx, anchor.ChannelID, anchor.RootTs, "Plan ready for review", planThreadBlocks(rc.ID, plan, base)); err != nil {
				n.logf("post plan in thread", err)
			}
		}
		ts, err := n.poster.PostBlocks(ctx, anchor.ChannelID, anchor.RootTs, "Plan ready for review in uzi", gateBlocks(rc.ID, base, rc.RepoAgentNames))
		if err != nil {
			n.logf("post gate", err)
			return
		}
		// currentGen is a per-run plan-message count (small in practice); clamp before
		// narrowing so an implausibly large count saturates rather than wrapping to a
		// negative generation that could suppress a fresh gate. The explicit bound also
		// makes the cast provable to gosec G115 / CodeQL.
		gen := currentGen
		if gen > math.MaxInt32 {
			gen = math.MaxInt32
		}
		if _, err := n.store.SetSlackRunGateGen(ctx, store.SetSlackRunGateGenParams{
			RunID: rc.ID, GateTs: pgconv.Text(ts), GateState: pgconv.Text(gateStateOpen),
			GateGeneration: pgtype.Int4{Int32: int32(gen), Valid: true},
		}); err != nil {
			n.logf("record gate", err)
		}
		return
	}

	if gateOpen {
		if err := n.poster.UpdateBlocks(ctx, anchor.ChannelID, anchor.GateTs.String, "Plan gate closed",
			gateResolvedBlocks("Plan already handled — this gate is closed.")); err != nil {
			n.logf("close gate", err)
		}
		if _, err := n.store.SetSlackRunGate(ctx, store.SetSlackRunGateParams{RunID: rc.ID}); err != nil {
			n.logf("clear gate", err)
		}
	}
}

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

func (n *Notifier) logf(what string, err error) {
	n.logger.Warn("slack notifier: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// rootBlocks builds the content-minimized run-status root as Block Kit (message
// family A, PRD #268 M2): a section carrying the status glyph + bold label, the
// repo#iid as inline code and the bold issue title; then a context block assembling —
// in order, and only for the elements that apply — the milestone counter, the MR/PR
// link, a health flag, and the Open-in-uzi deep link. No plan/diff — the plan is one
// click away. It returns the blocks plus the plain-text fallback (the OS-notification
// text): `{Label} · {repo}#{iid} — {title}`, never a raw title.
//
// The forge-controlled repo path and issue title are mrkdwn-escaped AND ScrubSecrets'd
// individually so they cannot inject a spoofed <url|label> link, a <@Uxxx> mention, or
// a leaked token into the DM. The deep-link and MR-link markup keeps its raw <url|label>
// mrkdwn (never EscapeMrkdwn'd — the base is operator-set, the id a uuid, and mrLink
// https-guards + escapes the forge URL), but each context string is still ScrubSecrets'd
// before it enters the block: blocks are not exempt from the outbound-scrub rule, and a
// scrub is a no-op on a clean URL. The context block is omitted entirely when no element
// applies (Slack rejects an empty one).
func rootBlocks(rc store.GetSlackRunContextRow, base string) (blocks []slack.Block, fallback string) {
	emoji, label := statusGlyph(rc)
	repo := ScrubSecrets(EscapeMrkdwn(rc.PathWithNamespace))
	title := ScrubSecrets(EscapeMrkdwn(rc.IssueTitle))

	head := "*" + label + "*"
	if emoji != "" {
		head = emoji + " " + head
	}
	sectionText := fmt.Sprintf("%s\n`%s#%d` · *%s*", head, repo, iid(rc.IssueIid), title)
	blocks = []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, sectionText, false, false), nil, nil)}

	var ctxElems []slack.MixedElement
	if done, total, ok := milestoneCounts(rc); ok {
		ctxElems = append(ctxElems, slack.NewTextBlockObject(slack.MarkdownType,
			fmt.Sprintf("🧩 %d/%d milestones", done, total), false, false))
	}
	if mr := mrContextElem(rc); mr != "" {
		ctxElems = append(ctxElems, slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets(mr), false, false))
	}
	if h := healthContextLabel(rc); h != "" {
		ctxElems = append(ctxElems, slack.NewTextBlockObject(slack.MarkdownType,
			"⚠️ "+ScrubSecrets(EscapeMrkdwn(h)), false, false))
	}
	if u := runURL(base, rc.ID); u != "" {
		ctxElems = append(ctxElems, slack.NewTextBlockObject(slack.MarkdownType,
			ScrubSecrets(fmt.Sprintf("🔗 <%s|Open in uzi>", u)), false, false))
	}
	if len(ctxElems) > 0 {
		blocks = append(blocks, slack.NewContextBlock("slack_run_root_ctx", ctxElems...))
	}

	fallback = fmt.Sprintf("%s · %s#%d — %s", label, repo, iid(rc.IssueIid), title)
	return blocks, fallback
}

// mrContextElem is the MR/PR context element for the root — `🔀 <url|MR !N>` in the
// run's own forge vocabulary (PR #N on Forgejo/GitHub), or "" when the run has no
// merge request yet. mrLink supplies the https-guarded, mrkdwn-escaped URL; the label
// is server-derived (a forge noun + iid), so there is nothing hostile to escape in it.
func mrContextElem(rc store.GetSlackRunContextRow) string {
	url := mrLink(rc)
	if url == "" {
		return ""
	}
	if rc.MrIid.Valid {
		return fmt.Sprintf("🔀 <%s|%s %s%d>", url, forgeMrAbbrev(rc.ForgeType), forgeMrRef(rc.ForgeType), rc.MrIid.Int64)
	}
	return fmt.Sprintf("🔀 <%s|View %s>", url, forgeMrAbbrev(rc.ForgeType))
}

// decodeMilestones decodes a runs.milestones_frozen jsonb array into a
// []apitypes.Milestone. A nil/empty/non-array value (a run with no milestones, or a
// malformed column) degrades to a nil slice — never a panic — so a no-milestone run
// renders exactly as today. apitypes is a stdlib-only leaf, so importing it here keeps
// slacksvc off workersvc (which would be an import cycle).
func decodeMilestones(raw []byte) []apitypes.Milestone {
	if len(raw) == 0 {
		return nil
	}
	var ms []apitypes.Milestone
	if err := json.Unmarshal(raw, &ms); err != nil {
		return nil
	}
	return ms
}

// decodeMilestoneIDs decodes a jsonb array of milestone ids (milestones_completed /
// milestones_in_progress) into a []string, degrading a nil/empty/non-array value to a
// nil slice.
func decodeMilestoneIDs(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil
	}
	return ids
}

// milestoneCounts derives a run's milestone progress from the frozen list and the
// completed-id set: total is len(frozen); done is the number of completed ids that are
// MEMBERS of the frozen list (a stale id referencing a milestone no longer frozen, or a
// duplicate, is not counted, so done can never exceed total). ok is false when the run
// has no frozen milestones, so every caller appends/posts nothing — the no-milestone run
// behaves exactly as today.
func milestoneCounts(rc store.GetSlackRunContextRow) (done, total int, ok bool) {
	frozen := decodeMilestones(rc.MilestonesFrozen)
	if len(frozen) == 0 {
		return 0, 0, false
	}
	inFrozen := make(map[string]struct{}, len(frozen))
	for _, m := range frozen {
		inFrozen[m.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(frozen))
	for _, id := range decodeMilestoneIDs(rc.MilestonesCompleted) {
		if _, member := inFrozen[id]; !member {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		done++
	}
	return done, len(frozen), true
}

// handleMilestone posts ONE `🧩 N/M` thread line when a milestone-structured run's
// completed COUNT strictly advances past the count the anchor last recorded (PRD #122
// M4). It is the milestone counterpart to handleGate/handleQuestion, bound to the
// existing-message branch of handle: the first post (the ErrNoRows branch) is the run's
// initial transition, which sets up the root and never a progress line.
//
// Dedup is on the COUNT, not on status: PublishState fires on every `running` report, so
// a redelivered event carries the same count and posts nothing, while a `+2` jump in one
// turn (1/7 → 3/7) posts ONE line and is not lost. The count-guarded setter refuses a
// regressing/equal write, so a slow drain can never re-spam.
//
// All best-effort: a failure is logged (redacted) and never affects the run.
func (n *Notifier) handleMilestone(ctx context.Context, rc store.GetSlackRunContextRow, anchor store.SlackRunMessage) {
	done, total, ok := milestoneCounts(rc)
	if !ok {
		return
	}
	notified := 0
	if anchor.MilestonesNotifiedCompleted.Valid {
		notified = int(anchor.MilestonesNotifiedCompleted.Int32)
	}
	if done <= notified {
		return // no advance since the last line — a redelivered report, not new progress
	}

	line := fmt.Sprintf("🧩 %d/%d", done, total)
	if title := inProgressTitle(rc); title != "" {
		line += " · working " + EscapeMrkdwn(title)
	}
	if _, perr := n.poster.Post(ctx, anchor.ChannelID, anchor.RootTs, ScrubSecrets(line)); perr != nil {
		n.logf("post milestone progress", perr)
		return // do not advance the notified count; a later event retries
	}
	if _, err := n.store.SetSlackRunMilestoneNotified(ctx, store.SetSlackRunMilestoneNotifiedParams{
		RunID: rc.ID, Count: pgtype.Int4{Int32: int32(done), Valid: true}, //nolint:gosec // G115: done is a per-run milestone count, a small bounded value, never near int32 range
	}); err != nil {
		n.logf("record milestone notified", err)
	}
}

// inProgressTitle returns the human title of the first in-progress milestone that HAS a
// non-empty title in the frozen list (skipping any in-progress id that is unknown or has
// a blank title), or "" when nothing is in progress or no in-progress id resolves to a
// title — in which case the thread line drops its ` · working …` suffix. The title is
// UNTRUSTED display text, so the caller routes it through EscapeMrkdwn like every other field.
func inProgressTitle(rc store.GetSlackRunContextRow) string {
	ids := decodeMilestoneIDs(rc.MilestonesInProgress)
	if len(ids) == 0 {
		return ""
	}
	titles := make(map[string]string)
	for _, m := range decodeMilestones(rc.MilestonesFrozen) {
		titles[m.ID] = m.Title
	}
	for _, id := range ids {
		if t := strings.TrimSpace(titles[id]); t != "" {
			return t
		}
	}
	return ""
}

// renderThreadBlocks builds the threaded terminal-transition event as Block Kit
// (message family B, PRD #268 M3), or returns ok=false when the transition is not
// worth interrupting the owner for. Each event is a status section (canonical
// glyph + bold label) plus, where they apply, a context block carrying the MR link,
// the failure reason (a FULL section, never context), the park detail and the run
// deep link. The plain-text fallback is built from fixed labels + the escaped
// repo#iid, never a raw model/forge field alone.
//
// 🔴 THIS USED TO READ "for a terminal transition, or "" for a NON-TERMINAL one",
// and PRD #35 made that false: `limit_wait` is non-terminal and posts. Stated as a
// RULING rather than left to be rediscovered, because the old sentence is exactly
// what would make the next reader file the park case as a violation and delete it.
//
// The rule is "worth interrupting for", and terminality was only ever a proxy for
// it. What broke the proxy is a status that lasts HOURS BY DESIGN — the contract was
// written before one existed. The mechanism that forces the widening: the root line
// is EDITED, and a Slack edit raises no notification, so a park with no threaded post
// is never communicated at all. The user learns their run is idle by happening to
// look, having lost the window in which they might have cancelled it.
//
// TWO PROPERTIES BOUND THE WIDENING, and they are what make it safe rather than
// merely better — check any future non-terminal case against both:
//
//  1. It is bounded by construction. RUN_LIMIT_MAX_WAITS caps parks per run
//     (default 5), so a run can post this at most that many times over its life.
//  2. The RESUME posts nothing. `queued` falls to the default arm (ok=false), which is
//     right — resuming is a return to normal and the edited root already shows it.
//     Without this half, a run that parks five times would produce ten posts and the
//     feature would read as a notification stream.
//
// The worker-originated failure reason is untrusted free text with no source-side
// length bound, so it is ScrubSecrets'd, whole-blob EscapeMrkdwn'd, and length-bounded
// (boundReason) before it becomes its own section. The MR link and deep link keep their
// raw <url|label> markup (server/forge-derived, https-guarded), ScrubSecrets'd as a
// no-op-on-clean last line of defense.
func renderThreadBlocks(rc store.GetSlackRunContextRow, base string) (blocks []slack.Block, fallback string, ok bool) {
	repo := ScrubSecrets(EscapeMrkdwn(rc.PathWithNamespace))
	linkElem := func() (slack.MixedElement, bool) {
		if u := runLink(base, rc.ID); u != "" {
			return threadMrkdwnElem(ScrubSecrets("🔗 " + u)), true
		}
		return nil, false
	}

	switch rc.Status {
	case "completed":
		// PRD #634 (surface-only): a run that completed because an operator scope
		// directive narrowed it lands as status='completed' AND stop_kind='scope_capped'
		// (m3). Slack SHOWS that partial — it adds no set-scope/stop control from Slack.
		// A normal completion (any other stop_kind, or NULL) is byte-identical to before.
		scopeCapped := rc.StopKind.Valid && rc.StopKind.String == "scope_capped"
		section := "✅ *Completed*"
		fallback := fmt.Sprintf("Completed · %s#%d", repo, iid(rc.IssueIid))
		if scopeCapped {
			section = "✅ *Completed — narrowed by operator scope directive*"
			fallback = fmt.Sprintf("Completed (scope-capped) · %s#%d", repo, iid(rc.IssueIid))
		}
		blocks = []slack.Block{threadSectionBlock(section)}
		var ctxElems []slack.MixedElement
		if scopeCapped {
			// Render the milestone COUNTS (N of M) — integers, so nothing to escape —
			// from the same decoders the running-thread lines use: the completed id set is
			// the numerator, the frozen {id,title} list the denominator.
			ctxElems = append(ctxElems, threadMrkdwnElem(fmt.Sprintf("%d of %d milestones",
				len(decodeMilestoneIDs(rc.MilestonesCompleted)), len(decodeMilestones(rc.MilestonesFrozen)))))
		}
		if mr := mrContextElem(rc); mr != "" {
			ctxElems = append(ctxElems, threadMrkdwnElem(ScrubSecrets(mr)))
		}
		if el, has := linkElem(); has {
			ctxElems = append(ctxElems, el)
		}
		if len(ctxElems) > 0 {
			blocks = append(blocks, slack.NewContextBlock("slack_thread_completed_ctx", ctxElems...))
		}
		return blocks, fallback, true

	case "failed":
		reason := strings.TrimSpace(rc.FailureReason.String)
		if reason == "run cancelled" {
			return cancelledThreadBlocks(linkElem), fmt.Sprintf("Cancelled · %s#%d", repo, iid(rc.IssueIid)), true
		}
		blocks = []slack.Block{threadSectionBlock("❌ *Failed*")}
		fallback = fmt.Sprintf("Failed · %s#%d", repo, iid(rc.IssueIid))
		if reason != "" {
			esc := EscapeMrkdwn(ScrubSecrets(boundReason(reason)))
			blocks = append(blocks, threadSectionBlock(esc))
			fallback += " — " + esc
		}
		if el, has := linkElem(); has {
			blocks = append(blocks, slack.NewContextBlock("slack_thread_failed_ctx", el))
		}
		return blocks, fallback, true

	case "cancelled":
		return cancelledThreadBlocks(linkElem), fmt.Sprintf("Cancelled · %s#%d", repo, iid(rc.IssueIid)), true

	case "limit_wait":
		// The ONE non-terminal case, ruled rather than accidental. The reasoning and the
		// two properties that bound it live on this function's doc comment above, in one
		// place, so they cannot drift from the contract they amend.
		blocks = []slack.Block{threadSectionBlock("⏸️ *Paused · usage limit*")}
		var ctxElems []slack.MixedElement
		if detail := limitWaitDetail(rc); detail != "" {
			ctxElems = append(ctxElems, threadMrkdwnElem(ScrubSecrets(detail)))
		}
		if el, has := linkElem(); has {
			ctxElems = append(ctxElems, el)
		}
		if len(ctxElems) > 0 {
			blocks = append(blocks, slack.NewContextBlock("slack_thread_limit_ctx", ctxElems...))
		}
		return blocks, fmt.Sprintf("Paused · usage limit · %s#%d", repo, iid(rc.IssueIid)), true

	default:
		return nil, "", false
	}
}

// cancelledThreadBlocks is the shared shape for both cancellation paths — a `failed`
// row whose reason is the sentinel "run cancelled", and a genuine `cancelled` status.
// A single 🚫 section plus the run deep link (when a base URL resolves).
func cancelledThreadBlocks(linkElem func() (slack.MixedElement, bool)) []slack.Block {
	blocks := []slack.Block{threadSectionBlock("🚫 *Cancelled*")}
	if el, has := linkElem(); has {
		blocks = append(blocks, slack.NewContextBlock("slack_thread_cancelled_ctx", el))
	}
	return blocks
}

// threadSectionBlock / threadMrkdwnElem are the tiny mrkdwn constructors the thread
// event builder reuses; the text is already scrubbed/escaped by the caller.
func threadSectionBlock(text string) *slack.SectionBlock {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)
}

func threadMrkdwnElem(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, text, false, false)
}

// limitWaitDetail renders ONLY the park detail suffix — the rate-limit type, the
// resume `<!date^…>` reader-local-time token, and the pause count — for the park
// thread event's context block; the head (`⏸️ Paused · usage limit`) is the section
// (PRD #268 M3, formerly limitWaitLabel which glued head+detail into one line).
//
// Every part is omitted when unknown rather than defaulted, exactly as the server's
// own failure-reason composition does — the line never claims a fact uzi does not
// have. A park with neither a window nor a stamp yields "", so the context block is
// just the deep link (or omitted when no base URL resolves).
func limitWaitDetail(rc store.GetSlackRunContextRow) string {
	var b strings.Builder
	if rc.RateLimitType.Valid && rc.RateLimitType.String != "" {
		// EscapeMrkdwn even though workersvc has already allowlisted this to a
		// seven-member enum and 00091's CHECK backstops it. The escape costs nothing and
		// covers the exact population the CHECK exists for: a backfill, an admin tool, or
		// a later writer that bypassed the coercion.
		b.WriteString("(" + EscapeMrkdwn(rc.RateLimitType.String) + ")")
	}
	if rc.RetryNotBefore.Valid {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		// Slack's own date markup, so the timestamp renders in the READER's timezone
		// rather than the server's. The fallback after `|` is what Slack shows when it
		// cannot render the token, and it is UTC-explicit so a fallback is never
		// ambiguous about which zone it means.
		fmt.Fprintf(&b, "resumes <!date^%d^{time}|%s>",
			rc.RetryNotBefore.Time.Unix(), rc.RetryNotBefore.Time.UTC().Format("15:04 MST"))
	}
	if rc.LimitWaitCount > 1 {
		// Only from the SECOND park. "attempt 1" on a first park is noise; a rising
		// count is the signal that this run is burning its retry budget and may be about
		// to fail for good.
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "(pause %d)", rc.LimitWaitCount)
	}
	return b.String()
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

// statusGlyph is the canonical (emoji, label) pair for a run's status on the root line
// (PRD #268 M2). Every glyph is emoji-presentation so the DM reads consistently beside
// the full-color ✅ ❌ 🚫. The MR ref is NOT inlined into the completed label — the MR
// now rides the root's context block (mrContextElem) — and the milestone/health flags
// live there too, not glued onto the label. A `failed` row whose reason is the sentinel
// "run cancelled" reads as Cancelled, exactly as the old statusLabel special-cased it.
func statusGlyph(rc store.GetSlackRunContextRow) (emoji, label string) {
	switch rc.Status {
	case "queued":
		return "⏳", "Queued"
	case "claimed", "running":
		return "▶️", "Running"
	case "awaiting_approval":
		return "⏸️", "Needs your approval"
	case "awaiting_input":
		// Without this case the default arm below renders the raw enum `awaiting_input`
		// on the root line of a user-facing DM — the web has a replace(/_/g," ") fallback,
		// Slack has none (PRD #88 M3).
		return "❓", "Needs your answer"
	case "awaiting_followup":
		// PRD #517: an interactive task parked awaiting the user's next follow-up. Distinct
		// emoji and label from awaiting_input's ❓ "Needs your answer" — this is a follow-up
		// park, not a clarification. Without this case the default arm renders the raw enum
		// `awaiting_followup` on the root line (Slack has no _→space fallback).
		return "💬", "Awaiting your follow-up"
	case "limit_wait":
		return "⏸️", "Paused · usage limit"
	case "completed":
		return "✅", "Completed"
	case "failed":
		if strings.TrimSpace(rc.FailureReason.String) == "run cancelled" {
			return "🚫", "Cancelled"
		}
		return "❌", "Failed"
	case "cancelled":
		return "🚫", "Cancelled"
	default:
		// An unknown enum keeps its raw string with no glyph, mirroring the old default
		// arm — a byte-honest degrade rather than a fabricated emoji.
		return "", rc.Status
	}
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

// mrLink is the forge merge/pull-request URL for the completed-run thread line. It
// prefers the forge-supplied mr_web_url the worker persisted at MR creation (PRD
// #65 D8) — the only correct link on Forgejo, whose `/{owner}/{repo}/pulls/N`
// grammar the GitLab reconstruction below never knew. That value is WORKER-supplied
// and stored without scheme validation, so it is https-guarded here exactly like
// the web's isHttpsUrl before it becomes a rendered link. Rows created before
// mr_web_url landed (all GitLab — the forgejo gate flips last) fall back to
// reconstructing the GitLab URL from the repo web url. The chosen URL is
// mrkdwn-escaped either way (a normal URL has no & < >, so this is a no-op for legit
// links and only neutralizes a hostile one).
func mrLink(rc store.GetSlackRunContextRow) string {
	if rc.MrWebUrl.Valid && isHTTPSURL(rc.MrWebUrl.String) {
		return EscapeMrkdwn(rc.MrWebUrl.String)
	}
	if !rc.MrIid.Valid || rc.WebUrl == "" {
		return ""
	}
	return fmt.Sprintf("%s/-/merge_requests/%d", EscapeMrkdwn(strings.TrimRight(rc.WebUrl, "/")), rc.MrIid.Int64)
}

// isHTTPSURL guards a worker-supplied URL before it becomes a rendered link — the
// Go twin of the web's isHttpsUrl (api.ts). https-only, so a hostile http:/
// javascript: mr_web_url is never surfaced.
func isHTTPSURL(u string) bool {
	return strings.HasPrefix(u, "https://")
}

// forgeMrAbbrev / forgeMrRef are the Go twins of web/src/lib/forgeNoun.ts (PRD #65
// D2, #238 D2), kept adjacent-in-review with the SAME mapping so a Forgejo/GitHub
// run's DM reads in its own vocabulary: "PR #N" rather than GitLab's "MR !N". Both
// PR-forges (Forgejo AND GitHub) are named explicitly — a missing github arm would
// silently render "MR !N" for a GitHub card (the D2 trap). Any unknown/absent
// forge_type is GitLab's form.
func forgeMrAbbrev(forgeType string) string {
	if forgeType == "forgejo" || forgeType == "github" {
		return "PR"
	}
	return "MR"
}

func forgeMrRef(forgeType string) string {
	// Forgejo "#N" for a pull request; GitHub "#N" (its PRs and issues share one
	// number namespace, so "#42" is correct). GitLab writes "!N".
	if forgeType == "forgejo" || forgeType == "github" {
		return "#"
	}
	return "!"
}

func iid(v pgtype.Int8) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
