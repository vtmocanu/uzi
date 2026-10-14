// Package runpresentation supplies shared human-facing run labels.
package runpresentation

// HumanStopReason returns the recognized human stop, or an empty string.
// A cancelled status takes precedence over conflicting or missing metadata.
func HumanStopReason(status, stopKind string) string {
	if status == "cancelled" {
		return "cancelled"
	}
	if status == "failed" {
		switch stopKind {
		case "cancelled", "plan_rejected":
			return stopKind
		}
	}
	return ""
}

// HumanStopLabel returns a full label from the closed human-stop vocabulary.
func HumanStopLabel(status, stopKind string) string {
	switch HumanStopReason(status, stopKind) {
	case "cancelled":
		return "stopped (cancelled)"
	case "plan_rejected":
		return "stopped (plan rejected)"
	default:
		return ""
	}
}
