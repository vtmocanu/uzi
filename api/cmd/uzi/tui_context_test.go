package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// leadCtxMsg builds a lead usage frame carrying a top-level "context" reading (PRD #516). msgDTO
// only emits a {"text":…} payload, which has no "context" key and so never drives the meter, so
// the meter tests need this dedicated builder. The frame is attributed to "lead" so it lands in
// the lead lane (laneKeyOf → Agent "lead" == laneLead).
func leadCtxMsg(seq int32, used, window, pct float64, at time.Time) apitypes.MessageDTO {
	agent := "lead"
	payload := fmt.Sprintf(`{"usage":{"input_tokens":1},"context":{"used":%v,"window":%v,"pct":%v}}`, used, window, pct)
	return apitypes.MessageDTO{Seq: seq, Kind: "usage", CreatedAt: at, Agent: &agent,
		Payload: json.RawMessage(payload)}
}

// --- pure-function unit tests ------------------------------------------------

// TestContextTone mirrors web contextState off the UNCLAMPED pct: near (pct>=95) → alarm,
// molten (pct>=70) → amber, cool → faintC. Boundaries are >=, near checked first.
func TestContextTone(t *testing.T) {
	m := tuiTestModel(t, nil, "")
	cases := []struct {
		pct  float64
		want interface{}
		name string
	}{
		{96, m.pal.alarm, "alarm"},
		{95, m.pal.alarm, "alarm"},
		{94, m.pal.amber, "amber"},
		{70, m.pal.amber, "amber"},
		{69, m.pal.faintC, "faintC"},
		{50, m.pal.faintC, "faintC"},
	}
	for _, c := range cases {
		if got := contextTone(m.pal, c.pct); got != c.want {
			t.Errorf("contextTone(pct=%v) = %v, want %s", c.pct, got, c.name)
		}
	}
}

// TestContextMeterCell — the bar CLAMPS via rateBarParts while the label carries the TRUE rounded
// pct, and there is NO used/window token count (no "/"). pct 112 → a full ▰ bar + "112%"; pct 62 →
// a partial bar (some ▱ empty) + "62%".
func TestContextMeterCell(t *testing.T) {
	m := tuiTestModel(t, nil, "")

	over := stripANSI(m.contextMeterCell(nil, contextFill{pct: 112}, 13))
	if !strings.Contains(over, "▰▰▰▰▰▰") {
		t.Errorf("pct 112 must clamp the bar to full ▰▰▰▰▰▰:\n%q", over)
	}
	if !strings.Contains(over, "112%") {
		t.Errorf("pct 112 label must show the TRUE unclamped 112%%:\n%q", over)
	}
	if strings.Contains(over, "/") {
		t.Errorf("meter must NOT carry a used/window token count (no `/`):\n%q", over)
	}

	partial := stripANSI(m.contextMeterCell(nil, contextFill{pct: 62}, 13))
	if !strings.Contains(partial, "62%") {
		t.Errorf("pct 62 label missing:\n%q", partial)
	}
	if !strings.Contains(partial, "▱") {
		t.Errorf("pct 62 bar must be partial (some ▱ empty), not full:\n%q", partial)
	}
	if strings.Contains(partial, "/") {
		t.Errorf("meter must NOT carry a used/window token count (no `/`):\n%q", partial)
	}
}

