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

// healthNudgeHead is the fixed, enum-keyed opening line of a threaded health nudge.
// Server-authored (no forge/worker content); the caller still runs ScrubSecrets on
// the whole message as a last line of defense.
func healthNudgeHead(health string) string {
	switch health {
	case healthStalled:
		return "⚠ This run has gone quiet and may be stuck."
	case healthLooping:
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
	body := healthNudgeHead(health)
	if reason != "" {
		body += " " + reason + "."
	}
	if link := runLink(base, runID); link != "" {
		body += "\n" + link
	}
	return body
}
