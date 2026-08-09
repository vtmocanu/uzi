package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
)

// The generic inbox-notification path (PRD #46 M2): PublishNotification →
// handleNotify. It reuses the run-state path's delivery gating but NOT its
// run/repo rendering, so these tests drive handleNotify directly with a
// notifyEvent and assert on the DM text and the drop conditions.

func linked(id string) pgtype.Text { return pgtype.Text{String: id, Valid: true} }

func TestHandleNotifyPostsDMWhenLinked(t *testing.T) {
	fs := &fakeNotifStore{delivery: linked("U123")}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handleNotify(context.Background(), notifyEvent{
		userID: uuid.New(),
		emoji:  "🔎",
		title:  "Run review ready",
		facts:  []string{"Verdict ✅ *ok*", "1 recommendation", "`improve_uzi`"},
		body:   "The plan looks solid.",
		link:   "https://uzi.example/runs/abc",
	})

	// PRD #268 M3: the generic notification is now a Block Kit post (family D).
	if len(fp.blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 Block Kit DM", len(fp.blocks))
	}
	got := fp.blocks[0].sectionText + fp.blocks[0].contextText
	// The emoji + bold title, the trusted facts (markup intact), the blockquoted body and
	// the deep link all ride the message.
	for _, want := range []string{"🔎", "*Run review ready*", "Verdict ✅ *ok*", "1 recommendation", "`improve_uzi`", "> The plan looks solid.", "<https://uzi.example/runs/abc|Open in uzi>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("DM %q missing %q", got, want)
		}
	}
	// The fallback carries the verdict/count facts with markup stripped — never a raw body alone.
	if fb := fp.blocks[0].fallback; !strings.Contains(fb, "Run review ready") || !strings.Contains(fb, "Verdict ✅ ok") || !strings.Contains(fb, "1 recommendation") {
		t.Fatalf("fallback %q must carry the escaped title + stripped facts", fb)
	}
	// Pin the FULL fallback exactly (mirroring renderThreadBlocks' exact-equality fallback
	// tests): escaped title — stripped facts (the `*` `_` `code` mrkdwn control chars all
	// removed, so `improve_uzi` reads `improveuzi`) — escaped body, joined by " — ", never a
	// raw body alone.
	if fb, want := fp.blocks[0].fallback, "Run review ready — Verdict ✅ ok, 1 recommendation, improveuzi — The plan looks solid."; fb != want {
		t.Fatalf("fallback = %q, want exactly %q", fb, want)
	}
	// PRD #268 M2: the `[uzi]` prefix is gone (a 1:1 DM with the bot never needed it).
	if strings.Contains(got, "[uzi]") {
		t.Fatalf("DM must not carry the [uzi] prefix: %q", got)
	}
	if fp.blocks[0].thread != "" {
		t.Fatalf("a notification is a top-level DM, got thread %q", fp.blocks[0].thread)
	}
}

func TestHandleNotifyDropsUnlinkedUser(t *testing.T) {
	// No delivery row (unlinked / opted out / unconfirmed) ⇒ silent drop.
	fs := &fakeNotifStore{deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), title: "t", body: "b"})

	if len(fp.posts) != 0 || len(fp.blocks) != 0 {
		t.Fatalf("unlinked user must get no DM, got %d posts %d blocks", len(fp.posts), len(fp.blocks))
	}
}

func TestHandleNotifyDropsEmptyDeliveryTarget(t *testing.T) {
	// A row exists but resolves to no target (invalid / blank) ⇒ drop.
	fs := &fakeNotifStore{delivery: pgtype.Text{}}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), title: "t", body: "b"})

	if len(fp.posts) != 0 || len(fp.blocks) != 0 {
		t.Fatalf("empty delivery target must get no DM, got %d posts %d blocks", len(fp.posts), len(fp.blocks))
	}
}

func TestNotificationBlocksEscapesAndScrubs(t *testing.T) {
	// The body is untrusted free text: a spoofed <url|label> link and an embedded
	// secret must both be neutralized, exactly like the run-state renderer does. A fact
	// carrying a secret is scrubbed too, but its trusted markup is kept.
	ev := notifyEvent{
		emoji: "🔎",
		title: "judge review ready",
		facts: []string{"Verdict ⚠️ *issues* token sk-ant-factDEF"},
		body:  "<https://evil.example|click> token sk-ant-abc123DEF",
		link:  "https://uzi.example/runs/1",
	}
	blocks, fallback := notificationBlocks(ev)
	_, section := blockSummary(blocks)
	got := section + contextText(blocks)

	if strings.Contains(got, "<https://evil.example|click>") {
		t.Fatalf("hostile mrkdwn link not escaped: %q", got)
	}
	if strings.Contains(got, "sk-ant-abc123DEF") || strings.Contains(got, "sk-ant-factDEF") {
		t.Fatalf("anthropic key not scrubbed: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected the scrubbed placeholder in %q", got)
	}
	// The trusted fact markup survives (facts are scrubbed but not escaped).
	if !strings.Contains(got, "Verdict ⚠️ *issues*") {
		t.Fatalf("trusted fact markup was mangled: %q", got)
	}
	// The untrusted body is blockquoted and escaped.
	if !strings.Contains(got, "> ") {
		t.Fatalf("body not blockquoted: %q", got)
	}
	// The trusted deep link is kept as real markup.
	if !strings.Contains(got, "<https://uzi.example/runs/1|Open in uzi>") {
		t.Fatalf("trusted deep link missing/mangled: %q", got)
	}
	// The fallback never leaks a secret either.
	if strings.Contains(fallback, "sk-ant-") {
		t.Fatalf("fallback leaked a secret: %q", fallback)
	}
}

func TestNotificationBlocksOmitsEmptyBodyFactsAndLink(t *testing.T) {
	blocks, fallback := notificationBlocks(notifyEvent{emoji: "🔧", title: "self-improvement MR opened"})
	// Only the head section — no facts context, no blockquote, no link context.
	if len(blocks) != 1 {
		t.Fatalf("want exactly one section block when body/facts/link are empty, got %d", len(blocks))
	}
	_, section := blockSummary(blocks)
	if !strings.Contains(section, "self-improvement MR opened") || !strings.Contains(section, "🔧") {
		t.Fatalf("head missing emoji/title: %q", section)
	}
	if strings.Contains(fallback, " — ") {
		t.Fatalf("no separator expected when there are no facts or body: %q", fallback)
	}
}

func TestPublishNotificationNeverBlocks(t *testing.T) {
	n := NewNotifier(&fakeNotifStore{}, &fakePoster{}, fixedBase, nil)
	// Overfill the queue: with nothing draining, PublishNotification must still
	// return (dropping the overflow) rather than block.
	for i := 0; i < notifierQueue+10; i++ {
		n.PublishNotification(uuid.New(), notifysvc.SlackRender{Title: "t", Body: "b"})
	}
}
