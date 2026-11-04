package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// harnessDecisionScene drives the shipped detail render for main's dark/light review.
func harnessDecisionScene(dark bool, now time.Time) tuiModel {
	zero, hundred, measured := 0.0, 100.0, 37.5
	m := uxModel(&uzicli.FakeClient{}, detailRunID, dark)
	m.height = 50
	return applyDetail(m, apitypes.RunDTO{ID: detailRunID, Kind: "issue", Harness: "claude", Status: "completed", Health: "ok",
		IssueTitle: "Stored harness choice at creation", StartedAt: tp(now.Add(-time.Minute)),
		HarnessDecision: &apitypes.HarnessDecisionDTO{Preferred: "codex", Chosen: "claude", Reason: "overflow",
			PreferredScore: &zero, OtherScore: &hundred, PreferredMeasured: &measured, OtherMeasured: &zero, PreferredHeld: true}}, nil)
}

func TestRailHarnessDecisionSnapshotAndBudget(t *testing.T) {
	zero, hundred, measured := 0.0, 100.0, 37.5
	m := tuiTestModel(t, &uzicli.FakeClient{}, "snapshot")
	m.width, m.height = 100, 60
	m = applyDetail(m, apitypes.RunDTO{ID: "snapshot", Status: "completed", Harness: "claude",
		HarnessDecision: &apitypes.HarnessDecisionDTO{
			Preferred: "codex", Chosen: "claude", Reason: "overflow",
			PreferredScore: &zero, OtherScore: &hundred, PreferredMeasured: &measured, OtherMeasured: &zero, PreferredHeld: true,
		}}, nil)
	want := "HARNESS at creation\npreferred: codex\nchosen: claude\nreason: overflow\npreferred score: 0%\nother score: 100%\npref measured: 37.5%\nother measured: 0%\npreferred held: true\nother held: false\npref reset: unknown\nother reset: unknown"
	const rows = 12
	used := m.transcriptViewport() - 1 - rows
	before := m.railHarnessDecision(used)
	if got := stripANSI(before); got != want {
		t.Fatalf("snapshot:\n%q\nwant:\n%q", got, want)
	}
	if got := m.railHarnessDecision(used + 1); got != "" {
		t.Fatalf("partial snapshot fits one row short: %q", got)
	}
	// Live readings and time do not change historical evidence.
	m.rateLimits = []apitypes.TokenRateLimitDTO{{IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 99}}}}
	if got := m.railHarnessDecision(used); got != before {
		t.Fatalf("live meter changed history: %q", got)
	}
	var b bytes.Buffer
	w := colorprofile.Writer{Forward: &b, Profile: colorprofile.NoTTY}
	if _, err := w.Write([]byte(before)); err != nil {
		t.Fatal(err)
	}
	if b.String() != want {
		t.Fatalf("NoTTY lost snapshot: %q", b.String())
	}
	// The real composed rail includes the block and budgets all later blocks below it.
	if rail := stripANSI(m.renderLaneRail()); !strings.Contains(rail, want) {
		t.Fatalf("snapshot absent from composed rail:\n%s", rail)
	}
	m.detail.run.HarnessDecision = nil
	if got := stripANSI(m.railHarnessDecision(0)); got != "HARNESS at creation\nnot recorded" {
		t.Fatalf("legacy: %q", got)
	}
	m.detail.run.HarnessDecision = &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "claude", Reason: "preferred"}
	if got := stripANSI(m.railHarnessDecision(0)); !strings.Contains(got, "preferred score: unknown\nother score: unknown") || !strings.Contains(got, "pref measured: unknown") {
		t.Fatalf("unknown collapsed to zero: %q", got)
	}
}