// TestLeadContextFillGuards — readContext parity: a reading is used only when used, window AND
// pct are all present; a partial/absent context yields no meter. Latest (newest) valid wins.
func TestLeadContextFillGuards(t *testing.T) {
	now := time.Now()

	// No context anywhere: no fill.
	lanes := buildLanes([]laneFrame{laneFrameFromMessage(msgDTO(1, "text", "lead", "", "", "hi", now))})
	if _, ok := leadContextFill(lanes); ok {
		t.Error("a lead lane with only text frames must yield no context fill")
	}

	// A partial context (missing pct) is skipped; an earlier complete one wins.
	agent := "lead"
	partial := apitypes.MessageDTO{Seq: 3, Kind: "usage", CreatedAt: now, Agent: &agent,
		Payload: json.RawMessage(`{"context":{"used":1,"window":2}}`)}
	lanes = buildLanes([]laneFrame{
		laneFrameFromMessage(leadCtxMsg(1, 100000, 200000, 50, now)),
		laneFrameFromMessage(msgDTO(2, "text", "lead", "", "", "thinking", now)),
		laneFrameFromMessage(partial),
	})
	fill, ok := leadContextFill(lanes)
	if !ok {
		t.Fatal("a complete earlier context must survive a later partial one")
	}
	if fill.pct != 50 {
		t.Errorf("newest-VALID wins: want pct 50, got %v", fill.pct)
	}
}

// leadFrame builds a lead-lane usage frame from a raw payload JSON, so a test can exercise the
// exact usage/context bytes leadContextFill decodes (mirrors leadCtxMsg but with hand-written
// payloads for the usage-gate cases).
func leadFrame(seq int32, payload string, at time.Time) apitypes.MessageDTO {
	agent := "lead"
	return apitypes.MessageDTO{Seq: seq, Kind: "usage", CreatedAt: at, Agent: &agent,
		Payload: json.RawMessage(payload)}
}

// TestLeadContextFillUsageGate — web reads `context` ONLY inside the `if (u)` branch (runUsage.ts:674),
// so a valid context is used only when the same frame ALSO carries a valid usage OBJECT. readUsage
// accepts any non-null object/array (empty {} counts), and rejects null/scalar/absent.
func TestLeadContextFillUsageGate(t *testing.T) {
	now := time.Now()

	// A newer frame with valid context but NO usage key is IGNORED; an earlier frame carrying
	// BOTH a valid usage object and context wins.
	lanes := buildLanes([]laneFrame{
		laneFrameFromMessage(leadFrame(1, `{"usage":{"input_tokens":1},"context":{"used":100000,"window":200000,"pct":50}}`, now.Add(-time.Minute))),
		laneFrameFromMessage(leadFrame(2, `{"context":{"used":180000,"window":200000,"pct":90}}`, now)),
	})
	fill, ok := leadContextFill(lanes)
	if !ok {
		t.Fatal("a context-only newer frame must be skipped, letting the earlier usage+context frame win")
	}
	if fill.pct != 50 {
		t.Errorf("usage-gated: want the earlier usage+context frame's pct 50, got %v", fill.pct)
	}

	// A frame with "usage":null + valid context is IGNORED (null is not a usage object).
	lanes = buildLanes([]laneFrame{
		laneFrameFromMessage(leadFrame(1, `{"usage":null,"context":{"used":100000,"window":200000,"pct":50}}`, now)),
	})
	if _, ok := leadContextFill(lanes); ok {
		t.Error(`a frame with "usage":null must be ignored (web's readUsage rejects null)`)
	}

	// A frame with "usage":{} (empty object) + valid context IS accepted — web's rec() accepts
	// any non-null object, the token values being irrelevant.
	lanes = buildLanes([]laneFrame{
		laneFrameFromMessage(leadFrame(1, `{"usage":{},"context":{"used":124000,"window":200000,"pct":62}}`, now)),
	})
	fill, ok = leadContextFill(lanes)
	if !ok {
		t.Fatal(`a frame with "usage":{} (empty object) + valid context must be accepted`)
	}
	if fill.pct != 62 {
		t.Errorf(`empty-usage-object frame: want pct 62, got %v`, fill.pct)
	}
}

