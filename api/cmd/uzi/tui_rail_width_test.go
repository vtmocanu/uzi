package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3406: the detail rail grows with the terminal ([tui] rail_min_width raises its floor) and
// [tui] milestone_numbers prefixes each rail milestone title with its frozen position.

func TestTUIRailWidthFormula(t *testing.T) {
	for _, tc := range []struct {
		width, minWidth, want int
	}{
		{0, 0, 26},     // before the first WindowSizeMsg
		{80, 0, 26},    // narrow: the transcript guard cannot hold 60, the rail stays 26
		{92, 0, 26},    // first width where both 26 and 60 fit
		{120, 0, 26},   // 20% = 24, below the floor
		{150, 0, 30},   // 20% of 150
		{200, 0, 40},   // 20% of 200
		{300, 0, 44},   // capped by railAutoMaxWidth
		{200, 60, 60},  // a configured floor may exceed the automatic cap
		{100, 40, 34},  // the transcript guard reduces a configured floor
		{80, 40, 26},   // ...but never below 26
		{200, 26, 40},  // the default floor changes nothing
		{1000, 0, 44},  // very wide: still capped
		{1000, 90, 90}, // a big floor on a big terminal is honoured
	} {
		m := tuiModel{width: tc.width, railMinWidth: tc.minWidth}
		if got := m.railWidth(); got != tc.want {
			t.Errorf("railWidth(width=%d, min=%d) = %d, want %d", tc.width, tc.minWidth, got, tc.want)
		}
		if tc.width >= 92 {
			if tw := m.transcriptWidth(); tw < railTranscriptMinCols {
				t.Errorf("width=%d min=%d: transcriptWidth %d < %d", tc.width, tc.minWidth, tw, railTranscriptMinCols)
			}
		}
	}
}

// railTestRun is a running issue run whose milestone titles are longer than the 22-col minimum cap.
func railTestRun(titles ...string) apitypes.RunDTO {
	run := apitypes.RunDTO{ID: "run-3406", Kind: "issue", Status: "running", Health: "ok", IssueTitle: "Rail width"}
	for i, title := range titles {
		run.Milestones = append(run.Milestones, apitypes.Milestone{ID: "m" + itoa(i+1), Title: title})
	}
	return run
}

// railTestModel drives the run through Update at the given terminal width and Ascii profile.
func railTestModel(t *testing.T, run apitypes.RunDTO, width int, configure func(*tuiModel)) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
	m.width = width
	if configure != nil {
		configure(&m)
	}
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	return applyDetail(next.(tuiModel), run, nil)
}

// railColumnWidth is the visual column of the rail divider in the composed view's milestone row.
func railColumnWidth(t *testing.T, view, marker string) int {
	t.Helper()
	for _, ln := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(ln, marker) {
			if i := strings.Index(ln, "▏"); i >= 0 {
				return visualWidth(ln[:i]) - 1 // joinColumns' divider is " ▏ "
			}
		}
	}
	t.Fatalf("no rail row containing %q in view:\n%s", marker, stripANSI(view))
	return 0
}

func TestTUIRailWidthDrivesComposedView(t *testing.T) {
	const title = "Merge base a86d6146 and rebase onto main"
	for _, tc := range []struct {
		width     int
		railWidth int
		wantRow   string
	}{
		{80, 26, " ○ Merge base a86d6146 a…"},
		{120, 26, " ○ Merge base a86d6146 a…"},
		{200, 40, " ○ Merge base a86d6146 and rebase onto…"},
	} {
		m := railTestModel(t, railTestRun(title), tc.width, nil)
		view := m.View().Content
		region := milestoneBlockRegion(t, view)
		if !strings.Contains(region, tc.wantRow+"\n") && !strings.HasSuffix(region, tc.wantRow) {
			t.Errorf("width %d: milestone row drifted, want %q in:\n%s", tc.width, tc.wantRow, region)
		}
		if got := railColumnWidth(t, view, "MILESTONES"); got != tc.railWidth {
			t.Errorf("width %d: composed rail column = %d, want %d", tc.width, got, tc.railWidth)
		}
		if got, want := m.transcriptWidth(), tc.width-tc.railWidth-railChromeCols; tc.width >= 92 && got != want {
			t.Errorf("width %d: transcriptWidth = %d, want %d (same rail width as the composed view)", tc.width, got, want)
		}
	}
}

func TestTUIRailMinWidthWidensComposedView(t *testing.T) {
	const title = "Merge base a86d6146 and rebase onto main, then gate"
	m := railTestModel(t, railTestRun(title), 200, func(m *tuiModel) { m.railMinWidth = 56 })
	view := m.View().Content
	if got := railColumnWidth(t, view, "MILESTONES"); got != 56 {
		t.Fatalf("rail_min_width=56 at 200 cols: rail column = %d, want 56", got)
	}
	if region := milestoneBlockRegion(t, view); !strings.Contains(region, " ○ "+title) {
		t.Fatalf("a 56-col rail should show the whole %d-char title, got:\n%s", len(title), region)
	}
}

