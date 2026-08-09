package slacksvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// chatMsgEvent is one chat run message frame bound for Slack turn streaming (PRD #191
// M3). kind is the MESSAGE kind (user_message/text/status/error), not the run kind;
// payload is the raw run_message payload JSON.
type chatMsgEvent struct {
	runID   uuid.UUID
	kind    string
	payload []byte
}

// chatConvo is the per-chat-run turn-coalescing state. Owned by the drain goroutine,
// so no lock. One "turn" spans a user_message (start) to the first terminal frame
// (result / error / a status-with-text end line); text frames in between accrue into
// buf, and the whole turn resolves as ONE edit of ONE placeholder — never an
// edit-per-frame (Decision 6: postMessage is ~1/sec special-tier, and a visibly
// rewriting message reads worse than a short wait).
type chatConvo struct {
	channel       string
	rootTS        string // the conversation thread parent (the user's opening message)
	link          string // "<url|Open in uzi>" or ""
	placeholderTS string // the current turn's posted placeholder, "" if the post failed
	buf           []string
	bufRunes      int // running rune count of buf, so a long turn can't amplify memory
	active        bool
}

const (
	// chatThinkingText is the per-turn placeholder, posted on user_message as a context
	// block and edited to the answer on turn end (PRD #268 M4). A sibling context element
	// carries the deep link so an orphaned placeholder (an api restart mid-turn) is still
	// useful (Decision 6).
	chatThinkingText = "💬 _uzi is thinking…_"
	// chatThinkingFallback is the fixed plain-text notification fallback for the
	// placeholder Block Kit post (never model text — the placeholder has none yet).
	chatThinkingFallback = "uzi is thinking…"
	// chatNoAnswerText resolves a turn that produced no text frames (e.g. a tool-only
	// turn) so the placeholder never strands.
	chatNoAnswerText = "_(this turn produced no text reply — open it in uzi for the details)_"
	// chatDynamicMax bounds a worker-authored status/error note (timeout line, redacted
	// error) — the answer body is bounded by truncateForSlackSection, but `extra` rides
	// its own path, so it needs its own cap (mirrors renderThread's boundReason).
	chatDynamicMax = 500
	// chatConvosCap bounds the turn-state map. Above it, the drain evicts skip markers
	// (nil entries — pure classification cache, cheap to rebuild), so a long-lived
	// process that has seen many runs cannot grow the map without bound even if a
	// terminal transition was missed (a crash) or a frame trailed one.
	chatConvosCap = 4096
)

// chatRelevantKind reports whether a message kind can participate in a chat turn, so
// PublishMessage enqueues only those (Decision 6: thinking/tool frames never post).
func chatRelevantKind(kind string) bool {
	switch kind {
	case "user_message", "text", "status", "error", "proposal", "run_request":
		return true
	default:
		return false
	}
}

// handleChatMsg routes one chat message frame through turn coalescing. The first frame
// for a run classifies it (chat + anchored, or skip); the decision is cached so later
// frames — and PublishMessage's hot path — drop in O(1) for a non-chat run.
func (n *Notifier) handleChatMsg(ctx context.Context, ev chatMsgEvent) {
	convo, known := n.chatConvos[ev.runID]
	if !known {
		// Bound the map before adding a new classification: a terminal transition frees
		// an entry (evictChatConvo), but a missed one (a crash, or a frame trailing the
		// terminal state) would otherwise leak. Skip markers are a pure cache, so evict
		// them when over the cap and rebuild on demand.
		if len(n.chatConvos) >= chatConvosCap {
			n.evictChatSkips()
		}
		convo = n.setupChatConvo(ctx, ev.runID)
		n.chatConvos[ev.runID] = convo // may be nil = "skip" marker
		n.chatDecided.Store(ev.runID, convo != nil)
	}
	if convo == nil {
		return // non-chat, or a chat with no Slack anchor (web-originated)
	}
	n.applyChatFrame(ctx, convo, ev)
}

