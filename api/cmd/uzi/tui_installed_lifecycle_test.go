package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func releasePoll(v string) buildInfoMsg {
	msg := buildInfoMsg{version: "0.83.0"}
	if v != "" {
		msg.latest = &apitypes.LatestReleaseDTO{Version: v}
	}
	return msg
}

func observationResults(cmd tea.Cmd) []installedVersionMsg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case installedVersionMsg:
		return []installedVersionMsg{msg}
	case tea.BatchMsg:
		var out []installedVersionMsg
		for _, child := range msg {
			out = append(out, observationResults(child)...)
		}
		return out
	}
	return nil
}

func TestM2CandidateLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []string
		fail    bool
	}{
		{"A to B success", []string{"v0.86.0"}, false},
		{"A to B failure", []string{"v0.86.0"}, true},
		{"A to B to A success", []string{"v0.86.0", "v0.85.0"}, false},
		{"A to B to A failure", []string{"v0.86.0", "v0.85.0"}, true},
		{"disappearance success", []string{""}, false},
		{"disappearance failure", []string{""}, true},
		{"invalid fact", []string{"vv0.85.0"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, "v0.83.0")
			m := updatePromptModel(t)
			m.updatePrompt.owner = "uzi-cli"
			calls := 0
			m.installedVersion = func(string) (string, error) {
				calls++
				if tc.fail && calls == 1 {
					return "", errors.New("failed")
				}
				return "v0.90.0", nil
			}
			next, cmd := m.Update(releasePoll("v0.85.0"))
			m = next.(tuiModel)
			results := observationResults(cmd)
			if len(results) != 1 || calls != 1 {
				t.Fatalf("initial observations=%d calls=%d", len(results), calls)
			}
			old := results[0]
			for _, v := range tc.changes {
				next, cmd = m.Update(releasePoll(v))
				m = next.(tuiModel)
				if got := observationResults(cmd); len(got) != 0 || calls != 1 {
					t.Fatalf("overlapping observation on %q", v)
				}
			}
			next, follow := m.Update(old)
			m = next.(tuiModel)
			if m.updatePrompt.showing || m.updatePrompt.shownThisSession || m.updatePrompt.installedVersion != "" {
				t.Fatalf("stale result applied: %+v", m.updatePrompt)
			}
			fresh := observationResults(follow)
			wantFollow := tc.changes[len(tc.changes)-1] != "" && tc.changes[len(tc.changes)-1] != "vv0.85.0"
			if (len(fresh) == 1) != wantFollow {
				t.Fatalf("follow-up observations=%d", len(fresh))
			}
			if wantFollow {
				if fresh[0].candidate != m.updatePrompt.candidate || fresh[0].candidate.generation == old.candidate.generation {
					t.Fatal("follow-up has stale identity")
				}
				next, cmd = m.Update(fresh[0])
				m = next.(tuiModel)
				if len(observationResults(cmd)) != 0 || m.updatePrompt.installedVersion != "v0.90.0" || m.updatePrompt.showing {
					t.Fatalf("fresh result not accepted: %+v", m.updatePrompt)
				}
				next, _ = m.Update(old)
				m = next.(tuiModel)
				if m.updatePrompt.installedVersion != "v0.90.0" {
					t.Fatal("duplicate stale reply changed current hint")
				}
			}
		})
	}
}

func TestM2ProbeReevaluatesEveryPollAndClearsHint(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	m.updatePrompt.owner = "uzi-cli"
	values := []string{"v0.86.0", "invalid", "v0.84.0", "v0.86.0"}
	calls := 0
	m.installedVersion = func(string) (string, error) { v := values[calls]; calls++; return v, nil }
	for i := range values {
		next, _ := settledUpdate(m, releasePoll("v0.85.0"))
		m = next.(tuiModel)
		if i == 0 && (m.updatePrompt.shownThisSession || m.updatePrompt.showing || m.updatePrompt.installedVersion != "v0.86.0") {
			t.Fatal("installed current consumed latch or prompted")
		}
		if i == 1 && (!m.updatePrompt.showing || m.updatePrompt.installedVersion != "") {
			t.Fatal("invalid observation retained hint")
		}
		if i == 2 {
			next, _ = m.updatePromptKey(keyEsc)
			m = next.(tuiModel)
		}
		if i == 3 && (m.updatePrompt.showing || m.updatePrompt.installedVersion != "v0.86.0") {
			t.Fatal("later poll did not recognize upgraded binary")
		}
	}
	if calls != 4 {
		t.Fatalf("calls=%d", calls)
	}
	next, cmd := m.Update(releasePoll(""))
	m = next.(tuiModel)
	if m.updatePrompt.installedVersion != "" || len(observationResults(cmd)) != 0 {
		t.Fatal("disappearance retained restart hint")
	}
}

