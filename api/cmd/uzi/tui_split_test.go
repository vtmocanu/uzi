package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func resizeSplit(m tuiModel, width, height int) tuiModel {
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return next.(tuiModel)
}
func TestSplitThresholdAndCycle(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = resizeSplit(m, 120, splitMinHeight+1)
	if m.splitDrawn() {
		t.Fatal("split entered below hysteresis threshold")
	}
	m = resizeSplit(m, 120, splitMinHeight+2)
	if !m.splitDrawn() {
		t.Fatal("split did not enter at threshold")
	}
	m = press(t, m, keyTab)
	if m.view != viewWorkers || m.top() != viewWorkers || m.bottom() != viewCI {
		t.Fatalf("first tab: focus=%v top=%v bottom=%v", m.view, m.top(), m.bottom())
	}
	m = press(t, m, keyTab)
	if m.view != viewPulls || m.bottom() != viewPulls {
		t.Fatalf("second tab: focus=%v bottom=%v", m.view, m.bottom())
	}
	m = press(t, m, keyTab)
	if m.view != viewCI || m.bottom() != viewCI {
		t.Fatalf("third tab: focus=%v bottom=%v", m.view, m.bottom())
	}
	m = press(t, m, keyTab)
	if m.view != viewBoard || m.top() != viewBoard || m.bottom() != viewCI {
		t.Fatalf("fourth tab: focus=%v top=%v bottom=%v", m.view, m.top(), m.bottom())
	}
	m = resizeSplit(m, 120, splitMinHeight)
	if !m.splitDrawn() {
		t.Fatal("split exited at minimum")
	}
	m = resizeSplit(m, 120, splitMinHeight-1)
	if m.splitDrawn() || m.view != viewBoard {
		t.Fatal("split did not collapse to floor")
	}
	m = resizeSplit(m, 120, splitMinHeight+2)
	if !m.splitDrawn() || m.view != viewBoard || m.bottom() != viewCI {
		t.Fatal("regrow did not retain floor focus and bottom tab")
	}
	m = press(t, m, "s")
	if m.splitDrawn() || m.view != viewBoard {
		t.Fatal("s did not collapse")
	}
	m = press(t, m, keyViewCI)
	m = press(t, m, "s")
	if !m.splitDrawn() || m.view != viewCI {
		t.Fatal("restoring split lost full-screen CI focus")
	}
}
func TestSplitScrollUsesPaneCapacity(t *testing.T) {
	runs := make([]apitypes.RunListItemDTO, 30)
	for i := range runs {
		runs[i] = apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: fmt.Sprintf("run-%02d", i), Kind: "issue", Status: "running", IssueTitle: fmt.Sprintf("item-%02d", i)}}
	}
	runs[29].CurrentActivity = activityFor("coder", "last-task", time.Now())
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.board.runs = runs
	m = resizeSplit(m, 120, splitMinHeight+2)
	for i := 0; i < 29; i++ {
		m = press(t, m, keyDown)
	}
	top, _ := m.splitHeights()
	frame := strings.Split(stripANSI(m.View().Content), "\n")
	if !strings.Contains(strings.Join(frame[:len(m.splitHeader(time.Now()))+top], "\n"), "item-29") || !strings.Contains(strings.Join(frame[:len(m.splitHeader(time.Now()))+top], "\n"), "last-task") {
		t.Fatalf("selected row and second line escaped top pane (scroll=%d):\n%s", m.board.scroll, strings.Join(frame[:len(m.splitHeader(time.Now()))+top], "\n"))
	}
	if m.board.scroll == 0 {
		t.Fatal("pane scroll did not move")
	}
}
func TestSplitForgeKeyScrollKeepsSecondLine(t *testing.T) {
	now := time.Now()
	ciRows := make([]apitypes.CIRunDTO, 0, 26)
	for i := 0; i < 25; i++ {
		r := sampleCIRuns(now)[0]
		r.ID = int64(2000 + i)
		r.Number = int64(2000 + i)
		ciRows = append(ciRows, r)
	}
	ciRows = append(ciRows, sampleCIRuns(now)[4])
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	ci := loadedCI(t, fake, ciRows)
	ci = resizeSplit(ci, 120, splitMinHeight+2)
	for i := 0; i < 25; i++ {
		ci = press(t, ci, keyDown)
	}
	top, bottom := ci.splitHeights()
	lines := strings.Split(stripANSI(ci.View().Content), "\n")
	forge := strings.Join(lines[len(lines)-bottom-1:len(lines)-1], "\n")
	if !strings.Contains(forge, "chore(release): 0.77.0") || ci.ci.scroll == 0 {
		t.Fatalf("CI key scroll lost selected second line (top=%d scroll=%d):\n%s", top, ci.ci.scroll, forge)
	}
	pullsRows := make([]apitypes.PullDTO, 0, 26)
	for i := 0; i < 25; i++ {
		p := samplePulls(now)[0]
		p.IID = int64(2000 + i)
		pullsRows = append(pullsRows, p)
	}
	pullsRows = append(pullsRows, samplePulls(now)[4])
	pr := loadedPulls(t, fake, pullsRows)
	pr = resizeSplit(pr, 120, splitMinHeight+2)
	for i := 0; i < 25; i++ {
		pr = press(t, pr, keyDown)
	}
	_, bottom = pr.splitHeights()
	lines = strings.Split(stripANSI(pr.View().Content), "\n")
	forge = strings.Join(lines[len(lines)-bottom-1:len(lines)-1], "\n")
	if !strings.Contains(forge, "no conflicts with main") || pr.pulls.scroll == 0 {
		t.Fatalf("pulls key scroll lost selected second line (scroll=%d):\n%s", pr.pulls.scroll, forge)
	}
}

