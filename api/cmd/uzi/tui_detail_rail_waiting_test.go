package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// Issue #2705: the crew rail explains a queued run held for its returning worker, with the
// deadline, under the worker line.

const pinReasonFixture = "waiting until 2026-07-12T12:19:00Z for its previous worker w1 to return; another worker may take it after that"

// waitingRailModel is a queued, waiting_worker run with the pin reason on the crew rail.
func waitingRailModel(t *testing.T, mutate func(m *tuiModel)) tuiModel {
	t.Helper()
	m := codexFloorRunAt(t, 40, false)
	m.detail.run.Status = "queued"
	m.detail.run.Health = "waiting_worker"
	m.detail.run.HealthReason = sp(pinReasonFixture)
	if mutate != nil {
		mutate(&m)
	}
	return m
}

func TestRailWaitingLineShowsDeadlineForPinnedQueuedRun(t *testing.T) {
	m := waitingRailModel(t, nil)
	rail := stripANSI(m.renderLaneRail())
	if !strings.Contains(rail, "queued ▸ waiting until") || !strings.Contains(rail, "2026-07-12T12:19:00Z") {
		t.Fatalf("rail lacks the waiting label and deadline:\n%s", rail)
	}
	// Wrapped to at most railWaitingMaxRows rows within laneRailWidth, directly under the title
	// row, the deadline kept whole and the tail ellipsized.
	lines := strings.Split(rail, "\n")
	want := []string{"queued ▸ waiting until", "2026-07-12T12:19:00Z for", "its previous worker w1 to…"}
	if len(want) != railWaitingMaxRows {
		t.Fatalf("fixture rows %d != railWaitingMaxRows %d", len(want), railWaitingMaxRows)
	}
	for i, w := range want {
		if lines[1+i] != w || visualWidth(lines[1+i]) > laneRailWidth {
			t.Fatalf("rail row %d = %q, want %q:\n%s", 1+i, lines[1+i], w, rail)
		}
	}
	if strings.Contains(lines[1+len(want)], "waiting") {
		t.Fatalf("waiting block exceeds %d rows:\n%s", railWaitingMaxRows, rail)
	}
	if out := stripANSI(m.View().Content); !strings.Contains(out, "2026-07-12T12:19:00Z") {
		t.Fatalf("the composed view lacks the deadline:\n%s", out)
	}
}

func TestRailWaitingLineAbsentUnlessQueuedWaiting(t *testing.T) {
	cases := map[string]func(m *tuiModel){
		"running":      func(m *tuiModel) { m.detail.run.Status = "running" },
		"healthy":      func(m *tuiModel) { m.detail.run.Health = "ok" },
		"no reason":    func(m *tuiModel) { m.detail.run.HealthReason = nil },
		"empty reason": func(m *tuiModel) { m.detail.run.HealthReason = sp("") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := waitingRailModel(t, mutate)
			if out := stripANSI(m.renderLaneRail()); strings.Contains(out, "queued ▸") || strings.Contains(out, "2026-07-12") {
				t.Fatalf("waiting line drawn:\n%s", out)
			}
		})
	}
}

// The line is plain text with a glyph label, so the Ascii and NoTTY colour profiles keep it.
func TestRailWaitingLineSurvivesColorDowngrade(t *testing.T) {
	for _, prof := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
		m := waitingRailModel(t, nil)
		next, _ := m.Update(tea.ColorProfileMsg{Profile: prof})
		m = next.(tuiModel)
		out := stripANSI(m.View().Content)
		if !strings.Contains(out, "queued ▸") || !strings.Contains(out, "2026-07-12T12:19:00Z") {
			t.Fatalf("profile %v dropped the waiting line:\n%s", prof, out)
		}
	}
}

// railAutoFolded charges the waiting rows to the fold budget: somewhere in the height sweep the
// rail folds only because of this line.
func TestRailWaitingLineChargedToFoldBudget(t *testing.T) {
	decided := false
	for h := 20; h <= 60; h++ {
		control := codexFloorRunAt(t, h, true)
		control.detail.run.Status, control.detail.run.Health = "queued", "waiting_worker"
		with := control
		with.detail.run.HealthReason = sp(pinReasonFixture)
		if !control.effectiveRailFolded(time.Now()) && with.effectiveRailFolded(time.Now()) {
			decided = true
			break
		}
	}
	if !decided {
		t.Fatal("the waiting rows never decided the auto-fold")
	}
}
