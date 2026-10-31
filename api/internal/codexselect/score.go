package codexselect

import (
	"math"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Reading is one account's stored limit reading: the buckets of its last successful poll and
// that poll's time (the freshness clock). Present is false when no reading exists.
type Reading struct {
	Present       bool
	Buckets       []apitypes.CodexRateLimitBucketDTO
	LastSuccessAt *time.Time
}

// mainBucket returns the account's top-level "codex" bucket, the only bucket selection
// reads. Feature buckets (additional_rate_limits) never score and never exclude.
func mainBucket(buckets []apitypes.CodexRateLimitBucketDTO) *apitypes.CodexRateLimitBucketDTO {
	for i := range buckets {
		if buckets[i].ID == codexauth.CodexMainBucketID {
			return &buckets[i]
		}
	}
	return nil
}

// Score rates an account's remaining capacity: 100 minus the highest utilization across
// every window the "codex" bucket reports, each clamped to 100, so the result is in [0,100].
// resetAt is the reset of the binding (most utilized) window; the first of primary then
// secondary wins an exact tie.
//
// known is false (and score and resetAt are zero) when the reading cannot steer a choice:
//   - the shared meter status is not fresh (meterFresh false);
//   - there is no reading or no successful poll time, or it is stale at interval;
//   - the bucket is missing or reports no window;
//   - any reported window's utilization is nil, NaN, infinite or negative;
//   - the binding window's reset has passed since the reading (usagereserve.PreReset): its
//     utilization describes the old window.
func Score(r Reading, meterFresh bool, interval time.Duration, now time.Time) (score float64, resetAt *time.Time, known bool) {
	if !meterFresh || !r.Present || r.LastSuccessAt == nil ||
		usagereserve.Stale(*r.LastSuccessAt, true, interval, now) {
		return 0, nil, false
	}
	b := mainBucket(r.Buckets)
	if b == nil {
		return 0, nil, false
	}
	var binding *apitypes.CodexRateLimitWindowDTO
	var maxUtil float64
	for _, w := range []*apitypes.CodexRateLimitWindowDTO{b.Primary, b.Secondary} {
		if w == nil {
			continue
		}
		if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 {
			return 0, nil, false
		}
		u := math.Min(*w.UsedPercent, 100)
		if binding == nil || u > maxUtil {
			binding, maxUtil = w, u
		}
	}
	if binding == nil {
		return 0, nil, false
	}
	reset := usagereserve.CodexWindowReset(binding, *r.LastSuccessAt)
	if usagereserve.PreReset(reset, *r.LastSuccessAt, now) {
		return 0, nil, false
	}
	return 100 - maxUtil, reset, true
}

// LimitReached reports whether the "codex" bucket says the account cannot take a run now:
// limit_reached true or allowed false. Other buckets are ignored, and an unreported (nil)
// signal is not a limit. The caller applies it only to a current, fresh reading.
func LimitReached(buckets []apitypes.CodexRateLimitBucketDTO) bool {
	b := mainBucket(buckets)
	if b == nil {
		return false
	}
	return (b.LimitReached != nil && *b.LimitReached) || (b.Allowed != nil && !*b.Allowed)
}