func TestSplitFrameFocusAndHostileRepo(t *testing.T) {
	repo := oneRepo()
	repo.PathWithNamespace = "host\x1b[2Jile/repo"
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m = resizeSplit(m, 80, splitMinHeight+2)
	m.boardReplied = true
	m.repos = []apitypes.RepoDTO{repo}
	m.reposLoaded, m.repoChosen = true, true
	m.board.admin = true
	m.showVersion = true
	m.serverVersion = "99.0.0"
	m = press(t, m, keyViewCI)
	frame := stripANSI(m.View().Content)
	if strings.Count(frame, "▚▚ uzi") != 1 || strings.Count(frame, "[ci]") != 1 || strings.Contains(frame, "[active runs]") {
		t.Fatalf("shared header or focus marker wrong:\n%s", frame)
	}
	// Plain removes the ESC control byte; its printable CSI tail remains text.
	if strings.Contains(m.View().Content, "\x1b[2J") || !strings.Contains(frame, m.renderer.Plain(repo.PathWithNamespace, 16)) {
		t.Fatalf("hostile repo bypassed renderer.Plain:\n%s", frame)
	}
	lines := strings.Split(frame, "\n")
	if len(lines) > m.height {
		t.Fatalf("split frame grew to %d lines, height %d", len(lines), m.height)
	}
	if !strings.Contains(lines[0], "active runs") || !strings.Contains(lines[len(lines)-1], "q quit") || !strings.Contains(lines[len(lines)-1], version) {
		t.Fatalf("header/footer lost admin label, quit or version:\n%s", frame)
	}
	m = press(t, m, keyViewFloor)
	frame = stripANSI(m.View().Content)
	if strings.Count(frame, "[active runs]") != 1 || strings.Contains(frame, "[ci]") {
		t.Fatalf("floor focus not unique:\n%s", frame)
	}
}

