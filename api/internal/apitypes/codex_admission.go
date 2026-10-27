package apitypes

import (
	"strings"
	"time"
)

const (
	codexRateAdmissionPrefix     = "waiting: provider reported a rate limit; account admission eligible after "
	codexOverloadAdmissionPrefix = "waiting: provider reported overload; account admission eligible after "
)

// CodexAdmissionReason describes eligibility, without promising a start time.
// Unknown classes produce no reason rather than mislabeling the provider signal.
func CodexAdmissionReason(class string, eligibleAt time.Time) string {
	var prefix string
	switch class {
	case "rate_limit":
		prefix = codexRateAdmissionPrefix
	case "overload":
		prefix = codexOverloadAdmissionPrefix
	default:
		return ""
	}
	return prefix + eligibleAt.UTC().Format(time.RFC3339Nano)
}

// IsCodexAdmissionReason recognizes persisted admission reasons for read-time replacement.
func IsCodexAdmissionReason(reason string) bool {
	return strings.HasPrefix(reason, codexRateAdmissionPrefix) ||
		strings.HasPrefix(reason, codexOverloadAdmissionPrefix)
}
