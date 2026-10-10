package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestSplitReviewFooterNoteYieldsToExistingFrame(t *testing.T) {
	oldVersion := version
	version = "v0.84.0"
	t.Cleanup(func() { version = oldVersion })
	for _, tc := range []struct {
		width, height   int
		collapsed, skew bool
	}{
		{80, 60, true, false}, {80, 34, false, false},
		{79, 60, false, false}, {100, 30, false, false},
		{100, 60, true, true}, {100, 30, false, true},
		{80, 60, true, true}, {80, 34, false, true},
	} {
		t.Run(fmt.Sprintf("%dx%d/collapsed=%v/skew=%v", tc.width, tc.height, tc.collapsed, tc.skew), func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.showVersion, m.serverVersion = tc.skew, "0.85.0"
			m = resizeSplit(m, tc.width, tc.height)
			m = press(t, m, "s")
			control := m
			control.splitMode = "off"
			control.splitNote, control.splitOff = "", false
			if got, want := m.View().Content, control.View().Content; got != want {
				t.Errorf("note changed full-screen frame:\n%s\nwant:\n%s", stripANSI(got), stripANSI(want))
			}
			frame := strings.Split(stripANSI(m.View().Content), "\n")
			footer := frame[len(frame)-1]
			if tc.skew && tc.width >= 100 && !strings.Contains(footer, "⇢ 0.85.0") {
				t.Errorf("footer lost skew cue: %q", footer)
			}
			if visualWidth(footer) > tc.width {
				t.Errorf("footer overflow: %q", footer)
			}
		})
	}
	for _, view := range []tuiView{viewCI, viewPulls} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m = resizeSplit(m, 80, 60)
		m = press(t, m, "s")
		m.setListView(view)
		m.pulls.pulls = samplePulls(time.Now())
		m.repos = []apitypes.RepoDTO{oneRepo(), oneRepo()}
		control := m
		control.splitMode = "off"
		control.splitNote, control.splitOff = "", false
		if got, want := m.View().Content, control.View().Content; got != want {
			// Shedding can leave room for the restore note. It must not shed
			// another hint to make room: the entire fitted legend survives.
			gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
			footer := gotLines[len(gotLines)-1]
			legend := wantLines[len(wantLines)-1]
			if !strings.Contains(stripANSI(footer), "s split") ||
				!strings.HasSuffix(footer, strings.TrimPrefix(legend, " ")) ||
				strings.Join(gotLines[:len(gotLines)-1], "\n") != strings.Join(wantLines[:len(wantLines)-1], "\n") {
				t.Errorf("%v note changed fitted hints or body", view)
			}
		}
	}
	// Every list's footer lets a split note yield to the existing footer.
	for _, view := range []tuiView{viewBoard, viewCI, viewPulls} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m = resizeSplit(m, 100, 30)
		m.setListView(view)
		before := strings.Split(m.View().Content, "\n")
		m = press(t, m, "s")
		after := strings.Split(m.View().Content, "\n")
		if after[len(after)-1] != before[len(before)-1] {
			t.Errorf("%v overlong note changed footer", view)
		}
	}
	// Config-off is exactly the original full-screen frame, even with stale note state.
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = resizeSplit(m, 100, 60)
	want := m.View().Content
	m.splitNote = "terminal too small to split"
	if got := m.View().Content; got != want {
		t.Error("config-off frame changed for a split note")
	}
	// A note is still useful when it fits; the split's own footer keeps the exit keys.
	m = tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = resizeSplit(m, 180, 30)
	m = press(t, m, "s")
	if !strings.Contains(stripANSI(m.View().Content), "terminal too small to split") {
		t.Error("a fitting size note was dropped")
	}
	m = resizeSplit(m, 180, 60)
	m = press(t, m, "s")
	if !strings.Contains(stripANSI(m.View().Content), "s split") {
		t.Error("a fitting restore hint was dropped")
	}
	m = press(t, m, "s")
	m = resizeSplit(m, 80, 60)
	for _, view := range []tuiView{viewBoard, viewCI, viewPulls} {
		m.setListView(view)
		footer := stripANSI(m.splitFooterLine())
		if !strings.Contains(footer, "? keys") || !strings.Contains(footer, "q quit") {
			t.Errorf("%v split footer lost exit keys: %q", view, footer)
		}
	}
}

