// Package factoryui is the PROPOSED "factory shift board" redesign of the uzi TUI,
// rendered as pure functions plus a small bubbletea model. It is DEMO-ONLY: the shipped
// TUI (api/cmd/uzi/tui_*.go) is untouched. This package is the single source of truth for
// the redesign — both the live demo (../demo) and the static screenshot mocks
// (../../uxlab_mock_test.go) render through it, so they cannot drift.
//
// The design thesis: the board's one job is triage ("which run needs me"), so status is a
// solid colour CHIP plus a colour SPINE down the left edge you scan in one pass; the detail
// view's one job is to watch a crew and act at the gate, so the moment that needs a human
// is an unmissable amber banner, not one blue word among many.
package factoryui

import (
	"image/color"
	"strings"

	lipgloss "charm.land/lipgloss/v2"
)

// laneRailWidth is the detail view's left rail column budget (matches the shipped TUI).
const laneRailWidth = 26

func hex(s string) color.Color { return lipgloss.Color(s) }

// Palette is the redesign's semantic colour set. The key departure from the shipped
// two-colour scheme (brand-blue + grey): status has its OWN colour axis, so running,
// awaiting, paused, completed and failed are told apart at a glance. Both themes defined.
type Palette struct {
	Dark   bool
	brand  color.Color // wordmark + structural accents ONLY
	ink    color.Color // primary text
	muted  color.Color // secondary text
	faint  color.Color // chrome, eyebrows, hints
	rule   color.Color // dividers
	chipFg color.Color // text drawn ON a solid status chip
	status map[string]color.Color
}

// NewPalette resolves the palette for a dark or light terminal.
func NewPalette(dark bool) Palette {
	if dark {
		return Palette{
			Dark:   true,
			brand:  hex("#7fd6ff"),
			ink:    hex("#e6e8ee"),
			muted:  hex("#9aa3b3"),
			faint:  hex("#5b6472"),
			rule:   hex("#2a2f3a"),
			chipFg: hex("#0e1016"),
			status: map[string]color.Color{
				"running":           hex("#4ade80"),
				"claimed":           hex("#7c8698"),
				"queued":            hex("#7c8698"),
				"awaiting_approval": hex("#fbbf24"),
				"awaiting_input":    hex("#fbbf24"),
				"limit_wait":        hex("#38bdf8"),
				"completed":         hex("#5eead4"),
				"failed":            hex("#f87171"),
				"cancelled":         hex("#7c8698"),
				"stalled":           hex("#fb923c"),
			},
		}
	}
	return Palette{
		Dark:   false,
		brand:  hex("#0b6a8f"),
		ink:    hex("#1a1d24"),
		muted:  hex("#5b6472"),
		faint:  hex("#9aa3b0"),
		rule:   hex("#d5d9e0"),
		chipFg: hex("#ffffff"),
		status: map[string]color.Color{
			"running":           hex("#1a7f4b"),
			"claimed":           hex("#6b7280"),
			"queued":            hex("#6b7280"),
			"awaiting_approval": hex("#b45309"),
			"awaiting_input":    hex("#b45309"),
			"limit_wait":        hex("#0369a1"),
			"completed":         hex("#0f766e"),
			"failed":            hex("#b91c1c"),
			"cancelled":         hex("#6b7280"),
			"stalled":           hex("#c2410c"),
		},
	}
}

func (p Palette) statusColor(s string) color.Color {
	if c, ok := p.status[s]; ok {
		return c
	}
	return p.muted
}

func (p Palette) fg(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }

// chip is the signature element: a solid block of colour with near-background text on it,
// so a status reads as a physical tag on the floor rather than coloured prose.
func (p Palette) chip(text string, bg color.Color) string {
	return lipgloss.NewStyle().Background(bg).Foreground(p.chipFg).Bold(true).Padding(0, 1).Render(text)
}

// keycap renders a hint as [k] label — the key bright, the label muted. Used in the vertical
// help list, where the bracket aids scanning.
func (p Palette) keycap(k, label string) string {
	return p.fg(p.brand).Bold(true).Render("["+k+"]") + p.fg(p.muted).Render(" "+label)
}

// hint is the compact footer form: a bright key, a muted label, no bracket. hintbar joins a
// row of them with a faint middot so the whole keymap fits one line.
func (p Palette) hint(k, label string) string {
	return p.fg(p.brand).Bold(true).Render(k) + " " + p.fg(p.muted).Render(label)
}

func (p Palette) hintbar(hints ...string) string {
	return strings.Join(hints, p.fg(p.faint).Render(" · "))
}

func (p Palette) eyebrow(s string) string { return p.fg(p.faint).Render(strings.ToUpper(s)) }

// ---- geometry helpers (mirrors of the shipped TUI's, kept local so this package does not
// import package main) ----

func visualWidth(s string) int { return lipgloss.Width(s) }

func padVisual(s string, n int) string {
	if w := visualWidth(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func capCell(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func padCell(s string, n int) string {
	c := capCell(s, n)
	if pad := n - len([]rune(c)); pad > 0 {
		return c + strings.Repeat(" ", pad)
	}
	return c
}