// TestContextMeterCellLabelClamped — the DISPLAYED label is bounded to 4 cols ("999%") even for an
// absurd pct, so a lead row never overflows the rail; the bar/tone still ride the true pct.
func TestContextMeterCellLabelClamped(t *testing.T) {
	m := tuiTestModel(t, nil, "")

	row := strings.SplitN(stripANSI(
		m.laneRow(agentLane{Key: laneLead, Role: "lead"}, false, crewIdle, "", contextFill{pct: 1e9}, true)),
		"\n", 2)[0]
	if w := visualWidth(row); w != laneRailWidth {
		t.Errorf("an absurd pct must keep the row at laneRailWidth %d, got %d:\n%q", laneRailWidth, w, row)
	}
	if strings.Contains(row, "1000000000%") {
		t.Errorf("the label must be clamped, not show the raw 1e9 pct:\n%q", row)
	}
	if !strings.Contains(row, "999%") {
		t.Errorf("the clamped label must read 999%% for an over-999 pct:\n%q", row)
	}
}

// TestContextMeterCellExtremePctClampedInFloatDomain — a finite-but-out-of-int-range pct passes
// finite() and reaches contextMeterCell, so the clamp MUST happen in the float domain before the
// int conversion (Go leaves out-of-range float→int implementation-defined). math.MaxFloat64 →
// "999%" + a full ▰ bar; a large-negative pct → "0%". This guards the pre-fix path where the
// conversion happened before any clamp.
func TestContextMeterCellExtremePctClampedInFloatDomain(t *testing.T) {
	m := tuiTestModel(t, nil, "")

	huge := stripANSI(m.contextMeterCell(nil, contextFill{pct: math.MaxFloat64}, 13))
	if !strings.Contains(huge, "999%") {
		t.Errorf("math.MaxFloat64 pct must clamp the label to 999%%:\n%q", huge)
	}
	if !strings.Contains(huge, "▰▰▰▰▰▰") {
		t.Errorf("math.MaxFloat64 pct must clamp the bar to full ▰▰▰▰▰▰:\n%q", huge)
	}
	if strings.Contains(huge, "/") {
		t.Errorf("meter must NOT carry a used/window token count (no `/`):\n%q", huge)
	}

	neg := stripANSI(m.contextMeterCell(nil, contextFill{pct: -math.MaxFloat64}, 13))
	if !strings.Contains(neg, "0%") {
		t.Errorf("a large-negative pct must clamp the label to 0%%:\n%q", neg)
	}
	if strings.Contains(neg, "▰") {
		t.Errorf("a large-negative pct must render an empty bar (no ▰):\n%q", neg)
	}
}

// sgrTruecolor pulls the `38;2;r;g;b` foreground triplet out of a reference render (e.g. a
// paintSeg span) so an expected colour is DERIVED from the live palette at runtime, not hardcoded
// as hex — this keeps the assertion immune to theme/hex drift.
var sgrTruecolor = regexp.MustCompile(`38;2;[0-9]+;[0-9]+;[0-9]+`)

func toneCode(t *testing.T, c interface{ RGBA() (r, g, b, a uint32) }) string {
	t.Helper()
	ref := paintSeg(c, nil, false, "▰")
	code := sgrTruecolor.FindString(ref)
	if code == "" {
		t.Fatalf("no 38;2;r;g;b triplet in reference render %q", ref)
	}
	return code
}

// TestContextMeterCellToneColour — the render-level twin of TestContextTone (AC3): verify that
// contextMeterCell actually APPLIES the tone to the FILLED run in the raw SGR output, so the
// tone→colour mapping is gated through the render path and not just the pure function. Expected
// colours are derived from m.pal at runtime (via a paintSeg reference) rather than hardcoded hex.
func TestContextMeterCellToneColour(t *testing.T) {
	m := tuiTestModel(t, nil, "")

	alarmCode := toneCode(t, m.pal.alarm)
	amberCode := toneCode(t, m.pal.amber)
	faintCode := toneCode(t, m.pal.faintC)

	cases := []struct {
		name string
		pct  float64
		want string // the SGR triplet the FILLED run must carry
	}{
		{"near", 98, alarmCode},
		{"molten", 80, amberCode},
		{"cool", 50, faintCode},
	}
	for _, c := range cases {
		// The FILLED run is the tone SGR code immediately followed by a ▰ glyph; asserting the
		// code sits directly on ▰ (not merely somewhere in the string) pins the colour to the
		// filled portion, distinct from the faint leading space / empty run / label.
		raw := m.contextMeterCell(nil, contextFill{pct: c.pct}, 13)
		wantFilled := "\x1b[" + c.want + "m▰"
		if !strings.Contains(raw, wantFilled) {
			t.Errorf("%s (pct=%v): filled run must carry tone %s directly on ▰:\n%q",
				c.name, c.pct, c.want, raw)
		}
	}

	// Non-vacuity guard: the cool meter must carry NEITHER the amber NOR the alarm colour, so the
	// test would fail if contextMeterCell ever painted one fixed accent regardless of pct.
	cool := m.contextMeterCell(nil, contextFill{pct: 50}, 13)
	if strings.Contains(cool, amberCode) {
		t.Errorf("cool meter must not carry the amber (molten) colour %s:\n%q", amberCode, cool)
	}
	if strings.Contains(cool, alarmCode) {
		t.Errorf("cool meter must not carry the alarm (near) colour %s:\n%q", alarmCode, cool)
	}
}