func TestNumberedMilestoneTitle(t *testing.T) {
	for _, tc := range []struct {
		pos, total  int
		title, want string
	}{
		{1, 4, "Merge base a86d6146", "M1 Merge base a86d6146"},
		{2, 4, "M2 range review", "M2 range review"},
		{3, 4, "M7 mismatched number", "M3 mismatched number"},
		{4, 4, "m4 - records, single-commit", "M4 records, single-commit"},
		{1, 4, "M5: Fail-closed", "M1 Fail-closed"},
		{1, 4, "M6.Gate positively", "M1 Gate positively"},
		{1, 4, "   M7) Behavioral proof", "M1 Behavioral proof"},
		{1, 4, "M8:Informative", "M1 Informative"},
		{1, 4, "M12 two digits", "M1 two digits"},
		{1, 4, "M3Migration of the store", "M1 M3Migration of the store"},
		{1, 4, "M3D render pass", "M1 M3D render pass"},
		{1, 4, "Milestone 3: left alone", "M1 Milestone 3: left alone"},
		{1, 4, "Mark the spot", "M1 Mark the spot"},
		{4, 4, "M9", "M4 M9"},       // stripping would leave nothing: keep the original
		{4, 4, "M9 - ", "M4 M9 - "}, // separator only: same fallback
		{2, 12, "M2 range review", " M2 range review"},
		{12, 12, "Wrap up", "M12 Wrap up"},
		{7, 100, "Seventh", "  M7 Seventh"},
	} {
		if prefix, rest := numberedMilestoneTitle(tc.pos, tc.total, tc.title); prefix+rest != tc.want {
			got := prefix + rest
			t.Errorf("numberedMilestoneTitle(%d, %d, %q) = %q, want %q", tc.pos, tc.total, tc.title, got, tc.want)
		}
	}
}

func TestTUIMilestoneNumbersComposedView(t *testing.T) {
	run := railTestRun("Merge base a86d6146", "M2 range review", "M3 isolated mutation", "m4 - records")
	run.MilestonesCompleted = []string{"m1"}
	run.MilestonesInProgress = []string{"m2"}

	off := milestoneBlockRegion(t, railTestModel(t, run, 120, nil).View().Content)
	const wantOff = "MILESTONES ▰▱▱▱ 1/4 · m2\n" +
		" ✓ Merge base a86d6146\n" +
		" ◕ M2 range review\n" +
		" ○ M3 isolated mutation\n" +
		" ○ m4 - records"
	if off != wantOff {
		t.Errorf("milestone_numbers off must render titles verbatim.\n--- got ---\n%s\n--- want ---\n%s", off, wantOff)
	}

	on := milestoneBlockRegion(t, railTestModel(t, run, 120, func(m *tuiModel) { m.milestoneNumbers = true }).View().Content)
	const wantOn = "MILESTONES ▰▱▱▱ 1/4 · m2\n" +
		" ✓ M1 Merge base a86d6146\n" +
		" ◕ M2 range review\n" +
		" ○ M3 isolated mutation\n" +
		" ○ M4 records"
	if on != wantOn {
		t.Errorf("milestone_numbers on drifted.\n--- got ---\n%s\n--- want ---\n%s", on, wantOn)
	}

	// The prefix shares the title budget: at the 26-col rail the numbered row still fits 22 cols.
	long := railTestRun("Merge base a86d6146 and rebase")
	got := milestoneBlockRegion(t, railTestModel(t, long, 120, func(m *tuiModel) { m.milestoneNumbers = true }).View().Content)
	if !strings.Contains(got, " ○ M1 Merge base a86d614…") {
		t.Errorf("numbered title must share the 22-col cap, got:\n%s", got)
	}
}

func TestTUIMilestoneNumbersAlignTenOrMore(t *testing.T) {
	var titles []string
	for i := 1; i <= 10; i++ {
		titles = append(titles, "M"+itoa(i)+" step")
	}
	got := milestoneBlockRegion(t, railTestModel(t, railTestRun(titles...), 120, func(m *tuiModel) { m.milestoneNumbers = true }).View().Content)
	for _, want := range []string{" ○  M1 step\n", " ○  M9 step\n", " ○ M10 step"} {
		if !strings.Contains(got, want) {
			t.Errorf("10-milestone list must right-align positions, want %q in:\n%s", want, got)
		}
	}
}

func TestTUIMilestoneNumbersHostileTitle(t *testing.T) {
	run := railTestRun("M3 \x1b[2Jpwn\x07ed \x1b]8;;https://evil.example\x07link")
	m := railTestModel(t, run, 200, func(m *tuiModel) { m.milestoneNumbers = true })
	raw := m.View().Content
	for _, bad := range []string{"\x1b[2J", "\x07", "\x1b]8;;"} {
		if strings.Contains(raw, bad) {
			t.Fatalf("hostile milestone title leaked %q into the numbered rail", bad)
		}
	}
	if region := milestoneBlockRegion(t, raw); !strings.Contains(region, " ○ M1 ") || !strings.Contains(region, "pwn") {
		t.Fatalf("hostile title should still render, numbered and sanitized:\n%s", region)
	}
}