func TestM2ProbeEligibilityAndDismissal(t *testing.T) {
	for _, kind := range []string{"unknown", "optout", "hidden version", "dismissed", "session latch", "failure", "nil probe"} {
		t.Run(kind, func(t *testing.T) {
			withVersion(t, "v0.83.0")
			m := updatePromptModel(t)
			m.updatePrompt.owner = "uzi-cli"
			calls := 0
			m.installedVersion = func(string) (string, error) {
				calls++
				if kind == "failure" {
					return "", errors.New("timeout\x1b[2J")
				}
				return "v0.84.0", nil
			}
			switch kind {
			case "unknown":
				m.updatePrompt.owner = ""
			case "optout":
				m.skewCheck = false
			case "hidden version":
				m.showVersion = false
			case "dismissed":
				m.store = uzicli.NewStore(t.TempDir())
				m.serverURL = "https://server.example"
				if err := m.store.RecordDismissedUpdate(m.serverURL, "v0.85.0"); err != nil {
					t.Fatal(err)
				}
			case "session latch":
				m.updatePrompt.shownThisSession = true
			case "nil probe":
				m.installedVersion = nil
			}
			next, _ := settledUpdate(m, releasePoll("v0.85.0"))
			m = next.(tuiModel)
			wantProbe := kind != "unknown" && kind != "optout" && kind != "hidden version" && kind != "nil probe"
			if (calls == 1) != wantProbe {
				t.Fatalf("calls=%d", calls)
			}
			wantModal := kind == "unknown" || kind == "failure" || kind == "nil probe"
			if m.updatePrompt.showing != wantModal {
				t.Fatalf("modal=%v", m.updatePrompt.showing)
			}
		})
	}
}

func TestM2ModalPollingThroughView(t *testing.T) {
	priorInterval := skewPollInterval
	skewPollInterval = 0 // Drive the rearm command without waiting five minutes.
	t.Cleanup(func() { skewPollInterval = priorInterval })
	withVersion(t, "v0.83.0")
	fake := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: "0.83.0", Latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}}}
	m := updatePromptModel(t)
	m.client = fake
	m.updatePrompt.owner = "uzi-cli"
	installed := "v0.84.0"
	m.installedVersion = func(string) (string, error) { return installed, nil }
	next, _ := settledUpdate(m, m.fetchBuildInfoCmd()())
	m = next.(tuiModel)
	if !m.updatePrompt.showing || !strings.Contains(stripANSI(m.View().Content), "Update available") {
		t.Fatal("initial modal missing")
	}
	installed = "v0.86.0"
	next, cmd := m.Update(skewTickMsg{})
	m = next.(tuiModel)
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("modal skew tick did not schedule fetch plus tick")
	}
	// Drive the fetch, not the rearmed timer.
	next, cmd = m.Update(batch[0]())
	m = next.(tuiModel)
	results := observationResults(cmd)
	if len(results) != 1 {
		t.Fatal("poll while modal open did not probe installed version")
	}
	next, _ = m.Update(results[0])
	m = next.(tuiModel)
	if m.updatePrompt.showing || !strings.Contains(stripANSI(m.View().Content), "v0.86.0 installed, restart uzi to use it") {
		t.Fatalf("modal not replaced with restart hint: %s", stripANSI(m.View().Content))
	}
	m.quitting = true
	_, cmd = m.Update(skewTickMsg{})
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Fatal("quitting skew tick still schedules fetch")
	}
}

