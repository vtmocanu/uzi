package settings

// This file holds the run-extension cap accessor and its write-time validator
// (PRD #1189 M1). It is split from settings_health.go because its bounds differ:
// an extension cap is 0 (disabled) or [1h, 7d], NOT the health seconds range, so it
// cannot reuse validateHealthSeconds.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// runExtensionCapSecondsMin / runExtensionCapSecondsMax bound the per-run extension cap
// (PRD #1189 D3): a value must be 0 (extending disabled) or within [min, max]. A cap under
// an hour is noise; over a week is effectively no cap.
const (
	runExtensionCapSecondsMin = 3600   // 1h
	runExtensionCapSecondsMax = 604800 // 7d
)

// RunExtensionCapSeconds returns the TOTAL extra wall-clock seconds an owner may grant a
// single run through Extend (PRD #1189). 0 means extending is disabled instance-wide.
func (c *Cache) RunExtensionCapSeconds(ctx context.Context) (int, error) {
	return c.intSetting(ctx, KeyRunExtensionCapSeconds)
}

// validateExtensionCapSeconds is the write-time gate for the extension cap (PRD #1189 D3):
// a base-10 integer that is either 0 (disable extending) or within
// [runExtensionCapSecondsMin, runExtensionCapSecondsMax]. Negatives, non-integers, 1–3599,
// and values above the 7-day cap are rejected. Bounds differ from validateHealthSeconds, so
// it is a separate validator by design, not a reuse.
func validateExtensionCapSeconds(value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return errors.New("must be a whole number of seconds")
	}
	if n == 0 {
		return nil
	}
	if n < runExtensionCapSecondsMin || n > runExtensionCapSecondsMax {
		return fmt.Errorf("must be 0 (disabled) or between %d and %d seconds", runExtensionCapSecondsMin, runExtensionCapSecondsMax)
	}
	return nil
}