func TestSplitReviewSecondLinesRespectFocus(t *testing.T) {
	now := time.Now()
	activity := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "activity", Kind: "issue", Status: "running", CurrentActivity: activityFor("coder", "unique-step", now)}}
	credential := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "credential", Kind: "issue", Status: statusPaused, HoldReason: sp(holdCredentialDisabled)}}
	vault := apitypes.RunListItemDTO{RunDTO: vaultParkRun("vault")}
	disk := vault
	disk.ID, disk.RecoveryWaitCause = "disk", sp(dataVolumeFullCause)
	codex := apitypes.RunListItemDTO{RunDTO: codexHoldRun(sp("relogin_required"), sp("personal"))}
	for _, dark := range []bool{true, false} {
		for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii, colorprofile.NoTTY} {
			for _, tc := range []struct {
				name string
				pane tuiView
				run  apitypes.RunListItemDTO
			}{
				{"board/activity", viewBoard, activity}, {"board/credential", viewBoard, credential},
				{"board/vault", viewBoard, vault}, {"board/disk", viewBoard, disk}, {"board/codex", viewBoard, codex},
				{"ci", viewCI, activity}, {"pulls", viewPulls, activity},
			} {
				t.Run(fmt.Sprintf("%s/dark=%v/profile=%v", tc.name, dark, profile), func(t *testing.T) {
					m := tuiTestModel(t, &uzicli.FakeClient{}, "")
					m = resizeSplit(m, 120, 60)
					m.dark, m.pal = dark, newPalette(dark)
					m.renderer, _ = newTUIRenderer(m.transcriptWidth(), dark)
					m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
					m.board.runs = []apitypes.RunListItemDTO{tc.run}
					m.ci.runs, m.pulls.pulls = sampleCIRuns(now), samplePulls(now)
					m.setListView(tc.pane)
					line := func() string {
						switch tc.pane {
						case viewCI:
							return m.ciSecondLine(m.ci.runs[0])
						case viewPulls:
							return m.pullSecondLine(m.pulls.pulls[0])
						default:
							return m.boardSecondLine(tc.run)
						}
					}
					focused := line()
					m.splitOff = true
					if got := line(); got != focused {
						t.Error("focused second line differs from full-screen")
					}
					m.splitOff = false
					if tc.pane == viewBoard {
						m.setListView(viewCI)
					} else {
						m.setListView(viewBoard)
					}
					next, _ := m.Update(tea.ColorProfileMsg{Profile: profile})
					m = next.(tuiModel)
					unfocused := line()
					if unfocused == "" {
						t.Fatal("fixture has no second line")
					}
					if strings.Contains(unfocused, bgFillSGR(m.pal.selBg)) || strings.Contains(unfocused, "▸") || !strings.Contains(unfocused, "›") {
						t.Errorf("unfocused second line retained selection: %q", unfocused)
					}
					if profile == colorprofile.TrueColor && !strings.Contains(m.View().Content, unfocused) {
						t.Error("second line missing from split frame")
					}
				})
			}
		}
	}
}

func TestSplitReviewVersionedFooterShowsFittingNote(t *testing.T) {
	oldVersion := version
	version = "v0.84.0"
	t.Cleanup(func() { version = oldVersion })
	for _, tc := range []struct {
		width, height int
		server, note  string
		fullReadout   bool
	}{
		{120, 60, "0.85.0", "s split", true},
		{180, 30, "0.85.0", "terminal too small to split", true},
		{120, 60, "0.99999999999999999999999999999999999999999999999999.0", "s split", false},
	} {
		t.Run(fmt.Sprintf("%dx%d/full=%v", tc.width, tc.height, tc.fullReadout), func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.showVersion, m.serverVersion = true, tc.server
			m = resizeSplit(m, tc.width, tc.height)
			m = press(t, m, "s")
			fullFits := visualWidth(m.boardFooter())+1+visualWidth(m.versionReadout()) <= m.width
			if fullFits != tc.fullReadout {
				t.Fatal("fixture did not select the expected readout")
			}
			lines := strings.Split(stripANSI(m.View().Content), "\n")
			footer := lines[len(lines)-1]
			for _, want := range []string{tc.note, "q quit", "v0.84.0"} {
				if !strings.Contains(footer, want) {
					t.Errorf("footer lost %q: %q", want, footer)
				}
			}
			if tc.fullReadout && !strings.Contains(footer, "⇢ 0.85.0") {
				t.Errorf("footer lost full skew cue: %q", footer)
			}
			if visualWidth(footer) > m.width {
				t.Errorf("footer overflow: %q", footer)
			}
		})
	}
}

