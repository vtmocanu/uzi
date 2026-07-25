package main

import (
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	lipgloss "charm.land/lipgloss/v2"
)

// D7's render layer: every untrusted string the TUI draws passes through sanitizeTTY
// BEFORE Glamour, and model-authored free text is drawn inside chrome the UI owns.
//
// THE ORDER IS A FUNCTIONAL REQUIREMENT, NOT A CONVENTION. Glamour EMITS the ANSI
// escapes that make styled output work, so sanitizing after it strips its own styling
// and leaves the literal SGR text on screen. Measured on glamour v2.0.1:
// sanitize→render yields styled output carrying hundreds of escapes, render→sanitize
// yields zero escapes and visible `[38;5;39;1m##` garbage. Getting it backwards is a
// broken screen rather than a silent hole, which is what makes it testable —
// TestTUIRenderOrderIsSanitizeThenGlamour pins it.

// tuiRenderer wraps a Glamour renderer with the D7 ordering baked in, so a caller
// cannot reach the markdown renderer without passing through the sanitizer first.
// That is the point: a bare *glamour.TermRenderer in the model would let any later
// render path skip the step.
type tuiRenderer struct {
	md *glamour.TermRenderer
}

// newTUIRenderer builds the markdown renderer for a given width and background.
//
// The style is chosen explicitly from the already-known background rather than by
// letting Glamour auto-detect: the TUI learns dark-vs-light from Bubble Tea's
// BackgroundColorMsg, and a second independent detection could disagree with the
// palette the rest of the frame is drawn in.
func newTUIRenderer(width int, dark bool) (*tuiRenderer, error) {
	style := styles.LightStyle
	if dark {
		style = styles.DarkStyle
	}
	if width < 20 {
		width = 20
	}
	md, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(style),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}
	return &tuiRenderer{md: md}, nil
}

// Markdown renders untrusted markdown for the transcript: sanitize, THEN Glamour.
//
// On a render error it falls back to the sanitized PLAIN text rather than the raw
// input. A failure here must never become a hole — the whole reason this path exists
// is that the input is attacker-influenceable.
func (r *tuiRenderer) Markdown(s string) string {
	clean := sanitizeTTY(s)
	if r == nil || r.md == nil {
		return clean
	}
	out, err := r.md.Render(clean)
	if err != nil {
		return clean
	}
	return strings.TrimRight(out, "\n")
}

// Plain renders untrusted text that must NOT go through markdown at all — lane labels,
// issue titles, table cells. compactText folds newlines and tabs and caps the length,
// which sanitizeTTY alone does not do: D7 names only sanitizeTTY, and for a
// fixed-width cell that is insufficient.
func (r *tuiRenderer) Plain(s string, maxRunes int) string {
	return capCell(cellText(s), maxRunes)
}

// provenanceBox draws untrusted free text inside a bordered box titled with WHERE the
// text came from.
//
// Sanitizing cannot solve structural spoofing: judge text of "# VERDICT: APPROVED"
// survives sanitizeTTY intact and renders as a genuine heading, because it IS valid
// markdown — and stripping markdown structure would defeat using Glamour at all. So
// the defence is ownership of the chrome: the border and its label are drawn by the
// UI, outside the region the untrusted string can reach, and the reader can always see
// which frame a heading is inside.
func provenanceBox(title, body string, width int, pal palette) string {
	if width < 10 {
		width = 10
	}
	inner := width - 2
	label := pal.boxTitle.Render(" " + cellText(title) + " ")
	return pal.box.Width(inner).Render(label + "\n" + body)
}

// palette is the TUI's colour set, resolved once from the terminal background.
//
// It is built with lipgloss.LightDark and NOT with AdaptiveColor: in lipgloss v2
// AdaptiveColor survives only in the compat shim, driven by a package-level
// HasDarkBackground evaluated at import time against os.Stdin/Stdout. That is wrong
// for a Bubble Tea program (which owns the terminal and is told the background by
// BackgroundColorMsg) and it fires a terminal query even when there is no TTY.
type palette struct {
	dark     bool
	title    lipgloss.Style
	faint    lipgloss.Style
	sel      lipgloss.Style
	box      lipgloss.Style
	boxTitle lipgloss.Style
	states   map[crewState]lipgloss.Style
}

func newPalette(dark bool) palette {
	ld := lipgloss.LightDark(dark)
	p := palette{dark: dark}
	p.title = lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("#005f87"), lipgloss.Color("#7fd6ff")))
	p.faint = lipgloss.NewStyle().Foreground(ld(lipgloss.Color("#6c6c6c"), lipgloss.Color("#8a8a8a")))
	p.sel = lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("#000000"), lipgloss.Color("#ffffff")))
	p.box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(ld(lipgloss.Color("#9e9e9e"), lipgloss.Color("#585858"))).Padding(0, 1)
	p.boxTitle = lipgloss.NewStyle().Bold(true).
		Foreground(ld(lipgloss.Color("#8700af"), lipgloss.Color("#d7afff")))
	p.states = map[crewState]lipgloss.Style{
		crewWorking: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("#00875f"), lipgloss.Color("#5fd7a7"))),
		crewStalled: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("#af5f00"), lipgloss.Color("#ffaf5f"))),
		crewWaiting: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("#005faf"), lipgloss.Color("#87d7ff"))),
		crewIdle:    p.faint,
		crewDone:    p.faint,
	}
	return p
}

func (p palette) state(s crewState) lipgloss.Style {
	if st, ok := p.states[s]; ok {
		return st
	}
	return p.faint
}
