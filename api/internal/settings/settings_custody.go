package settings

// This file holds the per-owner custody admission limit accessor and its write-time
// validator (#3150). Unlike the other integer settings it does NOT go through intSetting:
// intSetting substitutes the compiled-in default on a cold read error or an unparseable
// row, and for an admission limit that substitution could silently raise a lower
// configured value. This accessor fails closed instead.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxCustodyHoldLimit is the explicit ceiling of custody_hold_limit. It fits SQL int4 by a
// wide margin and sits far above any realistic count of open custody holds per owner (each
// can retain gigabytes of worker evidence); it only stops a typo from becoming "unlimited".
const maxCustodyHoldLimit = 1000

// custodyHoldLimitDigits is the strict form a stored value must take after trimming: base-10
// digits only, so "+12", "1e3" and "8.5" are refused exactly as the admin form refuses them.
var custodyHoldLimitDigits = regexp.MustCompile(`^[0-9]+$`)

// ErrCustodyHoldLimitUnavailable wraps every CustodyHoldLimit failure: a nil cache, a read with
// no usable snapshot, or a stored value that fails validation. Callers must refuse custody
// admission on it and never substitute a number.
var ErrCustodyHoldLimitUnavailable = errors.New("custody_hold_limit unavailable")

// CustodyHoldLimit returns the per-owner custody admission limit (#3150), always within
// [1, maxCustodyHoldLimit] when the error is nil.
//
// It resolves the effective value (ENV over DB row over the compiled-in default, the same
// precedence AdminView shows) from the cache snapshot, so it follows the cache's valid-cache
// policy: a failed refresh while a valid snapshot is cached serves that snapshot. A read
// with no valid snapshot (a cold cache, or the first read after Invalidate) returns an error.
// The default 8 applies only when a successfully read snapshot has no non-empty row for the
// key. A stored value that fails validateCustodyHoldLimit (a hand-edited row) is an error,
// never coerced to the default or to zero. A nil receiver is an error too, so an unwired
// cache behind an interface fails closed instead of panicking.
func (c *Cache) CustodyHoldLimit(ctx context.Context) (int32, error) {
	if c == nil {
		return 0, fmt.Errorf("%w: no settings cache", ErrCustodyHoldLimitUnavailable)
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrCustodyHoldLimitUnavailable, err)
	}
	v := strings.TrimSpace(c.effective(KeyCustodyHoldLimit, m))
	if verr := validateCustodyHoldLimit(v); verr != nil {
		return 0, fmt.Errorf("%w: stored value %s", ErrCustodyHoldLimitUnavailable, verr)
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrCustodyHoldLimitUnavailable, err)
	}
	return int32(n), nil
}

// validateCustodyHoldLimit is the write-time gate for custody_hold_limit (#3150): after
// trimming, base-10 digits only, in [1, maxCustodyHoldLimit]. Zero, negatives, signs,
// non-integers, the empty string and values above the ceiling are rejected — there is no
// zero-as-disable. The explicit Validate case backing this is load-bearing: Validate's
// default branch (ValidateLabel) would accept any short string.
func validateCustodyHoldLimit(value string) error {
	v := strings.TrimSpace(value)
	msg := fmt.Errorf("must be a whole number between 1 and %d", maxCustodyHoldLimit)
	if !custodyHoldLimitDigits.MatchString(v) {
		return msg
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxCustodyHoldLimit {
		return msg
	}
	return nil
}