func TestM2RestartFooterWidthsAndSanitization(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.TrueColor} {
		for _, width := range []int{0, 5, 11, 25, 80, 120, 300} {
			for _, installed := range []string{"v0.86.0", "v0.86.0+" + strings.Repeat("a", 140), "\x1b[2Jv0.86.0\x07\u202e"} {
				m := updatePromptModel(t)
				m.profile = profile
				m.width = width
				m.updatePrompt.installedVersion = installed
				m.serverVersion = "9.0.0"
				for _, line := range []string{m.boardFooterLine(), m.splitFooterLine()} {
					assertNoRawControls(t, "restart footer", line)
					if visualWidth(line) > width {
						t.Fatalf("width %d overflow: %q", width, line)
					}
					plain := stripANSI(line)
					if width >= 11 && !strings.Contains(plain, "restart uzi") {
						t.Fatalf("hint lost at width %d: %q", width, plain)
					}
					if width == 300 && !strings.Contains(plain, "installed, restart uzi to use it") {
						t.Fatalf("full hint lost: %q", plain)
					}
					if strings.Contains(plain, "9.0.0") {
						t.Fatal("skew overrode restart")
					}
					if profile == colorprofile.Ascii && strings.Contains(line, "\x1b[") {
						t.Fatal("ASCII footer carries styling")
					}
				}
			}
		}
	}
}

func TestM2PendingUpgradeVerification(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
		success   bool
	}{
		{"equal", "0.85.0", nil, true}, {"ahead", "v0.86.0", nil, true}, {"older", "v0.84.0", nil, false},
		{"invalid", "vv0.85.0", nil, false}, {"oversized", "v0.85.0+" + strings.Repeat("a", 1000), nil, false},
		{"hostile invalid", "\x1b[2Jv0.85.0\x07", nil, false}, {"control-sanitized valid", "v0.85.0\x07", nil, true}, {"probe failure", "", errors.New("bad\x1b[2J\u202e"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr strings.Builder
			brewCalls, probeCalls := 0, 0
			env := Env{Stdout: &out, Stderr: &stderr, Brew: func(fg bool, args ...string) (string, error) {
				brewCalls++
				if !fg || strings.Join(args, " ") != "upgrade vtmocanu/tap/uzi-cli-rc" {
					t.Fatal("wrong foreground argv")
				}
				return "", nil
			},
				InstalledVersion: func(formula string) (string, error) {
					probeCalls++
					if brewCalls != 1 || formula != "uzi-cli-rc" {
						t.Fatal("verification before brew or wrong formula")
					}
					return tc.raw, tc.err
				}}
			err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli-rc"}, "v0.85.0")
			if (err == nil) != tc.success || strings.Contains(out.String(), "Update complete") != tc.success || probeCalls != 1 {
				t.Fatalf("err=%v out=%q probes=%d", err, out.String(), probeCalls)
			}
			if tc.name == "older" && stderr.String() != "Installed CLI is v0.84.0; requested v0.85.0 was not reached. Try again later.\n" {
				t.Fatalf("older diagnostic=%q", stderr.String())
			}
			if err != nil {
				assertNoRawControls(t, "verification diagnostic", err.Error())
			}
		})
	}
	for _, argv := range [][]string{{"upgrade", "uzi-cli-rc"}, {"upgrade", "other/tap/uzi-cli"}, {"upgrade", "vtmocanu/tap/uzi-cli", "--force"}, {"install", "vtmocanu/tap/uzi-cli"}} {
		calls := 0
		env := Env{Brew: func(bool, ...string) (string, error) { calls++; return "", nil }}
		if err := runPendingUpgrade(env, argv, "v0.85.0"); err == nil || calls != 0 {
			t.Fatalf("invalid argv executed: %v", argv)
		}
	}
}

func TestM2ReleaseTagsRemainStrict(t *testing.T) {
	withVersion(t, "v0.83.0")
	for _, owner := range []string{"uzi-cli", "uzi-cli-rc"} {
		t.Run(owner, func(t *testing.T) {
			m := updatePromptModel(t)
			m.updatePrompt.owner = owner
			calls := 0
			m.installedVersion = func(string) (string, error) { calls++; return "v0.86.0", nil }
			fact := &apitypes.LatestReleaseDTO{Version: "v0.85.0\x07"}
			poll := buildInfoMsg{latest: fact}
			if owner == "uzi-cli-rc" {
				fact.Version = "v0.85.0-rc.1\x07"
				poll = buildInfoMsg{latestRC: fact}
			}
			next, cmd := m.Update(poll)
			m = next.(tuiModel)
			if len(observationResults(cmd)) != 0 || calls != 0 || m.updatePrompt.showing || m.updatePrompt.candidate.target != "" {
				t.Fatal("malformed server tag became an installed-version candidate")
			}
		})
	}
}

