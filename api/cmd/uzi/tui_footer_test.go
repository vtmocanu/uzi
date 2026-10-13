package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestFooterRenderedProtectedKeys(t *testing.T) {
	withVersion(t, "v0.84.0")
	for _, width := range []int{80, 81, 100, 120, 240} {
		for _, height := range []int{34, 60} {
			for _, view := range []tuiView{viewBoard, viewCI, viewPulls} {
				for _, mode := range []string{"ordinary", "folded", "admin", "subscription", "unreported", "linked"} {
					for _, server := range []string{"off", "0.84.0", "0.85.0"} {
						t.Run(fmt.Sprintf("%d/%d/%v/%s/%s", width, height, view, mode, server), func(t *testing.T) {
							m := tuiTestModel(t, &uzicli.FakeClient{}, "")
							m.width, m.height, m.splitMode = width, height, "off"
							m.repos = []apitypes.RepoDTO{oneRepo(), oneRepo()}
							m.reposLoaded = true
							m.setListView(view)
							m.showVersion, m.serverVersion = server != "off", server
							m.board.hideDone = mode == "folded"
							m.board.admin = mode == "admin"
							m.selfUsageReady = true
							m.selfUsage.Last7SubscriptionRunCount = 0
							if mode == "subscription" {
								m.selfUsage.Last7SubscriptionRunCount = 1
							}
							if mode == "unreported" {
								m.selfUsage.Last7UnreportedRunCount = 1
							}
							if mode == "linked" {
								id := "run"
								m.pulls.pulls = []apitypes.PullDTO{{RunID: &id}}
							}
							lines := strings.Split(m.View().Content, "\n")
							footer := lines[len(lines)-1]
							plain := stripANSI(footer)
							for _, want := range []string{"? keys", "q quit"} {
								if !strings.Contains(plain, want) {
									t.Errorf("lost %q: %q", want, plain)
								}
							}
							if visualWidth(footer) > width {
								t.Errorf("overflow: %q", plain)
							}
							if view == viewBoard && (mode == "subscription" || mode == "unreported") && !strings.Contains(plain, "+ partial") {
								t.Errorf("lost cost cue: %q", plain)
							}
							if view == viewPulls && mode == "linked" {
								for _, want := range []string{"u run", "w rework", "f fix ci"} {
									if !strings.Contains(plain, want) {
										t.Errorf("lost action %q: %q", want, plain)
									}
								}
							}
						})
					}
				}
			}
		}
	}
}

func TestFooterInstalledEssentialBudget(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
		for _, marked := range []bool{false, true} {
			for _, installed := range []string{"v0.86.0", "v0.86.0+" + strings.Repeat("a", 24), strings.Repeat("a", 100)} {
				m := tuiTestModel(t, &uzicli.FakeClient{}, "")
				m.width, m.splitMode, m.showVersion, m.profile = 80, "off", true, profile
				m.updatePrompt.installedVersion = installed
				m.selfUsageReady = true
				if marked {
					m.selfUsage.Last7UnreportedRunCount = 1
				}
				lines := strings.Split(m.View().Content, "\n")
				got := stripANSI(lines[len(lines)-1])
				for _, want := range []string{"? keys", "q quit", "restart uzi", "enter/→ open"} {
					if !strings.Contains(got, want) {
						t.Errorf("installed %q marked=%v lost %q: %q", installed, marked, want, got)
					}
				}
				if marked && !strings.Contains(got, "+ partial") {
					t.Errorf("lost partial: %q", got)
				}
				if visualWidth(got) > 80 {
					t.Errorf("overflow: %q", got)
				}
				if installed != "v0.86.0" && strings.Contains(got, "installed,") {
					t.Errorf("expected compact banner: %q", got)
				}
				if installed == "v0.86.0" && !marked && !strings.Contains(got, "v0.86.0 installed, restart uzi to use it") {
					t.Errorf("short full banner lost: %q", got)
				}
				m.width = 240
				wide := stripANSI(m.boardFooterLine())
				if installed != "v0.86.0" && len(installed) < 80 && !strings.Contains(wide, installed+" installed, restart uzi to use it") {
					t.Errorf("wide banner lost: %q", wide)
				}
				m.width = 80
				split := m.splitFooterLine()
				if !strings.Contains(stripANSI(split), "? keys · q quit") {
					t.Errorf("split lost keys: %q", split)
				}
			}
		}
	}
}

func TestFooterOversizedClient(t *testing.T) {
	withVersion(t, "v0.84.0+"+strings.Repeat("a", 160))
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.splitMode, m.showVersion = 80, "off", true
	m.selfUsageReady = true
	m.selfUsage.Last7SubscriptionRunCount = 1
	lines := strings.Split(m.View().Content, "\n")
	got := lines[len(lines)-1]
	for _, want := range []string{"? keys", "q quit", "+ partial", "enter/→ open"} {
		if !strings.Contains(stripANSI(got), want) {
			t.Errorf("lost %q: %q", want, stripANSI(got))
		}
	}
	if visualWidth(got) > 80 {
		t.Errorf("overflow: %q", stripANSI(got))
	}
}

