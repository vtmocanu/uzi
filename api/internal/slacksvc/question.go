package slacksvc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// questionPayload is the `question` run-message payload the worker persists at a
// clarification park (PRD #88 M1). It mirrors QuestionPayload in
// agent/src/protocol.ts; only the fields Slack renders are declared, and every
// string in it is MODEL-AUTHORED and therefore untrusted.
//
// It is parsed here rather than projected in SQL — see GetLatestRunQuestion's own
// comment for why this inverts GetSlackRunContext's repo_agent_names precedent.
type questionPayload struct {
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
	QuestionID string         `json:"question_id"`
	Questions  []questionItem `json:"questions"`
}

type questionItem struct {
	Question    string           `json:"question"`
	Header      string           `json:"header"`
	Options     []questionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect"`
}

type questionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// parseQuestionPayload decodes a `question` run-message payload, reporting false for
// anything the notifier must not post: unparseable JSON, a missing identity (nothing
// to dedupe on and nothing an answer could name), or no question carrying text.
//
// Defensive in the same register as the worker's own parseQuestions: a malformed
// payload yields "no question", never a partial post and never a panic. The strings
// are already clamped worker-side (2000/60/200 runes), so the caps are not re-applied
// here — only the total blob is bounded, at the Slack section limit.
func parseQuestionPayload(raw []byte) (questionPayload, bool) {
	var p questionPayload
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return questionPayload{}, false
	}
	p.QuestionID = strings.TrimSpace(p.QuestionID)
	if p.QuestionID == "" {
		return questionPayload{}, false
	}
	kept := make([]questionItem, 0, len(p.Questions))
	for _, q := range p.Questions {
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" {
			continue // a question with no text is not a question
		}
		q.Header = strings.TrimSpace(q.Header)
		kept = append(kept, q)
	}
	if len(kept) == 0 {
		return questionPayload{}, false
	}
	p.Questions = kept
	return p, true
}

// questionBody renders the questions as ONE untrusted plain-text blob, carrying no
// markup of its own. Everything in it is model-authored, so the caller escapes it
// wholesale and keeps every trusted element (the prompt, the deep link) in its own
// block — see questionThreadBlocks.
//
// Options are listed as suggestions, never as a closed set: free text is always a
// valid answer (PRD #88 Decision 1), and on Slack it is the ONLY way to answer, since
// M3 ships no option buttons.
func questionBody(p questionPayload) string {
	var b strings.Builder
	for i, q := range p.Questions {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if len(p.Questions) > 1 {
			fmt.Fprintf(&b, "%d. ", i+1)
		}
		if q.Header != "" {
			b.WriteString(q.Header + "\n")
		}
		b.WriteString(q.Question)
		if len(q.Options) == 0 {
			continue
		}
		if q.MultiSelect {
			b.WriteString("\nSuggested answers (any that apply, or your own words):")
		} else {
			b.WriteString("\nSuggested answers (or your own words):")
		}
		for _, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				continue
			}
			b.WriteString("\n• " + label)
			if d := strings.TrimSpace(o.Description); d != "" {
				b.WriteString(": " + d)
			}
		}
	}
	return b.String()
}

// questionThreadBlocks renders a clarification question into the run's DM thread
// (PRD #88 M3). It reuses the plan gate's pipeline verbatim (PRD #41 Decision 10,
// D-J): ScrubSecrets → EscapeMrkdwn of the whole UNTRUSTED blob → truncate on a rune
// boundary → trusted markup appended as SEPARATE blocks.
//
// The whole-blob escape is the same documented exception planThreadBlocks takes (see
// redact.go's per-field rule): the exception holds because the blob carries no trusted
// markup of its own, so escaping it wholesale is exactly right and neutralizes any
// <@Uxxx> mention or spoofed <https://evil|Open> link an injected question embeds. The
// ASSEMBLED message is never escaped — the prompt and the deep link are their own
// blocks, outside the truncated region, so an over-long question cannot displace them.
//
// There are no buttons: unlike the plan gate, the answer round-trip needs no anchor
// state, because the distinct awaiting_input status IS the routing signal (D5). The
// threaded reply is the whole affordance.
func questionThreadBlocks(runID uuid.UUID, p questionPayload, base string) []slack.Block {
	prompt := "*The run needs your answer.* Reply in this thread — your reply is the answer."
	if len(p.Questions) > 1 {
		prompt = fmt.Sprintf("*The run needs your answer to %d questions.* Reply in this thread — one reply answers the card.",
			len(p.Questions))
	}
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, prompt, false, false), nil, nil),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType,
			truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(questionBody(p)))), false, false), nil, nil),
	}
	if u := runURL(base, runID); u != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_question_link",
			slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("<%s|Answer in uzi instead>", u), false, false)))
	}
	return blocks
}
