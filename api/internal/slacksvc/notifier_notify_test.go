package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
		title:  "judge review ready",
		body:   "verdict: issues",
		link:   "https://uzi.example/runs/abc",
	})

	if len(fp.posts) != 1 {
		t.Fatalf("posts = %d, want 1 DM", len(fp.posts))
	}
	got := fp.posts[0].text
	for _, want := range []string{"judge review ready", "verdict: issues", "<https://uzi.example/runs/abc|Open in uzi>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("DM %q missing %q", got, want)
		}
	}
	// PRD #268 M2: the `[uzi]` prefix is gone (a 1:1 DM with the bot never needed it).
	if strings.Contains(got, "[uzi]") {
		t.Fatalf("DM must not carry the [uzi] prefix: %q", got)
	}
	if fp.posts[0].thread != "" {
		t.Fatalf("a notification is a top-level DM, got thread %q", fp.posts[0].thread)
	}
}

func TestHandleNotifyDropsUnlinkedUser(t *testing.T) {
	// No delivery row (unlinked / opted out / unconfirmed) ⇒ silent drop.
	fs := &fakeNotifStore{deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), title: "t", body: "b"})

	if len(fp.posts) != 0 {
		t.Fatalf("unlinked user must get no DM, got %d posts", len(fp.posts))
	}
}

func TestHandleNotifyDropsEmptyDeliveryTarget(t *testing.T) {
	// A row exists but resolves to no target (invalid / blank) ⇒ drop.
	fs := &fakeNotifStore{delivery: pgtype.Text{}}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)

	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), title: "t", body: "b"})

	if len(fp.posts) != 0 {
		t.Fatalf("empty delivery target must get no DM, got %d posts", len(fp.posts))
	}
}

func TestRenderNotificationEscapesAndScrubs(t *testing.T) {
	// The body is untrusted free text: a spoofed <url|label> link and an embedded
	// secret must both be neutralized, exactly like the run-state renderer does.
	ev := notifyEvent{
		title: "judge review ready",
		body:  "<https://evil.example|click> token sk-ant-abc123DEF",
		link:  "https://uzi.example/runs/1",
	}
	got := renderNotification(ev)

	if strings.Contains(got, "<https://evil.example|click>") {
		t.Fatalf("hostile mrkdwn link not escaped: %q", got)
	}
	if strings.Contains(got, "sk-ant-abc123DEF") {
		t.Fatalf("anthropic key not scrubbed: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected the scrubbed placeholder in %q", got)
	}
	// The trusted deep link is kept as real markup.
	if !strings.Contains(got, "<https://uzi.example/runs/1|Open in uzi>") {
		t.Fatalf("trusted deep link missing/mangled: %q", got)
	}
}

func TestRenderNotificationOmitsEmptyBodyAndLink(t *testing.T) {
	got := renderNotification(notifyEvent{title: "self-improvement MR opened"})
	if strings.Contains(got, " — ") {
		t.Fatalf("no body separator expected when body is empty: %q", got)
	}
	if strings.Contains(got, "Open in uzi") {
		t.Fatalf("no link markup expected when link is empty: %q", got)
	}
	if !strings.Contains(got, "self-improvement MR opened") {
		t.Fatalf("title missing: %q", got)
	}
}

func TestPublishNotificationNeverBlocks(t *testing.T) {
	n := NewNotifier(&fakeNotifStore{}, &fakePoster{}, fixedBase, nil)
	// Overfill the queue: with nothing draining, PublishNotification must still
	// return (dropping the overflow) rather than block.
	for i := 0; i < notifierQueue+10; i++ {
		n.PublishNotification(uuid.New(), "t", "b", "")
	}
}
