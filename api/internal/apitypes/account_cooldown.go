package apitypes

import "time"

// AccountCooldownClass is the provider signal holding an active operation.
type AccountCooldownClass string

const AccountCooldownReason = "waiting for account cooldown"

// AccountCooldownLine describes the earliest eligibility, never a dispatch promise.
func AccountCooldownLine(class AccountCooldownClass, at time.Time) string {
	if class != "rate_limit" && class != "overload" {
		return ""
	}
	return AccountCooldownReason + " (" + string(class) + "); earliest eligible at " + at.UTC().Format(time.RFC3339Nano)
}
