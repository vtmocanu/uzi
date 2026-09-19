package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1251 M1 — the codex-style startup update prompt. Deterministic, offline: no brew is
// run and no network call is made; the brew shell-out is exercised through the injected seam.

// updatePromptModel builds a model allowed to probe (skewCheck + showVersion), the state
// newTUICmd's RunE sets on the real path, so the update-prompt gate can fire in a test.
func updatePromptModel(t *testing.T) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.skewCheck = true
	m.showVersion = true
	return m
}

// showingUpdateModel returns a model with the modal already open in the given state, for the
// View→string render assertions (bypassing the show gate on purpose).
func showingUpdateModel(t *testing.T, up updatePromptState) tuiModel {
	t.Helper()
	m := updatePromptModel(t)
	up.showing = true
	m.updatePrompt = up
	return m
}

// brewRec records the (foreground, args) of each Env.Brew call and returns canned results
// per subcommand, so brew detection and the upgrade hand-off are asserted without forking brew.
type brewRec struct {
	calls     []brewCall
	listErr   error  // response for `brew list uzi-cli`
	prefixOut string // response for `brew --prefix`
	prefixErr error
}

type brewCall struct {
	foreground bool
	args       []string
}

func (b *brewRec) fn(foreground bool, args ...string) (string, error) {
	b.calls = append(b.calls, brewCall{foreground: foreground, args: append([]string(nil), args...)})
	switch {
	case len(args) >= 1 && args[0] == "list":
		return "", b.listErr
	case len(args) >= 1 && args[0] == "--prefix":
		return b.prefixOut, b.prefixErr
	default:
		return "", nil
	}
}

func (b *brewRec) upgradeCall() *brewCall {
	for i := range b.calls {
		if len(b.calls[i].args) > 0 && b.calls[i].args[0] == "upgrade" {
			return &b.calls[i]
		}
	}
	return nil
}

// ---- Update → msg (the show gate) --------------------------------------------------------

func TestUpdatePromptShowsOnNewerStable(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	next, _ := m.Update(buildInfoMsg{version: "0.83.0", latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	if !m.updatePrompt.showing {
		t.Fatal("expected the update prompt to show for a newer stable release")
	}
	if m.updatePrompt.latestVersion != "v0.85.0" {
		t.Errorf("latestVersion = %q, want v0.85.0", m.updatePrompt.latestVersion)
	}
	if !m.updatePrompt.shownThisSession {
		t.Error("showing the prompt must latch shownThisSession")
	}
}

func TestUpdatePromptNotShownWhenLatestNil(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	next, _ := m.Update(buildInfoMsg{version: "0.83.0", latest: nil})
	m = next.(tuiModel)
	if m.updatePrompt.showing {
		t.Fatal("a nil Latest (no check ran / feature disabled) must not prompt")
	}
}

func TestUpdatePromptNotShownWhenCurrentOrAhead(t *testing.T) {
	for _, tc := range []struct{ name, cli, latest string }{
		{"equal", "v0.85.0", "v0.85.0"},
		{"ahead", "v0.90.0", "v0.85.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, tc.cli)
			m := updatePromptModel(t)
			next, _ := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: tc.latest}})
			m = next.(tuiModel)
			if m.updatePrompt.showing {
				t.Fatalf("must not prompt when CLI is %s and latest is %s", tc.cli, tc.latest)
			}
		})
	}
}

// TestUpdatePromptNeverOffersPrerelease is the RC guard (PRD #1251 M1): the prompt must
// never point a user at an -rc.N, whether the rc's base is higher than or equal to the CLI.
func TestUpdatePromptNeverOffersPrerelease(t *testing.T) {
	withVersion(t, "v0.83.0")
	for _, latest := range []string{"v0.85.0-rc.1", "v0.83.0-rc.2", "v0.90.0-rc.3"} {
		t.Run(latest, func(t *testing.T) {
			m := updatePromptModel(t)
			next, _ := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: latest}})
			m = next.(tuiModel)
			if m.updatePrompt.showing {
				t.Fatalf("must never prompt to upgrade to a prerelease (%s)", latest)
			}
		})
	}
}

func TestUpdatePromptNotShownWhenProbeDisabled(t *testing.T) {
	withVersion(t, "v0.83.0")
	// tuiTestModel leaves skewCheck/showVersion false, as --demo and direct construction do.
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	next, _ := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	if m.updatePrompt.showing {
		t.Fatal("a session that may not probe (skewCheck/showVersion false) must not prompt")
	}
}

func TestUpdatePromptRespectsDismissal(t *testing.T) {
	withVersion(t, "v0.83.0")
	store := uzicli.NewStore(t.TempDir())
	const url = "https://uzi.example"
	if err := store.RecordDismissedUpdate(url, "v0.85.0"); err != nil {
		t.Fatal(err)
	}

	dismissed := updatePromptModel(t)
	dismissed.store, dismissed.serverURL = store, url
	next, _ := dismissed.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	dismissed = next.(tuiModel)
	if dismissed.updatePrompt.showing {
		t.Fatal("a version dismissed via 'don't remind me' must not re-prompt")
	}

	// A NEWER release re-prompts despite the older dismissal.
	newer := updatePromptModel(t)
	newer.store, newer.serverURL = store, url
	next, _ = newer.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.86.0"}})
	newer = next.(tuiModel)
	if !newer.updatePrompt.showing {
		t.Fatal("a release newer than the dismissed one must re-prompt")
	}
}

