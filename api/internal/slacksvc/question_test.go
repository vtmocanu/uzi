package slacksvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// questionJSON builds a `question` run-message payload the way the worker persists
// it (agent/src/protocol.ts QuestionPayload).
func questionJSON(t *testing.T, id string, qs ...questionItem) []byte {
	t.Helper()
	b, err := json.Marshal(questionPayload{QuestionID: id, Questions: qs})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return b
}

func parkedQuestionCtx(t *testing.T, questionID string) (store.GetSlackRunContextRow, []byte) {
	t.Helper()
	rc := baseRun("awaiting_input")
	return rc, questionJSON(t, questionID, questionItem{
		Header: "Storage backend", Question: "Which store should the cache use?",
	})
}

// contextTexts flattens the context blocks of a Block Kit slice. blockSummary only
// reads sections + buttons, and the deep link deliberately rides a CONTEXT block
// outside the truncated region, so it needs its own reader.
func contextTexts(blks []slack.Block) string {
	out := ""
	for _, b := range blks {
		if cb, ok := b.(*slack.ContextBlock); ok && cb.ContextElements.Elements != nil {
			for _, el := range cb.ContextElements.Elements {
				if txt, ok := el.(*slack.TextBlockObject); ok {
					out += txt.Text
				}
			}
		}
	}
	return out
}

// A run parking at awaiting_input posts the question into its DM thread and records
// the question's identity on the anchor. The root line must NOT read the raw enum.
func TestNotifierAwaitingInputPostsQuestionInThread(t *testing.T) {
	rc, payload := parkedQuestionCtx(t, "q-1")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root1"},
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fp.blocks) != 1 || fp.blocks[0].thread != "root1" {
		t.Fatalf("want exactly one threaded block post for the question: %+v", fp.blocks)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "Which store should the cache use?") ||
		!strings.Contains(fp.blocks[0].sectionText, "Storage backend") {
		t.Fatalf("question text and header must reach the thread: %q", fp.blocks[0].sectionText)
	}
	if len(fp.blocks[0].actionIDs) != 0 {
		t.Fatalf("the question card carries no buttons (D5 — the status is the routing signal): %+v", fp.blocks[0].actionIDs)
	}
	if len(fs.questionSet) != 1 || fs.questionSet[0].QuestionID.String != "q-1" || fs.questionSet[0].RunID != rc.ID {
		t.Fatalf("the posted question's identity must be recorded on the anchor: %+v", fs.questionSet)
	}
	// The ts of the card is recorded with it, because that is what an inbound reply is
	// ordered against. An id with no ts would dedupe correctly and leave every reply
	// unbindable — the failure would show up only on the inbound side.
	if fs.questionSet[0].QuestionTs.String != "ts1" {
		t.Fatalf("the card's own ts must be recorded so replies can be ordered against it: %+v", fs.questionSet[0])
	}
	root, ok := findUpdateBlock(fp.updateBlocks, "root1")
	if !ok || !strings.Contains(root.sectionText, "Needs your answer") {
		t.Fatalf("root label must read the parked state, not the raw enum: %+v", fp.updateBlocks)
	}
	if strings.Contains(root.sectionText, "awaiting_input") {
		t.Fatalf("root line leaked the raw status enum: %q", root.sectionText)
	}
}

// The SAME question re-broadcast — which is what a worker death produces, since the
// run re-queues and the resumed worker re-parks re-using the question id — must not
// post the card a second time.
//
// 🔴 This also carries D-E case 2, and the assertion that matters is the NEGATIVE one.
// Because nothing is written, the anchor's question_ts still points at the ORIGINAL
// card — so an answer the user submitted before the worker died is still *after* it and
// is still honoured. Advance the ts here and that reply becomes retroactively stale:
// the user answered correctly, got a ✅, and the run discards it.
func TestNotifierAwaitingInputRepostSameQuestionIsNoop(t *testing.T) {
	rc, payload := parkedQuestionCtx(t, "q-1")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{
			RunID: rc.ID, ChannelID: "D1", RootTs: "root1",
			QuestionID: pgtype.Text{String: "q-1", Valid: true},
			QuestionTs: pgtype.Text{String: "origcard", Valid: true},
		},
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fp.blocks) != 0 {
		t.Fatalf("a re-park on the same question must not re-post it: %+v", fp.blocks)
	}
	if len(fs.questionSet) != 0 {
		t.Fatalf("no post, no anchor write — the recorded ts must keep pointing at the original card: %+v", fs.questionSet)
	}
}

