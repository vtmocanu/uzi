package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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

// answerDraft belongs to one detail session. Snapshot is never edited; a changed
// structure blocks its indexed selections permanently. Each identity retains its
// own delivery guard even when a replacement draft becomes active.
type answerDraft struct {
	snapshot                                   questionPayload
	seq                                        int32
	selected                                   [][]bool
	answerText                                 []string
	position, option, reviewScroll, editScroll int
	manualScroll                               bool
	detailFocus, review                        bool
	pending                                    uint64
	sent, uncertain, conflict, closed, drift   bool
}

const answerUncertainNotice = "delivery not confirmed; answer from the web or Slack if the run does not resume. Resend guard is local to this TUI session."

type answerState struct {
	drafts       []answerDraft
	active       int
	open         bool
	requestSeq   uint64
	answerNotice string
}
type answerResultMsg struct {
	runID, questionID string
	gen, requestID    uint64
	err               error
}

func (a *answerState) draft() *answerDraft {
	if len(a.drafts) == 0 || a.active < 0 || a.active >= len(a.drafts) {
		return nil
	}
	return &a.drafts[a.active]
}

func (m *tuiModel) reconcileAnswer() {
	a := &m.detail.answer
	// Missing history or a transient status change is not proof of closure.
	// Scan all held evidence: an answer followed by a same-id question in one
	// batch must not resurrect the answered identity. The scan is bounded by the
	// session's held frames and drafts; malformed frames do not stop siblings.
	for _, f := range m.detail.frames {
		if f.Kind != kindAnswer && f.Kind != "question" {
			continue
		}
		p, ok := parseAnswerableQuestion(f.Payload)
		for i := range a.drafts {
			d := &a.drafts[i]
			if f.Seq < d.seq {
				continue
			}
			if f.Kind == kindAnswer && f.Seq > d.seq {
				d.closed = true
			} else if f.Kind == "question" {
				if ok && p.QuestionID == d.snapshot.QuestionID {
					if !reflect.DeepEqual(p, d.snapshot) {
						d.drift = true
					}
				} else if f.Seq > d.seq {
					d.closed = true
				}
			}
		}
	}
	if d := a.draft(); d != nil && d.closed && (d.sent || a.answerNotice == "sending answer…" ||
		(d.uncertain && a.answerNotice == answerUncertainNotice)) {
		a.answerNotice = "" // echo won the race, so a late HTTP result cannot restore success
	}
}

func (m tuiModel) answerEligible(d *answerDraft) bool {
	if d == nil || d.closed || d.drift || d.pending != 0 || d.sent || d.uncertain || d.conflict ||
		m.detail.steer.access != steerAllowed {
		return false
	}
	f, open := m.openQuestionFrame()
	if !open {
		return false
	}
	p, ok := parseAnswerableQuestion(f.Payload)
	return ok && p.QuestionID == d.snapshot.QuestionID && reflect.DeepEqual(p, d.snapshot)
}

func (m *tuiModel) openAnswer() {
	if m.detail.steer.access != steerAllowed || m.detail.review.open || m.detail.steer.mode != steerIdle {
		return
	}
	f, open := m.openQuestionFrame()
	if !open {
		return
	}
	p, ok := parseAnswerableQuestion(f.Payload)
	if !ok {
		m.detail.answer.answerNotice = "cannot answer this question payload here; use the web or Slack"
		return
	}
	a := &m.detail.answer
	for i := range a.drafts {
		if a.drafts[i].snapshot.QuestionID == p.QuestionID {
			a.active = i
			if a.drafts[i].pending == 0 && !a.drafts[i].sent && !a.drafts[i].uncertain && !a.drafts[i].conflict {
				a.open = true
			}
			return
		}
	}
	d := answerDraft{snapshot: p, seq: f.Seq, selected: make([][]bool, len(p.Questions)),
		answerText: make([]string, len(p.Questions)), detailFocus: len(p.Questions[0].Options) == 0}
	for i, q := range p.Questions {
		d.selected[i] = make([]bool, len(q.Options))
	}
	a.drafts = append(a.drafts, d)
	a.active, a.open, a.answerNotice = len(a.drafts)-1, true, ""
}

