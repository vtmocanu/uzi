package main

import (
	"encoding/json"
	"fmt"
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

	over := stripANSI(m.contextMeterCell(nil, contextFill{used: 224000, window: 200000, pct: 112}))
	if !strings.Contains(over, "▰▰▰▰▰▰") {
		t.Errorf("pct 112 must clamp the bar to full ▰▰▰▰▰▰:\n%q", over)
	}
	if !strings.Contains(over, "112%") {
		t.Errorf("pct 112 label must show the TRUE unclamped 112%%:\n%q", over)
	}
	if strings.Contains(over, "/") {
		t.Errorf("meter must NOT carry a used/window token count (no `/`):\n%q", over)
	}

	partial := stripANSI(m.contextMeterCell(nil, contextFill{used: 124000, window: 200000, pct: 62}))
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