func TestSplitMinHeightShowsEightRowsInBothPanes(t *testing.T) {
	now := time.Now()
	m := bothProvidersModel(t, 100,
		[]apitypes.TokenRateLimitDTO{
			okMeter("sec-personal", "personal", true, 35, 62),
			okMeter("sec-meta", "meta", false, 88, 44),
		},
		[]apitypes.CodexAccountRateLimitDTO{
			codexAcct("cx-primary", "primary", true, "fresh", cwin(71), cwin(29)),
			codexAcct("cx-team", "team", false, "fresh", cwin(66), cwin(13)),
		})
	m = resizeSplit(m, 100, splitMinHeight+2)
	m = resizeSplit(m, 100, splitMinHeight)
	m.vaultLocked = true
	if got := len(m.boardMeterLayout(now).lines); got != 2 || m.vaultIndicatorLine() == "" {
		t.Fatalf("worst-case header precondition: meter rows=%d vault=%q", got, m.vaultIndicatorLine())
	}
	m.repos, m.reposLoaded, m.repoChosen = []apitypes.RepoDTO{oneRepo()}, true, true
	m.ci.loaded, m.pulls.loaded = true, true
	m.board.adminDenied = true
	m.board.err = fmt.Errorf("board refresh failed")
	statuses := []string{"awaiting_approval", "awaiting_input", "awaiting_followup", "running", "running", "completed", "completed", "completed"}
	for i, status := range statuses {
		m.board.runs = append(m.board.runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{
			ID: fmt.Sprintf("floor-%02d", i), Kind: "issue", Status: status,
			IssueTitle: fmt.Sprintf("floor-row-%02d", i),
		}})
	}
	m.board.runs[0].CurrentActivity = activityFor("coder", "floor-second-line", now)
	m.ci.err = fmt.Errorf("ci refresh failed")
	ciSamples := sampleCIRuns(now)
	for i := 0; i < 8; i++ {
		r := ciSamples[0]
		if i >= 3 && i < 5 {
			r = ciSamples[2]
		}
		if i >= 5 {
			r = ciSamples[3]
		}
		r.ID, r.Number = int64(3000+i), int64(3000+i)
		r.Name = fmt.Sprintf("ci-row-%02d", i)
		m.ci.runs = append(m.ci.runs, r)
	}
	for _, bottom := range []tuiView{viewCI, viewPulls} {
		if bottom == viewPulls {
			m.pulls.err = fmt.Errorf("pulls refresh failed")
			samples := samplePulls(now)
			for i := 0; i < 8; i++ {
				p := samples[0]
				if i >= 3 && i < 5 {
					p = samples[2]
				}
				if i >= 5 {
					p = samples[4]
				}
				p.IID = int64(4000 + i)
				p.Title = fmt.Sprintf("pull-row-%02d", i)
				m.pulls.pulls = append(m.pulls.pulls, p)
			}
		}
		m.bottomTab = bottom
		frame := strings.Split(stripANSI(m.View().Content), "\n")
		for _, meter := range m.boardMeterLayout(now).lines {
			if !strings.Contains(strings.Join(frame, "\n"), stripANSI(meter)) {
				t.Errorf("%v min-height frame lost meter line %q", bottom, stripANSI(meter))
			}
		}
		if !strings.Contains(strings.Join(frame, "\n"), stripANSI(m.vaultIndicatorLine())) {
			t.Errorf("%v min-height frame lost vault hint", bottom)
		}
		if len(frame) > m.height {
			t.Fatalf("%v frame has %d lines, height %d", bottom, len(frame), m.height)
		}
		topHeight, bottomHeight := m.splitHeights()
		floor := strings.Join(frame[len(frame)-topHeight-bottomHeight-2:len(frame)-bottomHeight-2], "\n")
		forge := strings.Join(frame[len(frame)-bottomHeight-1:len(frame)-1], "\n")
		for _, heading := range []string{"NEEDS YOU", "ON THE FLOOR", "DONE"} {
			if !strings.Contains(floor, heading) {
				t.Errorf("floor missing %q band", heading)
			}
		}
		if !strings.Contains(floor, "floor-second-line") {
			t.Error("floor selected second line missing")
		}
		forgeBands := []string{"RUNNING", "FAILED", "RECENT"}
		if bottom == viewPulls {
			forgeBands = []string{"NEEDS YOU", "IN FLIGHT", "READY"}
		}
		for _, heading := range forgeBands {
			if !strings.Contains(forge, heading) {
				t.Errorf("%v forge missing %q band", bottom, heading)
			}
		}
		for i := 0; i < 8; i++ {
			if !strings.Contains(floor, fmt.Sprintf("floor-row-%02d", i)) {
				t.Errorf("%v floor lost data row %d:\n%s", bottom, i, floor)
			}
			want := fmt.Sprintf("#%d", 3000+i)
			if bottom == viewPulls {
				want = fmt.Sprintf("pull-row-%02d", i)
			}
			if !strings.Contains(forge, want) {
				t.Errorf("%v forge lost data row %d:\n%s", bottom, i, forge)
			}
		}
	}
}