func (m tuiModel) answerRoom() int {
	rows := m.height - len(m.detailHeaderLines())
	if m.transportLine() != "" {
		rows--
	}
	return max(0, rows)
}

func (m tuiModel) answerTooSmall() bool {
	return m.width < 40 || m.height < 10 || m.answerRoom() < 6
}

func (m tuiModel) answerKey(k string) (tea.Model, tea.Cmd, bool) {
	a := &m.detail.answer
	if !a.open {
		if k != keyAnswerQuestion || m.detail.steer.mode != steerIdle || m.detail.review.open {
			return m, nil, false
		}
		m.openAnswer()
		return m, nil, true
	}
	d := a.draft()
	if k == keyEsc {
		a.open = false
		return m, nil, true
	}
	if d == nil {
		return m, nil, true
	}
	if !m.answerEligible(d) || m.answerTooSmall() {
		return m, nil, true
	}
	if d.review {
		_, limit, _ := m.answerLayout(m.answerRoom())
		d.reviewScroll = min(d.reviewScroll, limit)
		switch k {
		case "shift+tab":
			d.review, d.position, d.option = false, len(d.snapshot.Questions)-1, 0
			d.manualScroll, d.editScroll = false, 0
			d.detailFocus = len(d.snapshot.Questions[d.position].Options) == 0
		case "up":
			d.reviewScroll = max(0, d.reviewScroll-1)
		case "down":
			d.reviewScroll++
		case "pgup":
			d.reviewScroll = max(0, d.reviewScroll-5)
		case "pgdown":
			d.reviewScroll += 5
		case keyEnter:
			body := composeQuestionAnswers(d.snapshot, d.selected, d.answerText)
			if !answersReady(body.Answers, len(d.snapshot.Questions)) {
				return m, nil, true
			}
			wire, err := json.Marshal(body)
			if err != nil {
				a.answerNotice = err.Error()
				return m, nil, true
			}
			a.requestSeq++
			d.pending = a.requestSeq
			a.answerNotice = "sending answer…"
			c, ctx, runID, gen, id, req := m.client, m.ctx, m.detail.runID, m.detail.gen, d.snapshot.QuestionID, d.pending
			// The command owns the serialized original identity and answer snapshot.
			cmd := func() tea.Msg {
				_, err := c.SubmitRunInput(ctx, runID, kindAnswer, string(wire), nil, false, nil)
				return answerResultMsg{runID: runID, gen: gen, questionID: id, requestID: req, err: err}
			}
			return m, cmd, true
		}
		_, limit, _ = m.answerLayout(m.answerRoom())
		d.reviewScroll = min(d.reviewScroll, limit)
		return m, nil, true
	}
	q := d.snapshot.Questions[d.position]
	// Paging is independent of the picker. Returning to a question or acting on
	// an option restores automatic focus; free text keeps the chosen prose page.
	switch k {
	case "shift+tab", "up", "down":
		d.manualScroll, d.editScroll = false, 0
	}
	switch k {
	case "pgup", "pgdown":
		_, limit, start := m.answerLayout(m.answerRoom())
		d.editScroll = start
		if !d.manualScroll {
			d.manualScroll = true
			// Pinning the current option changes the available prose rows.
			_, limit, _ = m.answerLayout(m.answerRoom())
		}
		if k == "pgup" {
			d.editScroll = max(0, d.editScroll-1)
		} else {
			d.editScroll = min(limit, d.editScroll+1)
		}
	case "shift+tab":
		if d.position > 0 {
			d.position--
			d.option = 0
			d.detailFocus = len(d.snapshot.Questions[d.position].Options) == 0
		}
	case "tab":
		if len(q.Options) > 0 {
			d.detailFocus = !d.detailFocus
		}
	case "up":
		if !d.detailFocus {
			d.option = max(0, d.option-1)
		}
	case "down":
		if !d.detailFocus {
			d.option = min(len(q.Options)-1, d.option+1)
		}
	case keyEnter:
		body := composeQuestionAnswers(d.snapshot, d.selected, d.answerText)
		if trimAnswerSpace(body.Answers[d.position]) == "" {
			a.answerNotice = "choose an option or enter an answer"
		} else {
			a.answerNotice = ""
			if d.position == len(d.snapshot.Questions)-1 {
				d.review = true
				d.reviewScroll = 0
			} else {
				d.position++
				d.manualScroll, d.editScroll = false, 0
				d.option = 0
				d.detailFocus = len(d.snapshot.Questions[d.position].Options) == 0
			}
		}
	case "backspace":
		r := []rune(d.answerText[d.position])
		if d.detailFocus && len(r) > 0 {
			d.answerText[d.position] = string(r[:len(r)-1])
		}
	default:
		if !d.detailFocus && (k == "space" || k == " ") {
			d.manualScroll, d.editScroll = false, 0
			d.selected[d.position] = toggleAnswerOption(q, d.selected[d.position], d.option)
		} else if !d.detailFocus && len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
			idx := int(k[0] - '1')
			if idx < len(q.Options) {
				d.option = idx
				d.manualScroll, d.editScroll = false, 0
				d.selected[d.position] = toggleAnswerOption(q, d.selected[d.position], idx)
			}
		} else {
			if k == "space" {
				k = " "
			}
			printable := k != ""
			for _, r := range k {
				if !unicode.IsPrint(r) {
					printable = false
				}
			}
			// Named navigation/control keys are consumed, never inserted as prose.
			if printable && (len([]rune(k)) == 1 || !isAnswerNamedKey(k)) {
				d.detailFocus = true
				d.answerText[d.position] += k
			}
		}
	}
	return m, nil, true
}