// A post whose ts came back empty must NOT be recorded. It is the one combination that
// is worse than failing outright: the dedupe would be satisfied (never re-posting)
// while every reply stayed unbindable, so the question would sit on screen answerable
// by nobody.
func TestNotifierQuestionWithNoTsIsNotRecorded(t *testing.T) {
	rc, payload := parkedQuestionCtx(t, "q-1")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root1"},
	}
	fp := &tslessPoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fs.questionSet) != 0 {
		t.Fatalf("a card with no ts must not be recorded as posted: %+v", fs.questionSet)
	}
}

// tslessPoster is a fakePoster whose block posts report no ts, modelling a Slack
// response uzi cannot thread replies against.
type tslessPoster struct{ fakePoster }

func (p *tslessPoster) PostBlocks(ctx context.Context, ch, thread, fallback string, blks []slack.Block) (string, error) {
	if _, err := p.fakePoster.PostBlocks(ctx, ch, thread, fallback, blks); err != nil {
		return "", err
	}
	return "", nil
}

// A genuinely NEW question (question 2 of the run) carries a new id and does post,
// which is the control that makes the dedupe above a dedupe rather than a mute.
func TestNotifierAwaitingInputSecondQuestionPosts(t *testing.T) {
	rc, payload := parkedQuestionCtx(t, "q-2")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{
			RunID: rc.ID, ChannelID: "D1", RootTs: "root1",
			QuestionID: pgtype.Text{String: "q-1", Valid: true},
		},
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fp.blocks) != 1 {
		t.Fatalf("a new question id must post a fresh card: %+v", fp.blocks)
	}
	if len(fs.questionSet) != 1 || fs.questionSet[0].QuestionID.String != "q-2" {
		t.Fatalf("the anchor must advance to the new question: %+v", fs.questionSet)
	}
}

// The state report can reach the notifier before the question run-message is durable.
// Posting "the run needs your answer" with no question would be worse than late, so
// the notifier waits for a later event instead.
func TestNotifierAwaitingInputWithNoQuestionYetWaits(t *testing.T) {
	rc := baseRun("awaiting_input")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), // question nil ⇒ the query reports no row
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root1"},
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fp.blocks) != 0 || len(fs.questionSet) != 0 {
		t.Fatalf("no question message yet ⇒ nothing posted and nothing recorded: %+v %+v", fp.blocks, fs.questionSet)
	}
}

// A failed post must not record the question as delivered, or the retry on the next
// state event would be deduped away and the question would never reach Slack.
func TestNotifierQuestionPostFailureIsNotRecorded(t *testing.T) {
	rc, payload := parkedQuestionCtx(t, "q-1")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root1"},
	}
	fp := &fakePoster{postErr: errors.New("channel_not_found")}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_input"})

	if len(fs.questionSet) != 0 {
		t.Fatalf("a failed post must leave the anchor untouched so a later event retries: %+v", fs.questionSet)
	}
}

// A run that is not parked never posts a question card, even with a question message
// left over from an earlier park — the status is what says "answer me now".
func TestNotifierNonParkedStatusPostsNoQuestion(t *testing.T) {
	_, payload := parkedQuestionCtx(t, "q-1")
	rc := baseRun("running")
	fs := &fakeNotifStore{
		rc: rc, delivery: txt("U123"), question: payload,
		msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root1"},
	}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.blocks) != 0 || len(fs.questionSet) != 0 {
		t.Fatalf("only awaiting_input posts a question: %+v %+v", fp.blocks, fs.questionSet)
	}
}

