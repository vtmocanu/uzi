package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func progressDetail(t *testing.T, profile colorprofile.Profile, run apitypes.RunDTO) string {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	next, _ := m.Update(tea.ColorProfileMsg{Profile: profile})
	m = next.(tuiModel)
	m = applyDetail(m, run, nil)
	var out bytes.Buffer
	w := colorprofile.Writer{Forward: &out, Profile: profile}
	if _, err := w.WriteString(m.View().Content); err != nil {
		t.Fatal(err)
	}
	if profile != colorprofile.TrueColor && strings.Contains(out.String(), "\x1b[38") {
		t.Errorf("profile %v kept colour escapes", profile)
	}
	return stripANSI(out.String())
}

func TestDetailRailProgressVariants(t *testing.T) {
	pct := 70
	since := time.Date(2026, 10, 9, 16, 41, 0, 0, time.UTC)
	gate := time.Date(2026, 10, 9, 16, 50, 0, 0, time.UTC)
	blocker := "fca7a801-aaaa-bbbb-cccc-dddddddddddd"
	ms := []apitypes.Milestone{{ID: "m1", Title: "api-completion-receipt"}, {ID: "m2", Title: "worker-receipt-ordering"}, {ID: "m3", Title: "recovery-docs-validation"}}
	base := func(status string, p *apitypes.RunProgress) apitypes.RunDTO {
		return apitypes.RunDTO{ID: "9363c1c0-1111", Kind: "issue", Status: status, Health: "ok", IssueTitle: "t", Progress: p}
	}
	percent := base("running", &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneDone: 2, MilestoneTotal: 3, ActiveMilestoneID: "m3", Phase: "implement"})
	percent.Milestones, percent.MilestonesCompleted, percent.MilestonesInProgress = ms, []string{"m1", "m2"}, []string{"m3"}
	noActive := base("running", &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneDone: 2, MilestoneTotal: 3})
	noActive.Milestones, noActive.MilestonesCompleted = ms, []string{"m1", "m2"}
	stalled := base("running", &apitypes.RunProgress{State: "stalled"})
	stalled.HealthSince = &since
	waiting := base("awaiting_approval", &apitypes.RunProgress{State: "waiting"})
	waiting.StatusSince = &gate
	blockedQ := base("awaiting_input", &apitypes.RunProgress{State: "waiting", MaybeBlockedByRunID: &blocker})
	blockedQ.StatusSince = &gate
	parked := base("recovery_wait", &apitypes.RunProgress{State: "parked"})
	cases := []struct {
		name string
		run  apitypes.RunDTO
		want []string
		not  []string
	}{
		{"percent", percent, []string{"PROGRESS ≈70% · 2/3", "phase ▸ implement", "MILESTONES"}, []string{"milestone 3 of 3"}},
		{"no active", noActive, []string{"PROGRESS ≈70% · 2/3"}, []string{"phase ▸"}},
		{"stalled", stalled, []string{"PROGRESS ◼ stalled", "since 16:41"}, nil},
		{"plan gate", waiting, []string{"PROGRESS ● waits on you", "plan gate since 16:50"}, nil},
		{"blocked by", blockedQ, []string{"PROGRESS ● waits on you", "question since 16:50", "⧗ may be blocked by", "fca7a801"}, []string{"fca7a801-"}},
		{"parked", parked, []string{"PROGRESS ⏸ recovery wait"}, nil},
		{"queued", base("queued", &apitypes.RunProgress{State: "queued"}), []string{"PROGRESS queued"}, nil},
		{"planning", base("running", &apitypes.RunProgress{State: "planning"}), []string{"PROGRESS planning", "no milestones frozen yet"}, nil},
		{"none", base("running", &apitypes.RunProgress{State: "none"}), nil, []string{"PROGRESS"}},
		{"nil", base("running", nil), nil, []string{"PROGRESS"}},
	}
	for _, c := range cases {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
			t.Run(c.name+"/"+profile.String(), func(t *testing.T) {
				// The rail rows are interleaved with the transcript pane, so check each line's
				// rail prefix by substring on the whole frame.
				out := progressDetail(t, profile, c.run)
				for _, w := range c.want {
					if !strings.Contains(out, w) {
						t.Errorf("missing %q in:\n%s", w, out)
					}
				}
				for _, w := range c.not {
					if strings.Contains(out, w) {
						t.Errorf("unexpected %q in:\n%s", w, out)
					}
				}
			})
		}
	}
}

