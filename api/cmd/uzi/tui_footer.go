package main

import "strings"

// footerHint keeps styling and removal policy separate from rendered ANSI text.
// A zero rank protects contextual actions, help, quit and the partial-cost cue.
type footerHint struct {
	text        string
	removalRank int
}

func (m tuiModel) footerHint(key, label string) footerHint {
	rank := 0
	switch key {
	case "r":
		rank = 1
	case "h":
		rank = 2
	case "a":
		rank = 3
	case "/":
		rank = 4
	case "R":
		rank = 5
	case "tab":
		rank = 6
	}
	return footerHint{text: m.keyHint(key, label), removalRank: rank}
}

func (m tuiModel) renderFooterHints(hints []footerHint, removedThrough int) string {
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		if hint.removalRank == 0 || hint.removalRank > removedThrough {
			parts = append(parts, hint.text)
		}
	}
	return " " + strings.Join(parts, m.pal.faint.Render(" · "))
}

// fitFooterHints makes at most six removals, skipping absent ranks and stopping
// as soon as the legend fits. Below the protected minimum it may truncate.
func (m tuiModel) fitFooterHints(hints []footerHint, budget int) string {
	line := m.renderFooterHints(hints, 0)
	for rank := 1; rank <= 6 && visualWidth(line) > budget; rank++ {
		line = m.renderFooterHints(hints, rank)
	}
	return clampVisual(line, max(0, budget))
}
