package slacksvc

import (
	"fmt"
	"time"

	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run-health flag values (PRD #47), mirrored here so slacksvc keys its own Slack
// framing off the enum WITHOUT importing workersvc's detector constants — the
// detector owns the reason TEXT (it travels in the PublishHealth event and the
// health_reason column); slacksvc owns the Slack-facing wording keyed off the enum.
//
// TWO EXCEPTIONS, spelled out here rather than only at their sites below, because
// this is the paragraph a reader hits first. Both are the SAME shape: an enum that
// carries two causes, where the enum alone cannot tell them apart and the existing
// head sentence is FALSE of the second cause. healthNudgeHead therefore compares the
// reason against a mirrored constant in those two arms only:
//
//   - `looping`, since PRD #108 M4 — reasonPersistFailing;
//   - `waiting_worker`, since issue #182 — reasonVerdictUndelivered.
//
// Everything else still keys off the enum, and each exception degrades to its
// enum-keyed wording if the mirror ever drifts.
//
// A THIRD instance of this shape should stop and reconsider the mechanism rather than
// adding a third mirrored constant: at that point the enum is carrying more meanings
// than it has values, and the fix is a health enum (migration + web union + badge
// label), not more reason-sniffing here.
const (
	healthStalled       = "stalled"
	healthLooping       = "looping"
	healthSlow          = "slow"
	healthWaitingWorker = "waiting_worker"
	healthApprovalIdle  = "approval_idle"
)

// reasonPersistFailing mirrors workersvc's PRD #108 M4 reason string — the ONE
// place slacksvc reads a reason rather than only relaying it, and the paragraph
// above is qualified rather than falsified by it.
//
// Why the exception is necessary: 'looping' now carries two genuinely different
// causes. The tool-window arm means the agent is repeating the same call; the
// persistence arm means the agent's updates cannot be SAVED, so it re-sends them.
// The enum alone cannot tell them apart, and the existing head sentence ("repeating
// the same step") is simply false of the second. Adding a health enum instead would
// need a migration on runs.health's CHECK, a RunHealth union change in web, a badge
// label and this same case — for wording.
//
// Why it is safe: a miss (workersvc rewords, this constant does not) degrades to
// the generic looping head, which is the behaviour before this existed. Pinned from
// BOTH sides, because a one-sided pin would catch only one drift direction and the
// other one fails silently: TestReasonPersistFailingIsMirroredBySlack in
// workersvc/persistfail_test.go, and TestReasonPersistFailingMirrorsWorkersvc in
// health_nudge_head_test.go beside this file. Both carry the literal.
const reasonPersistFailing = "the agent's updates can't be saved, so it keeps resending them"

// reasonVerdictUndelivered mirrors workersvc's issue #182 reason string, on the same
// terms as reasonPersistFailing above and for the same reason.
//
// Why the exception is necessary: 'waiting_worker' now carries two causes. The queued
// arm means NO worker has claimed the run; the approval-gate arm means the run's
// worker already holds it and has not yet acted on the response its owner submitted.
// The enum alone cannot tell them apart, and the existing head sentence ("still
// waiting for a worker to pick it up") is FALSE of the second: it tells an owner their
// run is unclaimed while a worker is holding it. Worse, it lands right beside the
// detector's own reason, so the message contradicts itself in two consecutive
// sentences — and it does so on the one path #182 exists to fix, since every Slack
// approve is selectionless and therefore never bumps runs.updated_at.
//
// Why it is safe: a miss (workersvc rewords, this constant does not) degrades to the
// generic waiting-for-a-worker head, which is the behaviour before this existed.
// Pinned from BOTH sides for the reason the sibling gives — a one-sided pin catches
// one drift direction and the other fails silently: TestReasonVerdictUndeliveredIsMirroredBySlack
// in workersvc/health_verdict_test.go, and TestReasonVerdictUndeliveredMirrorsWorkersvc
// in health_nudge_head_test.go beside this file. Both carry the literal.
const reasonVerdictUndelivered = "the worker hasn't picked up your response yet"

// isHealthFlaggableStatus mirrors the server's flaggable set (Decision 3): the root
// carries its ⚠️ context flag only while the run is still in one of these, so a
// terminal run never shows a stale flag on its Slack root.
func isHealthFlaggableStatus(status string) bool {
	switch status {
	case "queued", "running", "awaiting_approval":
		return true
	default:
		return false
	}
}

// healthRootLabel is the short word appended to the root status line for a flag, or
// "" for a healthy/unknown value.
func healthRootLabel(health string) string {
	switch health {
	case healthStalled:
		return "stalled"
	case healthLooping:
		return "looping"
	case healthSlow:
		return "near timeout"
	case healthWaitingWorker:
		return "waiting for a worker"
	case healthApprovalIdle:
		return "still needs approval"
	default:
		return ""
	}
}

// healthContextLabel is the health flag word for the root's context block (PRD #268
// M2, formerly the ⚠ label-suffix healthSuffix). Empty unless the run is flagged AND
// still in a flaggable status, so a healthy or terminal run's root carries no flag.
// The caller (rootBlocks) prefixes the ⚠️ glyph and scrubs/escapes the word.
func healthContextLabel(rc store.GetSlackRunContextRow) string {
	if rc.Health == "" || rc.Health == "ok" || !isHealthFlaggableStatus(rc.Status) {
		return ""
	}
	return healthRootLabel(rc.Health)
}

// healthNudgeHead is the fixed opening line of a threaded health nudge, keyed off the
// enum and — for the two enums that carry more than one cause (`looping`,
// `waiting_worker`) — off the server-controlled reason. Server-authored (no
// forge/worker content); the caller still runs ScrubSecrets on the whole message as a
// last line of defense.
//
// NOT mirrored into healthRootLabel, deliberately and consistently with `looping`:
// the root suffix is a short badge word with no reason argument, so both causes of an
// enum share it. A head is a SENTENCE and can be false; a one-word label is only ever
// imprecise.
func healthNudgeHead(health, reason string) string {
	switch health {
	case healthStalled:
		return "💤 This run has gone quiet and may be stuck."
	case healthLooping:
		// PRD #108 M4 ADDED the persistence arm; it did not change the tool-repetition
		// one. PRD #268 M3 swaps the leading glyph on BOTH looping lines to the canonical
		// 🔁 (from the generic ⚠️) — a glyph change, not a wording change, so the
		// sentences after the glyph stay exactly as written and no nudge's wording moves.
		if reason == reasonPersistFailing {
			return "🔁 This run's updates aren't being saved, so it keeps re-sending them."
		}
		return "🔁 This run looks like it's repeating the same step."
	case healthSlow:
		// PRD #1170 reframes the wall-clock `slow` flag as a budget-relative near-timeout
		// warning: the enum value survives (D1) but every human-facing word changes, so the
		// nudge now names the actual consequence — the run will be stopped at its timeout.
		return "⏰ This run is close to its timeout and will be stopped when it reaches it."
	case healthWaitingWorker:
		// Issue #182 ADDED this arm; it did not change the one below it. The existing
		// sentence stays exactly as written for the unclaimed-run cause it was written
		// for, so no existing nudge's wording moves.
		//
		// The replacement leads with what the owner needs to know first: they are not
		// the blocker. Saying so explicitly matters more here than in the sibling
		// exception, because the sentence it replaces asks for an action the owner has
		// ALREADY taken.
		if reason == reasonVerdictUndelivered {
			return "⏳ You've already responded; this run is waiting on its worker, not on you."
		}
		return "⏳ This run is still waiting for a worker to pick it up."
	case healthApprovalIdle:
		return "⏸️ This run is still waiting for your approval."
	default:
		return "⚠️ This run needs a look."
	}
}

// healthNudgeBlocks builds the threaded run-health nudge as Block Kit (message family
// E, PRD #268 M3): a section carrying the enum-keyed framing head + the
// server-controlled reason (empty ok), and a context block with the run deep link when
// a base URL resolves. The head is server-authored (a fixed per-enum sentence carrying
// its own canonical glyph), so it needs no escaping; the reason carries NO forge- or
// worker-controlled field either, but it is still ScrubSecrets'd (last line of defense)
// and EscapeMrkdwn'd (a server reword can never smuggle markup into the DM).
//
// The fallback is the head sentence alone (plain, no markup): it is a fixed per-enum
// string, never a raw reason, so it is a safe OS-notification line on its own.
//
// PRD #1189 (M4) — the near-timeout (`slow`) flag ONLY gains two extras, guarded so
// every OTHER flag's blocks stay byte-identical to before:
//
//   - a line appended to the section: `Stops at <!date^<unix>^{time}|HH:MM UTC>
//     (1h 05m left). Last checkpoint pushed 9m ago.` — the deadline in the viewer's own
//     locale via Slack's <!date^…> form (D9: no stored time zone; the fallback is UTC),
//     the time left as deadline − now, and the checkpoint age from checkpointTipAt. The
//     checkpoint clause is omitted when checkpointTipAt is null, and the whole line is
//     omitted when deadline is nil (a run with no wall deadline never gets the slow flag,
//     so this is only a defensive guard).
//   - a second context line: `Extend: ` + a `uzi run extend <id> --by <dur>` code span —
//     the duration FITS the remaining allowance (2h when there is room, else the remaining
//     allowance floored to whole minutes), and the line is omitted when extending is exhausted
//     (budget_extension_seconds >= extensionCap), turned off (extensionCap == 0), or the sliver
//     left is under the 60s extend floor — so the DM never suggests a command that would 409.
//
// Both extras are entirely server-computed (a unix stamp, two durations, the run uuid,
// and a fixed verb), so they carry no forge/worker text and need no escaping; the
// <!date^…> token and the backticks are intentional Slack markup. deadline is computed
// by the caller (workersvc.RunDeadline over the rc fields) so this file keeps no
// workersvc import, and now is passed in so the durations are testable.
func healthNudgeBlocks(health, reason, base string, rc store.GetSlackRunContextRow, deadline *time.Time, extensionCap int, now time.Time) (blocks []slack.Block, fallback string) {
	head := healthNudgeHead(health, reason)
	section := head
	if reason != "" {
		section += " " + EscapeMrkdwn(ScrubSecrets(reason)) + "."
	}
	var extendCtx string
	if health == healthSlow {
		if deadline != nil {
			line := fmt.Sprintf("Stops at <!date^%d^{time}|%s UTC> (%s left).",
				deadline.Unix(), deadline.UTC().Format("15:04"), nearTimeoutLeft(deadline.Sub(now)))
			if rc.CheckpointTipAt.Valid {
				line += " Last checkpoint pushed " + humanWait(now.Sub(rc.CheckpointTipAt.Time)) + " ago."
			}
			section += "\n" + line
		}
		if extensionCap > 0 {
			// Render a duration that FITS the remaining allowance, not a fixed 2h: with less
			// than 2h left the server 409s a `--by 2h`, so the nudge would suggest a command
			// that fails. extendNudgeBy returns "" when under the 60s extend floor (line omitted).
			if by := extendNudgeBy(extensionCap - int(rc.BudgetExtensionSeconds)); by != "" {
				extendCtx = "Extend: `uzi run extend " + rc.ID.String() + " --by " + by + "`"
			}
		}
	}
	blocks = []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, section, false, false), nil, nil)}
	if link := runLink(base, rc.ID); link != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_health_nudge_ctx",
			slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets("🔗 "+link), false, false)))
	}
	if extendCtx != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_health_nudge_extend",
			slack.NewTextBlockObject(slack.MarkdownType, extendCtx, false, false)))
	}
	return blocks, head
}