func TestTUIConfigPrefsMilestoneNumbersAndRailMinWidth(t *testing.T) {
	store := uzicli.NewStore(t.TempDir())
	write := func(t *testing.T, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(store.Dir(), "config.toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, body string
		numbers    bool
		railMin    int
	}{
		{"absent", "[tui]\n", false, 26},
		{"numbers false", "[tui]\nmilestone_numbers = false\n", false, 26},
		{"numbers true", "[tui]\nmilestone_numbers = true\n", true, 26},
		{"rail 26", "[tui]\nrail_min_width = 26\n", false, 26},
		{"rail 40", "[tui]\nrail_min_width = 40\n", false, 40},
		{"both with split", "[tui]\nsplit = \"off\"\nmilestone_numbers = true\nrail_min_width = 32\n", true, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tc.body)
			got, err := tuiConfigPrefs(store)
			if err != nil || got.milestoneNumbers != tc.numbers || got.railMinWidth != tc.railMin {
				t.Fatalf("prefs=%+v err=%v, want numbers=%v railMin=%d", got, err, tc.numbers, tc.railMin)
			}
		})
	}
	for _, tc := range []struct {
		name, body, key string
	}{
		{"rail below floor", "[tui]\nrail_min_width = 25\n", "[tui] rail_min_width"},
		{"rail zero", "[tui]\nrail_min_width = 0\n", "[tui] rail_min_width"},
		{"rail negative", "[tui]\nrail_min_width = -1\n", "[tui] rail_min_width"},
		{"rail string", "[tui]\nrail_min_width = \"wide\"\n", "[tui] rail_min_width"},
		{"rail float", "[tui]\nrail_min_width = 30.5\n", "[tui] rail_min_width"},
		{"numbers string", "[tui]\nmilestone_numbers = \"true\"\n", "[tui] milestone_numbers"},
		{"numbers int", "[tui]\nmilestone_numbers = 1\n", "[tui] milestone_numbers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tc.body)
			_, err := tuiConfigPrefs(store)
			if uzicli.ExitCodeFor(err) != uzicli.ExitUsage || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "config.toml") {
				t.Fatalf("want a usage error naming %s, got %v", tc.key, err)
			}
			cmd := newTUICmd(Env{Store: store, StdoutTTY: true}, &globalFlags{})
			if err := cmd.RunE(cmd, nil); uzicli.ExitCodeFor(err) != uzicli.ExitUsage {
				t.Fatalf("tui exit = %d, want usage 2 (err=%v)", uzicli.ExitCodeFor(err), err)
			}
		})
	}
	t.Run("unrelated config error stays unrelated", func(t *testing.T) {
		write(t, "current = 42\n[tui]\nmilestone_numbers = true\nrail_min_width = 30\n")
		_, err := tuiConfigPrefs(store)
		if err == nil || uzicli.ExitCodeFor(err) == uzicli.ExitUsage || strings.Contains(err.Error(), "accepted values") {
			t.Fatalf("unrelated config decode error misreported as a [tui] key: %v", err)
		}
	})
}

func TestTUIConfigPrefsSurviveContextAndLoginWrites(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{}, TUI: uzicli.TUIConfig{MilestoneNumbers: true, RailMinWidth: ip32(36)}}
	env := seedStore(t, cfg, &uzicli.Credentials{Contexts: map[string]uzicli.Credential{}})
	if _, stderr, code := runCLI(t, env, "context", "set", "work", "--url", "https://work.example"); code != uzicli.ExitOK {
		t.Fatalf("context set exit=%d stderr=%s", code, stderr)
	}
	if err := finishLogin(env, &globalFlags{quiet: true}, "work", "https://work.example", uzicli.CLIAuthPollResult{Token: "uzc_x"}); err != nil {
		t.Fatalf("finishLogin: %v", err)
	}
	got, err := env.Store.LoadConfig()
	if err != nil || !got.TUI.MilestoneNumbers || got.TUI.RailMinWidth == nil || *got.TUI.RailMinWidth != 36 {
		t.Fatalf("config after writes=%+v err=%v", got, err)
	}
}

// The answer composer composes the same rail beside the transcript (renderAnswerDetail), so it must
// use the same computed width as the plain detail view.
func TestTUIRailWidthInAnswerComposer(t *testing.T) {
	m, _ := answerFlowModel(t, answerFlowPayload)
	m.width, m.height = 200, 60
	m = answerKeys(t, m, "i")
	if !m.detail.answer.open {
		t.Fatal("answer composer did not open")
	}
	if got := railColumnWidth(t, m.View().Content, "CREW"); got != 40 {
		t.Fatalf("answer composer rail column at 200 cols = %d, want 40", got)
	}
}
