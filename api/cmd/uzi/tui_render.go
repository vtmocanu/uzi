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

// tuiGlamourStyle is the stock dark/light glamour style with ONE retune (PRD #325 M6, revised):
// inline `code` ships as xterm colour 203 (#ff5f5f) on a filled dark BACKGROUND, which both
// reads as an ERROR in a status UI and violates the ANDON rule that only ONE surface on screen
// is ever filled (the amber attention band). Drop the background entirely and recolour the span
// to the tungsten accent, so a code word reads as code by its COLOUR, not by a pill — the same
// two-intensity treatment (tungsten over faint) the rest of the frame uses. Only the inline Code
// style is touched; CodeBlock (fenced) and everything else are left as glamour ships them.
//
// The StyleConfig is a value copy of the package default, and Code is a value field, so
// mutating cfg.Code here changes only this copy — the shared styles.*Config global is untouched.
func tuiGlamourStyle(dark bool) ansi.StyleConfig {
	// The stock inline-code style pads with a leading/trailing space (Prefix/Suffix " ") to give
	// the filled pill breathing room; with the background gone those become stray spaces around
	// the word, so clear them too.
	if dark {
		cfg := styles.DarkStyleConfig
		c := "#c9a061" // tungsten (dark), matching newPalette
		cfg.Code.Color = &c
		cfg.Code.BackgroundColor = nil
		cfg.Code.Prefix, cfg.Code.Suffix = "", ""
		return cfg
	}
	cfg := styles.LightStyleConfig
	c := "#7c5200" // tungsten (light), matching newPalette
	cfg.Code.Color = &c
	cfg.Code.BackgroundColor = nil
	cfg.Code.Prefix, cfg.Code.Suffix = "", ""
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

// palette is the TUI's ANDON colour set ("the factory at night"), resolved once from the
// terminal background. The board is dark and quiet when the factory is healthy and exactly
// ONE thing on screen is ever a filled surface — the amber band/row that needs a human.
// Everything else is foreground ink over one warm material at two intensities: tungsten
// (chrome) and andon amber (attention).
//
// It is built with lipgloss.LightDark and NOT with AdaptiveColor: in lipgloss v2
// AdaptiveColor survives only in the compat shim, driven by a package-level
// HasDarkBackground evaluated at import time against os.Stdin/Stdout. That is wrong
// for a Bubble Tea program (which owns the terminal and is told the background by
// BackgroundColorMsg) and it fires a terminal query even when there is no TTY.
type palette struct {
	dark     bool
	title    lipgloss.Style // tungsten bold: wordmark, eyebrows, key-hint letters, pane focus bar
	faint    lipgloss.Style // ids, ages, chrome, whole done rows
	sel      lipgloss.Style // tungsten accent: the ▸ cursor and the selected id
	box      lipgloss.Style // structural boxes (quit modal, confirm) — bordered, never filled
	boxTitle lipgloss.Style // tungsten bold box label
	states   map[crewState]lipgloss.Style

	// The ANDON semantic colours, held as raw color.Color so a caller can compose a
	// foreground word, a spine glyph or (for the amber band alone) a fill on the fly.
	// stateColor/stateToken resolve a run's colour from these; nothing else maps a status
	// to a colour, so the board row, the board strip and the detail header cannot disagree.
	tungsten color.Color // chrome / selection accent / eyebrows
	amber    color.Color // andon attention: needs-you, the gate band, ⚑ ✎
	alarm    color.Color // failed, judge "issues", high severity
	stall    color.Color // stalled / looping / slow health
	sage     color.Color // running (deliberately dimmer — running is normal)
	indigo   color.Color // planning
	wait     color.Color // rate-limited, crew-waiting, reconnecting
	faintC   color.Color // ids, ages, done, unknown states
	selBg    color.Color // warm-tinted full-row cursor bar
	bandFg   color.Color // near-bg ink drawn on the one filled surface (the amber band)
}

func newPalette(dark bool) palette {
	ld := lipgloss.LightDark(dark)
	// The ANDON tokens. Light value first, dark second, matching ld(light, dark).
	tungsten := ld(lipgloss.Color("#7c5200"), lipgloss.Color("#c9a061"))
	amber := ld(lipgloss.Color("#b45309"), lipgloss.Color("#ffb454"))
	alarm := ld(lipgloss.Color("#b91c1c"), lipgloss.Color("#f87171"))
	stall := ld(lipgloss.Color("#c2410c"), lipgloss.Color("#fb923c"))
	sage := ld(lipgloss.Color("#2f7d4f"), lipgloss.Color("#6fbf8f"))
	indigo := ld(lipgloss.Color("#4f46e5"), lipgloss.Color("#818cf8"))
	wait := ld(lipgloss.Color("#0369a1"), lipgloss.Color("#38bdf8"))
	faintC := ld(lipgloss.Color("#6c6c6c"), lipgloss.Color("#8a8a8a"))
	selBg := ld(lipgloss.Color("#f3ead8"), lipgloss.Color("#33302a"))
	// Near-bg ink for the amber band: light amber is dark, so white reads on it; dark amber
	// is light, so near-black reads on it.
	bandFg := ld(lipgloss.Color("#ffffff"), lipgloss.Color("#0e1016"))

	p := palette{
		dark:     dark,
		tungsten: tungsten, amber: amber, alarm: alarm, stall: stall, sage: sage,
		indigo: indigo, wait: wait, faintC: faintC, selBg: selBg, bandFg: bandFg,
	}
	p.title = lipgloss.NewStyle().Bold(true).Foreground(tungsten)
	p.faint = lipgloss.NewStyle().Foreground(faintC)
	p.sel = lipgloss.NewStyle().Bold(true).Foreground(tungsten)
	p.box = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
		BorderForeground(tungsten).Padding(0, 1)
	p.boxTitle = lipgloss.NewStyle().Bold(true).Foreground(tungsten)
	p.states = map[crewState]lipgloss.Style{
		crewWorking: lipgloss.NewStyle().Foreground(sage),
		crewStalled: lipgloss.NewStyle().Foreground(stall),
		crewWaiting: lipgloss.NewStyle().Foreground(wait),
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

// stateGlyphWord is the DISPLAY vocabulary for a run's state — a single-cell glyph and a
// human word — shared by the board row, the board strip and the detail header so they can
// never disagree (the baseline rendered `stalled` differently on the board and the header;
// this is the single source that fixes that class of drift). The underlying status/health
// strings still drive all logic, filtering and sorting; this only changes what is shown.
//
// HEALTH OVERRIDE: a WARN health flag (stalled/looping/slow) replaces the status token
// entirely with ▲ + the health word, because a run that needs attention is what the board
// is FOR. ok/empty health shows the status token.
func stateGlyphWord(status, health string, isPlanning bool) (glyph, word string) {
	if stalledHealth[health] {
		return "▲", health
	}
	switch effectiveRunStatus(status, isPlanning) {
	case "running":
		return "●", "running"
	case "planning":
		return "○", "planning"
	case "awaiting_approval":
		return "⚑", "plan gate"
	case "awaiting_input":
		return "✎", "needs input"
	case statusLimitWait:
		return "~", "rate-limited"
	case "completed":
		return "✓", "done"
	case "failed":
		return "✗", "failed"
	default: // queued, claimed, cancelled, unknown
		// Status is a uzi-controlled closed enum (not a D7 hole), but keep the defensive posture
		// the old code had: strip any control bytes rather than drawing the raw status here.
		return "·", cellText(strings.ToLower(status))
	}
}

// stateColor is the colour half of the state token, resolved from the same ANDON tokens.
// Health warn wins (→ stall orange); otherwise the status maps to its material.
func (p palette) stateColor(status, health string, isPlanning bool) color.Color {
	if stalledHealth[health] {
		return p.stall
	}
	switch effectiveRunStatus(status, isPlanning) {
	case "running":
		return p.sage
	case "planning":
		return p.indigo
	case "awaiting_approval", "awaiting_input":
		return p.amber
	case statusLimitWait:
		return p.wait
	case "failed":
		return p.alarm
	default: // completed, queued, claimed, cancelled, unknown
		return p.faintC
	}
}

// runToken is the whole state token: glyph + human word + colour.
type runToken struct {
	glyph string
	word  string
	color color.Color
}

// stateToken is the ONE shared helper the design mandates: (glyph, colour, word) from a
// run's status/health/planning, used on the board row, the board strip and the detail
// header so they cannot render one run three ways.
func (p palette) stateToken(status, health string, isPlanning bool) runToken {
	g, w := stateGlyphWord(status, health, isPlanning)
	return runToken{glyph: g, word: w, color: p.stateColor(status, health, isPlanning)}
}

// verdictColor maps a judge verdict to a severity colour: issues → alarm red, everything
// else (ideal/ok/unknown) → faint. The old completed-teal is retired — a ✓ plus faint
// already says "done" — so good news no longer competes for attention with a real alarm.
// Shared by the board's ⚖ marker and the review overlay's verdict word.
func (p palette) verdictColor(verdict string) color.Color {
	if verdict == "issues" {
		return p.alarm
	}
	return p.faintC
}