func TestM2OfferedFactsPreserveDismissal(t *testing.T) {
	withVersion(t, "v0.83.0")
	for _, tag := range []string{"0.85.0", "v0.85.0+" + strings.Repeat("a", 210)} {
		t.Run(tag, func(t *testing.T) {
			m := updatePromptModel(t)
			m.updatePrompt.owner = "uzi-cli"
			m.store, m.serverURL = uzicli.NewStore(t.TempDir()), "https://server.example"
			if err := m.store.RecordDismissedUpdate(m.serverURL, tag); err != nil {
				t.Fatal(err)
			}
			calls := 0
			m.installedVersion = func(string) (string, error) { calls++; return "v0.84.0", nil }
			next, _ := settledUpdate(m, releasePoll(tag))
			m = next.(tuiModel)
			if calls != 1 || m.updatePrompt.showing || m.updatePrompt.latestVersion != tag {
				t.Fatalf("valid offered tag changed dismissal semantics: probes=%d modal=%v tag=%q", calls, m.updatePrompt.showing, m.updatePrompt.latestVersion)
			}
		})
	}
}

func TestM2LongOfferedUpgrade(t *testing.T) {
	var out strings.Builder
	calls := 0
	env := Env{Stdout: &out, Stderr: &out,
		Brew:             func(bool, ...string) (string, error) { calls++; return "", nil },
		InstalledVersion: func(string) (string, error) { return "v0.85.0", nil },
	}
	err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0+"+strings.Repeat("a", 210))
	if err != nil || calls != 1 || !strings.Contains(out.String(), "Update complete") {
		t.Fatalf("valid offered semver was rejected: calls=%d err=%v", calls, err)
	}
}

func TestRestartHintKeepsFooterKeyHints(t *testing.T) {
	base := updatePromptModel(t)
	base.updatePrompt.installedVersion = "v0.86.0"

	at := func(m tuiModel, width int) tuiModel {
		m.width = width
		return m
	}
	b := at(base, 100)
	if line := stripANSI(b.boardFooterLine()); !strings.Contains(line, "q quit") || !strings.Contains(line, "restart uzi") {
		t.Fatalf("board footer lost hints or restart text: %q", line)
	}

	for _, tc := range []struct {
		view tuiView
		want string
	}{{viewBoard, "tab pane"}, {viewWorkers, "ctrl+w focus"}, {viewCI, "R repo"}, {viewPulls, "R repo"}} {
		m := at(base, 160)
		m.view = tc.view
		line := stripANSI(m.splitFooterLine())
		if !strings.Contains(line, tc.want) || !strings.Contains(line, "restart uzi") {
			t.Fatalf("split footer view %v at 160 lacks %q or restart text: %q", tc.view, tc.want, line)
		}
	}

	type variant struct {
		name    string
		profile colorprofile.Profile
		dark    bool
	}
	for _, v := range []variant{
		{"ascii", colorprofile.Ascii, true},
		{"dark", colorprofile.TrueColor, true},
		{"light", colorprofile.TrueColor, false},
	} {
		for _, width := range []int{40, 80, 100, 160} {
			m := at(base, width)
			m.profile = v.profile
			m.dark, m.pal = v.dark, newPalette(v.dark)
			lines := map[string]string{"board": m.boardFooterLine()}
			for name, view := range map[string]tuiView{"split-board": viewBoard, "split-workers": viewWorkers, "split-ci": viewCI, "split-pulls": viewPulls} {
				sm := m
				sm.view = view
				lines[name] = sm.splitFooterLine()
			}
			for name, line := range lines {
				where := fmt.Sprintf("%s/%s/w=%d", v.name, name, width)
				if got := visualWidth(line); got > width {
					t.Errorf("%s: width %d exceeds %d: %q", where, got, width, line)
				}
				plain := stripANSI(line)
				// The board footer (unchanged by #2609) sheds "q quit" at 40 columns.
				needQuit := name != "board" || width >= 80
				if !strings.Contains(plain, "restart uzi") || (needQuit && !strings.Contains(plain, "q quit")) {
					t.Errorf("%s: missing restart text or q quit: %q", where, plain)
				}
				assertNoRawControls(t, where, line)
				if v.profile == colorprofile.Ascii && strings.Contains(line, "\x1b[") {
					t.Errorf("%s: ANSI escape under Ascii: %q", where, line)
				}
			}
		}
	}
}