// evictChatConvo frees a run's turn-streaming state (drain-owned; no lock). Called on
// every terminal transition, for every run kind, so skip markers do not accumulate.
func (n *Notifier) evictChatConvo(runID uuid.UUID) {
	delete(n.chatConvos, runID)
	n.chatDecided.Delete(runID)
}

// evictChatSkips drops all skip markers (nil entries) — the classification cache —
// keeping only live conversations. A hard backstop for entries a terminal transition
// never cleaned. Drain-owned; no lock.
func (n *Notifier) evictChatSkips() {
	for id, c := range n.chatConvos {
		if c == nil {
			delete(n.chatConvos, id)
			n.chatDecided.Delete(id)
		}
	}
}

// setupChatConvo classifies a run on its first message: a kind='chat' run WITH a Slack
// anchor gets a live conversation; anything else (an issue/judge run, or a web chat
// with no anchor) returns nil and is cached as skip. The anchor is inserted by the
// opener before the run is claimed, so a Slack chat's anchor is present by its first
// frame; the rare miss loses that conversation's streaming (best-effort).
func (n *Notifier) setupChatConvo(ctx context.Context, runID uuid.UUID) *chatConvo {
	cc, err := n.store.GetSlackChatContext(ctx, runID)
	if err != nil {
		return nil // not a chat run (or gone)
	}
	if isTerminalStatus(cc.Status) {
		// A frame trailing the terminal transition (the maps were already freed): do not
		// re-create a live conversation for an ended chat — streaming into it would strand
		// a placeholder no future transition will clean. Cache a skip marker instead.
		return nil
	}
	anchor, err := n.store.GetSlackRunMessage(ctx, runID)
	if err != nil {
		return nil // no Slack DM anchor → web-originated chat
	}
	base, _ := n.baseURL(ctx)
	return &chatConvo{channel: anchor.ChannelID, rootTS: anchor.RootTs, link: chatLink(base, runID)}
}

// applyChatFrame advances the turn state machine for one frame. Turn boundaries are
// observable at this seam: user_message starts a turn, and a turn ends on the SDK
// result frame (status.event=="result", happy path), an error frame, or a
// status-with-text end line (timeout, or the conversation-end line that also resolves a
// cancelled turn — cancel emits nothing of its own). All four resolve the placeholder,
// so "uzi is thinking…" never strands on the turns a user most needs explained.
func (n *Notifier) applyChatFrame(ctx context.Context, convo *chatConvo, ev chatMsgEvent) {
	var p struct {
		Text  string `json:"text"`
		Event string `json:"event"`
	}
	if ev.kind == "proposal" {
		// A proposal card is a standalone message, not part of the turn body — post it
		// and leave the active turn's placeholder/buffer untouched (M4).
		n.postProposalCard(ctx, convo, ev.runID, ev.payload)
		return
	}
	if ev.kind == "run_request" {
		// A start-run card, likewise standalone (M5).
		n.postRunRequestCard(ctx, convo, ev.payload)
		return
	}

	if err := json.Unmarshal(ev.payload, &p); err != nil {
		// A payload we can't parse: ignore it rather than let a malformed status frame
		// mis-resolve an open turn (drop later text). Server-generated JSON, so rare.
		return
	}

	switch ev.kind {
	case "user_message":
		// A new turn: resolve any still-open prior turn first (defensive — turns are
		// sequential, so this normally no-ops), then post this turn's placeholder.
		n.flushChatTurn(ctx, convo, "")
		n.startChatTurn(ctx, convo)
	case "text":
		// Buffer text frames up to the render cap; anything beyond would be truncated
		// away at flush anyway, so dropping it here bounds per-turn memory (a long or
		// runaway turn can't amplify the drain goroutine's heap).
		if convo.active && convo.bufRunes < maxSlackSectionRunes {
			convo.buf = append(convo.buf, p.Text)
			convo.bufRunes += len([]rune(p.Text))
		}
	case "status":
		if p.Event == "result" {
			n.flushChatTurn(ctx, convo, "") // happy-path turn end
			return
		}
		// Only a status that carries TEXT (a turn timeout line, the conversation-end
		// line) resolves an open turn. A text-less heartbeat (event:"init", and any
		// future eventless status) must NOT flush the turn before its answer buffers.
		if convo.active && strings.TrimSpace(p.Text) != "" {
			n.flushChatTurn(ctx, convo, chatDynamic(p.Text))
		}
	case "error":
		n.flushChatTurn(ctx, convo, "⚠️ "+chatDynamic(p.Text))
	}
}