func TestUpdatePromptShowsOncePerSession(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	next, _ := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	m = press(t, m, keyEsc) // "not now" closes it for the session
	if m.updatePrompt.showing {
		t.Fatal("esc must close the modal")
	}
	next, _ = m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	if m.updatePrompt.showing {
		t.Fatal("the prompt must fire at most once per session")
	}
}

// ---- View → string -----------------------------------------------------------------------

func TestUpdatePromptRenderNormal(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true})
	out := m.View().Content
	stripped := stripANSI(out)
	for _, want := range []string{
		"Update available", "v0.83.0", "v0.85.0",
		"Update now", "brew upgrade uzi-cli", "Not now", "Don't remind me for v0.85.0",
	} {
		if !strings.Contains(stripped, want) {
			t.Errorf("normal update prompt missing %q\n%s", want, stripped)
		}
	}
	if !strings.Contains(out, fgSGR(m.pal.sage)) {
		t.Error("the latest version should be rendered in the sage colour")
	}
	if strings.Contains(out, bgFillSGR(m.pal.amber)) {
		t.Error("a routine (non-security) release must not use the amber band fill")
	}
}

func TestUpdatePromptRenderSecurityBand(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true, security: true})
	out := m.View().Content
	if !strings.Contains(stripANSI(out), "Security update") {
		t.Errorf("security variant must be worded as a security update\n%s", stripANSI(out))
	}
	if !strings.Contains(out, bgFillSGR(m.pal.amber)) {
		t.Errorf("a security release must render the amber andon band fill\n%s", out)
	}
}

func TestUpdatePromptRenderNonBrewInfo(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{
		latestVersion:  "v0.85.0",
		latestNotesURL: "https://github.com/vtmocanu/uzi/releases/tag/v0.85.0",
		isBrew:         false,
	})
	out := stripANSI(m.View().Content)
	if strings.Contains(out, "brew upgrade") || strings.Contains(out, "Update now") {
		t.Errorf("the non-brew info variant must not offer the brew action\n%s", out)
	}
	if !strings.Contains(out, "releases/tag/v0.85.0") {
		t.Errorf("the non-brew info variant must show the release-notes URL\n%s", out)
	}
	for _, want := range []string{"Not now", "Don't remind me"} {
		if !strings.Contains(out, want) {
			t.Errorf("the non-brew variant must still offer %q\n%s", want, out)
		}
	}
}

func TestUpdatePromptSelectedRowStyling(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true, sel: 0})
	out := m.View().Content
	cursor := paintSeg(m.pal.tungsten, m.pal.selBg, true, "▸ ")
	if !strings.Contains(out, cursor) {
		t.Errorf("the selected row must draw a bold tungsten ▸ cursor over the selBg fill\nwant %q\n%s", cursor, out)
	}
}

func TestUpdatePromptRenderAsciiFallback(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true, security: true})
	next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.Ascii})
	m = next.(tuiModel)
	out := m.View().Content
	if strings.Contains(out, bgFillSGR(m.pal.amber)) {
		t.Errorf("under the Ascii profile the amber band fill must be stripped\n%s", out)
	}
	stripped := stripANSI(out)
	if !strings.Contains(stripped, "Security update") {
		t.Errorf("the Ascii security prompt must carry the 'Security update' word\n%s", stripped)
	}
	if !strings.Contains(stripped, "->") {
		t.Errorf("the Ascii prompt must use the -> arrow so 'behind' survives without colour\n%s", stripped)
	}
}

// TestUpdatePromptStripsControlBytes is the D7 hostile-value render proof for the modal:
// server-authored Latest fields (Version, Name, NotesURL) carrying control + bidi bytes must
// be sanitized before they reach the frame, while each field's marker still survives (so the
// render path actually drew it, not a vacuous pass).
func TestUpdatePromptStripsControlBytes(t *testing.T) {
	withVersion(t, "v0.83.0")
	// ED erase + RLO bidi override (U+202E) + BEL + SOH, all stripped by the D7 sanitizers.
	// The bidi rune is built from a numeric codepoint at runtime rather than written as a
	// literal in source, so no bidi character sits in this file (Trojan-source hygiene).
	nasty := "\x1b[2J" + string(rune(0x202e)) + "\x07\x01"
	m := showingUpdateModel(t, updatePromptState{
		latestVersion:  nasty + "verok",
		latestName:     nasty + "nameok",
		latestNotesURL: nasty + "urlok",
		isBrew:         false, // draws the notes URL
	})
	out := m.View().Content
	assertNoRawControls(t, "update prompt", out)
	stripped := stripANSI(out)
	for _, marker := range []string{"verok", "nameok", "urlok"} {
		if !strings.Contains(stripped, marker) {
			t.Errorf("update-prompt field marker %q missing — a Latest field is not drawn (or was clamped away)\n%s", marker, stripped)
		}
	}
}