func TestSplitReviewHelpHasOneAlignedNavigationEntry(t *testing.T) {
	for _, view := range []tuiView{viewBoard, viewCI, viewPulls} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.setListView(view)
		unsplit := m.renderHelp()
		m = resizeSplit(m, 120, 60)
		lines := strings.Split(stripANSI(m.renderHelp()), "\n")
		tabs := 0
		for i, line := range lines {
			if strings.HasPrefix(line, "tab ") {
				tabs++
				if line != "tab        cycle floor, workers, pulls, ci" {
					t.Errorf("wrong tab help: %q", line)
				}
				if view == viewBoard && (i+1 == len(lines) || lines[i+1] != "1/2 top floor/workers · 3/4 bottom pulls/ci") {
					t.Error("board numbered navigation is not grouped after tab")
				}
			}
			for _, key := range []string{"shift+tab", "ctrl+w", "s"} {
				if strings.HasPrefix(line, key+" ") && len(line)-len(strings.TrimLeft(line[len(key):], " ")) != 11 {
					t.Errorf("misaligned key column: %q", line)
				}
			}
		}
		if tabs != 1 {
			t.Errorf("%v help has %d tab entries", view, tabs)
		}
		for _, want := range []string{"shift+tab  cycle ci, pulls, workers, floor", "ctrl+w     switch pane focus", "s          collapse / restore split"} {
			if !strings.Contains(strings.Join(lines, "\n"), want) {
				t.Errorf("%v help missing %q", view, want)
			}
		}
		for _, mode := range []string{"off", "auto"} {
			m.splitMode = mode
			if mode == "auto" {
				m = resizeSplit(m, 79, 60)
				m.setListView(view)
			}
			if got := m.renderHelp(); got != unsplit {
				t.Errorf("%v %s unsplit help changed", view, mode)
			}
		}
	}
}

func TestSplitReviewSeparatorFitsRepoToAvailableWidth(t *testing.T) {
	repo := oneRepo()
	repo.PathWithNamespace = "example/long-repository-name-for-split-view"
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = resizeSplit(m, 120, 60)
	m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{repo}, true, true
	if got := stripANSI(m.splitSeparatorLine()); !strings.Contains(got, repo.PathWithNamespace) {
		t.Errorf("wide separator truncated repo: %q", got)
	}
	// Keep 20 columns after tabs, filter, summary and the repo's three-column gap.
	m.width = 80
	base := " pulls · ci   /"
	summary := stripANSI(m.ciSummary())
	m.ci.filter = strings.Repeat("f", m.width-20-visualWidth(base)-3-visualWidth(summary)-3)
	if got, want := stripANSI(m.splitSeparatorLine()), m.renderer.Plain(repo.PathWithNamespace, 20); !strings.Contains(got, want) || visualWidth(got) != m.width {
		t.Errorf("pressured repo: want %q in %q", want, got)
	}
	m.ci.filter += strings.Repeat("f", 5)
	if got := stripANSI(m.splitSeparatorLine()); strings.Contains(got, "example/") || !strings.Contains(got, "pulls · ci") {
		t.Errorf("repo below 16-column floor not dropped: %q", got)
	}
	repo.PathWithNamespace = "example/\x1b[2J" + strings.Repeat("界", 30)
	m.repos[0] = repo
	m.ci.filter = ""
	got := m.splitSeparatorLine()
	if strings.Contains(got, "\x1b[2J") || visualWidth(got) > m.width || !strings.Contains(got, "example/") {
		t.Errorf("unsafe or overflowing wide repo: %q", got)
	}
}

func TestSplitReviewNeedsYouSceneDistinct(t *testing.T) {
	now := time.Now()
	for _, dark := range []bool{true, false} {
		frame := splitScene(dark, now, "needs-you")
		if frame == splitScene(dark, now, "ci") || !strings.Contains(stripANSI(frame), "Review the plan before approval") {
			t.Errorf("needs-you scene has no distinct waiting activity")
		}
	}
}