func TestSplitActivationOnlyOnFirstBoardReply(t *testing.T) {
	for _, order := range []string{"repos-first", "board-first", "board-failed"} {
		t.Run(order, func(t *testing.T) {
			first, second := oneRepo(), oneRepo()
			second.ID, second.PathWithNamespace = "r2", "example/second"
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m = resizeSplit(m, 120, splitMinHeight+2)
			runs := []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "latest", RepoID: sp(second.ID), Status: "running", CreatedAt: time.Now()}}}
			board := boardRunsMsg{reqID: m.board.waitID, runs: runs}
			if order == "board-failed" {
				board.err = fmt.Errorf("first board failed")
				board.runs = nil
			}
			repos := reposMsg{repos: []apitypes.RepoDTO{first, second}}
			if order == "repos-first" {
				next, _ := m.Update(repos)
				m = next.(tuiModel)
				if m.repoChosen {
					t.Fatal("repo selected before board reply")
				}
			}
			next, _ := m.Update(board)
			m = next.(tuiModel)
			if order != "repos-first" {
				next, _ = m.Update(repos)
				m = next.(tuiModel)
			}
			wantRepo := second.ID
			if order == "board-failed" {
				wantRepo = first.ID
			}
			if repo, ok := m.currentRepo(); !ok || repo.ID != wantRepo || m.ci.waitID == 0 {
				t.Fatalf("initial activation: repo=%+v ready=%v ci wait=%d", repo, ok, m.ci.waitID)
			}
			initialSeq := m.ci.reqSeq
			next, _ = m.Update(ciMsg{reqID: m.ci.waitID})
			m = next.(tuiModel)
			_ = m.startBoardReq()
			next, _ = m.Update(boardRunsMsg{reqID: m.board.waitID, runs: runs})
			m = next.(tuiModel)
			if m.ci.waitID != 0 || m.ci.reqSeq != initialSeq {
				t.Fatalf("later board poll issued extra CI fetch: wait=%d seq=%d, initial=%d", m.ci.waitID, m.ci.reqSeq, initialSeq)
			}
		})
	}
}

func TestSplitDisplayedPollAndModalPause(t *testing.T) {
	fake := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{oneRepo()}}
	m := tuiTestModel(t, fake, "")
	m = resizeSplit(m, 120, splitMinHeight+2)
	m = idleBoard(t, m, nil)
	next, _ := m.Update(reposMsg{repos: fake.Repos})
	m = next.(tuiModel)
	if !m.repoChosen || m.ci.waitID == 0 {
		t.Fatal("floor split did not select repo and fetch CI")
	}
	m.ci.waitID = 0
	next, cmd := m.Update(ciTickMsg{gen: m.ci.tickGen})
	m = next.(tuiModel)
	if cmd == nil || m.ci.waitID == 0 {
		t.Fatal("displayed CI did not poll")
	}
	m.ci.waitID = 0
	m.showHelp = true
	next, cmd = m.Update(ciTickMsg{gen: m.ci.tickGen})
	m = next.(tuiModel)
	if cmd == nil || m.ci.waitID != 0 {
		t.Fatal("help modal polled CI")
	}
}