func TestRailHarnessDecisionHostileWidth(t *testing.T) {
	for _, width := range []int{80, 100, 160} {
		for _, dark := range []bool{false, true} {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "hostile")
			m.width, m.height = width, 60
			m.dark, m.pal = dark, newPalette(dark)
			m.renderer, _ = newTUIRenderer(m.transcriptWidth(), dark)
			hostile := "ok\x1b[2J\u202e\n\t" + strings.Repeat("界", 80)
			m = applyDetail(m, apitypes.RunDTO{ID: "hostile", Status: "completed", HarnessDecision: &apitypes.HarnessDecisionDTO{Preferred: hostile, Chosen: hostile, Reason: hostile}}, nil)
			rawBlock := m.railHarnessDecision(0)
			// Stripping ANSI first would hide the injected erase-screen command.
			if strings.Contains(rawBlock, "\x1b[2J") {
				t.Fatalf("unsafe escape in raw block: %q", rawBlock)
			}
			block := stripANSI(rawBlock)
			for _, prefix := range []string{"preferred: ok", "chosen: ok", "reason: ok"} {
				if !strings.Contains(block, prefix) {
					t.Fatalf("field not exercised: %q", block)
				}
			}
			if strings.ContainsAny(block, "\x1b\u202e\t") || strings.Contains(block, "ok\n") {
				t.Fatalf("unsafe block: %q", block)
			}
			for _, row := range strings.Split(block, "\n") {
				if visualWidth(row) > m.railWidth() {
					t.Fatalf("row exceeds width %d: %q", m.railWidth(), row)
				}
			}
			// These are the shipped full-rail draws, with the stored evidence still present.
			rawRail := m.renderLaneRail()
			if strings.Contains(rawRail, "\x1b[2J") {
				t.Fatalf("unsafe escape in composed rail: %q", rawRail)
			}
			rail := stripANSI(rawRail)
			if !strings.Contains(rail, "HARNESS at creation") || strings.ContainsAny(rail, "\x1b\u202e") {
				t.Fatalf("unsafe or missing rail block: %q", rail)
			}
		}
	}
}

func TestRailHarnessDecisionProtectsFoldBudget(t *testing.T) {
	now := time.Now()
	run, msgs := autoFoldRun(false, false)
	run.HarnessDecision = &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "codex", Reason: "overflow"}
	found := false
	for height := 24; height <= 60; height++ {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m.width, m.height = 100, height
		m = applyDetail(m, run, msgs)
		legacy := m
		legacy.detail.run.HarnessDecision = nil
		if m.railAutoFolded(now) && !legacy.railAutoFolded(now) {
			rail := stripANSI(m.renderLaneRail())
			if strings.Contains(rail, "other reset: unknown") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("recorded decision never protected by folding the roster")
	}
}

func TestRailHarnessDecisionConsumesMeterBudget(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "budget")
	m.width, m.height = 100, 60
	m = applyDetail(m, apitypes.RunDTO{ID: "budget", Status: "completed", HarnessDecision: &apitypes.HarnessDecisionDTO{Preferred: "claude", Chosen: "codex", Reason: "overflow"}}, nil)
	m.rateLimits = []apitypes.TokenRateLimitDTO{{SecretID: "shown", Label: "shown", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 10}, SevenDay: &apitypes.RateLimitWindow{Pct: 20}}}}
	// Search a bounded set of small terminal heights for the composition boundary:
	// the historical block fits, while a live meter that fits without it is dropped.
	found := false
	for height := 18; height <= 40; height++ {
		m.height = height
		with := stripANSI(m.renderLaneRail())
		legacy := m
		legacy.detail.run.HarnessDecision = nil
		without := stripANSI(legacy.renderLaneRail())
		if strings.Contains(with, "HARNESS at creation") && !strings.Contains(with, "ACCOUNTS") && strings.Contains(without, "ACCOUNTS") {
			found = true
			if !strings.Contains(with, "other reset: unknown") {
				t.Fatalf("partial historical block: %q", with)
			}
			if len(strings.Split(with, "\n")) > m.transcriptViewport() {
				t.Fatalf("composed rail exceeds viewport: %q", with)
			}
			break
		}
	}
	if !found {
		t.Fatal("no whole-block meter budget boundary observed")
	}
}