// The PROGRESS block's height must tip the rail's auto-fold: there is a terminal height where the
// same run stays expanded without progress and folds with it.
func TestRailAutoFoldCountsProgressBlock(t *testing.T) {
	base, messages := autoFoldRun(false, false)
	with := base
	pct := 70
	with.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneDone: 2, MilestoneTotal: 4, ActiveMilestoneID: "m3", Phase: "implement"}
	for height := 16; height <= 80; height++ {
		model := func(run apitypes.RunDTO) tuiModel {
			m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
			m.width, m.height = 100, height
			return applyDetail(m, run, messages)
		}
		plain, prog := model(base), model(with)
		if !plain.railAutoFolded(time.Now()) && prog.railAutoFolded(time.Now()) {
			out := stripANSI(prog.renderLaneRail())
			if !strings.Contains(out, "PROGRESS") || !strings.Contains(out, "MILESTONES") {
				t.Fatalf("folded rail at height %d dropped a protected block:\n%s", height, out)
			}
			return
		}
	}
	t.Fatal("no height where the PROGRESS block alone tips the auto-fold")
}

// The standard running scene (uxlab detail-running, 100x34) keeps the crew roster unfolded with
// the PROGRESS block drawn: the compact percent form must not push the rail over its budget.
func TestDetailRunningSceneKeepsCrewExpandedWithProgress(t *testing.T) {
	out := stripANSI(detailRunning(false, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)))
	for _, w := range []string{"CREW  ▾", "PROGRESS ≈70%", "MILESTONES"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// PROGRESS is not protected content: a run whose only rail block is PROGRESS (planning, no
// milestones, usage, account or worker) never auto-folds at any height.
func TestRailAutoFoldIgnoresProgressOnlyRun(t *testing.T) {
	run, messages := autoFoldRun(false, false)
	run.Milestones, run.MilestonesCompleted, run.MilestonesInProgress = nil, nil, nil
	run.Progress = &apitypes.RunProgress{State: "planning"}
	for height := 8; height <= 80; height++ {
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m.width, m.height = 100, height
		m = applyDetail(m, run, messages)
		if m.renderProgress() == "" {
			t.Fatal("fixture draws no PROGRESS block")
		}
		if m.railAutoFolded(time.Now()) {
			t.Fatalf("PROGRESS-only run auto-folded at height %d", height)
		}
	}
}

// The blocked-by hint is drawn in the wait ink, not the faint ink of the surrounding rows.
func TestDetailRailProgressBlockedByUsesWaitInk(t *testing.T) {
	blocker := "fca7a801-aaaa-bbbb-cccc-dddddddddddd"
	run := apitypes.RunDTO{ID: "r-wait", Kind: "issue", Status: "awaiting_input", Health: "ok", IssueTitle: "t",
		Progress: &apitypes.RunProgress{State: "waiting", MaybeBlockedByRunID: &blocker}}
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m = applyDetail(m, run, nil)
	raw := m.renderProgress()
	for _, w := range []string{paintSeg(m.pal.wait, nil, false, "⧗ may be blocked by"), paintSeg(m.pal.wait, nil, false, "fca7a801")} {
		if !strings.Contains(raw, w) {
			t.Errorf("hint segment not in wait ink: %q in %q", w, raw)
		}
	}
	if paintSeg(m.pal.wait, nil, false, "x") == m.pal.faint.Render("x") {
		t.Fatal("wait and faint inks are indistinguishable; the assertion proves nothing")
	}
}

// A hostile Phase and blocked-by id reach the PROGRESS block through renderer.Plain only.
func TestDetailRailProgressStripsHostileText(t *testing.T) {
	const nasty = "\x1b[2J\u202E\x07\x01"
	pct := 50
	blk := "\u202E\x07\x01\x1bidsafe"
	for _, c := range []struct {
		name, phase string
		blocked     *string
		marker      string
	}{
		{"phase", nasty + "phasesafe", nil, "phasesafe"},
		{"blocked-by", "", &blk, "idsafe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := apitypes.RunDTO{ID: "r-hostile", Kind: "issue", Status: "running", Health: "ok", IssueTitle: "t",
				Progress: &apitypes.RunProgress{State: "percent", Pct: &pct, MilestoneTotal: 1, Phase: c.phase, MaybeBlockedByRunID: c.blocked}}
			m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
			m = applyDetail(m, run, nil)
			raw := m.renderProgress()
			assertNoRawControls(t, "progress block", raw)
			if !strings.Contains(stripANSI(raw), c.marker) {
				t.Fatalf("PROGRESS block did not draw %q:\n%s", c.marker, stripANSI(raw))
			}
		})
	}
}
