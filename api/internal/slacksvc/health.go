package slacksvc

import (
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Run-health flag values (PRD #47), mirrored here so slacksvc keys its own Slack
// framing off the enum WITHOUT importing workersvc's detector constants — the
// detector owns the reason TEXT (it travels in the PublishHealth event and the
// health_reason column); slacksvc owns the Slack-facing wording keyed off the enum.
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
// the workersvc side by TestReasonPersistFailingMatchesSlackMirror.
const reasonPersistFailing = "the agent's updates can't be saved, so it keeps resending them"

// isHealthFlaggableStatus mirrors the server's flaggable set (Decision 3): the root
// label appends its ⚠ variant only while the run is still in one of these, so a
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

// healthSuffix is the ⚠ fragment appended to the root's status label (Decision 7).
// Empty unless the run is flagged AND still in a flaggable status, so a healthy or
// terminal run's root is unchanged.
func healthSuffix(rc store.GetSlackRunContextRow) string {
	if rc.Health == "" || rc.Health == "ok" || !isHealthFlaggableStatus(rc.Status) {
		return ""
	}
	if label := healthRootLabel(rc.Health); label != "" {
		return " · ⚠ " + label
	}
	return ""
}

// healthNudgeHead is the fixed opening line of a threaded health nudge, keyed off
// the enum and — for the one enum that carries two causes — off the
// server-controlled reason. Server-authored (no forge/worker content); the caller
// still runs ScrubSecrets on the whole message as a last line of defense.
func healthNudgeHead(health, reason string) string {
	switch health {
	case healthStalled:
		return "⚠ This run has gone quiet and may be stuck."
	case healthLooping:
		// PRD #108 M4 ADDED this arm; it did not change the one below it. The existing
		// sentence stays exactly as written for the tool-repetition cause it was
		// written for, so no existing nudge's wording moves.
		if reason == reasonPersistFailing {
			return "⚠ This run's updates aren't being saved, so it keeps re-sending them."
		}
		return "⚠ This run looks like it's repeating the same step."
	case healthSlow:
		return "⚠ This run is taking longer than usual."
	case healthWaitingWorker:
		return "⏳ This run is still waiting for a worker to pick it up."
	case healthApprovalIdle:
		return "⏸ This run is still waiting for your approval."
	default:
		return "⚠ This run needs a look."
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
