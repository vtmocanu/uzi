package main

import (
	"encoding/json"
	"image/color"
	"math"
	"strconv"
)

// The lead context-window meter (PRD #516, issue #565): the terminal twin of web's
// ContextMeter. The lead's context reading rides laneFrame.Payload as a top-level
// "context" key {used, window, pct} on the lead's usage frames — nothing else in this
// package decodes it. This mirrors the web semantics EXACTLY:
//   - contextState (web ActivityFeed.tsx): pct>=95 → near, pct>=70 → molten, else cool;
//     boundaries are >=, near checked first.
//   - contextBarWidth = clamp(pct, 0, 100): the bar CLAMPS, but the label shows the TRUE
//     (unclamped) rounded pct, so pct 112 → a full bar + "112%".
//   - readContext (web runUsage.ts): render nothing unless used, window AND pct are all
//     finite numbers — a malformed/partial context yields no meter.
//   - leadContext: latest-wins across LEAD frames only; a subagent frame's context is
//     ignored.

// contextFill is a decoded, validated lead context reading.
type contextFill struct {
	used, window, pct float64
}

// leadContextFill finds the lead lane and returns its newest valid context reading, mirroring
// web's leadContext (latest-wins across lead frames only). It scans the lead lane's frames
// NEWEST-FIRST and returns the first frame carrying a context whose used, window AND pct are
// all present and finite — the exact readContext guard. A frame whose payload has no "context"
// key (a lead text frame) or fails to decode is skipped. No lead lane, or no valid context on
// it, yields (contextFill{}, false) and no meter.
func leadContextFill(lanes []agentLane) (contextFill, bool) {
	for _, l := range lanes {
		if l.Key != laneLead {
			continue
		}
		for i := len(l.Frames) - 1; i >= 0; i-- {
			var w struct {
				Context *struct{ Used, Window, Pct *float64 } `json:"context"`
			}
			f := l.Frames[i]
			if json.Unmarshal(f.Payload, &w) != nil || w.Context == nil {
				continue
			}
			c := w.Context
			if c.Used == nil || c.Window == nil || c.Pct == nil {
				continue
			}
			if !finite(*c.Used) || !finite(*c.Window) || !finite(*c.Pct) {
				continue
			}
			return contextFill{used: *c.Used, window: *c.Window, pct: *c.Pct}, true
		}
		// The lead lane exists but carries no valid context: no meter, and no other lane
		// can be the lead (only laneLead is the lead).
		return contextFill{}, false
	}
	return contextFill{}, false
}

// finite is web's Number.isFinite guard: a value is usable only when it is neither ±Inf nor NaN.
func finite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

// contextTone mirrors web's contextState off the UNCLAMPED pct: near (pct>=95) → alarm,
// molten (pct>=70) → amber, cool → faintC (cool reads quiet, no accent). color.Color is the
// same type rateTone returns.
func contextTone(pal palette, pct float64) color.Color {
	switch {
	case pct >= 95:
		return pal.alarm
	case pct >= 70:
		return pal.amber
	default:
		return pal.faintC
	}
}

// contextMeterCell renders the inline lead context meter: a leading space, the tone-coloured
// filled run, the faint empty run, then the faint " NN%" label. The bar CLAMPS via rateBarParts
// while the label carries the TRUE rounded pct (so pct 112 → full bar + "112%"). Every segment
// carries bg so a selected lead row's meter rides the selection background like the rest of the
// row; under cool the tone is faintC, so the whole bar reads faint (no accent). No used/window
// token counts — this is a bare pct meter.
func (m tuiModel) contextMeterCell(bg color.Color, fill contextFill) string {
	r := int(math.Round(fill.pct))
	filled, empty := rateBarParts(r)
	return paintSeg(m.pal.faintC, bg, false, " ") +
		paintSeg(contextTone(m.pal, fill.pct), bg, false, filled) +
		paintSeg(m.pal.faintC, bg, false, empty) +
		paintSeg(m.pal.faintC, bg, false, " "+strconv.Itoa(r)+"%")
}