func isAnswerNamedKey(k string) bool {
	switch k {
	case "left", "right", "home", "end", "delete", "pgup", "pgdown", "insert", "enter", "esc", "tab":
		return true
	}
	return strings.HasPrefix(k, "ctrl+") || strings.HasPrefix(k, "alt+") || strings.HasPrefix(k, "shift+") ||
		(len(k) > 1 && k[0] == 'f' && k[1] >= '0' && k[1] <= '9')
}

func (m *tuiModel) applyAnswerResult(msg answerResultMsg) tea.Cmd {
	if msg.runID != m.detail.runID || msg.gen != m.detail.gen {
		return nil
	}
	a := &m.detail.answer
	for i := range a.drafts {
		d := &a.drafts[i]
		if d.snapshot.QuestionID != msg.questionID || d.pending != msg.requestID || msg.requestID == 0 {
			continue
		}
		d.pending = 0 // even an old identity must retire its own request
		m.reconcileAnswer()
		var ex *uzicli.ExitError
		switch {
		case msg.err == nil:
			d.sent = true
			if i == a.active && !d.closed {
				a.answerNotice = "answer sent; waiting for the agent to resume"
			}
		case errors.As(msg.err, &ex) && ex.Code == uzicli.ExitConflict:
			d.conflict = true
			if i != a.active || d.closed {
				return nil
			}
			a.answerNotice = "question no longer open"
			var cmds []tea.Cmd
			if m.detail.metaWaitID == 0 {
				cmds = append(cmds, m.startDetailMetaReq())
			}
			if m.detail.highSeq > 0 && m.detail.catchupWaitID == 0 {
				cmds = append(cmds, m.startDetailCatchupReq())
			} else if m.detail.highSeq == 0 && !m.detail.tailInFlight {
				m.detail.tailInFlight = true
				cmds = append(cmds, m.loadTailCmd(m.detail.runID))
			}
			return tea.Batch(cmds...)
		case errors.As(msg.err, &ex) && (ex.Code == uzicli.ExitUsage || ex.Code == uzicli.ExitAuth || ex.Code == uzicli.ExitNotFound):
			if i == a.active && !d.closed {
				a.answerNotice = "answer rejected: " + msg.err.Error()
			}
		default:
			d.uncertain = true
			if i == a.active && !d.closed {
				a.answerNotice = answerUncertainNotice
			}
		}
		return nil
	}
	return nil
}