// --- model-level tests -------------------------------------------------------

// railLine returns the first stripped rail line containing the substring, and whether one exists.
func railLine(t *testing.T, m tuiModel, role string) (string, bool) {
	t.Helper()
	for _, ln := range strings.Split(stripANSI(m.renderLaneRail()), "\n") {
		if strings.Contains(ln, role) {
			return ln, true
		}
	}
	return "", false
}

// TestDetailLeadContextMeterAC1 — a run with a lead frame carrying context AND a subagent frame:
// the lead row shows the ▰…▱ bar and NN%, while the subagent row and the synthetic ALL lane row
// do NOT. The positive assertion runs first so the negative one is proven non-vacuous.
func TestDetailLeadContextMeterAC1(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "ctx-ac1", "ok", []apitypes.MessageDTO{
		leadCtxMsg(1, 124000, 200000, 62, now),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "code", now),
	})

	// Non-vacuity: the lead row shows the meter.
	lead, ok := railLine(t, m, "lead")
	if !ok {
		t.Fatal("no lead row in the rail")
	}
	for _, want := range []string{"▰", "▱", "62%"} {
		if !strings.Contains(lead, want) {
			t.Errorf("lead row missing context-meter cue %q:\n%q", want, lead)
		}
	}

	// The subagent row carries no meter.
	if sub, ok := railLine(t, m, "coder"); ok {
		if strings.Contains(sub, "%") || strings.Contains(sub, "▰") {
			t.Errorf("subagent row must NOT carry a context meter:\n%q", sub)
		}
	} else {
		t.Error("no coder row in the rail")
	}

	// The synthetic ALL lane (present with ≥2 real lanes) carries no meter.
	if all, ok := railLine(t, m, "all agents"); ok {
		if strings.Contains(all, "%") || strings.Contains(all, "▰") {
			t.Errorf("ALL lane row must NOT carry a context meter:\n%q", all)
		}
	} else {
		t.Fatal("expected the aggregated ALL lane with two real lanes")
	}
}

// TestDetailLeadContextMeterAC4 — a run whose lead frames carry NO context (only text) shows no
// meter on the lead row.
func TestDetailLeadContextMeterAC4(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "ctx-ac4", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "planning", now),
		msgDTO(2, "text", "lead", "", "", "still planning", now),
	})
	lead, ok := railLine(t, m, "lead")
	if !ok {
		t.Fatal("no lead row in the rail")
	}
	if strings.Contains(lead, "▰") || strings.Contains(lead, "%") {
		t.Errorf("a lead with no context must show no meter:\n%q", lead)
	}
}

// TestDetailLeadContextMeterAC5 — latest-wins across lead frames: two context frames (pct 40 then
// 80, the higher seq later) → the rail shows 80%, not 40%.
func TestDetailLeadContextMeterAC5(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "ctx-ac5", "ok", []apitypes.MessageDTO{
		leadCtxMsg(1, 80000, 200000, 40, now.Add(-time.Minute)),
		leadCtxMsg(2, 160000, 200000, 80, now),
	})
	rail := stripANSI(m.renderLaneRail())
	if !strings.Contains(rail, "80%") {
		t.Errorf("latest-wins: rail must show 80%%:\n%s", rail)
	}
	if strings.Contains(rail, "40%") {
		t.Errorf("latest-wins: stale 40%% must not appear:\n%s", rail)
	}
}

