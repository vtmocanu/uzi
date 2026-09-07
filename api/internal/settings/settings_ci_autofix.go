package settings

// This file holds the CI-autofix admin-gate accessor (PRD #914), mirroring the
// MR-rework accessor in settings_mr_rework.go.

import "context"

// CiAutofixEnabled reports whether the CI-autofix feature is enabled instance-wide
// (PRD #914): the admin global kill-switch. Stored as the text "true"/"false"; any
// OTHER value falls back to the compiled-in default (true). This feature ships ON —
// like MrReworkEnabled and the opposite of JudgeEnabled — so a malformed row never
// silently turns a default-on feature off.
//
// The read is DELIBERATELY three-state and error-propagating, not the best-effort
// swallow the other bool readers use. Reconciling default-ON with fail-closed means
// distinguishing present-true / present-false / absent (all a value) from a store
// READ ERROR: absent → ON (the default), but a genuine error must NOT be misread as
// absent→ON, which fails OPEN. So this reader PROPAGATES its error to the caller and
// must not collapse it to false itself; the CALLER (the detector) is the one that maps
// a non-nil error to OFF (fail closed). Do not "helpfully" swallow the error to false
// here — that would move the fail-closed decision away from the caller that owns it.
func (c *Cache) CiAutofixEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyCiAutofixEnabled)
}
