package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The board footer carries an ALWAYS-ON compact CLI-vs-server version readout (issue #687,
// superseding #681's conditional skew sentence): three-way (behind/equal/ahead), pinned
// bottom-right, gated only by m.showVersion (the off switches). These tests drive it through
// the Update→View seam with no real timers — buildInfoMsg / skewTickMsg are injected directly.
// The readout renders from m.serverVersion whenever m.showVersion is set; skewCheck governs
// only the auto-probe, so a directly-constructed model with showVersion=true and an injected
// buildInfoMsg exercises the draw path without ever probing.

// showModel builds a board model wide enough (width 200) that the full readout fits alongside
// the help legend, with showVersion=true so the readout renders. skewCheck stays false, so no
// auto-probe fires from the deterministic test path.
func showModel(t *testing.T) tuiModel {
	t.Helper()
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width = 200
	m.showVersion = true
	return m
}

func footerText(m tuiModel) string {
	return stripANSI(m.View().Content)
}

// alarmRedPrefix derives the leading SGR escape the palette's alarm colour renders, so the
// raw-view colour assertions track the palette automatically rather than hardcoding a code.
func alarmRedPrefix(m tuiModel) string {
	wantRed := lipgloss.NewStyle().Foreground(m.pal.alarm).Render("X")
	return wantRed[:strings.Index(wantRed, "m")+1]
}

// 1. Equal (v0.63.0 / 0.63.0): the client version shows, neutral (no alarm red), and the bare
// server string does not appear a second time — an in-sync readout is one neutral version.
func TestTUIFooterVersionEqual(t *testing.T) {
	withVersion(t, "v0.63.0")
	m := showModel(t)
	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)

	out := footerText(m)
	if !strings.Contains(out, "v0.63.0") {
		t.Fatalf("expected the client version v0.63.0 in the footer\n%s", out)
	}
	// The client is "v0.63.0" (one occurrence of the "0.63.0" substring). A second occurrence
	// would mean the server number was rendered too, which an equal readout must not do.
	if n := strings.Count(out, "0.63.0"); n != 1 {
		t.Errorf("equal readout should show one version only; %q occurred %d times\n%s", "0.63.0", n, out)
	}
	if raw := m.View().Content; strings.Contains(raw, alarmRedPrefix(m)) {
		t.Errorf("equal readout must not be alarm-red\n%s", raw)
	}
}

// 2. Behind (v0.1.0 / 0.63.0): both versions show, and the readout is alarm-red.
func TestTUIFooterVersionBehind(t *testing.T) {
	withVersion(t, "v0.1.0")
	m := showModel(t)
	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)

	out := footerText(m)
	if !strings.Contains(out, "v0.1.0") || !strings.Contains(out, "0.63.0") {
		t.Fatalf("behind readout should name both client v0.1.0 and server 0.63.0\n%s", out)
	}
	if raw := m.View().Content; !strings.Contains(raw, alarmRedPrefix(m)) {
		t.Errorf("behind readout must be alarm-red\n%s", raw)
	}
}

// 3. Ahead (v0.64.0 / 0.63.0): both versions show, neutral (never alarm) — a dev laptop ahead
// of a stable deployment is normal and there is nothing to act on.
func TestTUIFooterVersionAhead(t *testing.T) {
	withVersion(t, "v0.64.0")
	m := showModel(t)
	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)

	out := footerText(m)
	if !strings.Contains(out, "v0.64.0") || !strings.Contains(out, "0.63.0") {
		t.Fatalf("ahead readout should name both client v0.64.0 and server 0.63.0\n%s", out)
	}
	if raw := m.View().Content; strings.Contains(raw, alarmRedPrefix(m)) {
		t.Errorf("ahead readout must not be alarm-red\n%s", raw)
	}
}

// 4. Dev build (version=dev, showVersion=true, no probe): the client `dev` shows alone, with no
// server digits, and skewCheck stays false so no auto-probe is armed.
func TestTUIFooterVersionDevBuild(t *testing.T) {
	withVersion(t, "dev")
	m := showModel(t)
	// No buildInfoMsg at all. Scope the digit check to the footer LINE, not the whole view
	// (the empty board draws "0 runs"); the help legend itself carries no digits.
	line := stripANSI(m.boardFooterLine())
	if !strings.Contains(line, "dev") {
		t.Fatalf("a dev build should still show its client version `dev`\n%s", line)
	}
	if strings.ContainsAny(line, "0123456789") {
		t.Errorf("a dev build with no probe must show no server digits\n%s", line)
	}
	if m.skewCheck {
		t.Error("skewCheck must be false on the test path so no auto-probe fires for a dev build")
	}
}

// 5. Server unknown (showVersion=true, v0.63.0, no probe): the client version shows, neutral,
// with no arrow glyph — there is nothing to compare against yet.
func TestTUIFooterVersionServerUnknown(t *testing.T) {
	withVersion(t, "v0.63.0")
	m := showModel(t)
	// No buildInfoMsg: serverVersion stays empty.
	out := footerText(m)
	if !strings.Contains(out, "v0.63.0") {
		t.Fatalf("expected the client version v0.63.0 with no server probed yet\n%s", out)
	}
	for _, arrow := range []string{"⇢", "⇠", "->", "<-"} {
		if strings.Contains(out, arrow) {
			t.Errorf("no arrow expected before any server probe, found %q\n%s", arrow, out)
		}
	}
	if raw := m.View().Content; strings.Contains(raw, alarmRedPrefix(m)) {
		t.Errorf("an unprobed readout must be neutral\n%s", raw)
	}
}

