package main

import "strings"

// railHarnessDecision is a stored snapshot, fitted whole with its separator.
// Recorded decisions have twelve rows; legacy decisions have two. Content is
// width-clamped after Plain so wide glyphs cannot wrap.
func (m tuiModel) railHarnessDecision(usedRows int) string {
	lines := []string{"HARNESS at creation"}
	for _, line := range railHarnessDecisionLines(m.detail.run.HarnessDecision) {
		lines = append(lines, clampVisual(m.renderer.Plain(line, m.railWidth()), m.railWidth()))
	}
	if len(lines) > m.transcriptViewport()-usedRows-1 {
		return ""
	}
	lines[0] = m.pal.faint.Render(lines[0])
	return strings.Join(lines, "\n")
}
