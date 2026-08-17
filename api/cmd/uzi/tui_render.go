package main

import (
	"image/color"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
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
	if width < 20 {
		width = 20
	}
	md, err := glamour.NewTermRenderer(
		glamour.WithStyles(tuiGlamourStyle(dark)),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil, err
	}
	return &tuiRenderer{md: md}, nil
}

// tuiGlamourStyle is the stock dark/light glamour style with ONE retune (PRD #325 M6):
// inline `code` ships as xterm colour 203 (#ff5f5f) in BOTH themes, which reads as an
// ERROR in a status UI. Recolour it to a calm, cool neutral on the same code background so
// a code span reads as code, not as a failure. Only the inline Code foreground is touched;
// CodeBlock (fenced) and everything else are left as glamour ships them.
//
// The StyleConfig is a value copy of the package default, and Code is a value field, so
// reassigning cfg.Code.Color mutates only this copy — the shared styles.*Config global is
// untouched.
func tuiGlamourStyle(dark bool) ansi.StyleConfig {
	if dark {
		cfg := styles.DarkStyleConfig
		c := "#b9c0cb" // cool light grey on the #303030-ish code bg
		cfg.Code.Color = &c
		return cfg
	}
	cfg := styles.LightStyleConfig
	c := "#334155" // slate on the light code bg
	cfg.Code.Color = &c
	return cfg
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

	// The RUN-STATUS colour axis (PRD #325 M2), a SEPARATE axis from the crew-lane
	// `states` dots above. statuses maps a run status to its bucket colour; statusStalled
	// is the health override; statusDefault covers unrecognised statuses; chipFg is the
	// text drawn on a solid status chip. Read (never re-populated) by M3's detail header
	// chips and M6's verdict severity via statusColor/chip.
	statuses      map[string]color.Color
	statusStalled color.Color
	statusDefault color.Color
	chipFg        color.Color
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

	// Run-status colour buckets (PRD #325 M2). Light value first (dark bg gets the
	// brighter second value), matching the `ld(light, dark)` convention above.
	p.statuses = map[string]color.Color{
		"running": ld(lipgloss.Color("#1a7f4b"), lipgloss.Color("#4ade80")),
		// issue #321: the pre-approval planning phase, a DERIVED effective status (not a
		// runs.status value). Indigo, matching the web `plan` badge tone — distinct from
		// the green `running` bucket. Light indigo-600 / dark indigo-400.
		"planning":          ld(lipgloss.Color("#4f46e5"), lipgloss.Color("#818cf8")),
		"queued":            ld(lipgloss.Color("#6b7280"), lipgloss.Color("#7c8698")),
		"claimed":           ld(lipgloss.Color("#6b7280"), lipgloss.Color("#7c8698")),
		"awaiting_approval": ld(lipgloss.Color("#b45309"), lipgloss.Color("#fbbf24")),
		"awaiting_input":    ld(lipgloss.Color("#b45309"), lipgloss.Color("#fbbf24")),
		"limit_wait":        ld(lipgloss.Color("#0369a1"), lipgloss.Color("#38bdf8")),
		"completed":         ld(lipgloss.Color("#0f766e"), lipgloss.Color("#5eead4")),
		"failed":            ld(lipgloss.Color("#b91c1c"), lipgloss.Color("#f87171")),
		"cancelled":         ld(lipgloss.Color("#6b7280"), lipgloss.Color("#7c8698")),
	}
	p.statusStalled = ld(lipgloss.Color("#c2410c"), lipgloss.Color("#fb923c"))
	p.statusDefault = ld(lipgloss.Color("#6c6c6c"), lipgloss.Color("#8a8a8a"))
	p.chipFg = ld(lipgloss.Color("#ffffff"), lipgloss.Color("#0e1016"))

	return p
}

func (p palette) state(s crewState) lipgloss.Style {
	if st, ok := p.states[s]; ok {
		return st
	}
	return p.faint
}

// statusColor resolves a run's spine/chip colour (PRD #325 M2 seam), applying the
// status→bucket map and the status-vs-health precedence: health "stalled" overrides the
// status bucket (a stalled run is what triage is FOR), so it wins → orange. Non-stalled
// health does not override. An unrecognised status falls to the default grey bucket, per
// the forward-compat note in docs/cli.md (a newer server may ship a status this CLI has
// no colour for).
func (p palette) statusColor(status, health string) color.Color {
	if health == "stalled" {
		return p.statusStalled
	}
	if c, ok := p.statuses[status]; ok {
		return c
	}
	return p.statusDefault
}

// verdictColor maps a judge verdict to a severity colour (PRD #325 M6): issues → red,
// ideal/ok → the completed teal, anything else → the default grey. Shared by the board's
// ⚖ marker and the review overlay's verdict chip so the two cannot disagree.
func (p palette) verdictColor(verdict string) color.Color {
	switch verdict {
	case "ideal", "ok":
		return p.statusColor("completed", "")
	case "issues":
		return p.statusColor("failed", "")
	default:
		return p.statusDefault
	}
}

// chip renders text as a solid status tag: a filled block of bg with near-background
// text on it, so a status reads as a physical tag rather than coloured prose. Under
// NO_COLOR / an Ascii profile lipgloss strips the fill and the chip degrades to its bold
// text (still legible) — the caller supplies the NO_COLOR-independent signal (the spine
// glyph, a bordered banner) separately.
func (p palette) chip(text string, bg color.Color) string {
	return lipgloss.NewStyle().Background(bg).Foreground(p.chipFg).Bold(true).Padding(0, 1).Render(text)
}
