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

// HealthEnabled reports whether the run-health detector is enabled instance-wide
// (PRD #47). Stored as "true"/"false"; any other value falls back to the
// compiled-in default (true), the same junk-tolerance as SlackEnabled but
// defaulting ON — a malformed value never silently disables detection.
func (c *Cache) HealthEnabled(ctx context.Context) (bool, error) {
	return c.boolSetting(ctx, KeyHealthEnabled)
}

// HealthStallSeconds / HealthSlowSeconds / HealthQueuedSeconds /
// HealthApprovalSeconds / HealthNudgeCooldownSeconds return the integer-seconds
// health thresholds (PRD #47 Decision 5). 0 means the caller disables that signal.
// The RUN_TIMEOUT clamp on the slow threshold is applied read-time by the sweeper,
// not here (Validate is pure with no config access).
func (c *Cache) HealthStallSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthStallSeconds)
}
func (c *Cache) HealthSlowSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyHealthSlowSeconds)
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
// values above the day cap are rejected. The health_slow_seconds < RUN_TIMEOUT rule
// is NOT enforced here — Validate is pure with no config access, so that is a
// read-time clamp in the sweeper.
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