// answerNoticeLines renders outside the conditional question card, so refresh,
// status changes and disappearing frames cannot hide a delivery outcome.
func (m tuiModel) answerNoticeLines() []string {
	if m.detail.answer.answerNotice == "" {
		return nil
	}
	text := m.renderer.Plain(m.detail.answer.answerNotice, 2000)
	text = strings.Replace(text, ". Resend guard", ".\nResend guard", 1)
	lines := strings.Split(ansi.Hardwrap(ansi.Wrap(text, max(1, m.width), " "), max(1, m.width), true), "\n")
	budget := max(2, min(4, m.height/4))
	if len(lines) > budget || len([]rune(m.detail.answer.answerNotice)) > 200 {
		lines = append(lines[:min(len(lines), budget-1)], clampVisual(m.answerNoticeOverflow(), m.width))
	}
	return lines
}

func (m tuiModel) answerNoticeOverflow() string {
	if d := m.detail.answer.draft(); d != nil && d.uncertain {
		return "… web/Slack · local resend guard"
	}
	return "… more; use web/Slack · esc close"
}

// answerCardLines reserves input and controls before clipping scrollable prose.
// Untrusted prose is sanitized before wrapping; focus has a text marker
// so it survives an Ascii writer as well as a colour-capable terminal.
func (m tuiModel) answerCardLines(rows int) []string {
	lines, _, _ := m.answerLayout(rows)
	return lines
}

