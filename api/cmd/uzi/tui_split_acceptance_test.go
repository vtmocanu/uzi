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

func TestSplitAcceptanceDrillReturn(t *testing.T) {
	for _, origin := range []struct {
		name, key    string
		list, detail tuiView
	}{
		{"floor", keyViewFloor, viewBoard, viewDetail},
		{"ci", keyViewCI, viewCI, viewCIRun},
		{"pulls", keyViewPulls, viewPulls, viewPR},
	} {
		for _, collapse := range []string{"none", "resize", "session"} {
			t.Run(origin.name+"/"+collapse, func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m = resizeSplit(m, 120, splitMinHeight+2)
				m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
				m.board.runs = []apitypes.RunListItemDTO{
					{RunDTO: apitypes.RunDTO{ID: "first", Kind: "issue", Status: "running"}},
					{RunDTO: apitypes.RunDTO{ID: "second", Kind: "issue", Status: "running"}},
				}
				m.ci.runs = sampleCIRuns(time.Now())
				m.pulls.pulls = samplePulls(time.Now())
				m = press(t, m, origin.key)
				m = press(t, m, keyDown)
				m = press(t, m, keyEnter)
				if m.view != origin.detail || !m.fromSplit {
					t.Fatalf("drill-in: view=%v fromSplit=%v", m.view, m.fromSplit)
				}
				switch collapse {
				case "resize":
					m = resizeSplit(m, 120, splitMinHeight-1)
				case "session":
					m.splitOff = true // Session collapse while a full-screen detail is open.
				}
				m = press(t, m, keyEsc)
				want := origin.list
				if collapse != "none" {
					want = viewBoard
				}
				if m.view != want || (collapse == "none" && !m.splitDrawn()) {
					t.Fatalf("return: view=%v split=%v, want %v", m.view, m.splitDrawn(), want)
				}
				if m.board.cursor != boolInt(origin.list == viewBoard) || m.ci.cursor != boolInt(origin.list == viewCI) || m.pulls.cursor != boolInt(origin.list == viewPulls) {
					t.Fatalf("list cursors lost: floor=%d ci=%d pulls=%d", m.board.cursor, m.ci.cursor, m.pulls.cursor)
				}
				if collapse == "resize" {
					m = resizeSplit(m, 120, splitMinHeight+2)
					if !m.splitDrawn() || m.view != viewBoard || (origin.list != viewBoard && m.bottom() != origin.list) {
						t.Fatalf("regrow: view=%v bottom=%v", m.view, m.bottom())
					}
				}
			})
		}
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestSplitAcceptanceReverseKeysAndRepoScope(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = resizeSplit(m, 120, splitMinHeight+2)
	r2 := oneRepo()
	r2.ID = "r2"
	m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo(), r2}, true, true
	for _, step := range []struct {
		key          string
		want, bottom tuiView
	}{
		{"shift+tab", viewCI, viewCI},
		{"shift+tab", viewPulls, viewPulls},
		{"shift+tab", viewWorkers, viewPulls},
		{"shift+tab", viewBoard, viewPulls},
		{"ctrl+w", viewPulls, viewPulls},
		{"ctrl+w", viewBoard, viewPulls},
		{keyViewPulls, viewPulls, viewPulls},
		{keyViewFloor, viewBoard, viewPulls},
		{"ctrl+w", viewPulls, viewPulls},
		{keyViewCI, viewCI, viewCI},
	} {
		m = press(t, m, step.key)
		if m.view != step.want || m.bottom() != step.bottom {
			t.Fatalf("%q: focus=%v bottom=%v, want %v/%v", step.key, m.view, m.bottom(), step.want, step.bottom)
		}
	}
	m = press(t, m, keyViewFloor)
	m = press(t, m, keyRepoCycle)
	if m.repoIdx != 0 {
		t.Fatalf("floor changed scoped repo: %d", m.repoIdx)
	}
	m = press(t, m, keyViewCI)
	m = press(t, m, keyRepoCycle)
	if m.repoIdx != 1 {
		t.Fatalf("bottom did not cycle scoped repo: %d", m.repoIdx)
	}
}

