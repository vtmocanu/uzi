// Package weeklywarn sends the once-per-observed-weekly-period Slack warning that a credential is
// near or at its weekly limit (PRD #4123 module 5). The pollers hand it each accepted reading; it
// keeps one bounded ledger row per credential (weekly_limit_alerts) and, in a separate claim
// transaction, atomically flips the row's warned flag together with its notifications row.
//
// This file is the pure core: the whole-reading validity gate and the per-credential state machines
// are functions of the locked ledger row and the reading, with no clock and no I/O, so every window
// control is a table test.
package weeklywarn

import (
	"math"
	"sort"
	"time"

	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// marginSecs is usagereserve.ResetEpochMoveMargin in whole seconds. Every boundary comparison here is
// over integer epoch seconds.
const marginSecs = int64(usagereserve.ResetEpochMoveMargin / time.Second)

// Row is the part of a weekly_limit_alerts row the state machines read.
type Row struct {
	AnchorReset  *time.Time
	AnchorPct    *float64
	AnchorBucket *string // Codex only
	AnchorMulti  bool    // Codex only
	Warned       bool
}

// Next is the state a reading leaves the row in. PeriodOK is the claim's period gate: the reading
// belongs to the anchored period (Claude: its reset is within the margin of the anchor; Codex: it
// shows a same-boundary window, or it formed the anchor).
type Next struct {
	AnchorReset  *time.Time
	AnchorPct    *float64
	AnchorBucket *string
	AnchorMulti  bool
	Warned       bool
	PeriodOK     bool
}

func epoch(t *time.Time) int64 { return t.Unix() }

func secs(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := time.Unix(t.Unix(), 0).UTC()
	return &v
}

func absInt(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func ptrF(v float64) *float64 { return &v }

// ClaudeWindow is the single seven_day window of an accepted Claude reading.
type ClaudeWindow struct {
	Reset *time.Time
	Pct   float64
}

// nextClaude is the Claude state machine. obs is the observation instant (the persisted synced_at).
//
//   - No anchor: anchor on the fresh reset when it is after obs, else stay NULL.
//   - ClaudeResetObserved arm A (a reset more than the margin later): new period, re-anchor.
//   - Arm B (unchanged boundary, utilization cleared): rearm, boundary kept.
//   - A reset within the margin of the anchor: in-window; a refill (arm B against the fresh reset)
//     rearms, otherwise the anchor follows the reading.
//   - obs after the anchor and the reset absent or outside the margin (arm A not fired): the verified
//     post-boundary re-anchor, or a NULL anchor when no future reset exists.
//   - Anything else (a reset before anchor - margin, or no reset before the boundary) is a replay and
//     changes nothing.
func nextClaude(prev Row, obs time.Time, w ClaudeWindow) Next {
	fresh := secs(w.Reset)
	pct := w.Pct
	anchorFromFresh := func() Next {
		if fresh != nil && epoch(fresh) > obs.Unix() {
			return Next{AnchorReset: fresh, AnchorPct: ptrF(pct), PeriodOK: true}
		}
		return Next{}
	}
	if prev.AnchorReset == nil {
		return anchorFromFresh()
	}
	anchor := secs(prev.AnchorReset)
	var priorPct *int16
	if prev.AnchorPct != nil && !math.IsNaN(*prev.AnchorPct) {
		v := int16(math.Round(*prev.AnchorPct))
		priorPct = &v
	}
	if usagereserve.ClaudeResetObserved(*anchor, priorPct, fresh, int(pct)) {
		if fresh != nil && fresh.Equal(*anchor) { // arm B: the boundary is unchanged
			return Next{AnchorReset: anchor, AnchorPct: ptrF(pct), PeriodOK: true}
		}
		return anchorFromFresh() // arm A
	}
	if fresh != nil && absInt(epoch(fresh)-epoch(anchor)) <= marginSecs {
		// Reset jitter within the margin: the boundary is the same one, so a refill is judged against
		// the prior utilization BEFORE the anchor follows the reading (equal anchors select arm B
		// only). Overwriting anchor_pct first would hide the 90 to 0 drop and arm B could never fire.
		if usagereserve.ClaudeResetObserved(*fresh, priorPct, fresh, int(pct)) {
			return Next{AnchorReset: fresh, AnchorPct: ptrF(pct), PeriodOK: true}
		}
		return Next{AnchorReset: fresh, AnchorPct: ptrF(pct), Warned: prev.Warned, PeriodOK: true}
	}
	if obs.Unix() > epoch(anchor) {
		return anchorFromFresh()
	}
	return Next{AnchorReset: prev.AnchorReset, AnchorPct: prev.AnchorPct, Warned: prev.Warned}
}

// CodexView is an accepted Codex reading as the state machine sees it: every weekly window (the
// shared CodexWeekly extraction, so each has a finite non-negative utilization) and the bucket ids
// that appear on more than one bucket.
type CodexView struct {
	Windows []usagereserve.CodexWeeklyWindow
	Dup     map[string]bool
}

func usable(w usagereserve.CodexWeeklyWindow) bool { return w.ResetsAt != nil }

func slotRank(s string) int {
	if s == "primary" {
		return 0
	}
	return 1
}

// better orders two windows for the (re)anchor rule: higher utilization, then earlier reset, then the
// lexically smaller bucket id, then primary before secondary.
func better(a, b usagereserve.CodexWeeklyWindow) bool {
	if a.Pct != b.Pct {
		return a.Pct > b.Pct
	}
	if ea, eb := epoch(a.ResetsAt), epoch(b.ResetsAt); ea != eb {
		return ea < eb
	}
	if a.BucketID != b.BucketID {
		return a.BucketID < b.BucketID
	}
	return slotRank(a.Slot) < slotRank(b.Slot)
}

// choose is the canonical account-period window (PRD D16): the valid weekly window with the highest
// utilization whose reset is after obs and, when floor is set, not earlier than floor.
func (v CodexView) choose(obs int64, floor *int64) *usagereserve.CodexWeeklyWindow {
	var best *usagereserve.CodexWeeklyWindow
	for i := range v.Windows {
		w := v.Windows[i]
		if !usable(w) || epoch(w.ResetsAt) <= obs || (floor != nil && epoch(w.ResetsAt) < *floor) {
			continue
		}
		if best == nil || better(w, *best) {
			c := w
			best = &c
		}
	}
	return best
}

func (v CodexView) windowsIn(bucket string) int {
	n := 0
	for _, w := range v.Windows {
		if w.BucketID == bucket {
			n++
		}
	}
	return n
}

// multi is the bounded evidence that a bucket shows two or more weekly windows (counting one with no
// usable reset), a duplicated bucket id counting as true.
func (v CodexView) multi(bucket string) bool { return v.Dup[bucket] || v.windowsIn(bucket) >= 2 }

// Trigger is the window the near/at verdict is read from: the highest utilization, ties to a window
// with a usable reset, then the earliest, then bucket and slot, so the verdict never depends on the
// payload order.
func (v CodexView) Trigger() (usagereserve.WeeklyReading, bool) {
	if len(v.Windows) == 0 {
		return usagereserve.WeeklyReading{}, false
	}
	ws := append([]usagereserve.CodexWeeklyWindow(nil), v.Windows...)
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.Pct != b.Pct {
			return a.Pct > b.Pct
		}
		if usable(a) != usable(b) {
			return usable(a)
		}
		if usable(a) && epoch(a.ResetsAt) != epoch(b.ResetsAt) {
			return epoch(a.ResetsAt) < epoch(b.ResetsAt)
		}
		if a.BucketID != b.BucketID {
			return a.BucketID < b.BucketID
		}
		return slotRank(a.Slot) < slotRank(b.Slot)
	})
	return usagereserve.WeeklyReading{OK: true, Pct: ws[0].Pct, ResetsAt: ws[0].ResetsAt}, true
}

