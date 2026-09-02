package main

// Shared visual/layout/duration helpers for the TUI board and detail views (PRD #1009 M3):
// visual-width padding/truncation, row-segment painting, run-id / cell formatting, and compact
// duration/int formatting.

import (
	"image/color"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// clampVisual truncates s to n visual columns (ANSI- and wide-rune-aware),
// appending an ellipsis when it cuts. It is padVisual's dual: together they hold
// a column to exactly n columns regardless of content, so joinColumns' divider
// sits at one fixed column on every row.
func clampVisual(s string, n int) string {
	if n < 1 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// padVisual pads to n columns ignoring ANSI escapes, which lipgloss has already added
// by this point — counting them as width would shove every row right.
func padVisual(s string, n int) string {
	w := visualWidth(s)
	if w >= n {
		return s
	}
	return s + strings.Repeat(" ", n-w)
}

// visualWidth is lipgloss's own width measurement.
//
// It was a hand-rolled rune count that skipped ANSI sequences, and that was wrong in a
// way a CLI table can tolerate but a two-column TUI cannot: a CJK or emoji rune occupies
// TWO terminal columns and was counted as one, so every such rune in a transcript walked
// the split pane's right column one place left. lipgloss.Width delegates to
// ansi.StringWidth, which is both ANSI-aware and wide-rune-aware, and lipgloss v2 is
// already a direct dependency — the correct implementation was linked into the binary
// the whole time.
func visualWidth(s string) int { return lipgloss.Width(s) }

// paintSeg styles one row segment with an optional foreground and (for a selected row) the warm
// selection background. Applying the bg to EVERY segment — text and padding alike — is what
// keeps it continuous across the row: wrapping pre-styled fg spans in an outer bg would be
// dropped by their inner resets (the documented lipgloss gotcha), so each span carries its own.
func paintSeg(fg, bg color.Color, bold bool, s string) string {
	st := lipgloss.NewStyle()
	if fg != nil {
		st = st.Foreground(fg)
	}
	if bg != nil {
		st = st.Background(bg)
	}
	if bold {
		st = st.Bold(true)
	}
	return st.Render(s)
}

// padSeg right-pads s to n visual columns with background-carrying spaces, so a selected row's
// warm bar reaches the padded width rather than stopping at the last glyph.
func padSeg(s string, n int, bg color.Color) string {
	if w := visualWidth(s); w < n {
		return s + paintSeg(nil, bg, false, strings.Repeat(" ", n-w))
	}
	return s
}

// shortRunID is the board's id cell: the first 8 of a UUID, which is the rule
// shortRecID already uses for random UUIDs (unlike an SDK tool-use id, a run id has no
// constant prefix, so a head is the right end to take).
func shortRunID(id string) string {
	r := []rune(id)
	if len(r) <= 8 {
		return id
	}
	return string(r[:8])
}

// padCell right-pads to n RUNES. Rune-based to match capCell, so a multibyte title
// does not shift the column.
func padCell(s string, n int) string {
	c := capCell(s, n)
	if pad := n - len([]rune(c)); pad > 0 {
		return c + strings.Repeat(" ", pad)
	}
	return c
}

// runDuration is the run's elapsed WORK time for the header: a live run's time since it
// started, or a terminal run's total from start to finish. Start is StartedAt, or ClaimedAt
// during the brief claimed-but-not-started window; end is FinishedAt when set, else now.
//
// CreatedAt is deliberately NOT a fallback: it would turn a queued run's header into its
// queue-WAIT age dressed up as run-elapsed time (and a just-created run into a literal "0s"),
// conflating two different clocks. A run with no start stamp yet (queued) shows nothing.
func runDuration(run apitypes.RunDTO, now time.Time) string {
	var start time.Time
	switch {
	case run.StartedAt != nil && !run.StartedAt.IsZero():
		start = *run.StartedAt
	case run.ClaimedAt != nil && !run.ClaimedAt.IsZero():
		start = *run.ClaimedAt
	default:
		return "" // not started yet: no work-elapsed time to show
	}
	end := now
	if run.FinishedAt != nil && !run.FinishedAt.IsZero() {
		end = *run.FinishedAt
	}
	return shortDuration(end.Sub(start))
}

// shortDuration formats an elapsed duration compactly for the header: "45s", "12m", "3h4m",
// "2d5h". Negative clamps to "0s". It carries the second unit (unlike relAge, which is
// single-unit for the queue-age column) because a run's total reads better as "3h4m" than "3h".
func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d.Hours())
		if mins := int(d.Minutes()) % 60; mins != 0 {
			return itoa(h) + "h" + itoa(mins) + "m"
		}
		return itoa(h) + "h"
	default:
		days := int(d.Hours()) / 24
		if h := int(d.Hours()) % 24; h != 0 {
			return itoa(days) + "d" + itoa(h) + "h"
		}
		return itoa(days) + "d"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