func TestSplitAcceptanceRunPRRunReturn(t *testing.T) {
	run := apitypes.RunDTO{ID: prLinkedRunID, Kind: "issue", Status: "running", RepoID: sp("r1"), MrIID: ip(1254)}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.board.runs = []apitypes.RunListItemDTO{{RunDTO: run}}
	m = resizeSplit(m, 120, splitMinHeight+2)
	m = press(t, m, keyEnter)
	m = applyDetail(m, run, nil)
	m = press(t, m, keyPRView)
	if m.view != viewPR || !m.fromSplit {
		t.Fatalf("run to PR lost split origin: view=%v fromSplit=%v", m.view, m.fromSplit)
	}
	detail := prDetail(1254, "approved", nil, nil, apitypes.MergeStateDTO{})
	next, _ := m.Update(prMsg{reqID: m.pr.waitID, gen: m.pr.gen, detail: detail})
	m = next.(tuiModel)
	m = press(t, m, keyRunLink)
	if m.view != viewDetail || !m.fromSplit {
		t.Fatalf("PR to run lost split origin: view=%v fromSplit=%v", m.view, m.fromSplit)
	}
	for _, want := range []tuiView{viewPR, viewDetail, viewBoard} {
		m = press(t, m, keyEsc)
		if m.view != want {
			t.Fatalf("run/PR/run return: view=%v, want %v", m.view, want)
		}
	}
	if !m.splitDrawn() || m.fromSplit {
		t.Fatalf("run/PR/run return missed split floor: split=%v fromSplit=%v", m.splitDrawn(), m.fromSplit)
	}
}

func TestSplitAcceptanceMonoFocusAndUnfocusedAmber(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.board.runs = []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{
		ID: "needs-attention", Kind: "issue", Status: "awaiting_approval", IssueTitle: "approval-required-title",
	}}}
	m = resizeSplit(m, 120, splitMinHeight+2)
	m = press(t, m, keyViewCI)
	amber := toneCode(t, m.pal.amber)
	var floorRow string
	for _, line := range strings.Split(m.View().Content, "\n") {
		if strings.Contains(line, "approval-required-title") {
			floorRow = line
			break
		}
	}
	if floorRow == "" || !strings.Contains(floorRow, amber) || !strings.Contains(floorRow, "›") || strings.Contains(floorRow, "▸") {
		t.Fatalf("unfocused NEEDS YOU row lost amber or hollow cursor: %q", floorRow)
	}
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	frame := stripANSI(m.View().Content)
	if strings.Count(frame, "[ci]") != 1 || strings.Contains(frame, "[floor]") || !strings.Contains(frame, "›") {
		t.Fatalf("Ascii split lost unique focus or hollow cursor:\n%s", frame)
	}
}

func TestSplitAcceptanceHiddenTabAndModalPollGuards(t *testing.T) {
	for _, bottom := range []tuiView{viewCI, viewPulls} {
		for _, modal := range []string{"none", "help", "quit", "update", "detail"} {
			t.Run(fmt.Sprintf("%v/%s", bottom, modal), func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m = resizeSplit(m, 120, splitMinHeight+2)
				m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
				m.setListView(bottom)
				m.setListView(viewBoard)
				switch modal {
				case "help":
					m.showHelp = true
				case "quit":
					m.quitting = true
				case "update":
					m.updatePrompt.showing = true
				case "detail":
					m.view = viewDetail
				}
				next, _ := m.Update(ciTickMsg{gen: m.ci.tickGen})
				m = next.(tuiModel)
				next, _ = m.Update(pullsTickMsg{gen: m.pulls.tickGen})
				m = next.(tuiModel)
				wantPoll := modal == "none"
				if (m.ci.waitID != 0) != (wantPoll && bottom == viewCI) || (m.pulls.waitID != 0) != (wantPoll && bottom == viewPulls) {
					t.Fatalf("bottom=%v modal=%s: ci wait=%d pulls wait=%d", bottom, modal, m.ci.waitID, m.pulls.waitID)
				}
			})
		}
	}
}
