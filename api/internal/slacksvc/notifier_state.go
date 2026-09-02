package slacksvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The run-state DM seam (PRDs #25/#122/#268): handle turns a run-state transition
// into the root/gate/question/milestone renderers and the thread blocks.

// handle processes one transition: resolve the owner's delivery target, then
// post the root DM (first time) or edit it and thread the outcome. Every failure
// path logs redacted and returns — a run is never affected.
func (n *Notifier) handle(ctx context.Context, ev stateEvent) {
	// On a terminal transition — for EVERY run kind — free any chat turn-streaming
	// state so the maps track only in-flight runs (PRD #191 M3). Deferred so it runs
	// after the render/handleChat call on all paths; a frame that trails this is caught
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
	if rc.Kind == runkind.Judge || rc.Kind == runkind.SelfImprove {
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
