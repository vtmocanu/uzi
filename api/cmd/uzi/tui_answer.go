package main

import (
	"encoding/json"
	"strings"
)

type answerOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type answerQuestion struct {
	Question    string         `json:"question"`
	Header      string         `json:"header"`
	Options     []answerOption `json:"options"`
	MultiSelect bool           `json:"multiSelect"`
}

// parseAnswerableQuestion rejects any malformed question row: dropping one would
// shift the answers the worker zips against the original questions. Options may
// be omitted; every question still accepts free text. Identity is preserved.
func parseAnswerableQuestion(payload json.RawMessage) (questionPayload, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return questionPayload{}, false
	}
	id := questionString(fields["question_id"])
	var rows []json.RawMessage
	if trimAnswerSpace(id) == "" || json.Unmarshal(fields["questions"], &rows) != nil || len(rows) == 0 {
		return questionPayload{}, false
	}
	p := questionPayload{QuestionID: id}
	for _, raw := range rows {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil {
			return questionPayload{}, false
		}
		text := questionString(row["question"])
		if trimAnswerSpace(text) == "" {
			return questionPayload{}, false
		}
		q := answerQuestion{Question: text, Header: questionString(row["header"]), Options: []answerOption{}}
		// Only the JSON boolean true enables multiple selections, as on the web.
		_ = json.Unmarshal(row["multiSelect"], &q.MultiSelect)
		var options []json.RawMessage
		if json.Unmarshal(row["options"], &options) == nil {
			for _, rawOption := range options {
				var option map[string]json.RawMessage
				if json.Unmarshal(rawOption, &option) != nil {
					continue
				}
				label := trimAnswerSpace(questionString(option["label"]))
				if label != "" {
					q.Options = append(q.Options, answerOption{Label: label, Description: questionString(option["description"])})
				}
			}
		}
		p.Questions = append(p.Questions, q)
	}
	return p, true
}

// trimAnswerSpace matches ECMAScript String.trim (not Go's TrimSpace):
// U+FEFF is whitespace, while U+0085 is retained.
func trimAnswerSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\u0009', '\u000a', '\u000b', '\u000c', '\u000d', '\u0020',
			'\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
			return true
		}
		return r >= '\u2000' && r <= '\u200a'
	})
}

func composeAnswer(selected []string, freeText string) string {
	picks := make([]string, 0, len(selected))
	for _, label := range selected {
		if pick := trimAnswerSpace(label); pick != "" {
			picks = append(picks, pick)
		}
	}
	text := trimAnswerSpace(freeText)
	if len(picks) == 0 {
		return text
	}
	joined := strings.Join(picks, ", ")
	if text == "" {
		return joined
	}
	return joined + " — " + text
}

func answersReady(answers []string, questionCount int) bool {
	if len(answers) != questionCount {
		return false
	}
	for _, answer := range answers {
		if trimAnswerSpace(answer) == "" {
			return false
		}
	}
	return true
}

// toggleAnswerOption returns a fresh selection slice aligned with q.Options.
// Invalid indexes leave selections unchanged; toggling affects only this question.
func toggleAnswerOption(q answerQuestion, selected []bool, optionIndex int) []bool {
	next := make([]bool, len(q.Options))
	copy(next, selected)
	if optionIndex < 0 || optionIndex >= len(next) {
		return next
	}
	if next[optionIndex] {
		next[optionIndex] = false
		return next
	}
	if !q.MultiSelect {
		clear(next)
	}
	next[optionIndex] = true
	return next
}

// composeQuestionAnswers returns the existing answerBody wire shape. Selections
// are indexed by question, then option; labels and answers follow payload order.
// Missing selections/text produce empty answers, which answersReady blocks.
func composeQuestionAnswers(p questionPayload, selected [][]bool, texts []string) answerBody {
	body := answerBody{QuestionID: p.QuestionID, Answers: make([]string, len(p.Questions))}
	for qi, q := range p.Questions {
		var labels []string
		for oi, option := range q.Options {
			if qi < len(selected) && oi < len(selected[qi]) && selected[qi][oi] {
				labels = append(labels, option.Label)
			}
		}
		text := ""
		if qi < len(texts) {
			text = texts[qi]
		}
		body.Answers[qi] = composeAnswer(labels, text)
	}
	return body
}
