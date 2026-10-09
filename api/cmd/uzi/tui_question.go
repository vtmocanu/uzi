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

// formatTranscriptQuestions is also the prose/chrome seam for the future card.
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