// ---- Brew seam + foreground-exit upgrade -------------------------------------------------

func TestDetectBrewViaList(t *testing.T) {
	b := &brewRec{} // `brew list uzi-cli` exits 0
	if !detectBrew(b.fn) {
		t.Fatal("brew list uzi-cli exiting 0 must detect a brew install")
	}
	if len(b.calls) == 0 {
		t.Fatal("detectBrew made no calls")
	}
	first := b.calls[0]
	if first.foreground {
		t.Error("the detection probe must be quiet (foreground=false)")
	}
	if strings.Join(first.args, " ") != "list uzi-cli" {
		t.Errorf("first probe args = %v, want [list uzi-cli]", first.args)
	}
}

func TestDetectBrewViaPrefixPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("no executable path: %v", err)
	}
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	// `brew list` fails, but the running binary is under `brew --prefix`.
	b := &brewRec{listErr: errors.New("not installed"), prefixOut: filepath.Dir(exe)}
	if !detectBrew(b.fn) {
		t.Errorf("an executable under `brew --prefix` (%q) must detect a brew install", filepath.Dir(exe))
	}
}

func TestDetectBrewNonBrewOnBothDetectorsFailing(t *testing.T) {
	b := &brewRec{listErr: errors.New("not installed"), prefixErr: errors.New("brew: not found")}
	if detectBrew(b.fn) {
		t.Error("both detectors failing must read as NON-brew (info variant)")
	}
}

func TestDetectBrewNilSeam(t *testing.T) {
	if detectBrew(nil) {
		t.Error("a nil brew seam must read as non-brew, never panic")
	}
}

func TestUpdatePromptBrewDetectionFlipsVariant(t *testing.T) {
	withVersion(t, "v0.83.0")
	b := &brewRec{} // brew user
	m := updatePromptModel(t)
	m.brew = b.fn
	next, cmd := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	if !m.updatePrompt.showing {
		t.Fatal("the prompt should show")
	}
	if m.updatePrompt.isBrew {
		t.Error("isBrew should default false until the detection probe returns")
	}
	if cmd == nil {
		t.Fatal("showing the prompt must kick the brew-detection command")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if !m.updatePrompt.isBrew {
		t.Error("a successful brew-detection probe must flip the variant to brew")
	}
}

func TestUpdatePromptUpdateNowArmsForegroundUpgrade(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true, sel: 0})
	next, cmd := m.updatePromptKey(keyEnter)
	m = next.(tuiModel)
	if !m.updatePrompt.pendingUpgrade {
		t.Fatal("'Update now' must arm the pending upgrade")
	}
	if strings.Join(m.updatePrompt.upgradeArgv, " ") != "upgrade uzi-cli" {
		t.Errorf("upgradeArgv = %v, want [upgrade uzi-cli]", m.updatePrompt.upgradeArgv)
	}
	if m.updatePrompt.showing {
		t.Error("'Update now' must close the modal before exiting")
	}
	if cmd == nil {
		t.Fatal("'Update now' must return a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("'Update now' must return tea.Quit, got %T", cmd())
	}
}

func TestUpdatePromptDismissPersists(t *testing.T) {
	withVersion(t, "v0.83.0")
	store := uzicli.NewStore(t.TempDir())
	const url = "https://uzi.example"
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", isBrew: true})
	m.store, m.serverURL = store, url
	// Move to the "Don't remind me" choice (index 2 for a brew user) and select it.
	m = press(t, m, keyDown)
	m = press(t, m, keyDown)
	m = press(t, m, keyEnter)
	if m.updatePrompt.showing {
		t.Fatal("'Don't remind me' must close the modal")
	}
	if got := store.DismissedUpdateTag(url); got != "v0.85.0" {
		t.Errorf("'Don't remind me' must persist the dismissal, DismissedUpdateTag=%q", got)
	}
}

func TestRunPendingUpgradeCallsSeamForeground(t *testing.T) {
	b := &brewRec{}
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Brew: b.fn}
	if err := runPendingUpgrade(env, []string{"upgrade", "uzi-cli"}); err != nil {
		t.Fatalf("runPendingUpgrade: %v", err)
	}
	call := b.upgradeCall()
	if call == nil {
		t.Fatal("runPendingUpgrade did not invoke the brew seam with an upgrade")
	}
	if !call.foreground {
		t.Error("the upgrade must run in the foreground so compile output is visible (D1)")
	}
	if strings.Join(call.args, " ") != "upgrade uzi-cli" {
		t.Errorf("brew args = %v, want [upgrade uzi-cli]", call.args)
	}
}

func TestRunPendingUpgradeNilSeam(t *testing.T) {
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Brew: nil}
	if err := runPendingUpgrade(env, []string{"upgrade", "uzi-cli"}); err == nil {
		t.Error("a nil brew seam must return an error, not panic")
	}
}