// Question text is model-authored from repo/issue content, so it reaches Slack through
// the plan gate's pipeline: secrets scrubbed, the whole untrusted blob rendered by
// SlackMrkdwn (which owns its escaping, so no live mention and no spoofed link survive),
// and the one trusted element — the deep link — in its OWN block, so it can neither be
// escaped nor truncated away.
func TestQuestionThreadBlocksEscapesAndScrubs(t *testing.T) {
	runID := uuid.New()
	p := questionPayload{QuestionID: "q-1", Questions: []questionItem{{
		Header:   "<@U0HOSTILE> heads up",
		Question: "Use <https://evil.example|Open in uzi> with token xoxb-1234-hostile & proceed?",
		Options:  []questionOption{{Label: "<b>yes</b>", Description: "ship it & go"}},
	}}}

	blocks := questionThreadBlocks(runID, p, "https://uzi.example")
	_, section := blockSummary(blocks)

	if strings.Contains(section, "xoxb-1234-hostile") || !strings.Contains(section, "[redacted]") {
		t.Fatalf("a credential pattern in the question must be scrubbed: %q", section)
	}
	if strings.Contains(section, "<@U0HOSTILE>") || strings.Contains(section, "<https://evil.example|") {
		t.Fatalf("mentions and spoofed links must be escaped out of the question blob: %q", section)
	}
	if !strings.Contains(section, "&amp;") {
		t.Fatalf("mrkdwn control characters must be escaped: %q", section)
	}
	if !strings.Contains(section, "&lt;b&gt;yes&lt;/b&gt;") {
		t.Fatalf("option labels are untrusted too and must be escaped: %q", section)
	}
	ctxText := contextTexts(blocks)
	if !strings.Contains(ctxText, "<https://uzi.example/runs/"+runID.String()+"|") {
		t.Fatalf("the deep link must survive as trusted markup in its own block: %q", ctxText)
	}
}

// The question blob is now RENDERED by SlackMrkdwn (PRD #292 M5): a markdown bold
// becomes *bold*, a list becomes • bullets, and an https link becomes <url|label>.
func TestQuestionThreadBlocksRendersMarkdown(t *testing.T) {
	runID := uuid.New()
	p := questionPayload{QuestionID: "q-1", Questions: []questionItem{{
		Question: "**pick** one:\n\n- alpha\n- beta\n\nsee [docs](https://x)",
	}}}

	blocks := questionThreadBlocks(runID, p, "https://uzi.example")
	_, section := blockSummary(blocks)

	if !strings.Contains(section, "*pick*") {
		t.Errorf("**pick** must render as *pick*, got %q", section)
	}
	if !strings.Contains(section, "• alpha") || !strings.Contains(section, "• beta") {
		t.Errorf("a markdown list must render as • bullets, got %q", section)
	}
	if !strings.Contains(section, "<https://x|docs>") {
		t.Errorf("an https link must render as <url|label>, got %q", section)
	}
}

// The untrusted blob is bounded at Slack's section limit on a rune boundary, and the
// deep link sits OUTSIDE that bound, so an over-long question cannot displace it.
func TestQuestionThreadBlocksTruncatesBodyNotLink(t *testing.T) {
	runID := uuid.New()
	p := questionPayload{QuestionID: "q-1", Questions: []questionItem{{
		Question: strings.Repeat("é", maxSlackSectionRunes+500),
	}}}

	blocks := questionThreadBlocks(runID, p, "https://uzi.example")
	_, section := blockSummary(blocks)

	if n := len([]rune(section)); n > maxSlackSectionRunes+200 {
		t.Fatalf("question body must be truncated near the section limit, got %d runes", n)
	}
	if strings.Contains(section, "�") {
		t.Fatalf("truncation must land on a rune boundary, never mid-rune: %q", section[:64])
	}
	if !strings.Contains(contextTexts(blocks), runID.String()) {
		t.Fatalf("the deep link must survive an over-long question")
	}
}

// A payload the notifier cannot use — unparseable, identity-less, or carrying no
// question with text — yields no post rather than a park nobody can explain or answer.
func TestParseQuestionPayloadRejectsUnusable(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"not json", []byte("{oops")},
		{"no question id", questionJSON(t, "", questionItem{Question: "what?"})},
		{"no questions", questionJSON(t, "q-1")},
		{"question with no text", questionJSON(t, "q-1", questionItem{Header: "h", Question: "   "})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseQuestionPayload(tc.raw); ok {
				t.Fatalf("payload must be rejected: %s", tc.raw)
			}
		})
	}
	if p, ok := parseQuestionPayload(questionJSON(t, " q-1 ",
		questionItem{Question: "a?"}, questionItem{Question: " "})); !ok ||
		p.QuestionID != "q-1" || len(p.Questions) != 1 {
		t.Fatalf("a usable payload must keep only questions carrying text, with a trimmed id: %+v %v", p, ok)
	}
}
