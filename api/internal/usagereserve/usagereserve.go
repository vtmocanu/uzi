// Package usagereserve is the pure evaluation core of the weekly usage reserve (PRD #3522):
// given a user's threshold and the latest 7-day reading of the credential or account a run
// would spend, it decides ok / held / unknown. It reads no database and no clock: every
// input, including "now", is a parameter, so the callers (claim admission, the sweeper, the
// settings surface) own I/O and this package stays a table-testable function.
//
// The package also single-sources two rules the usage surfaces already apply, so the reserve
// and those surfaces cannot drift: the staleness window (Stale) and the observed-weekly-reset
// predicates the early-reset detectors use (ClaudeResetObserved, CodexResetObserved).
package usagereserve

import (
	"fmt"
	"math"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// WeeklyWindowSeconds is the reported duration that identifies a Codex 7-day window. A Codex
// window is recognised by this duration, never by its primary/secondary position.
const WeeklyWindowSeconds int64 = 604800

// Reset-observation margins, shared by the Claude and Codex early-reset detectors and by the
// reserve's manual-resume exemption so all three agree on what counts as "the week reset".
const (
	// ResetEpochMoveMargin: a fresh reset epoch must be strictly more than this past the
	// anchor to count as a moved boundary. A genuine reset moves it by days, while a
	// usage_endpoint to header_probe source flip re-derives the same instant from a different
	// wire field and can differ by seconds; +1h absorbs that jitter (PRD #1114 D8).
	ResetEpochMoveMargin = 1 * time.Hour
	// PctResetFloor / PctResetCeil: a utilization drop from >= PctResetFloor to
	// <= PctResetCeil, with the boundary unmoved, counts as a reset (the fixed-grid model,
	// where used% zeroes but the boundary does not move). Utilization is monotonic between
	// resets, so a genuine drop only happens at one; the small buffer stops a cross-source
	// rounding delta from reading as a reset (PRD #1114 D5/D8).
	PctResetFloor = 5
	PctResetCeil  = 1
)

// staleFactor is the multiple of the poll interval after which a reading is stale.
const staleFactor = 3

// ReasonPrefix opens every visible reserve reason (HeldReason, UnknownReason). Surfaces that must
// recognise a reserve reason among free-text hold reasons (the health pill's nudge suppression)
// match on it, so the prefix has one definition.
const ReasonPrefix = "weekly usage reserve: "

// UnknownReason is the visible reason a run is held while the weekly usage cannot be read.
const UnknownReason = ReasonPrefix + "weekly usage unknown"

// HeldReason is the visible reason a run is held at or above the threshold.
func HeldReason(pct, threshold int) string {
	return fmt.Sprintf("%s%d%% >= %d%%", ReasonPrefix, pct, threshold)
}

// State is the outcome of one evaluation.
type State int

const (
	// OK: no hold (no threshold, an unmetered credential, or a fresh reading below it).
	OK State = iota
	// Held: a fresh reading is at or above the threshold.
	Held
	// Unknown: a threshold is set but there is no usable fresh reading.
	Unknown
)

func (s State) String() string {
	switch s {
	case OK:
		return "ok"
	case Held:
		return "held"
	case Unknown:
		return "unknown"
	}
	return "invalid"
}

// Verdict is the evaluation result. Pct and Threshold are set for Held; Fresh is true only when
// a usable fresh reading backed the verdict (an OK for "no threshold" or "unmetered" has no
// reading, so Fresh is false). ResetsAt is the reading's 7-day reset when known. Reason is the
// visible hold reason, empty for OK.
type Verdict struct {
	State     State
	Pct       int
	Threshold int
	ResetsAt  *time.Time
	Fresh     bool
	Reason    string
}

// Exempt is the verdict for a credential that structurally has no subscription meter (a Codex
// API-key alias, an OpenAI API key). Applicability is decided by structure, never by a missing
// reading, which would fail open on a poller outage.
func Exempt() Verdict { return Verdict{State: OK} }

// Stale reports whether a reading synced at syncedAt is stale at now: the poller is disabled
// (interval <= 0, so nothing refreshes it), the sync time is unknown (!valid), or it is older
// than three poll intervals. It is the single staleness rule of the Claude and Codex rate-limit
// surfaces.
func Stale(syncedAt time.Time, valid bool, interval time.Duration, now time.Time) bool {
	if interval <= 0 || !valid {
		return true
	}
	return now.Sub(syncedAt) > staleFactor*interval
}

// ClaudeReading is the stored per-token reading. Present is false when no row exists.
type ClaudeReading struct {
	Present          bool
	SevenDayPct      *int16
	SevenDayResetsAt *time.Time
	SyncedAt         *time.Time
}

// preReset reports a reading synced before a 7-day reset that has since passed: its
// utilization describes the old window, so it proves nothing about the new one. A reset
// passing only triggers reconsideration; release waits for a post-reset poll.
func preReset(resetsAt *time.Time, syncedAt, now time.Time) bool {
	return resetsAt != nil && !resetsAt.After(now) && syncedAt.Before(*resetsAt)
}

func unknown() Verdict { return Verdict{State: Unknown, Reason: UnknownReason} }

func decide(threshold int16, used float64, resetsAt *time.Time) Verdict {
	// Floor, not round: the displayed percent never reaches the threshold on an OK verdict
	// (69.6 vs 70 shows 69), while Held compares the unrounded value.
	pct := int(math.Floor(used))
	if used >= float64(threshold) {
		return Verdict{State: Held, Pct: pct, Threshold: int(threshold), ResetsAt: resetsAt, Fresh: true,
			Reason: HeldReason(pct, int(threshold))}
	}
	return Verdict{State: OK, Pct: pct, Threshold: int(threshold), ResetsAt: resetsAt, Fresh: true}
}

// EvaluateClaude evaluates a Claude token's 7-day reading against the user's threshold. A nil
// threshold is always OK. A missing, stale, pre-reset or utilization-less reading is Unknown.
// A reading equal to the threshold is Held.
func EvaluateClaude(threshold *int16, r ClaudeReading, interval time.Duration, now time.Time) Verdict {
	if threshold == nil {
		return Verdict{State: OK}
	}
	if !r.Present || r.SevenDayPct == nil || r.SyncedAt == nil ||
		Stale(*r.SyncedAt, true, interval, now) || preReset(r.SevenDayResetsAt, *r.SyncedAt, now) {
		return unknown()
	}
	return decide(*threshold, float64(*r.SevenDayPct), r.SevenDayResetsAt)
}

// CodexReading is the stored per-account reading. LastSuccessAt is the account's last
// successful poll, the freshness clock of its buckets.
type CodexReading struct {
	Present       bool
	Buckets       []apitypes.CodexRateLimitBucketDTO
	LastSuccessAt *time.Time
}

// EvaluateCodex evaluates a Codex account against the user's threshold. The 7-day windows are
// the primary or secondary windows whose reported duration is exactly WeeklyWindowSeconds,
// never chosen by position. A weekly window whose used percent is missing, non-finite or
// negative makes the whole reading Unknown (fail closed); over 100 is clamped to 100. Buckets carry no model field, so
// the PRD's fallback applies: the highest used percent across every bucket's weekly windows
// decides, and its reset is that window's. No weekly window is Unknown.
//
// Rounding: the verdict is Held iff used >= threshold on the unrounded percent (so 69.9 does
// not hold a 70 threshold); the reported Pct is the used percent rounded down.
func EvaluateCodex(threshold *int16, r CodexReading, interval time.Duration, now time.Time) Verdict {
	if threshold == nil {
		return Verdict{State: OK}
	}
	if !r.Present || r.LastSuccessAt == nil || Stale(*r.LastSuccessAt, true, interval, now) {
		return unknown()
	}
	var best *apitypes.CodexRateLimitWindowDTO
	for i := range r.Buckets {
		for _, w := range []*apitypes.CodexRateLimitWindowDTO{r.Buckets[i].Primary, r.Buckets[i].Secondary} {
			if w == nil || w.LimitWindowSeconds == nil || *w.LimitWindowSeconds != WeeklyWindowSeconds {
				continue
			}
			// Fail closed (PRD D6): a weekly window whose utilization is missing or not finite
			// makes the whole reading Unknown rather than letting a sibling window decide. Over
			// 100% is the most-held state there is, so it is clamped to 100, never skipped.
			if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 {
				return unknown()
			}
			if best == nil || clampPercent(*w.UsedPercent) > clampPercent(*best.UsedPercent) {
				best = w
			}
		}
	}
	if best == nil {
		return unknown()
	}
	var resetsAt *time.Time
	switch {
	case best.ResetAt != nil && validReset(*best.ResetAt):
		t := time.Unix(*best.ResetAt, 0).UTC()
		resetsAt = &t
	case best.ResetAfterSeconds != nil && *best.ResetAfterSeconds >= 0:
		t := r.LastSuccessAt.Add(time.Duration(*best.ResetAfterSeconds) * time.Second).UTC()
		resetsAt = &t
	}
	if preReset(resetsAt, *r.LastSuccessAt, now) {
		return unknown()
	}
	return decide(*threshold, clampPercent(*best.UsedPercent), resetsAt)
}

func clampPercent(p float64) float64 { return math.Min(p, 100) }

// ClaudeResetObserved reports whether a fresh Claude 7-day reading shows the week reset past
// the anchor T (the previously reported reset). Arm A: the fresh reset epoch moved strictly
// more than ResetEpochMoveMargin past T (the rolling-boundary model). Arm B: the boundary is
// present and unmoved (== T), the prior utilization was >= PctResetFloor and the fresh one is
// <= PctResetCeil (the fixed-grid model). It carries no "how early" time gate; the early-reset
// alert adds that on top. A zero anchor never observes a reset.
func ClaudeResetObserved(anchor time.Time, priorPct *int16, freshResetsAt *time.Time, freshPct int) bool {
	if anchor.IsZero() {
		return false
	}
	armA := freshResetsAt != nil && freshResetsAt.After(anchor.Add(ResetEpochMoveMargin))
	armB := freshResetsAt != nil &&
		freshResetsAt.Equal(anchor) &&
		priorPct != nil &&
		int(*priorPct) >= PctResetFloor &&
		freshPct <= PctResetCeil
	return armA || armB
}

// CodexResetObserved is the Codex counterpart over epoch seconds: the fresh reset moved more
// than ResetEpochMoveMargin past the old one, or it is unchanged while the used percent fell
// from >= PctResetFloor to <= PctResetCeil. Both percents must be finite and within 0..100 for
// the unchanged-boundary arm.
func CodexResetObserved(oldReset, freshReset int64, oldPct, freshPct *float64) bool {
	forward := freshReset > oldReset+int64(ResetEpochMoveMargin/time.Second)
	cleared := freshReset == oldReset && validPercent(oldPct) && validPercent(freshPct) &&
		*oldPct >= PctResetFloor && *freshPct <= PctResetCeil
	return forward || cleared
}

func validReset(reset int64) bool { return reset > 0 && reset <= 253402300799 }

func validPercent(pct *float64) bool {
	return pct != nil && !math.IsNaN(*pct) && !math.IsInf(*pct, 0) && *pct >= 0 && *pct <= 100
}
