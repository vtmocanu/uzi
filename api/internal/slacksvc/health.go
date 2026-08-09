package slacksvc

import (
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
		return "slow"
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
		return "⚠️ This run has gone quiet and may be stuck."
	case healthLooping:
		// PRD #108 M4 ADDED this arm; it did not change the one below it. The existing
		// sentence stays exactly as written for the tool-repetition cause it was
		// written for, so no existing nudge's wording moves.
		if reason == reasonPersistFailing {
			return "⚠️ This run's updates aren't being saved, so it keeps re-sending them."
		}
		return "⚠️ This run looks like it's repeating the same step."
	case healthSlow:
		return "⚠️ This run is taking longer than usual."
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

// healthNudgeText is the threaded nudge body: the enum-keyed framing, the
// server-controlled reason label (empty ok), and the run deep link. It carries NO
// forge- or worker-controlled field, so there is nothing to EscapeMrkdwn; the caller
// still passes the whole string through ScrubSecrets.
func healthNudgeText(health, reason, base string, runID uuid.UUID) string {
	body := healthNudgeHead(health, reason)
	if reason != "" {
		body += " " + reason + "."
	}
	if link := runLink(base, runID); link != "" {
		body += "\n" + link
	}
	return body
}
