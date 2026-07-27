package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// NotifierStore is the slice of generated queries the notifier reads/writes.
// *store.Queries satisfies it.
type NotifierStore interface {
	GetSlackRunContext(ctx context.Context, runID uuid.UUID) (store.GetSlackRunContextRow, error)
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
}

type stateEvent struct {
	runID  uuid.UUID
	status string
}

// notifyEvent is a generic inbox notification bound for a user's DM (PRD #46 M2).
// title is a caller-set fixed label; body is dynamic, potentially untrusted free
// text; link is an in-app deep-link URL. All three are escaped/scrubbed at render.
type notifyEvent struct {
	userID uuid.UUID
	title  string
	body   string
	link   string
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
		store:    s,
		poster:   poster,
		baseURL:  baseURL,
		logger:   logger,
		ch:       make(chan stateEvent, notifierQueue),
		notifyCh: make(chan notifyEvent, notifierQueue),
		healthCh: make(chan healthEvent, notifierQueue),
	}
}

// PublishNotification enqueues a generic inbox notification for delivery to the
// user's Slack DM (PRD #46 M2). It implements notifysvc.Slacker. Like PublishState
// it MUST NOT block: it enqueues and returns, dropping the event if the queue is
// full (Slack is strictly best-effort — the inbox row is already durable).
func (n *Notifier) PublishNotification(userID uuid.UUID, title, body, link string) {
	select {
	case n.notifyCh <- notifyEvent{userID: userID, title: title, body: body, link: link}:
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

// PublishMessage is a deliberate no-op: run message CONTENT never goes to Slack
// (content minimization — only status/title/links do).
func (n *Notifier) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
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

	if _, err := n.poster.Post(ctx, channel, "", renderNotification(ev)); err != nil {
		n.logf("post notification", err)
	}
}

// renderNotification builds the content-minimized DM for a generic notification.
// The title is a fixed caller-set label and the body is dynamic, potentially
// untrusted free text (a judge verdict summary, a repo/agent name), so BOTH are
// mrkdwn-escaped individually — exactly like renderRoot escapes the issue title —
// before they sit beside the trusted deep-link markup. The link is an in-app URL
// whose base is operator-set and http(s)-validated, rendered raw as <url|label>
// like runLink. The whole line is then ScrubSecrets'd as a last line of defense.
func renderNotification(ev notifyEvent) string {
	head := "[uzi] " + EscapeMrkdwn(strings.TrimSpace(ev.title))
	if body := strings.TrimSpace(ev.body); body != "" {
		head += " — " + EscapeMrkdwn(boundReason(body))
	}
	if link := notifyLink(ev.link); link != "" {
		head += "\n" + link
	}
	return ScrubSecrets(head)
}

// notifyLink wraps a caller-supplied in-app deep-link URL as a Slack mrkdwn link,
// or returns "" when no link is set. The URL is trimmed; an empty value yields no
// markup so the DM is just the title + body.
func notifyLink(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	return fmt.Sprintf("<%s|Open in uzi>", url)
}

// handle processes one transition: resolve the owner's delivery target, then
// post the root DM (first time) or edit it and thread the outcome. Every failure
// path logs redacted and returns — a run is never affected.
func (n *Notifier) handle(ctx context.Context, ev stateEvent) {
	rc, err := n.store.GetSlackRunContext(ctx, ev.runID)
	if err != nil {
		// No row for a chat run (PRD #39): GetSlackRunContext INNER-JOINs repos, and a
		// chat run has no repo (repo_id NULL), so it returns ErrNoRows here. Chat
		// transitions have no repo-scoped DM to send — skip silently (as for a run
		// deleted out from under us), never a noisy error per chat transition.
		if !errors.Is(err, pgx.ErrNoRows) {
			n.logf("load run context", err)
		}
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
	root := ScrubSecrets(renderRoot(rc, base))

	existing, err := n.store.GetSlackRunMessage(ctx, ev.runID)
	var anchor store.SlackRunMessage
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		ts, perr := n.poster.Post(ctx, channel, "", root)
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
		if uerr := n.poster.Update(ctx, existing.ChannelID, existing.RootTs, root); uerr != nil {
			n.logf("update root", uerr)
		}
		if evt := renderThread(rc, base); evt != "" {
			if _, perr := n.poster.Post(ctx, existing.ChannelID, existing.RootTs, ScrubSecrets(evt)); perr != nil {
				n.logf("post thread event", perr)
			}
		}
	}

	n.handleGate(ctx, rc, anchor, base)
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
		if _, err := n.store.SetSlackRunGateGen(ctx, store.SetSlackRunGateGenParams{
			RunID: rc.ID, GateTs: pgText(ts), GateState: pgText(gateStateOpen),
			GateGeneration: pgtype.Int4{Int32: int32(currentGen), Valid: true},
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

	// Re-render the root so the ⚠ label reflects the current flag (create it if a
	// stuck queued run never got a state DM). renderRoot carries the flag via
	// healthSuffix, keyed off the run context's current health.
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
	if _, perr := n.poster.Post(ctx, channel, threadTS, ScrubSecrets(healthNudgeText(ev.health, ev.reason, base, rc.ID))); perr != nil {
		n.logf("post health nudge", perr)
	}
}

// ensureRoot returns the run's DM anchor, posting the root message when none exists
// yet (a stuck queued run that never sent a state DM still reaches its owner) or
// editing the existing one to the current health-aware render. ok is false on an
// unrecoverable error. It is the health path's counterpart to handle's inline anchor
// flow (which also threads terminal outcome events, so the two are not merged).
func (n *Notifier) ensureRoot(ctx context.Context, rc store.GetSlackRunContextRow, channel, base string) (store.SlackRunMessage, bool) {
	root := ScrubSecrets(renderRoot(rc, base))
	existing, err := n.store.GetSlackRunMessage(ctx, rc.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		ts, perr := n.poster.Post(ctx, channel, "", root)
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
		if uerr := n.poster.Update(ctx, existing.ChannelID, existing.RootTs, root); uerr != nil {
			n.logf("update root (health)", uerr)
		}
		return existing, true
	}
}

func (n *Notifier) logf(what string, err error) {
	n.logger.Warn("slack notifier: "+what+" failed (best-effort)", "error", Redact(err.Error()))
}

// renderRoot builds the content-minimized root line: repo#iid «title» — status,
// plus an Open-in-uzi deep link. No plan/diff — the plan is one click away. The
// forge-controlled repo path and issue title are mrkdwn-escaped individually so
// they cannot inject a spoofed <url|label> link or a <@Uxxx> mention into the DM;
// the deep-link markup below stays raw (its base is operator-set, its id a uuid).
func renderRoot(rc store.GetSlackRunContextRow, base string) string {
	head := fmt.Sprintf("[uzi] run on %s#%d «%s» — %s%s",
		EscapeMrkdwn(rc.PathWithNamespace), iid(rc.IssueIid), EscapeMrkdwn(rc.IssueTitle), statusLabel(rc), healthSuffix(rc))
	if link := runLink(base, rc.ID); link != "" {
		head += "\n" + link
	}
	return head
}

// renderThread returns the threaded event for a transition, or "" when the
// transition is not worth interrupting the owner for. Completed carries the MR link;
// failed the reason. The worker-originated failure reason is length-bounded and
// mrkdwn-escaped before it goes out (it is untrusted free text with no source-side
// length bound).
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
//  2. The RESUME posts nothing. `queued` falls to the default arm, which is right —
//     resuming is a return to normal and the edited root already shows it. Without
//     this half, a run that parks five times would produce ten posts and the feature
//     would read as a notification stream.
func renderThread(rc store.GetSlackRunContextRow, base string) string {
	switch rc.Status {
	case "completed":
		if mr := mrLink(rc); mr != "" {
			return "✅ completed — " + mr
		}
		return "✅ completed"
	case "failed":
		reason := strings.TrimSpace(rc.FailureReason.String)
		if reason == "run cancelled" {
			return "🚫 cancelled"
		}
		if reason != "" {
			return "❌ failed: " + EscapeMrkdwn(boundReason(reason))
		}
		return "❌ failed"
	case "cancelled":
		return "🚫 cancelled"
	case "limit_wait":
		// The ONE non-terminal case, ruled rather than accidental. The reasoning and the
		// two properties that bound it live on this function's doc comment above, in one
		// place, so they cannot drift from the contract they amend.
		return limitWaitLabel(rc)
	default:
		return ""
	}
}

// limitWaitLabel renders a park for both the root line and the threaded event.
//
// ONE function for both on purpose: the two would otherwise drift into describing
// the same state differently in the same DM, which is the failure the reader
// notices and cannot explain.
//
// Every part is omitted when unknown rather than defaulted, exactly as the server's
// own failure-reason composition does — the line never claims a fact uzi does not
// have. A park with neither a window nor a stamp still renders honestly as
// "⏸ paused: usage limit".
func limitWaitLabel(rc store.GetSlackRunContextRow) string {
	s := "⏸ paused: usage limit"
	if rc.RateLimitType.Valid && rc.RateLimitType.String != "" {
		// EscapeMrkdwn even though workersvc has already allowlisted this to a
		// seven-member enum and 00090's CHECK backstops it. The escape costs nothing and
		// covers the exact population the CHECK exists for: a backfill, an admin tool, or
		// a later writer that bypassed the coercion. Per-field, never on the assembled
		// line, so the emoji and separators this function owns are not escaped.
		s += " (" + EscapeMrkdwn(rc.RateLimitType.String) + ")"
	}
	if rc.RetryNotBefore.Valid {
		// Slack's own date markup, so the timestamp renders in the READER's timezone
		// rather than the server's. The fallback after `|` is what Slack shows when it
		// cannot render the token, and it is UTC-explicit so a fallback is never
		// ambiguous about which zone it means.
		s += fmt.Sprintf("; resumes <!date^%d^{time}|%s>",
			rc.RetryNotBefore.Time.Unix(), rc.RetryNotBefore.Time.UTC().Format("15:04 MST"))
	}
	if rc.LimitWaitCount > 1 {
		// Only from the SECOND park. "attempt 1" on a first park is noise; a rising
		// count is the signal that this run is burning its retry budget and may be about
		// to fail for good.
		s += fmt.Sprintf(" (pause %d)", rc.LimitWaitCount)
	}
	return s
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

// statusLabel is the compact status shown on the root line.
func statusLabel(rc store.GetSlackRunContextRow) string {
	switch rc.Status {
	case "queued":
		return "queued"
	case "claimed", "running":
		return "▶ running"
	case "awaiting_approval":
		return "⏸ needs your approval"
	case "limit_wait":
		return limitWaitLabel(rc)
	case "completed":
		if rc.MrIid.Valid {
			return fmt.Sprintf("✅ completed (%s %s%d)", forgeMrAbbrev(rc.ForgeType), forgeMrRef(rc.ForgeType), rc.MrIid.Int64)
		}
		return "✅ completed"
	case "failed":
		reason := strings.TrimSpace(rc.FailureReason.String)
		if reason == "run cancelled" {
			return "🚫 cancelled"
		}
		return "❌ failed"
	case "cancelled":
		return "🚫 cancelled"
	default:
		return rc.Status
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
// D2), kept adjacent-in-review with the SAME mapping so a Forgejo run's DM reads in
// Forgejo's vocabulary: "PR #N" rather than GitLab's "MR !N". Any unknown/absent
// forge_type is GitLab's form — the only connection kind that exists until the
// handler gate flips (M6b).
func forgeMrAbbrev(forgeType string) string {
	if forgeType == "forgejo" {
		return "PR"
	}
	return "MR"
}

func forgeMrRef(forgeType string) string {
	if forgeType == "forgejo" {
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
