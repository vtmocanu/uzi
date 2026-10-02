package settings

import "context"

// DecisionsMemoEnabled reports the admin kill-switch for the run decisions memo (issue #2083).
// Stored as the text "true"/"false"; an absent or malformed row falls back to the compiled-in
// default (false). Like MrReworkEnabled the read propagates its store error rather than folding it
// into the default: the caller owns the fail-closed decision and maps a non-nil error to disabled.
func (c *Cache) DecisionsMemoEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyDecisionsMemoEnabled)
}