func TestSplitResizeUnderModalCommitsFocusedFilter(t *testing.T) {
	for _, modal := range []string{"help", "quit", "update"} {
		for _, bottom := range []tuiView{viewCI, viewPulls} {
			t.Run(fmt.Sprintf("%s/%v", modal, bottom), func(t *testing.T) {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m = resizeSplit(m, 120, splitMinHeight+2)
				m.setListView(bottom)
				if bottom == viewCI {
					m.ci.filter, m.ci.filtering = "pending", true
				} else {
					m.pulls.filter, m.pulls.filtering = "pending", true
				}
				switch modal {
				case "help":
					m.showHelp = true
				case "quit":
					m.quitting = true
				case "update":
					m.updatePrompt.showing = true
				}
				m = resizeSplit(m, 120, splitMinHeight-1)
				if m.view != viewBoard || m.splitLatch {
					t.Fatalf("resize kept bottom focus: view=%v latch=%v", m.view, m.splitLatch)
				}
				if bottom == viewCI && (m.ci.filtering || m.ci.filter != "pending") {
					t.Fatalf("CI filter was not committed: %+v", m.ci)
				}
				if bottom == viewPulls && (m.pulls.filtering || m.pulls.filter != "pending") {
					t.Fatalf("pulls filter was not committed: %+v", m.pulls)
				}
			})
		}
	}
}

func TestSplitFirstRepliesUnderModalActivateOnDismiss(t *testing.T) {
	for _, modal := range []string{"help", "update", "quit"} {
		for _, order := range []string{"repos-first", "board-first", "board-failed"} {
			for _, bottom := range []tuiView{viewCI, viewPulls} {
				t.Run(fmt.Sprintf("%s/%s/%v", modal, order, bottom), func(t *testing.T) {
					first, second := oneRepo(), oneRepo()
					second.ID, second.PathWithNamespace = "r2", "example/second"
					m := tuiTestModel(t, &uzicli.FakeClient{}, "")
					m = resizeSplit(m, 120, splitMinHeight+2)
					m.bottomTab = bottom
					switch modal {
					case "help":
						m.showHelp = true
					case "update":
						m.updatePrompt.showing = true
					case "quit":
						m.quitting = true
					}
					runs := []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{
						ID: "latest", RepoID: sp(second.ID), Status: "running", CreatedAt: time.Now(),
					}}}
					deliverRepos := func() {
						next, _ := m.Update(reposMsg{repos: []apitypes.RepoDTO{first, second}})
						m = next.(tuiModel)
					}
					deliverBoard := func() {
						msg := boardRunsMsg{reqID: m.board.waitID, runs: runs}
						if order == "board-failed" {
							msg.err, msg.runs = fmt.Errorf("first board failed"), nil
						}
						next, _ := m.Update(msg)
						m = next.(tuiModel)
					}
					if order == "repos-first" {
						deliverRepos()
						if m.repoChosen {
							t.Fatal("repo chosen before first board reply")
						}
						deliverBoard()
					} else {
						deliverBoard()
						deliverRepos()
					}
					if m.repoChosen || m.ci.waitID != 0 || m.pulls.waitID != 0 {
						t.Fatalf("modal fetched early: chosen=%v ci=%d pulls=%d", m.repoChosen, m.ci.waitID, m.pulls.waitID)
					}
					m = press(t, m, keyEsc)
					repo, ok := m.currentRepo()
					wantRepo := second.ID
					if order == "board-failed" {
						wantRepo = first.ID
						if m.board.errStreak != 1 {
							t.Fatalf("failed board reply lost retry streak: %d", m.board.errStreak)
						}
					}
					if !ok || repo.ID != wantRepo || !m.boardReplied || m.board.waitID != 0 {
						t.Fatalf("dismiss did not resolve board repo: repo=%+v ok=%v replied=%v wait=%d", repo, ok, m.boardReplied, m.board.waitID)
					}
					wait := m.ci.waitID
					if bottom == viewPulls {
						wait = m.pulls.waitID
					}
					if wait == 0 || !m.splitDrawn() {
						t.Fatalf("dismiss did not fetch displayed bottom: bottom=%v wait=%d", bottom, wait)
					}
					m = press(t, m, keyEsc)
					if bottom == viewCI && m.ci.reqSeq != 1 || bottom == viewPulls && m.pulls.reqSeq != 1 {
						t.Fatal("dismissal issued duplicate forge fetch")
					}
				})
			}
		}
	}
}

