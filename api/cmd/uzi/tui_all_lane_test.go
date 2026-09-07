package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// loadDetail applies a replay to a fresh detail view and returns the model.
func loadDetail(t *testing.T, runID, health string, msgs []apitypes.MessageDTO) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, runID)
	return applyDetail(m, apitypes.RunDTO{ID: runID, Status: "running", Health: health}, msgs)
}

// The aggregated "all agents" lane is gated on ≥2 real lanes (a single-actor run would only
// duplicate its one lane) and, when present, is prepended and selected by default so a run
// opens on the firehose.
func TestDetailAllLaneGateAndDefaultSelection(t *testing.T) {
	now := time.Now()

	// One actor: no aggregated lane.
	solo := loadDetail(t, "a11a11a1-1111", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "solo", now),
	})
	if len(solo.detail.lanes) != 1 {
		t.Fatalf("one actor produced %d lanes, want 1", len(solo.detail.lanes))
	}
	if solo.detail.lanes[0].Key == laneAllKey {
		t.Error("a single-lane run must NOT get the aggregated lane")
	}

	// Two actors: ALL prepended at 0, selected by default, holding every frame.
	m := loadDetail(t, "a11a11a1-2222", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "plan", now),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "code", now),
	})
	if got := m.detail.lanes[0].Key; got != laneAllKey {
		t.Fatalf("aggregated lane not first; lanes[0].Key=%q", got)
	}
	if m.detail.laneIdx != 0 {
		t.Errorf("run did not open on the aggregated lane; laneIdx=%d", m.detail.laneIdx)
	}
	sel, ok := m.detail.selectedLane()
	if !ok || sel.Key != laneAllKey || sel.Role != laneAllRole {
		t.Fatalf("selected lane = %+v, want the %q lane", sel, laneAllRole)
	}
	if len(sel.Frames) != len(m.detail.frames) {
		t.Errorf("aggregated lane holds %d frames, want all %d", len(sel.Frames), len(m.detail.frames))
	}
}

// The aggregated transcript interleaves every actor and tags each block with the SAME identity
// the crew rail draws for its lane; a single lane names itself once in the title and carries no
// per-frame tag.
func TestDetailAllLaneTagsMatchRailIdentity(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "a22a22a2-2222", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "planning", now.Add(-2*time.Minute)),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "writing", now.Add(-time.Minute)),
		msgDTO(3, "text", "tester", "toolu_b", "sweep", "running", now),
	})

	all := ansi.Strip(m.View().Content)
	if !strings.Contains(all, "TRANSCRIPT · all agents") {
		t.Errorf("aggregated pane title missing\n%s", all)
	}
	// Each text turn carries a "▪ <who>" speaker header naming its actor.
	for _, tag := range []string{"▪ lead", "▪ coder", "▪ tester"} {
		if !strings.Contains(all, tag) {
			t.Errorf("aggregated transcript missing speaker header %q\n%s", tag, all)
		}
	}

	// ALL -> lead -> coder.
	m = press(t, m, "j")
	m = press(t, m, "j")
	solo := ansi.Strip(m.View().Content)
	if !strings.Contains(solo, "TRANSCRIPT · coder") {
		t.Errorf("single-lane title should name the lane\n%s", solo)
	}
	// A single lane carries NO speaker headers at all (the pane title already names it), so no
	// other actor's header can leak in.
	if strings.Contains(solo, "▪ lead") || strings.Contains(solo, "▪ tester") {
		t.Errorf("single-lane transcript must not carry other actors' speaker headers\n%s", solo)
	}
}

// The aggregated row is a meta-lane, not an actor, so it wears a neutral ◉ and NEVER the
// alarming ▲ a stalled agent puts on its own row — even when the crew is stalled.
func TestDetailAllLaneIconIsNeutral(t *testing.T) {
	now := time.Now()
	m := loadDetail(t, "a33a33a3-3333", "stalled", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "a", now.Add(-time.Minute)),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "b", now), // newest -> the active (stalled) lane
	})
	if m.detail.lanes[0].Key != laneAllKey {
		t.Fatalf("lanes[0] is not the aggregated lane (%q)", m.detail.lanes[0].Key)
	}

	rail := ansi.Strip(m.View().Content)
	if !strings.Contains(rail, "◉ all agents") {
		t.Errorf("aggregated row should carry the neutral ◉ glyph\n%s", rail)
	}
	if strings.Contains(rail, "▲ all agents") {
		t.Errorf("aggregated row must NOT show the stalled ▲ glyph\n%s", rail)
	}
	// The real stalled lane still wears its ▲, so the neutral ALL icon is a deliberate choice,
	// not the absence of any stalled agent.
	if !strings.Contains(rail, "▲ coder") {
		t.Errorf("the active stalled lane should still show ▲\n%s", rail)
	}
}

// TestDetailSelectedCrewRoleHasExplicitForeground pins issue #938's crew-rail half: a SELECTED
// crew lane's role carries the explicit tungsten foreground so it stays legible on the warm
// selection bar (on a light terminal the default-ink role would otherwise wash into it), the
// same treatment boardRow's selected floor title got. Unselected rows keep the default ink
// (nil fg). Mutation that reddens this: reverting laneRow's selected role to paintSeg(nil,...).
func TestDetailSelectedCrewRoleHasExplicitForeground(t *testing.T) {
	now := time.Now()
	// Two actors -> lanes [ALL, lead, coder]. Move selection ALL -> lead -> coder so a REAL
	// lane (never the meta ALL row) is selected; coder never carries the lead's inline meter,
	// so the role span is drawn exactly as laneRow builds it, with nothing appended after it.
	m := loadDetail(t, "a44a44a4-4444", "ok", []apitypes.MessageDTO{
		msgDTO(1, "text", "lead", "", "", "planning", now.Add(-time.Minute)),
		msgDTO(2, "text", "coder", "toolu_a", "impl", "writing", now),
	})
	m = press(t, m, "j")
	m = press(t, m, "j")
	if sel, ok := m.detail.selectedLane(); !ok || sel.Role != "coder" {
		t.Fatalf("expected the coder lane selected, got %+v (ok=%v)", sel, ok)
	}
	// Raw content (NOT ansi.Strip): the assertion is about the SGR the role span carries.
	out := m.View().Content

	// The selected role is painted with tungsten over the warm selection bar, exactly as
	// laneRow draws it (leading space + capped role). This is the span that vanishes if the
	// selected branch falls back to nil fg.
	selRole := paintSeg(m.pal.tungsten, m.pal.selBg, false, " "+m.renderer.Plain("coder", 14))
	if !strings.Contains(out, selRole) {
		t.Errorf("selected crew role is not painted with the tungsten fg over the selection bar\nwant span %q\n%s", selRole, out)
	}
	// Control (non-vacuous): the UNSELECTED lead role must NOT carry the tungsten-over-selBg
	// span — an unselected row has no selection bg and keeps the default ink. If this appeared,
	// the assertion above could pass for the wrong reason (every role tungsten-painted).
	unselRole := paintSeg(m.pal.tungsten, m.pal.selBg, false, " "+m.renderer.Plain("lead", 14))
	if strings.Contains(out, unselRole) {
		t.Errorf("unselected crew role must keep default ink, not tungsten over the selection bar\ngot span %q\n%s", unselRole, out)
	}
}