// nextCodex is the Codex state machine over the anchored (bucket id, boundary) window. Identity is
// the bucket id and the boundary, never the slot or the array order.
//
//   - Matching: windows of the anchor bucket whose usable reset is within the margin of the anchor.
//     A duplicated anchor bucket id counts as absent.
//   - Warned and matched: only a refill (every candidate shows the cleared arm at the unchanged
//     boundary) rearms; otherwise the anchor is frozen.
//   - No match: a forward roll (only while anchor_multi is false and the bucket has exactly one
//     weekly window with a usable reset more than the margin later) or the verified post-boundary
//     passage (obs after the anchor) starts a new period on the (re)anchor choice, NULL when no
//     window has a future reset.
//   - Unwarned: the anchor follows the (re)anchor choice, never to a reset earlier than anchor - margin.
//
// anchor_multi is a sticky OR of this reading's evidence; it is recomputed only by a new-period
// transition and reset when an unwarned re-form moves to another bucket.
func nextCodex(prev Row, obs time.Time, v CodexView) Next {
	obsE := obs.Unix()
	anchorOn := func(c *usagereserve.CodexWeeklyWindow, multi bool) Next {
		return Next{AnchorReset: c.ResetsAt, AnchorPct: ptrF(c.Pct), AnchorBucket: strPtr(c.BucketID), AnchorMulti: multi, PeriodOK: true}
	}
	if prev.AnchorReset == nil || prev.AnchorBucket == nil {
		c := v.choose(obsE, nil)
		if c == nil {
			return Next{}
		}
		return anchorOn(c, v.multi(c.BucketID))
	}
	b, a := *prev.AnchorBucket, epoch(prev.AnchorReset)
	dup := v.Dup[b]
	multi := prev.AnchorMulti || v.multi(b)
	var cands []usagereserve.CodexWeeklyWindow
	if !dup {
		for _, w := range v.Windows {
			if w.BucketID == b && usable(w) && absInt(epoch(w.ResetsAt)-a) <= marginSecs {
				cands = append(cands, w)
			}
		}
	}
	floor := a - marginSecs
	frozen := Next{AnchorReset: prev.AnchorReset, AnchorPct: prev.AnchorPct, AnchorBucket: prev.AnchorBucket,
		AnchorMulti: multi, Warned: prev.Warned, PeriodOK: len(cands) > 0}

	// follow re-forms the anchor on the choice (an unwarned credential), ORing the flag within the
	// same bucket and recomputing it for another one.
	follow := func() Next {
		c := v.choose(obsE, &floor)
		if c == nil {
			return frozen
		}
		m := multi
		if c.BucketID != b {
			m = v.multi(c.BucketID)
		}
		n := anchorOn(c, m)
		n.PeriodOK = len(cands) > 0 || c.BucketID != b || epoch(c.ResetsAt) != a
		return n
	}
	// newPeriod starts a fresh period: the choice, with the flag recomputed from this reading.
	newPeriod := func() Next {
		c := v.choose(obsE, &floor)
		if c == nil {
			return Next{}
		}
		return anchorOn(c, v.multi(c.BucketID))
	}

	if len(cands) > 0 {
		if !prev.Warned {
			return follow()
		}
		top, refill := 0.0, true
		for _, c := range cands {
			if c.Pct > top {
				top = c.Pct
			}
			e := epoch(c.ResetsAt)
			pct := c.Pct
			if !usagereserve.CodexResetObserved(e, e, prev.AnchorPct, &pct) {
				refill = false
			}
		}
		if refill {
			return Next{AnchorReset: prev.AnchorReset, AnchorPct: ptrF(top), AnchorBucket: prev.AnchorBucket,
				AnchorMulti: v.multi(b), PeriodOK: true}
		}
		return frozen
	}
	if roll(v, b, a, multi, dup) || obsE > a {
		return newPeriod()
	}
	if !prev.Warned {
		return follow()
	}
	return frozen
}

// roll reports the inferred forward roll: the bucket never showed a second weekly window this period
// (anchor_multi false after ORing this reading), shows exactly one now, and its usable reset is more
// than the margin past the anchor.
func roll(v CodexView, bucket string, anchor int64, multi, dup bool) bool {
	if multi || dup || v.windowsIn(bucket) != 1 {
		return false
	}
	for _, w := range v.Windows {
		if w.BucketID == bucket {
			return usable(w) && epoch(w.ResetsAt) > anchor+marginSecs
		}
	}
	return false
}

func strPtr(s string) *string { return &s }

// Claimable is the pure half of the claim conditions: the period is still unwarned, the reading
// belongs to the anchored period, the warnings are on, the credential is near or at its limit, and
// the triggering window's reset is strictly after the observation instant.
func Claimable(n Next, obs time.Time, near usagereserve.NearVerdict, marginPts int) bool {
	if n.Warned || !n.PeriodOK || marginPts <= 0 {
		return false
	}
	if near.State != usagereserve.NearNear && near.State != usagereserve.NearAt {
		return false
	}
	return near.ResetsAt != nil && near.ResetsAt.Unix() > obs.Unix()
}