func TestSplitFilterReadoutOnlyInSeparator(t *testing.T) {
	for _, bottom := range []tuiView{viewCI, viewPulls} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m = resizeSplit(m, 120, splitMinHeight+2)
		m.setListView(bottom)
		if bottom == viewCI {
			m.ci.filter, m.ci.filtering = "needle", true
		} else {
			m.pulls.filter, m.pulls.filtering = "needle", true
		}
		frame := stripANSI(m.View().Content)
		if got := strings.Count(frame, "/needle"); got != 1 {
			t.Fatalf("%v split readout count=%d:\n%s", bottom, got, frame)
		}
		if !strings.Contains(stripANSI(m.splitSeparatorLine()), "/needle") {
			t.Fatalf("%v separator lost filter", bottom)
		}
		m.splitOff = true
		if !strings.Contains(stripANSI(m.View().Content), "/needle") {
			t.Fatalf("%v full-screen lost filter", bottom)
		}
	}
}

// splitPRFromDetail drills split floor → run detail → m (PR view), then collapses the split, so
// the PR view's esc can no longer return to the run view it was opened from.
func splitPRFromDetail(t *testing.T) (tuiModel, apitypes.RunDTO) {
	t.Helper()
	run := apitypes.RunDTO{ID: prLinkedRunID, Kind: "issue", Status: "running", RepoID: sp("r1"), MrIID: ip(1254)}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.board.runs = []apitypes.RunListItemDTO{{RunDTO: run}}
	m = resizeSplit(m, 120, splitMinHeight+2)
	m = press(t, m, keyEnter)
	m = applyDetail(m, run, nil)
	return m, run
}

func TestSplitCollapsedPRReturnClosesRunStream(t *testing.T) {
	m, run := splitPRFromDetail(t)
	own := streamOf(t, m.openStreamCmd(run.ID))
	next, _ := m.Update(own)
	m = next.(tuiModel)
	m = press(t, m, keyPRView)
	m = resizeSplit(m, 120, splitMinHeight-1)
	m = press(t, m, keyEsc)
	if m.view != viewBoard {
		t.Fatalf("collapsed PR esc returned to view=%v, want the board", m.view)
	}
	requireStreamClosed(t, own.stream, "the run stream left behind by a collapsed PR return")
	if m.detail.runID != "" {
		t.Fatalf("collapsed PR return kept the run detail session %q", m.detail.runID)
	}
}

func TestSplitCollapsedPRReturnClosesLateRunStream(t *testing.T) {
	m, run := splitPRFromDetail(t)
	late := streamOf(t, m.openStreamCmd(run.ID)) // opened before the exit, delivered after it
	m = press(t, m, keyPRView)
	m = resizeSplit(m, 120, splitMinHeight-1)
	m = press(t, m, keyEsc)
	next, _ := m.Update(late)
	m = next.(tuiModel)
	if m.detail.stream != nil {
		t.Fatalf("a stream arriving after the collapsed PR return was adopted")
	}
	requireStreamClosed(t, late.stream, "a run stream arriving after the collapsed PR return")
}

func TestSplitCIEmptySceneIsEmpty(t *testing.T) {
	frame := stripANSI(splitScene(true, time.Now(), "ci-empty"))
	if !strings.Contains(frame, "No CI runs yet") {
		t.Fatalf("the split-ci-empty scene renders CI runs instead of the empty state:\n%s", frame)
	}
}
