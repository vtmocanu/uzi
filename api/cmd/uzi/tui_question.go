package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

type transcriptQuestion struct {
	Question string
	Header   string
	Options  []transcriptQuestionOption
}

type transcriptQuestionOption struct {
	Label       string
	Description string
}

// parseTranscriptQuestions needs displayable prose only; question_id is an
// answerability constraint and does not affect this read-only transcript.
func parseTranscriptQuestions(payload json.RawMessage) []transcriptQuestion {
	var p map[string]json.RawMessage
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var rawQuestions []json.RawMessage
	if json.Unmarshal(p["questions"], &rawQuestions) != nil {
		return nil
	}
	var questions []transcriptQuestion
	for _, raw := range rawQuestions {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		text := questionString(fields["question"])
		if strings.TrimSpace(text) == "" {
			continue
		}
		q := transcriptQuestion{Question: text, Header: questionString(fields["header"])}
		var options []json.RawMessage
		if json.Unmarshal(fields["options"], &options) == nil {
			for _, option := range options {
				var fields map[string]json.RawMessage
				if json.Unmarshal(option, &fields) != nil {
					continue
				}
				label := strings.TrimSpace(questionString(fields["label"]))
				if label == "" {
					continue
				}
				q.Options = append(q.Options, transcriptQuestionOption{Label: label, Description: questionString(fields["description"])})
			}
		}
		questions = append(questions, q)
	}
	return questions
}

// openQuestionFrame derives the pending event from the run-wide log, independent
// of the selected lane and payload validity. Any newer answer closes the card.
func (m tuiModel) openQuestionFrame() (laneFrame, bool) {
	if m.detail.run.Status != "awaiting_input" {
		return laneFrame{}, false
	}
	var latest laneFrame
	found := false
	for _, frame := range m.detail.frames {
		if frame.Kind != "question" && frame.Kind != "answer" {
			continue
		}
		if !found || frame.Seq > latest.Seq {
			latest, found = frame, true
		}
	}
	return latest, found && latest.Kind == "question"
}

// questionCardLines is pure layout: the shared chrome budget excludes this card,
// avoiding a cycle through transcriptViewport and the rail's folding calculation.
func (m tuiModel) questionCardLines() []string {
	frame, open := m.openQuestionFrame()
	if !open || !m.detail.runLoaded || m.detail.loadErr != nil || m.detail.review.open {
		return nil
	}
	budget := m.transcriptBudgetWithoutCard()
	if budget < 4 {
		return nil // reserve at least three transcript content rows
	}
	questions := parseTranscriptQuestions(frame.Payload)
	count := max(1, len(questions))
	noun := "questions"
	if count == 1 {
		noun = "question"
	}
	title := fmt.Sprintf("┃ ✎ ANSWER REQUIRED · %d %s", count, noun)
	if m.detail.questionCollapsed || m.height < 24 {
		var headers []string
		for _, q := range questions {
			if header := strings.TrimSpace(m.renderer.Plain(q.Header, 200)); header != "" {
				headers = append(headers, header)
			}
		}
		const hint = " · z expand"
		room := m.width - ansi.StringWidth(title+hint)
		if len(headers) > 0 && room >= 4 {
			title += " (" + clampVisual(strings.Join(headers, ", "), room-3) + ")"
		}
		if ansi.StringWidth(title+hint) > m.width {
			return []string{clampVisual("┃ z expand", m.width)}
		}
		return []string{title + hint}
	}
	capRows := min(12, (budget+1)/2, budget-3)
	if capRows < 4 {
		return []string{clampVisual(title+" · z expand", m.width)}
	}
	// Use the full terminal width, rather than the narrower joined transcript.
	// Markdown keeps prose beyond Plain's cell cap; every physical line has a border.
	copy := m
	if renderer, err := newTUIRenderer(max(1, m.width-5)+4, m.dark); err == nil {
		copy.renderer = renderer
	}
	var content []string
	appendProse := func(text, indent string) {
		if rendered := copy.questionProse(text, indent, m.width); rendered != "" {
			content = append(content, strings.Split(rendered, "\n")...)
		}
	}
	if len(questions) == 0 {
		// A malformed newest event is shown instead of resurrecting an older question.
		appendProse(string(frame.Payload), "┃   ")
	}
	for i, q := range questions {
		header := fmt.Sprintf("┃ %d/%d", i+1, len(questions))
		if q.Header != "" {
			prefix := header + " · "
			indent := "┃" + strings.Repeat(" ", ansi.StringWidth(prefix)-1)
			prose := copy.questionProse(q.Header, indent, m.width)
			if prose != "" {
				header = prefix + strings.TrimPrefix(prose, indent)
			}
		}
		content = append(content, strings.Split(header, "\n")...)
		appendProse(q.Question, "┃   ")
		for j, option := range q.Options {
			prefix := fmt.Sprintf("┃   %d. ", j+1)
			indent := "┃" + strings.Repeat(" ", ansi.StringWidth(prefix)-1)
			label := copy.questionProse(option.Label, indent, m.width)
			if label != "" {
				label = prefix + strings.TrimPrefix(label, indent)
				content = append(content, strings.Split(label, "\n")...)
			}
			appendProse(option.Description, "┃      ")
		}
	}
	lines := []string{clampVisual(title+" · parked until answered", m.width)}
	available := capRows - 3 // title, collapse hint and divider are inside the cap
	if len(content) > available {
		visible := available - 1 // also reserve the overflow row
		lines = append(lines, content[:visible]...)
		lines = append(lines, clampVisual(fmt.Sprintf("┃ … +%d lines", len(content)-visible), m.width))
	} else {
		lines = append(lines, content...)
	}
	lines = append(lines, clampVisual("┃ z collapse", m.width),
		clampVisual("┃ "+strings.Repeat("─", max(0, m.width-2)), m.width))
	return lines
}