// extendNudgeBy renders the `--by` duration for the near-timeout Extend command as a
// Go-duration-parseable label with NO spaces ("2h", "1h58m", "45m"): the friendly 2h default
// when the remaining allowance leaves room, otherwise the remaining allowance floored to whole
// minutes so the suggested command always fits under the cap (a fixed 2h would 409 once under
// two hours remain). Returns "" when the remaining allowance is under the 60s extend floor, so
// the caller omits the line rather than suggest a command the server would reject. Mirrors
// workersvc.extendDurationLabel without importing workersvc (this file keeps no workersvc
// dependency — the deadline is computed by the caller).
func extendNudgeBy(remaining int) string {
	const (
		minExtendSeconds     = 60
		defaultExtendSeconds = 7200 // 2h
	)
	if remaining < minExtendSeconds {
		return ""
	}
	by := remaining
	if by > defaultExtendSeconds {
		by = defaultExtendSeconds
	}
	h := by / 3600
	m := (by % 3600) / 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// nearTimeoutLeft renders the time remaining before a run's wall-clock deadline for the
// near-timeout nudge, matching the PRD literal: "1h 05m" once an hour is in play (minutes
// zero-padded to two digits), "9m" under an hour, and "0m" at or past the deadline (a
// defensive clamp — the slow flag fires while the deadline is still ahead). It is a
// distinct, padded formatter from humanWait (which the checkpoint-age clause reuses),
// because the PRD writes the two clauses in different styles on purpose.
func nearTimeoutLeft(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}