// TestDetailLeadContextMeterAsciiSignalSurvives — under an Ascii (NO_COLOR) profile the ▰/▱ bar
// glyphs and the NN% label survive, so the meter stays legible when tone colour is gone (mirrors
// TestRailRateMetersAsciiSignalSurvives).
func TestDetailLeadContextMeterAsciiSignalSurvives(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "ctx-ascii", "ok", []apitypes.MessageDTO{
		leadCtxMsg(1, 124000, 200000, 62, now),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "code", now),
	})
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	rail := stripANSI(m.renderLaneRail())
	for _, want := range []string{"▰", "▱", "62%"} {
		if !strings.Contains(rail, want) {
			t.Errorf("Ascii-profile rail dropped the context-meter cue %q:\n%s", want, rail)
		}
	}
}

// TestDetailLeadContextMeterFillsRail — the inline lead meter fills the rail to its right edge:
// the rendered lead row is exactly laneRailWidth (26) visual cols and ends with the right-aligned
// percent, so the bar lands flush at col 26 rather than the old fixed 6-wide stub.
func TestDetailLeadContextMeterFillsRail(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "ctx-fill", "ok", []apitypes.MessageDTO{
		leadCtxMsg(1, 124000, 200000, 62, now),
	})
	lead, ok := railLine(t, m, "lead")
	if !ok {
		t.Fatal("no lead row in the rail")
	}
	if w := visualWidth(lead); w != laneRailWidth {
		t.Errorf("lead meter row width = %d, want the full laneRailWidth %d:\n%q", w, laneRailWidth, lead)
	}
	if !strings.HasSuffix(strings.TrimRight(lead, " "), "62%") {
		t.Errorf("lead row must end with the right-aligned percent (bar flush at col %d):\n%q", laneRailWidth, lead)
	}
}

// TestContextMeterCellShrinksWithPrefix — laneRow derives the meter bar width from the actual
// row prefix (▸● <role><suffix>): a wider prefix leaves a narrower bar, and the whole row never
// exceeds laneRailWidth. The lead prefix "lead" (7 cols) yields a 13-wide bar; a longer role +
// a ·N suffix yields a shorter one, both padded to exactly 26.
func TestContextMeterCellShrinksWithPrefix(t *testing.T) {
	m := tuiTestModel(t, nil, "")
	fill := contextFill{pct: 62}

	firstLine := func(s string) string {
		return strings.SplitN(stripANSI(s), "\n", 2)[0]
	}
	barGlyphs := func(s string) int {
		return strings.Count(s, "▰") + strings.Count(s, "▱")
	}

	leadRow := firstLine(m.laneRow(agentLane{Key: laneLead, Role: "lead"}, false, crewIdle, "", fill, true))
	longRow := firstLine(m.laneRow(agentLane{Key: "orch", Role: "orchestrator"}, false, crewIdle, "·2", fill, true))

	for name, row := range map[string]string{"lead": leadRow, "long": longRow} {
		if w := visualWidth(row); w != laneRailWidth {
			t.Errorf("%s row width = %d, want laneRailWidth %d (no overflow):\n%q", name, w, laneRailWidth, row)
		}
	}
	leadBars, longBars := barGlyphs(leadRow), barGlyphs(longRow)
	if leadBars != 13 {
		t.Errorf("lead prefix (7 cols) must leave a 13-wide bar, got %d:\n%q", leadBars, leadRow)
	}
	if longBars >= leadBars {
		t.Errorf("a wider prefix must shrink the bar: long=%d not < lead=%d:\n%q\n%q",
			longBars, leadBars, longRow, leadRow)
	}
}