// 6. Off switch (showVersion=false): the footer line equals the plain help legend — no readout,
// no reserved space.
func TestTUIFooterVersionOffSwitch(t *testing.T) {
	withVersion(t, "v0.1.0")
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width = 200
	m.showVersion = false
	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)

	line := clampVisual(m.boardFooterLine(), m.width)
	if stripANSI(line) != stripANSI(m.boardFooter()) {
		t.Errorf("off switch must render the plain help legend with no readout\ngot:  %q\nwant: %q",
			stripANSI(line), stripANSI(m.boardFooter()))
	}
}

// 7. Ascii profile, behind: the meaning is carried by TEXT, so the ascii arrow and the
// "(behind)" marker survive SGR stripping (NO_COLOR / Ascii).
func TestTUIFooterVersionAsciiProfile(t *testing.T) {
	withVersion(t, "v0.1.0")
	m := showModel(t)
	m.profile = colorprofile.Ascii
	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)

	out := footerText(m)
	if !strings.Contains(out, "->") {
		t.Errorf("ascii readout should use the -> arrow\n%s", out)
	}
	if !strings.Contains(out, "(behind)") {
		t.Errorf("ascii readout should carry the (behind) marker in text\n%s", out)
	}
}

// 8. Auto-refresh: starting equal (neutral), a later probe finding the server rolled forward
// lights the readout alarm-red — without a restart.
func TestTUIFooterVersionAutoRefresh(t *testing.T) {
	withVersion(t, "v0.63.0")
	m := showModel(t)

	next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
	m = next.(tuiModel)
	if raw := m.View().Content; strings.Contains(raw, alarmRedPrefix(m)) {
		t.Fatalf("no alarm expected while CLI equals server\n%s", raw)
	}

	next, _ = m.Update(buildInfoMsg{version: "0.99.0"})
	m = next.(tuiModel)
	raw := m.View().Content
	if !strings.Contains(raw, alarmRedPrefix(m)) {
		t.Fatalf("alarm expected after the server rolled forward to 0.99.0 (no restart)\n%s", raw)
	}
	if !strings.Contains(stripANSI(raw), "0.99.0") {
		t.Fatalf("the refreshed server version 0.99.0 should render\n%s", stripANSI(raw))
	}

	// A failed re-probe must preserve the last-known-good version, so the readout stays.
	next, _ = m.Update(buildInfoMsg{version: "", err: errFake("probe failed")})
	m = next.(tuiModel)
	if m.serverVersion != "0.99.0" {
		t.Errorf("a failed probe must preserve last-known-good serverVersion, got %q", m.serverVersion)
	}
}

// 9. Narrow width, behind: the readout degrades to client-only rather than overflowing, and the
// footer line never exceeds the terminal width.
func TestTUIFooterVersionNarrowWidth(t *testing.T) {
	withVersion(t, "v0.1.0")
	for _, w := range []int{40, 20} {
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.width = w
		m.showVersion = true
		next, _ := m.Update(buildInfoMsg{version: "0.63.0"})
		m = next.(tuiModel)

		// The renderBoard call site wraps the footer line with clampVisual(..., m.width); mirror it.
		line := clampVisual(m.boardFooterLine(), m.width)
		if got := visualWidth(stripANSI(line)); got > w {
			t.Errorf("width %d: footer line visual width %d overflows\n%q", w, got, stripANSI(line))
		}
		if !strings.Contains(stripANSI(line), "v0.1.0") {
			t.Errorf("width %d: narrow readout should keep the client version at the right edge\n%q", w, stripANSI(line))
		}
	}
}

// The skew ticker's self-sustaining loop is the mechanism behind AC3 ("lights within one
// interval, no restart"): each skewTickMsg must BOTH re-probe the server version AND re-arm
// the ticker. A bare cmd != nil check under-gates this — dropping just the re-arm would leave
// the fetch in the batch and still pass — so decompose the batch and assert both sub-commands,
// mirroring TestTUIStripTickRefetchesMeters.
func TestTUIFooterSkewTickRearms(t *testing.T) {
	// Shrink the cadence so the re-arm (tea.Tick) fires promptly instead of in 5m.
	orig := skewPollInterval
	skewPollInterval = time.Millisecond
	t.Cleanup(func() { skewPollInterval = orig })

	fake := &uzicli.FakeClient{Build: apitypes.BuildInfoDTO{Version: "0.99.0"}}
	m := tuiTestModel(t, fake, "")

	_, cmd := m.Update(skewTickMsg{})
	if cmd == nil {
		t.Fatal("skewTickMsg returned no command; the readout will never refresh on its own ticker")
	}
	// One cmd() call: a tea.Cmd is single-shot, and a lone tea.Tick (the shape of a
	// dropped-re-probe regression) blocks forever on a second call — so reuse msg in the
	// diagnostic rather than re-invoking cmd().
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("skewTickMsg command yielded %T, want a tea.BatchMsg", msg)
	}

	var sawProbe, sawRearm bool
	for _, inner := range batch {
		if inner == nil {
			continue
		}
		switch im := inner().(type) {
		case buildInfoMsg:
			// Non-vacuous: assert the fake's version actually reached the message, not merely
			// that the batch carried a buildInfoMsg.
			if im.version != "0.99.0" {
				t.Errorf("buildInfoMsg carried version %q, want the fake's 0.99.0", im.version)
			}
			sawProbe = true
		case skewTickMsg:
			sawRearm = true
		}
	}
	if !sawProbe {
		t.Error("the skewTickMsg batch did not re-probe the server version; the readout would freeze at its launch value")
	}
	if !sawRearm {
		t.Error("the skewTickMsg batch did not re-arm the skew ticker; it would fire only once (AC3 refresh breaks)")
	}
}

// errFake is a stand-in error for a failed probe; any non-nil error exercises the failure
// arm of the buildInfoMsg handler.
type errFake string

func (e errFake) Error() string { return string(e) }