func TestFooterBoardSheddingThresholds(t *testing.T) {
	for _, tc := range []struct {
		stampWidth        int
		removed, retained []string
	}{
		{11, []string{"r refresh"}, []string{"h fold done", "a factory", "/ filter"}},
		{12, []string{"r refresh", "h fold done"}, []string{"a factory", "/ filter"}},
		{26, []string{"r refresh", "h fold done", "a factory"}, []string{"/ filter"}},
		{38, []string{"r refresh", "h fold done", "a factory", "/ filter"}, []string{"enter/→ open"}},
	} {
		t.Run(fmt.Sprint(tc.stampWidth), func(t *testing.T) {
			withVersion(t, strings.Repeat("c", tc.stampWidth))
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.width, m.splitMode, m.showVersion = 80, "off", true
			got := m.boardFooterLine()
			plain := stripANSI(got)
			for _, hint := range tc.removed {
				if strings.Contains(plain, hint) {
					t.Errorf("retained %q: %q", hint, plain)
				}
			}
			for _, hint := range append(tc.retained, "? keys", "q quit") {
				if !strings.Contains(plain, hint) {
					t.Errorf("lost %q: %q", hint, plain)
				}
			}
			if !strings.HasSuffix(plain, version) || visualWidth(got) != 80 {
				t.Errorf("suffix not aligned: %q", plain)
			}
			if strings.Contains(plain, "⇢") {
				t.Errorf("unexpected full readout: %q", plain)
			}
		})
	}
}

func TestFooterPullsSheddingThresholds(t *testing.T) {
	for _, tc := range []struct {
		width             int
		removed, retained []string
	}{
		{113, nil, []string{"r refresh", "/ filter", "R repo", "tab views"}},
		{112, []string{"r refresh"}, []string{"/ filter", "R repo", "tab views"}},
		{101, []string{"r refresh"}, []string{"/ filter", "R repo", "tab views"}},
		{100, []string{"r refresh", "/ filter"}, []string{"R repo", "tab views"}},
		{90, []string{"r refresh", "/ filter"}, []string{"R repo", "tab views"}},
		{89, []string{"r refresh", "/ filter", "R repo"}, []string{"tab views"}},
		{81, []string{"r refresh", "/ filter", "R repo"}, []string{"tab views"}},
		{80, []string{"r refresh", "/ filter", "R repo", "tab views"}, nil},
	} {
		t.Run(fmt.Sprint(tc.width), func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.width, m.splitMode = tc.width, "off"
			m.repos = []apitypes.RepoDTO{oneRepo(), oneRepo()}
			id := "run"
			m.pulls.pulls = []apitypes.PullDTO{{RunID: &id}}
			got := stripANSI(m.pullsFooter())
			for _, hint := range tc.removed {
				if strings.Contains(got, hint) {
					t.Errorf("retained %q: %q", hint, got)
				}
			}
			for _, hint := range append(tc.retained, "u run", "w rework", "f fix ci", "? keys", "q quit") {
				if !strings.Contains(got, hint) {
					t.Errorf("lost %q: %q", hint, got)
				}
			}
		})
	}
	// Absent optional hints never cause another removal after the legend fits.
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.splitMode = 80, "off"
	m.repos = []apitypes.RepoDTO{oneRepo()}
	got := stripANSI(m.ciFooter())
	if strings.Contains(got, "R repo") || !strings.Contains(got, "r refresh") {
		t.Errorf("single repo policy: %q", got)
	}
	m.board.admin = true
	if got := stripANSI(m.boardFooterLine()); strings.Contains(got, "h fold") || !strings.Contains(got, "r refresh") {
		t.Errorf("admin policy: %q", got)
	}
}

func TestFooterVersionFallbackPreservesAlarm(t *testing.T) {
	withVersion(t, "v0.84.0")
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.width, m.splitMode, m.showVersion, m.serverVersion, m.profile = 80, "off", true, "0.85.0", profile
		got := m.boardFooterLine()
		plain := stripANSI(got)
		if strings.Contains(plain, "0.85.0") || !strings.HasSuffix(plain, "v0.84.0") || visualWidth(got) != 80 {
			t.Errorf("fallback: %q", plain)
		}
		if !strings.Contains(got, alarmRedPrefix(m)) {
			t.Errorf("fallback lost alarm: %q", got)
		}
		m.width = 240
		wide := stripANSI(m.boardFooterLine())
		arrow := "⇢ 0.85.0"
		if profile == colorprofile.Ascii {
			arrow = "-> 0.85.0 (behind)"
		}
		if !strings.Contains(wide, arrow) || !strings.HasPrefix(wide, stripANSI(m.boardFooter())) {
			t.Errorf("wide readout: %q", wide)
		}
	}
}

func TestFooterClientClampBoundary(t *testing.T) {
	for _, stampWidth := range []int{35, 36, 37} {
		t.Run(fmt.Sprint(stampWidth), func(t *testing.T) {
			withVersion(t, "v0.84.0+"+strings.Repeat("a", stampWidth-8))
			m := tuiTestModel(t, &uzicli.FakeClient{}, "")
			m.width, m.splitMode, m.showVersion, m.serverVersion = 80, "off", true, "0.85.0"
			m.selfUsageReady = true
			m.selfUsage.Last7UnreportedRunCount = 1
			got := m.boardFooterLine()
			plain := stripANSI(got)
			wantStamp := version
			if stampWidth == 37 {
				wantStamp = "v0.84.0+" + strings.Repeat("a", 27) + "…"
			}
			if !strings.HasSuffix(plain, wantStamp) || visualWidth(got) != 80 {
				t.Errorf("boundary suffix: %q", plain)
			}
			if !strings.Contains(plain, " enter/→ open · q quit · + partial · ? keys") {
				t.Errorf("essential content: %q", plain)
			}
			for _, hint := range []string{"r refresh", "h fold done", "a factory", "/ filter"} {
				if strings.Contains(plain, hint) {
					t.Errorf("optional hint survived before suffix clamp: %q", plain)
				}
			}
			if !strings.Contains(got, alarmRedPrefix(m)) {
				t.Errorf("clamp lost alarm: %q", got)
			}
		})
	}
}
