package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/slack-go/slack"
)

// The generic inbox-notification DM seam (PRD #46): handleNotify delivers a
// notifysvc-published notification to a user's DM and its Block Kit render.

// handleNotify delivers one generic inbox notification to the user's DM (PRD #46
// M2). It reuses the run-state path's delivery resolution + per-user opt-in gating
// (GetSlackDeliveryForUser) but NOT its rendering: there is no run/repo context, so
// it never calls GetSlackRunContext. Unlinked / opted-out / unconfirmed users drop
// silently; every failure logs redacted and returns (a Slack problem never affects
// the caller — the inbox row is already persisted).
func (n *Notifier) handleNotify(ctx context.Context, ev notifyEvent) {
	target, err := n.store.GetSlackDeliveryForUser(ctx, ev.userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // unlinked, opted out, or unconfirmed → drop silently
	}
	if err != nil {
		n.logf("resolve delivery target", err)
		return
	}
	if !target.Valid || target.String == "" {
		return
	}

	channel, err := n.poster.OpenDM(ctx, target.String)
	if err != nil {
		n.logf("open dm", err)
		return
	}

	blocks, fallback := notificationBlocks(ev)
	if _, err := n.poster.PostBlocks(ctx, channel, "", fallback, blocks); err != nil {
		n.logf("post notification", err)
	}
}

// notifyFactMarkupStripper removes the intentional mrkdwn markup from a fact so it can
// go into the plain-text notification fallback (the OS-notification text, which Slack
// parses for mrkdwn). Facts carry `*bold*`, “ `code` “ chips and verdict emoji built
// from closed enums; the emoji is harmless plain text, but the *, `, _ control chars
// must go so the fallback reads cleanly and can never leave a dangling mrkdwn token.
var notifyFactMarkupStripper = strings.NewReplacer("*", "", "`", "", "_", "")

// notificationBlocks builds the content-minimized DM for a generic notification as
// Block Kit (message family D, PRD #268 M3). Shape, in order and only for the parts
// that apply: a section with the caller emoji + bold title; a context block of the
// trusted facts; a blockquote section for the untrusted body; a context block for the
// deep link.
//
// The field-by-field trust discipline is the load-bearing part:
//
//   - title is a FIXED caller label, EscapeMrkdwn'd anyway (defense in depth) before it
//     becomes the bold head; the emoji is a fixed caller glyph rendered raw.
//   - facts are caller-built TRUSTED strings that INTENTIONALLY carry markup (bold, code
//     chips, verdict emoji) built from closed enums/ints, so they are ScrubSecrets'd but
//     NOT EscapeMrkdwn'd — escaping would break the intended markup.
//   - body is UNTRUSTED model/forge free text carrying no trusted markup of its own, so it
//     is whole-blob rendered by SlackMrkdwn (CommonMark→mrkdwn, PRD #292; SlackMrkdwn OWNS
//     its &<>-escaping and REPLACES EscapeMrkdwn — never nested) after ScrubSecrets, then
//     bounded to Slack's section limit. It is blockquote-prefixed line-by-line UNLESS the
//     render emitted a fenced/indented code block (reported by SlackMrkdwnBlock from the AST
//     walk, not a substring scan): a fenced body is emitted as a PLAIN section instead (PRD
//     #292 Decision 6), because a `> ` prefix injected into a fence's interior lines corrupts
//     Slack's code rendering.
//   - the deep link keeps its raw <url|label> markup (operator-set base, http(s)-validated),
//     ScrubSecrets'd as a no-op-on-clean last line of defense.
//
// The fallback is built from FIXED/escaped fields only — never a raw model summary alone:
// the escaped title, then the facts with their markup stripped (so the verdict/count still
// appear as the OS-notification text), then the escaped, length-bounded body.
func notificationBlocks(ev notifyEvent) (blocks []slack.Block, fallback string) {
	title := EscapeMrkdwn(strings.TrimSpace(ev.title))
	head := "*" + title + "*"
	if ev.emoji != "" {
		head = ev.emoji + " " + head
	}
	blocks = []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, head, false, false), nil, nil)}

	if len(ev.facts) > 0 {
		blocks = append(blocks, slack.NewContextBlock("slack_notify_facts",
			slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets(strings.Join(ev.facts, "  ·  ")), false, false)))
	}

	if body := strings.TrimSpace(ev.body); body != "" {
		markdown, hasCodeBlock := SlackMrkdwnBlock(ScrubSecrets(body))
		rendered := truncateForSlackSection(markdown)
		text := rendered
		if !hasCodeBlock {
			// No fenced code: blockquote every line, not just the first — Slack's `>`
			// quotes only the line it leads, so lines 2+ of a multi-line body would
			// otherwise escape the blockquote. A fenced body is emitted as a plain
			// section instead (PRD #292 Decision 6) — the `> ` prefix would corrupt the
			// fence's interior lines and break Slack's code rendering. hasCodeBlock comes
			// from the AST walk, so a literal ``` in PROSE (a Text node, not a fence) is
			// correctly still blockquoted.
			text = "> " + strings.ReplaceAll(rendered, "\n", "\n> ")
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil))
	}

	if url := strings.TrimSpace(ev.link); url != "" {
		blocks = append(blocks, slack.NewContextBlock("slack_notify_link",
			slack.NewTextBlockObject(slack.MarkdownType, ScrubSecrets(fmt.Sprintf("🔗 <%s|Open in uzi>", url)), false, false)))
	}

	return blocks, notificationFallback(ev)
}

// notificationFallback is the OS-notification text for a generic notification, built
// from fixed/escaped fields ONLY (never a raw model summary alone): the escaped title,
// the facts with their trusted markup stripped, and the escaped, length-bounded body.
func notificationFallback(ev notifyEvent) string {
	s := EscapeMrkdwn(strings.TrimSpace(ev.title))
	if len(ev.facts) > 0 {
		s += " — " + EscapeMrkdwn(notifyFactMarkupStripper.Replace(strings.Join(ev.facts, ", ")))
	}
	if body := strings.TrimSpace(ev.body); body != "" {
		// ev.body can now be MULTI-LINE (the judge summary preserves newlines for the
		// Slack blockquote). Collapse its whitespace/newlines to single spaces so the
		// OS-notification preview stays a clean one-liner, then bound + escape it exactly
		// as before (still escaped, still from the same field — just flattened).
		s += " — " + EscapeMrkdwn(boundReason(strings.Join(strings.Fields(body), " ")))
	}
	return ScrubSecrets(s)
}