// postProposalCard renders a chat agent's issue proposal as a Block Kit card with
// Create / Dismiss in the conversation thread (PRD #191 M4). The payload is the web
// IssueProposal shape the worker emits (uzi-tools.ts): id, title, description, labels,
// repo_path. The press is handled by ChatActions (the slack_chat_* namespace), not by
// this notifier.
func (n *Notifier) postProposalCard(ctx context.Context, convo *chatConvo, runID uuid.UUID, payload []byte) {
	var pp struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
		RepoPath    string   `json:"repo_path"`
	}
	if err := json.Unmarshal(payload, &pp); err != nil {
		n.logf("parse proposal payload", err)
		return
	}
	propID, err := uuid.Parse(strings.TrimSpace(pp.ID))
	if err != nil {
		n.logf("parse proposal id", err)
		return
	}
	blocks := proposalCardBlocks(runID, propID, pp.Title, pp.Description, pp.Labels, pp.RepoPath)
	if _, err := n.poster.PostBlocks(ctx, convo.channel, convo.rootTS, "New issue proposal from uzi chat", blocks); err != nil {
		n.logf("post proposal card", err)
	}
}

// postRunRequestCard renders a chat agent's start_run request as a Block Kit card with
// Start / Dismiss in the conversation thread (PRD #191 M5). The payload is the tool's
// {repo_path, issue_iid, title}; the press is handled by ChatActions (slack_chat_*).
func (n *Notifier) postRunRequestCard(ctx context.Context, convo *chatConvo, payload []byte) {
	var rr struct {
		RepoPath string `json:"repo_path"`
		IssueIID int64  `json:"issue_iid"`
		Title    string `json:"title"`
	}
	if err := json.Unmarshal(payload, &rr); err != nil {
		n.logf("parse run_request payload", err)
		return
	}
	if strings.TrimSpace(rr.RepoPath) == "" || rr.IssueIID <= 0 {
		n.logf("run_request payload", errors.New("missing repo_path or issue_iid"))
		return
	}
	blocks := runRequestCardBlocks(rr.RepoPath, rr.IssueIID, rr.Title)
	if _, err := n.poster.PostBlocks(ctx, convo.channel, convo.rootTS, "Start-run request from uzi chat", blocks); err != nil {
		n.logf("post run_request card", err)
	}
}

// startChatTurn posts the turn's placeholder in the conversation thread and records
// its ts for the later edit (PRD #268 M4: a Block Kit context block, not plain text).
// The placeholder is a single de-emphasized context block — chatThinkingText, plus a
// sibling context element carrying the deep link so an orphaned placeholder (an api
// restart mid-turn) still opens the run. A post failure leaves placeholderTS empty;
// flushChatTurn then posts the answer fresh rather than editing nothing.
func (n *Notifier) startChatTurn(ctx context.Context, convo *chatConvo) {
	convo.buf = nil
	convo.bufRunes = 0
	convo.active = true
	convo.placeholderTS = ""

	ctxElems := []slack.MixedElement{threadMrkdwnElem(chatThinkingText)}
	if convo.link != "" {
		// The deep link is server-derived <url|label> mrkdwn; ScrubSecrets is a no-op on a
		// clean link, kept as the last-line-of-defense outbound scrub every block obeys.
		ctxElems = append(ctxElems, threadMrkdwnElem(ScrubSecrets("🔗 "+convo.link)))
	}
	blocks := []slack.Block{slack.NewContextBlock("slack_chat_turn_placeholder", ctxElems...)}

	ts, err := n.poster.PostBlocks(ctx, convo.channel, convo.rootTS, chatThinkingFallback, blocks)
	if err != nil {
		n.logf("post chat placeholder", err)
		return
	}
	convo.placeholderTS = ts
}

