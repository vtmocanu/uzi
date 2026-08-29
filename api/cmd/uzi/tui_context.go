package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
//   - the context read is GATED on the frame ALSO carrying a valid (non-null JSON object or
//     array) "usage", matching web's `if (u)` branch (runUsage.ts:674): web reads "context"
//     only inside the block guarded by a truthy readUsage(payload.usage), and readUsage
//     returns a value for any non-null object/array (an empty {} counts) — the usage token
//     VALUES are irrelevant, only its presence as an object. A frame carrying a valid context
//     but no (or a null/scalar) usage is therefore ignored.
//   - leadContext: latest-wins across LEAD frames only; a subagent frame's context is
//     ignored.

// contextFill is a decoded, validated lead context reading. Only pct is stored — used/window
// are still GUARDED for finiteness (readContext parity) but nothing downstream reads them.
type contextFill struct {
	pct float64
}

// isUsageObject reports whether raw is a non-null JSON object or array, mirroring web's rec()
// acceptance (runUsage.ts:276-277: `v && typeof v === "object"`): an empty {} counts, a bare
// null fails (it trims to "null", first byte 'n'), and a missing key leaves raw nil/empty.
func isUsageObject(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

// leadContextFill finds the lead lane and returns the newest lead frame carrying BOTH a valid
// usage object AND a finite context, mirroring web's leadContext (latest-wins across lead
// frames only). It scans the lead lane's frames NEWEST-FIRST and returns the first frame whose
// usage is a non-null JSON object/array (web's `if (u)` gate) and whose context has used, window
// AND pct all present and finite — the exact readContext guard. A frame with no "context" key,
// no/invalid "usage", or a decode failure is skipped. No lead lane, or no such frame on it,
// yields (contextFill{}, false) and no meter.
func leadContextFill(lanes []agentLane) (contextFill, bool) {
	for _, l := range lanes {
		if l.Key != laneLead {
			continue
		}
		for i := len(l.Frames) - 1; i >= 0; i-- {
			var w struct {
				Usage   json.RawMessage                       `json:"usage"`
				Context *struct{ Used, Window, Pct *float64 } `json:"context"`
			}
			f := l.Frames[i]
			if json.Unmarshal(f.Payload, &w) != nil {
				continue
			}
			// Web reads context ONLY inside the `if (u)` branch, so the frame must carry a
			// valid usage object first (empty {} counts; null/scalar/absent does not).
			if !isUsageObject(w.Usage) {
				continue
			}
			if w.Context == nil {
				continue
			}
			c := w.Context
			if c.Used == nil || c.Window == nil || c.Pct == nil {
				continue
			}
			if !finite(*c.Used) || !finite(*c.Window) || !finite(*c.Pct) {
				continue
			}
			return contextFill{pct: *c.Pct}, true
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
func (m tuiModel) contextMeterCell(bg color.Color, fill contextFill, barW int) string {
	// Clamp pct in the FLOAT domain to the DISPLAYED-label budget [0,999] BEFORE converting to
	// int: Go leaves out-of-range float→int conversions implementation-defined, so an
	// extreme-but-finite pct (e.g. math.MaxFloat64, which passes finite()) would otherwise yield
	// a platform-dependent label and bar. The 4-col ("999%") budget is what laneRow reserves for
	// the meter tail. The bar still clamps to [0,100] in rateBarParts, and the tone still rides
	// the TRUE unclamped pct via contextTone (no int conversion there).
	p := fill.pct
	if p < 0 {
		p = 0
	} else if p > 999 {
		p = 999
	}
	r := int(math.Round(p))
	filled, empty := rateBarParts(r, barW)
	return paintSeg(m.pal.faintC, bg, false, " ") +
		paintSeg(contextTone(m.pal, fill.pct), bg, false, filled) +
		paintSeg(m.pal.faintC, bg, false, empty) +
		paintSeg(m.pal.faintC, bg, false, " "+fmt.Sprintf("%4s", strconv.Itoa(r)+"%"))
}
