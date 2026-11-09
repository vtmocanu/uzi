// Package runquestion parses the `question` run-message payload and derives the
// one-line excerpt the Overview shows (PRD #4040). It is a neutral leaf (stdlib plus
// termsafe) so the handler and slacksvc share one parser without importing each other.
package runquestion

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// ExcerptMaxRunes caps the excerpt text, the ellipsis included (PRD #4040). The
// " (+N more)" suffix is appended outside the cap.
const ExcerptMaxRunes = 280

// Payload is the `question` run-message payload the worker persists at a
// clarification park (PRD #88 M1). It mirrors QuestionPayload in
// agent/src/protocol.ts; only the fields the API surfaces declare, and every
// string in it is MODEL-AUTHORED and therefore untrusted.
//
// It is parsed here rather than projected in SQL — see GetLatestRunQuestion's own
// comment for why this inverts GetSlackRunContext's repo_agent_names precedent.
type Payload struct {
	// QuestionID is the question's stable identity: what the answer names, what the
	// resume guard compares, and what the DM anchor records so a re-park does not
	// post the same card twice. It is the ONLY staleness key in this feature.
	//
	// The payload deliberately carries no ordinal beside it (D-R): one existed, was
	// never read back, and was labelled "the stale-answer discriminator" by a design
	// doc — an inert display value wearing the name of a struck mechanism, which is
	// the shape a later reader "restores" into a real requeue-rejection bug. A surface
	// wanting "question 2 of this run" counts `question` messages in the feed, where
	// the ordinal is a fact rather than a claim. Slack wants no such thing.
	QuestionID string `json:"question_id"`
	Questions  []Item `json:"questions"`
}

// Item is one question in a Payload.
type Item struct {
	Question    string   `json:"question"`
	Header      string   `json:"header"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multiSelect"`
}

// Option is one suggested answer of an Item.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Parse decodes a `question` run-message payload, reporting false for anything no
// surface may show: unparseable JSON, a missing identity (nothing to dedupe on and
// nothing an answer could name), or no question carrying text.
//
// Defensive in the same register as the worker's own parseQuestions: a malformed
// payload yields "no question", never a partial render and never a panic. The strings
// are already clamped worker-side (2000/60/200 runes), so the caps are not re-applied
// here; each surface bounds its own output (Slack its section limit, Excerpt its cap).
func Parse(raw []byte) (Payload, bool) {
	var p Payload
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return Payload{}, false
	}
	p.QuestionID = strings.TrimSpace(p.QuestionID)
	if p.QuestionID == "" {
		return Payload{}, false
	}
	kept := make([]Item, 0, len(p.Questions))
	for _, q := range p.Questions {
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			continue // a question with no text is not a question
		}
		q.Header = strings.TrimSpace(q.Header)
		kept = append(kept, q)
	}
	if len(kept) == 0 {
		return Payload{}, false
	}
	p.Questions = kept
	return p, true
}

// Excerpt returns the one-line, plain-text excerpt of the first question (PRD #4040):
// whitespace runs collapse to one space BEFORE unsafe runes are stripped (termsafe.Unsafe
// also matches \n and \t, so the order keeps words apart), the result is trimmed and
// capped at ExcerptMaxRunes with the "…" inside the cap, and " (+N more)" follows
// outside the cap when the payload holds further questions. It reports false when the
// payload does not parse or the sanitized text is empty.
func Excerpt(raw []byte) (string, bool) {
	p, ok := Parse(raw)
	if !ok {
		return "", false
	}
	text := strings.Join(strings.FieldsFunc(p.Questions[0].Question, unicode.IsSpace), " ")
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if termsafe.Unsafe(r) {
			return -1
		}
		return r
	}, text))
	if text == "" {
		return "", false
	}
	if rs := []rune(text); len(rs) > ExcerptMaxRunes {
		text = strings.TrimRightFunc(string(rs[:ExcerptMaxRunes-1]), unicode.IsSpace) + "…"
	}
	if n := len(p.Questions) - 1; n > 0 {
		text += " (+" + strconv.Itoa(n) + " more)"
	}
	return text, true
}