// answerLayout returns the card, scroll limit and currently rendered prose offset.
func (m tuiModel) answerLayout(rows int) ([]string, int, int) {
	d := m.detail.answer.draft()
	if d == nil || rows == 0 {
		return nil, 0, 0
	}
	w := max(1, m.width)
	wrap := func(s string) []string {
		return strings.Split(ansi.Hardwrap(ansi.Wrap(s, max(1, w-2), " "), max(1, w-2), true), "\n")
	}
	// Prose uses the existing lossless question renderer; Plain is a capped cell.
	prose := m
	if r, err := newTUIRenderer(max(1, w-2)+4, m.dark); err == nil {
		prose.renderer = r
	}
	if m.answerTooSmall() {
		lines := []string{clampVisual("┃ resize required to answer", w), clampVisual("┃ esc cancel", w)}
		return lines[:min(rows, len(lines))], 0, 0
	}
	title := "┃ ✎ SEND ANSWER?"
	var content []string
	input, hints := "", "┃ enter send · shift-tab edit · esc cancel · ↑↓/pgup/pgdown scroll"
	if visualWidth(hints) > w {
		hints = "┃ ↵send ⇧tab edit esc cancel ↑↓/pgup/dn"
	}
	focusLine := 0
	if d.review {
		body := composeQuestionAnswers(d.snapshot, d.selected, d.answerText)
		for i, q := range d.snapshot.Questions {
			header := q.Header
			if header == "" {
				header = q.Question
			}
			// Plain uses cellText, whose 200-rune cap is inappropriate for review.
			// Use the same sanitizeTTY boundary as questionProse, with literal
			// formatting so Markdown cannot hide any part of the submitted answer.
			text := sanitizeTTY(fmt.Sprintf("%d. %s → %s", i+1, header, body.Answers[i]))
			content = append(content, wrap(strings.ReplaceAll(text, "\t", " "))...)
		}
	} else {
		q := d.snapshot.Questions[d.position]
		title = fmt.Sprintf("┃ ✎ ANSWER %d/%d", d.position+1, len(d.snapshot.Questions))
		if q.Header != "" {
			title += " · " + strings.ToUpper(m.renderer.Plain(q.Header, 10000))
		}
		content = append(content, strings.Split(prose.questionProse(q.Question, "", w-2), "\n")...)
		for i, o := range q.Options {
			mark := "  "
			if i == d.option {
				mark = "› "
				focusLine = len(content)
			}
			pick := "[ ]"
			if d.selected[d.position][i] {
				pick = "[x]"
			}
			content = append(content, wrap(fmt.Sprintf("%s%d %s %s", mark, i+1, pick, m.renderer.Plain(o.Label, len([]rune(o.Label)))))...)
			if o.Description != "" {
				content = append(content, strings.Split(prose.questionProse(o.Description, "   ", w-2), "\n")...)
			}
		}
		focus := "  "
		if d.detailFocus {
			focus = "› "
		}
		prefix := "┃ " + focus + "detail (optional) › "
		// Select from the full sanitized original before Plain's 200-rune cell
		// cap. Bound the suffix by actual columns so its insertion end survives.
		text := strings.Join(strings.Fields(uzicli.CellText(d.answerText[d.position])), " ")
		runes := []rune(text)
		if visualWidth(prefix+text) > w || len(runes) > 200 {
			prefix += "…"
			budget := max(0, w-visualWidth(prefix))
			start := len(runes)
			for start > 0 && len(runes)-start < 200 {
				if visualWidth(string(runes[start-1:])) > budget {
					break
				}
				start--
			}
			text = string(runes[start:])
		}
		input = prefix + m.renderer.Plain(text, 200)
		hints = "┃ ↑↓/1-9 pick · space toggle · tab detail · enter next · shift-tab back · esc cancel · pgup/pgdown"
		if visualWidth(hints) > w {
			hints = "┃ ↑↓/1-9 pick space tab ↵next ⇧tab back esc cancel pgup/pgdown"
		}
		if visualWidth(hints) > w {
			hints = "↑↓1-9 spc tab ↵next ⇧tab esc cancel Pg↑↓"
		}
	}
	var bottom []string
	if input != "" {
		bottom = append(bottom, clampVisual(input, w))
		if d.manualScroll {
			q := d.snapshot.Questions[d.position]
			if len(q.Options) > 0 {
				pick := "[ ]"
				if d.selected[d.position][d.option] {
					pick = "[x]"
				}
				bottom = append(bottom, clampVisual(fmt.Sprintf("┃ › %d %s %s", d.option+1, pick,
					m.renderer.Plain(q.Options[d.option].Label, 200)), w))
			}
		}
	}
	if d.drift {
		bottom = append(bottom, wrap("question changed; this draft is blocked; answer from the web or Slack")...)
	} else if d.closed {
		bottom = append(bottom, wrap("question no longer open; i opens a new question after esc")...)
	} else if m.detail.run.Status != "awaiting_input" {
		bottom = append(bottom, wrap("draft suspended until the run needs input again")...)
	} else if m.detail.steer.access != steerAllowed {
		bottom = append(bottom, wrap("draft suspended: ownership is not confirmed")...)
	}
	bottom = append(bottom, m.answerNoticeLines()...)
	if d.pending != 0 || d.sent || d.uncertain || d.conflict || d.closed || d.drift {
		hints = "┃ esc close · send/edit disabled"
	}
	// Bound notices before adding the single-row controls. Keep input and an
	// actionable fallback when a rejection or uncertainty explanation overflows.
	noticeBudget := max(1, rows-5)
	if len(bottom) > noticeBudget {
		bottom = append(bottom[:noticeBudget-1], clampVisual(m.answerNoticeOverflow(), w))
	}
	bottom = append(bottom, clampVisual(hints, w))
	available := max(0, rows-1-len(bottom))
	overflow := len(content) > available
	if overflow {
		available-- // direction indicator is separate from every reachable content row
	}
	limit := max(0, len(content)-available)
	start := 0
	if d.review {
		start = min(d.reviewScroll, limit)
	} else if d.manualScroll {
		start = min(d.editScroll, limit)
	} else if focusLine >= available {
		start = max(0, focusLine-available+1)
	}
	end := min(len(content), start+available)
	lines := []string{clampVisual(title, w)}
	for _, line := range content[start:end] {
		lines = append(lines, "┃ "+line)
	}
	if overflow {
		lines = append(lines, clampVisual(fmt.Sprintf("┃ … ↑ %d lines · ↓ %d lines", start, len(content)-end), w))
	}
	lines = append(lines, bottom...)
	return lines, limit, start
}

// Composition uses the existing pinned slot. If it cannot coexist with a useful
// transcript viewport, compact layout reclaims that body without changing scroll.
func (m tuiModel) renderAnswerDetail() string {
	header := m.detailHeaderLines()
	if line := m.transportLine(); line != "" {
		header = append(header, clampVisual(line, m.width))
	}
	room := m.answerRoom()
	card := m.answerCardLines(room)
	if len(card)+5 <= room {
		header = append(header, card...)
		header = append(header, strings.TrimRight(m.joinColumns(m.renderLaneRail(), m.renderTranscript(), laneRailWidth), "\n"))
	} else {
		header = append(header, card...)
	}
	return strings.Join(header, "\n")
}
