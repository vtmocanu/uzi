package settings

// This file holds the run-health accessors, their bounds consts and their
// write-time validator (PRD #1021 M3, split verbatim from settings.go).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// healthSecondsMin / healthSecondsMax bound the integer health settings (Decision
// 5): a value must be 0 (disable that signal) or within [min, max]. The lower bound
// keeps a signal from firing on a sub-minute jitter; the upper bound stops a
// fat-fingered value (e.g. an extra zero) from silently disabling it.
const (
	healthSecondsMin = 60
	healthSecondsMax = 86400
)

// healthPercentMin / healthPercentMax bound the near-timeout percentage (PRD #1170):
// a value must be 0 (disable) or within [min, max]. The floor keeps an operator from
// recreating this PRD's noise with a tiny threshold; 100 is excluded because the
// sweeper fires at 100%, so the flag would coincide with the kill.
const (
	healthPercentMin = 50
	healthPercentMax = 99
)

// HealthEnabled reports whether the run-health detector is enabled instance-wide
// (PRD #47). Stored as "true"/"false"; any other value falls back to the
// compiled-in default (true), the same junk-tolerance as SlackEnabled but
// defaulting ON — a malformed value never silently disables detection.
func (c *Cache) HealthEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyHealthEnabled)
}

// HealthStallSeconds / HealthQueuedSeconds / HealthApprovalSeconds /
// HealthNudgeCooldownSeconds return the integer-seconds health thresholds (PRD #47
// Decision 5). 0 means the caller disables that signal.
func (c *Cache) HealthStallSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthStallSeconds)
}

// HealthNearTimeoutPct returns the near-timeout threshold as a percentage of the run's
// effective wall-clock budget (PRD #1170). 0 disables the signal; otherwise it is in
// [50, 99]. The sweeper applies it per-run against budget_wall_seconds (or RUN_TIMEOUT)
// — no clamp is needed here, since a percentage is below the run's own deadline by
// construction.
func (c *Cache) HealthNearTimeoutPct(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthNearTimeoutPct)
}
func (c *Cache) HealthQueuedSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthQueuedSeconds)
}
func (c *Cache) HealthApprovalSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthApprovalSeconds)
}
func (c *Cache) HealthNudgeCooldownSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthNudgeCooldownSeconds)
}

// validateHealthSeconds is the write-time gate for an integer run-health threshold
// (PRD #47 Decision 5): a base-10 integer that is either 0 (disable that signal) or
// within [healthSecondsMin, healthSecondsMax]. Negatives, non-integers, 1–59, and
// values above the day cap are rejected.
func validateHealthSeconds(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of seconds")
	}
	if n == 0 {
		return nil
	}
	if n < healthSecondsMin || n > healthSecondsMax {
		return fmt.Errorf("must be 0 (disabled) or between %d and %d seconds", healthSecondsMin, healthSecondsMax)
	}
	return nil
}

// validateHealthPercent is the write-time gate for the near-timeout percentage (PRD
// #1170): a base-10 integer that is either 0 (disable the signal) or within
// [healthPercentMin, healthPercentMax]. The floor keeps a tiny threshold from
// recreating this feature's noise; 100 is excluded because the sweeper fires at 100%.
// No RUN_TIMEOUT check is needed — a percentage of a run's own budget is below that
// run's deadline by construction, so Validate stays pure.
func validateHealthPercent(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of percent")
	}
	if n == 0 {
		return nil
	}
	if n < healthPercentMin || n > healthPercentMax {
		return fmt.Errorf("must be 0 (disabled) or between %d and %d percent", healthPercentMin, healthPercentMax)
	}
	return nil
}