// flushChatTurn resolves the active turn as ONE Block Kit edit (PRD #268 M4). The body
// assembly is unchanged — the scrubbed/escaped/truncated text-frame body, with extra (a
// timeout/error note) appended or used alone. Then it is shaped into blocks:
//   - a NON-empty body becomes a `section` block, with the deep link moved OUT to its own
//     `context` block (never glued into the section text anymore);
//   - an EMPTY body (a rare post-M1 tool-only turn) degrades to a de-emphasized `context`
//     block carrying chatNoAnswerText, plus the deep-link element when present.
//
// The fallback is the assembled body when non-empty (already EscapeMrkdwn'd, so any
// mention/link in it is inert — safe as the OS-notification text), rune-bounded so a long
// turn can't flood it; chatNoAnswerText when empty. A no-op when no turn is open.
func (n *Notifier) flushChatTurn(ctx context.Context, convo *chatConvo, extra string) {
	if !convo.active {
		return
	}
	convo.active = false

	body := renderChatBody(strings.Join(convo.buf, ""))
	switch {
	case body != "" && extra != "":
		body += "\n\n" + extra
	case body == "":
		body = extra
	}

	linkElem := func() (slack.MixedElement, bool) {
		if convo.link != "" {
			// Server-derived <url|label> mrkdwn; ScrubSecrets is a no-op-on-clean last line
			// of defense, matching every other block's outbound scrub.
			return threadMrkdwnElem(ScrubSecrets("🔗 " + convo.link)), true
		}
		return nil, false
	}

	var blocks []slack.Block
	var fallback string
	if body != "" {
		// The body is already ScrubSecrets+EscapeMrkdwn'd+truncated by renderChatBody (or
		// chatDynamic for extra) — put it in the section verbatim, do NOT re-render it.
		blocks = append(blocks, threadSectionBlock(body))
		if el, has := linkElem(); has {
			blocks = append(blocks, slack.NewContextBlock("slack_chat_turn_link", el))
		}
		fallback = boundReason(body)
	} else {
		// Rare tool-only turn: degrade to a context block rather than a full section, per
		// PRD #268 M4.
		ctxElems := []slack.MixedElement{threadMrkdwnElem(chatNoAnswerText)}
		if el, has := linkElem(); has {
			ctxElems = append(ctxElems, el)
		}
		blocks = append(blocks, slack.NewContextBlock("slack_chat_turn_empty", ctxElems...))
		fallback = chatNoAnswerText
	}

	if convo.placeholderTS != "" {
		if err := n.poster.UpdateBlocks(ctx, convo.channel, convo.placeholderTS, fallback, blocks); err != nil {
			n.logf("edit chat turn", err)
		}
	} else {
		// The placeholder post had failed: post the answer fresh in the thread.
		if _, err := n.poster.PostBlocks(ctx, convo.channel, convo.rootTS, fallback, blocks); err != nil {
			n.logf("post chat turn", err)
		}
	}
	convo.buf = nil
	convo.bufRunes = 0
	convo.placeholderTS = ""
}

// renderChatBody runs model-authored answer text through the same
// scrub→escape→truncate pipeline the plan blob uses (gate.go): ScrubSecrets removes
// credential patterns, EscapeMrkdwn neutralizes the WHOLE blob (so a `<@U123>` mention,
// a `<https://evil|Open>` link, or stray mrkdwn is inert), then it is bounded to
// maxSlackSectionRunes. Empty in → empty out.
func renderChatBody(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return truncateForSlackSection(EscapeMrkdwn(ScrubSecrets(s)))
}

// chatDynamic renders a short worker-authored status/error note (a timeout line, a
// redacted error) inert and scrubbed, without the section truncation body text gets.
func chatDynamic(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > chatDynamicMax {
		s = string(r[:chatDynamicMax]) + "…"
	}
	return EscapeMrkdwn(ScrubSecrets(s))
}

// chatLink is the "<base>/chat/<id>|Open in uzi" mrkdwn link, or "" when no base URL
// is configured.
func chatLink(base string, runID uuid.UUID) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("<%s/chat/%s|Open in uzi>", strings.TrimRight(base, "/"), runID.String())
}