func questionString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func parseTranscriptAnswers(payload json.RawMessage) []string {
	var p map[string]json.RawMessage
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var rawAnswers []json.RawMessage
	if json.Unmarshal(p["answers"], &rawAnswers) != nil {
		return nil
	}
	var answers []string
	for _, raw := range rawAnswers {
		var s string
		// JSON null unmarshals to an empty string without error; it is not a string.
		if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
			answers = append(answers, s)
		}
	}
	return answers
}

// questionProse uses the sanitized Markdown renderer shared by this transcript
// build (buildFrameBlocksFrom). Remove its two-cell left document margin,
// then reflow at the chrome's available inner width, including long unbroken words.
// Every physical line receives UI-owned indentation; Plain would lose rune 201+.
func (m tuiModel) questionProse(text, indent string, width int) string {
	body := m.renderer.Markdown(text)
	lines := strings.Split(body, "\n")
	if m.renderer != nil && m.renderer.md != nil {
		for i, line := range lines {
			// Long-word wraps may have no right padding. Trim only actual
			// trailing spaces below, rather than deleting two content cells.
			end := max(2, ansi.StringWidth(line))
			lines[i] = ansi.Cut(line, 2, end)
		}
	}
	// Glamour's margin-only lines can contain SGR spans and spaces, so trimming
	// literal newlines alone leaves visible document gaps (including after labels).
	for i, line := range lines {
		visible := strings.TrimRight(ansi.Strip(line), " \t")
		lines[i] = ansi.Cut(line, 0, ansi.StringWidth(visible))
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	body = strings.Join(lines, "\n")
	inner := max(1, width-ansi.StringWidth(indent))
	body = ansi.Hardwrap(ansi.Wrap(body, inner, ""), inner, true)
	lines = strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = indent + strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// formatTranscriptQuestions renders structured question prose in the transcript.
// actor must come from this frame, never from a preceding question.
func (m tuiModel) formatTranscriptQuestions(questions []transcriptQuestion, actor string, width int) string {
	head := "✎ question"
	if actor != "" {
		head += " · " + m.renderer.Plain(actor, 24)
	}
	count := "question"
	if len(questions) != 1 {
		count = "questions"
	}
	head += fmt.Sprintf(" · %d %s", len(questions), count)
	lines := []string{clampVisual(m.pal.boxTitle.Render(head), width)}
	for i, q := range questions {
		if i > 0 {
			lines = append(lines, "")
		}
		if header := m.questionProse(q.Header, "  ", width); header != "" {
			lines = append(lines, header)
		}
		lines = append(lines, m.questionProse(q.Question, "  ", width))
		for j, option := range q.Options {
			prefix := fmt.Sprintf("  %d. ", j+1)
			label := m.questionProse(option.Label, strings.Repeat(" ", ansi.StringWidth(prefix)), width)
			// The number is UI chrome, replacing only the first line's indentation.
			if label != "" {
				label = prefix + strings.TrimPrefix(label, strings.Repeat(" ", ansi.StringWidth(prefix)))
			}
			lines = append(lines, label)
			if description := m.questionProse(option.Description, "     ", width); description != "" {
				lines = append(lines, description)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (m tuiModel) formatTranscriptAnswers(answers []string, actor string, width int) string {
	head := "✎ answer"
	if actor != "" {
		head += " · " + m.renderer.Plain(actor, 24)
	}
	return clampVisual(m.pal.boxTitle.Render(head), width) + "\n" +
		m.questionProse(strings.Join(answers, " · "), "  ", width)
}
