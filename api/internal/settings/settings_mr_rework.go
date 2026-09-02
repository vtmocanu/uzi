package settings

// This file holds the MR-rework accessors, their bounds const and their
// write-time validator (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxMrReworkCap bounds the per-MR rework-cycle cap (PRD #700 M5 Decision 2). The
// cap is a small loop guard (default 5, mirroring ci-autofix's maxAttempts), so the
// upper bound only catches a fat-fingered value — no MR legitimately needs hundreds
// of automated rework cycles.
const maxMrReworkCap = 100

// MrReworkEnabled reports whether the MR review-watcher auto-rework feature is
// enabled instance-wide (PRD #700 M5 Decision 5): the admin global kill-switch.
// Stored as the text "true"/"false"; any OTHER value falls back to the compiled-in
// default (true). This feature ships ON — the opposite of JudgeEnabled — so a
// malformed row never silently turns a default-on feature off.
//
// The read is DELIBERATELY three-state and error-propagating, not the best-effort
// swallow the other bool readers use. Decision 5 (review-fix R3) requires reconciling
// default-ON with fail-closed by distinguishing present-true / present-false / absent
// (all a value) from a store READ ERROR: absent → ON (the default), but a genuine
// error must NOT be misread as absent→ON, which fails OPEN. So this reader PROPAGATES
// its error to the caller and must not collapse it to false itself; the CALLER (the
// M3 detector) is the one that maps a non-nil error to OFF. Do not "helpfully" swallow
// the error to false here — that would move the fail-closed decision away from the
// caller that owns it.
func (c *Cache) MrReworkEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyMrReworkEnabled)
}

// MrReworkCap returns the admin-configured cap on rework cycles per MR (PRD #700 M5
// Decision 2), the loop guard mirroring ci-autofix's maxAttempts. An absent or blank
// value falls back to DefaultMrReworkCap (5). Unlike intSetting — which swallows an
// unparseable value to the compiled-in default — a PARSE ERROR is returned so the
// caller decides what to do with a cap it cannot read (a hand-edited junk row is not
// silently treated as 5). A cold store read error is propagated too.
func (c *Cache) MrReworkCap(ctx context.Context) (int, error) {
	v, err := c.get(ctx, KeyMrReworkCap)
	s := strings.TrimSpace(v)
	if s == "" {
		n, _ := strconv.Atoi(DefaultMrReworkCap)
		return n, err
	}
	n, perr := strconv.Atoi(s)
	if perr != nil {
		return 0, perr
	}
	return n, err
}

// validateMrReworkCap is the write-time gate for the per-MR rework-cycle cap (PRD
// #700 M5 Decision 2): a base-10 integer in [1, maxMrReworkCap]. It must be at least
// 1 — the admin kill-switch (mr_rework_enabled), not a zero cap, is how the feature
// is turned off. Negatives, non-integers, and values above the cap are refused.
//
// Like validateHostedWorkerQuota, this explicit Validate case is load-bearing: the
// default branch falls through to ValidateLabel, which accepts any non-empty
// ≤64-char string — so an integer key missing from the switch would accept "abc",
// and MrReworkCap would then return a parse error on every read of an admin value it
// was told saved fine.
func validateMrReworkCap(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of rework cycles")
	}
	if n < 1 || n > maxMrReworkCap {
		return fmt.Errorf("must be between 1 and %d rework cycles", maxMrReworkCap)
	}
	return nil
}
