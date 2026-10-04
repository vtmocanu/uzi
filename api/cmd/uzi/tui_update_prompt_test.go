package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1251 M1 — the codex-style startup update prompt. Model tests are deterministic
// and offline; the brew shell-out is exercised through the injected seam.

// updatePromptModel builds a model allowed to probe (skewCheck + showVersion), the state
// newTUICmd's RunE sets on the real path, so the update-prompt gate can fire in a test.
func updatePromptModel(t *testing.T) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.skewCheck = true
	m.showVersion = true
	m.updatePrompt.brewKnown = true
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
	calls        []brewCall
	stablePrefix string
	rcPrefix     string
	prefixErr    error
}

type brewCall struct {
	foreground bool
	args       []string
}

func (b *brewRec) fn(foreground bool, args ...string) (string, error) {
	b.calls = append(b.calls, brewCall{foreground: foreground, args: append([]string(nil), args...)})
	switch {
	case len(args) == 3 && args[0] == "--prefix" && args[1] == "--installed":
		if args[2] == "uzi-cli" {
			return b.stablePrefix, b.prefixErr
		}
		if args[2] == "uzi-cli-rc" {
			return b.rcPrefix, b.prefixErr
		}
		return "", nil
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

// TestUpdatePromptNeverOffersPrerelease guards the stable prompt against any prerelease,
// whether its base is higher than or equal to the CLI.
func TestIsRCTagExactPrerelease(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"v0.85.0-rc.1", true},
		{"v0.85.0+build-rc.1", false},
		{"v0.85.0-rc.1+build", false},
		{"v0.85.0-rc.01", false},
		{"v00.85.0-rc.1", false},
		{"0.85.0-rc.1", false},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			if got := isRCTag(tc.tag); got != tc.want {
				t.Errorf("isRCTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

func TestUpdatePromptNeverOffersPrerelease(t *testing.T) {
	withVersion(t, "v0.83.0")
	for _, latest := range []string{"v0.85.0-rc.1", "v0.83.0-rc.2", "v0.90.0-rc.3", "v0.85.0-beta.1", "v0.85.0-alpha.1"} {
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
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli"})
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

// Compare the version/body pair only: notes and the dismissal label may repeat the tag.
func TestUpdatePromptRenderReleaseName(t *testing.T) {
	withVersion(t, "v0.83.0")
	for _, channel := range []struct {
		name  string
		tag   string
		owner string
	}{
		{name: "stable", tag: "v0.85.0", owner: "uzi-cli"},
		{name: "rc", tag: "v0.85.0-rc.1", owner: "uzi-cli-rc"},
	} {
		t.Run(channel.name, func(t *testing.T) {
			longTag := channel.tag + strings.Repeat("x", 90)
			for _, tc := range []struct {
				name    string
				tag     string
				release string
				body    string // empty means the variant-specific fallback
			}{
				{name: "equal", tag: channel.tag, release: channel.tag},
				{name: "BEL", tag: channel.tag, release: "\x07" + channel.tag},
				{name: "SOH", tag: channel.tag, release: channel.tag + "\x01"},
				{name: "edge_whitespace", tag: channel.tag, release: " \t" + channel.tag + "\n "},
				{name: "bidi", tag: channel.tag, release: string(rune(0x202e)) + channel.tag},
				{name: "both_sanitized", tag: "\x01" + channel.tag + " ", release: "\x07" + channel.tag},
				{name: "long_equal", tag: longTag, release: longTag},
				{name: "distinct", tag: channel.tag, release: "\x07Highlights\x01", body: "Highlights"},
				{name: "empty", tag: channel.tag},
				{name: "sanitized_empty", tag: channel.tag, release: " \x07\x01 "},
			} {
				t.Run(tc.name, func(t *testing.T) {
					for _, security := range []bool{false, true} {
						t.Run(fmt.Sprintf("security=%t", security), func(t *testing.T) {
							for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.Ascii} {
								t.Run(fmt.Sprintf("profile=%v", profile), func(t *testing.T) {
									m := showingUpdateModel(t, updatePromptState{
										latestVersion:  tc.tag,
										latestName:     tc.release,
										latestNotesURL: "https://example.com/releases/" + channel.tag,
										security:       security,
									})
									m.width = 240 // keep the long version and dismissal label on one line
									next, _ := m.Update(tea.ColorProfileMsg{Profile: profile})
									m = next.(tuiModel)
									out := stripANSI(m.View().Content)
									body := tc.body
									if body == "" {
										body = "A newer release is available."
										if security {
											body = "A security update is available."
										}
									}
									lines := strings.Split(out, "\n")
									found := false
									for i, line := range lines {
										if !strings.Contains(line, "uzi v0.83.0") {
											continue
										}
										found = true
										if i+1 >= len(lines) {
											t.Fatalf("missing body after version line\n%s", out)
										}
										if got := strings.Trim(lines[i+1], " │"); got != body {
											t.Errorf("body = %q, want %q", got, body)
										}
										tag := channel.tag
										if tc.name == "long_equal" {
											tag = longTag
										}
										if got := strings.Count(line+"\n"+lines[i+1], tag); got != 1 {
											t.Errorf("version/body pair contains tag %d times, want once\n%s\n%s", got, line, lines[i+1])
										}
									}
									if !found {
										t.Fatalf("missing version line\n%s", out)
									}
									for _, want := range []string{
										"Don't remind me for " + strings.TrimSpace(strings.TrimPrefix(tc.tag, "\x01")),
										"Release notes: https://example.com/releases/" + channel.tag,
									} {
										if !strings.Contains(out, want) {
											t.Errorf("prompt missing %q\n%s", want, out)
										}
									}
									// Also retain the brew action for each channel.
									m.updatePrompt.owner = channel.owner
									if got := stripANSI(m.View().Content); !strings.Contains(got, "brew upgrade "+channel.owner) {
										t.Errorf("prompt missing brew action\n%s", got)
									}
								})
							}
						})
					}
				})
			}
		})
	}
}

func TestUpdatePromptRenderSecurityBand(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli", security: true})
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
		owner:          "",
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
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli", sel: 0})
	out := m.View().Content
	cursor := paintSeg(m.pal.tungsten, m.pal.selBg, true, "▸ ")
	if !strings.Contains(out, cursor) {
		t.Errorf("the selected row must draw a bold tungsten ▸ cursor over the selBg fill\nwant %q\n%s", cursor, out)
	}
}

func TestUpdatePromptRenderAsciiFallback(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli", security: true})
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
		owner:          "", // draws the notes URL
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

func TestBrewOwnerAndPromptChannels(t *testing.T) {
	for _, tc := range []struct{ name, current, stable, rc, owner, want string }{
		{"rc owner selects newer stable", "v0.84.0-rc.1", "v0.90.0", "v0.84.0-rc.2", "uzi-cli-rc", "v0.90.0"},
		{"rc offers stable without rc", "v0.85.0-rc.11", "v0.85.0", "", "uzi-cli-rc", "v0.85.0"},
		{"rc selects higher rc", "v0.85.0-rc.11", "v0.85.0", "v0.86.0-rc.1", "uzi-cli-rc", "v0.86.0-rc.1"},
		{"stable binary on rc channel offers rc", "v0.85.0", "v0.85.0", "v0.86.0-rc.1", "uzi-cli-rc", "v0.86.0-rc.1"},
		{"stable binary on rc channel offers stable", "v0.85.0", "v0.86.0", "", "uzi-cli-rc", "v0.86.0"},
		{"rc already on stable", "v0.85.0", "v0.85.0", "v0.85.0-rc.11", "uzi-cli-rc", ""},
		{"rc rejects malformed stable", "v0.85.0-rc.11", "v0.90.0.1", "", "uzi-cli-rc", ""},
		{"rc rejects beta stable fact", "v0.85.0-rc.11", "v0.90.0-beta.1", "", "uzi-cli-rc", ""},
		{"rc stable fact must be stable", "v0.85.0-rc.11", "v0.90.0-rc.1", "", "uzi-cli-rc", ""},
		{"rc ignores malformed rc when stable valid", "v0.85.0-rc.11", "v0.85.0", "v0.90.0-rc.01", "uzi-cli-rc", "v0.85.0"},
		{"rc ignores malformed stable when rc valid", "v0.85.0-rc.11", "dev", "v0.86.0-rc.1", "uzi-cli-rc", "v0.86.0-rc.1"},
		{"stable accepts version metadata", "v0.83.0", "v0.85.0+build", "", "uzi-cli", "v0.85.0+build"},
		{"stable ignores rc", "v0.83.0", "", "v0.90.0-rc.1", "uzi-cli", ""},
		{"stable rejects beta", "v0.83.0", "v0.85.0-beta.1", "", "uzi-cli", ""},
		{"unknown stable rejects beta", "v0.83.0", "v0.85.0-beta.1", "", "", ""},
		{"rc rejects invalid rc", "v0.84.0-rc.1", "", "v0.85.0-rc.01", "uzi-cli-rc", ""},
		{"rc rejects extended rc", "v0.84.0-rc.1", "", "v0.85.0-rc.1.extra", "uzi-cli-rc", ""},
		{"rc rejects beta", "v0.84.0-rc.1", "", "v0.85.0-beta.1", "uzi-cli-rc", ""},
		{"stable rejects malformed", "v0.83.0", "v0.85.0-beta.01", "", "uzi-cli", ""},
		{"unknown rc offers stable notes", "v0.85.0-rc.11", "v0.85.0", "", "", "v0.85.0"},
		{"unknown rc offers newer rc notes", "v0.85.0-rc.11", "v0.85.0", "v0.86.0-rc.1", "", "v0.86.0-rc.1"},
		{"handbuilt with both installed", "v0.83.0", "v0.85.0", "v0.90.0-rc.1", "", "v0.85.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, tc.current)
			m := updatePromptModel(t)
			m.updatePrompt.brewKnown = false
			dir := t.TempDir()
			stableDir := filepath.Join(dir, "Cellar", "uzi-cli", "1")
			rcDir := filepath.Join(dir, "Cellar", "uzi-cli-rc", "1")
			if err := os.MkdirAll(filepath.Join(stableDir, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(rcDir, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(dir, "handbuilt", "uzi")
			if tc.owner == "uzi-cli" {
				exe = filepath.Join(stableDir, "bin", "uzi")
			}
			if tc.owner == "uzi-cli-rc" {
				exe = filepath.Join(rcDir, "bin", "uzi")
			}
			if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exe, nil, 0600); err != nil {
				t.Fatal(err)
			}
			b := &brewRec{stablePrefix: stableDir, rcPrefix: rcDir}
			m.brew, m.executable = b.fn, func() (string, error) { return exe, nil }
			msg := buildInfoMsg{}
			if tc.stable != "" {
				msg.latest = &apitypes.LatestReleaseDTO{Version: tc.stable}
			}
			if tc.rc != "" {
				msg.latestRC = &apitypes.LatestReleaseDTO{Version: tc.rc}
			}
			next, cmd := m.Update(msg)
			m = next.(tuiModel)
			if cmd == nil || m.updatePrompt.showing || m.updatePrompt.shownThisSession {
				t.Fatal("probe must precede comparison and latch")
			}
			next, _ = m.Update(cmd())
			m = next.(tuiModel)
			if m.updatePrompt.owner != tc.owner {
				t.Fatalf("owner = %q, want %q", m.updatePrompt.owner, tc.owner)
			}
			if m.updatePrompt.showing != (tc.want != "") || m.updatePrompt.shownThisSession != (tc.want != "") {
				t.Fatalf("showing = %v, latched = %v, want %q", m.updatePrompt.showing, m.updatePrompt.shownThisSession, tc.want)
			}
			if tc.want == "" && (m.updatePrompt.pendingUpgrade || len(m.updatePrompt.upgradeArgv) != 0) {
				t.Fatalf("rejected target armed upgrade: %v", m.updatePrompt.upgradeArgv)
			}
			if tc.want != "" && m.updatePrompt.latestVersion != tc.want {
				t.Fatalf("target = %q", m.updatePrompt.latestVersion)
			}
			if tc.owner != "" && len(b.calls) != 2 {
				t.Fatalf("probes = %v", b.calls)
			}
			if tc.owner == "" && tc.want != "" && len(m.updateChoices()) != 2 {
				t.Fatal("unknown ownership must be info-only")
			}
			if tc.owner == "uzi-cli-rc" && tc.want != "" {
				if m.updateChoiceLabel(updateChoiceUpdateNow) != "Update now  (brew upgrade uzi-cli-rc)" {
					t.Fatal("wrong RC label")
				}
				next, _ = m.updatePromptKey(keyEnter)
				m = next.(tuiModel)
				if strings.Join(m.updatePrompt.upgradeArgv, " ") != "upgrade uzi-cli-rc" {
					t.Fatalf("argv = %v", m.updatePrompt.upgradeArgv)
				}
			}
		})
	}
}

func TestUpdatePromptKeepsRCFactAcrossFailedBuildProbe(t *testing.T) {
	withVersion(t, "v0.84.0-rc.1")
	m := updatePromptModel(t)
	m.updatePrompt.brewKnown = false
	m.brew = func(bool, ...string) (string, error) { return "", nil }
	m.executable = func() (string, error) { return "", errors.New("unknown executable") }
	next, cmd := m.Update(buildInfoMsg{latestRC: &apitypes.LatestReleaseDTO{Version: "v0.84.0-rc.2"}})
	m = next.(tuiModel)
	if cmd == nil || !m.updatePrompt.brewPending {
		t.Fatal("expected pending ownership probe")
	}
	next, _ = m.Update(buildInfoMsg{err: errors.New("transient failure")})
	m = next.(tuiModel)
	if m.updatePrompt.latestRC == nil {
		t.Fatal("failed build probe cleared the saved RC release")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if !m.updatePrompt.showing || m.updatePrompt.latestVersion != "v0.84.0-rc.2" {
		t.Fatalf("RC prompt lost after failed probe: %+v", m.updatePrompt)
	}
}

func TestUpdatePromptUsesNewRCFactOnLaterPoll(t *testing.T) {
	withVersion(t, "v0.84.0-rc.1")
	m := updatePromptModel(t)
	m.updatePrompt.owner = "uzi-cli-rc"
	next, _ := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.83.0"}})
	m = next.(tuiModel)
	if m.updatePrompt.showing || m.updatePrompt.shownThisSession {
		t.Fatal("no newer release fact must not latch the prompt")
	}
	next, _ = m.Update(buildInfoMsg{latestRC: &apitypes.LatestReleaseDTO{Version: "v0.84.0-rc.2"}})
	m = next.(tuiModel)
	if !m.updatePrompt.showing || m.updatePrompt.latestVersion != "v0.84.0-rc.2" {
		t.Fatalf("later RC fact did not open the prompt: %+v", m.updatePrompt)
	}
}

func TestCappedBrewOutput(t *testing.T) {
	var out cappedBrewOutput
	chunk := []byte(strings.Repeat("x", 40*1024))
	if n, err := out.Write(chunk); n != len(chunk) || err != nil {
		t.Fatalf("first write = %d, %v", n, err)
	}
	if n, err := out.Write(chunk); n != 24*1024 || !errors.Is(err, errBrewProbeOverflow) {
		t.Fatalf("overflow write = %d, %v", n, err)
	}
	if n, err := out.Write(chunk); n != 0 || !errors.Is(err, errBrewProbeOverflow) {
		t.Fatalf("later write = %d, %v", n, err)
	}
	if out.Len() != 64*1024 || !out.didOverflow() {
		t.Fatalf("accepted %d bytes, overflow = %v", out.Len(), out.didOverflow())
	}
}

func TestRealBrewQuietProbe(t *testing.T) {
	dir := t.TempDir()
	brew := filepath.Join(dir, "brew")
	// The fake prints past the cap on both pipes, then hangs if the probe keeps draining.
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"noisy) printf '%40000s' x; printf '%40000s' y >&2; exec sleep 30 ;;\n" +
		"hang) exec sleep 30 ;;\n" +
		"small) printf 'prefix\\n'; printf 'note\\n' >&2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(brew, []byte(script), 0700); err != nil { //nolint:gosec // G306: this temporary fake must be executable by the probe.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	t.Run("overflow cancels promptly", func(t *testing.T) {
		start := time.Now()
		output, err := realBrew(false, "noisy")
		if output != "" || !errors.Is(err, errBrewProbeOverflow) {
			t.Fatalf("realBrew noisy = %q, %v", output, err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("overflow took %s", elapsed)
		}
	})
	t.Run("timeout cancels", func(t *testing.T) {
		start := time.Now()
		output, err := realBrew(false, "hang")
		if output != "" || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("realBrew hang = %q, %v", output, err)
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("timeout took %s", elapsed)
		}
	})
	t.Run("small output", func(t *testing.T) {
		output, err := realBrew(false, "small")
		if err != nil || (output != "prefix\nnote\n" && output != "note\nprefix\n") {
			t.Fatalf("realBrew small = %q, %v", output, err)
		}
	})
}

func TestBrewOwnerSymlinkAndBoundary(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "Cellar", "uzi-cli", "1")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(prefix, "bin", "uzi")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "uzi")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	b := &brewRec{stablePrefix: prefix}
	if got := detectBrewOwner(b.fn, func() (string, error) { return link, nil }); got != "uzi-cli" {
		t.Fatalf("symlink owner = %q", got)
	}
	sibling := filepath.Join(dir, "Cellar", "uzi-cli", "10", "bin", "uzi")
	if err := os.MkdirAll(filepath.Dir(sibling), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := detectBrewOwner(b.fn, func() (string, error) { return sibling, nil }); got != "" {
		t.Fatalf("prefix boundary owner = %q", got)
	}
	b.stablePrefix = dir
	if got := detectBrewOwner(b.fn, func() (string, error) { return target, nil }); got != "" {
		t.Fatalf("generic prefix owner = %q", got)
	}
}

func TestUpdatePromptUpdateNowArmsForegroundUpgrade(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli", sel: 0})
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
	m := showingUpdateModel(t, updatePromptState{latestVersion: "v0.85.0", owner: "uzi-cli"})
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

func TestBuildInfoCarriesLatestRCAndRendersSafely(t *testing.T) {
	withVersion(t, "v0.83.0-rc.1")
	nasty := "\x1b[2J" + string(rune(0x202e)) + "\x07\x01"
	fake := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{
		Version:  "v0.83.0-rc.1",
		Latest:   &apitypes.LatestReleaseDTO{Version: "v0.82.0"},
		LatestRC: &apitypes.LatestReleaseDTO{Version: "v0.83.0-rc.2", Name: nasty + "rcname", NotesURL: nasty + "rcurl"},
	}}
	m := updatePromptModel(t)
	m.client = fake
	msg := m.fetchBuildInfoCmd()().(buildInfoMsg)
	if msg.latestRC == nil || msg.latestRC.Name != nasty+"rcname" {
		t.Fatalf("RC fact lost in buildInfoMsg: %+v", msg.latestRC)
	}
	m.updatePrompt.owner = "uzi-cli-rc"
	next, _ := m.Update(msg)
	m = next.(tuiModel)
	if !m.updatePrompt.showing {
		t.Fatal("RC fact should prompt RC owner")
	}
	out := m.View().Content
	assertNoRawControls(t, "RC update prompt", out)
	for _, marker := range []string{"rcname"} {
		if !strings.Contains(stripANSI(out), marker) {
			t.Fatalf("missing sanitized %s", marker)
		}
	}
}

func TestRunPendingUpgradeRCStatusAndError(t *testing.T) {
	var out, errOut strings.Builder
	env := Env{Stdout: &out, Stderr: &errOut, Brew: func(foreground bool, args ...string) (string, error) {
		if !foreground || strings.Join(args, " ") != "upgrade uzi-cli-rc" {
			t.Fatalf("wrong upgrade call: %v %v", foreground, args)
		}
		return "", fmt.Errorf("compile failed")
	}}
	if err := runPendingUpgrade(env, []string{"upgrade", "uzi-cli-rc"}); err == nil || !strings.Contains(err.Error(), "uzi-cli-rc") {
		t.Fatalf("RC error omitted formula: %v", err)
	}
	if !strings.Contains(out.String(), "Updating uzi-cli-rc via Homebrew") || !strings.Contains(errOut.String(), "compile failed") {
		t.Fatalf("status = %q, stderr = %q", out.String(), errOut.String())
	}
}
