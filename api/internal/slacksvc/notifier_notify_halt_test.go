package slacksvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
)

// PRD #1650 D3: the three actionable kinds (ci_autofix_halted, mr_rework_halted,
// guardrail_override_decided) hand the notifier RAW untrusted values — a forge branch
// ref, a repo path — in Title-adjacent Body text, plus a deep link that for
// ci_autofix_halted is forge-supplied. These tests drive the REAL notificationBlocks, so
// they guard the renderer's escaping contract where it is enforced. They build the
// notifyEvent by hand, so they do not prove what any producer sends; the producers'
// own tests pin that.

// hasLinkBlock reports whether the render carries the deep-link context block.
func hasLinkBlock(blocks []slack.Block) bool {
	for _, b := range blocks {
		if cb, ok := b.(*slack.ContextBlock); ok && cb.BlockID == "slack_notify_link" {
			return true
		}
	}
	return false
}

// A regression guard over notificationBlocks' escaping, fed body shapes like the halt
// and override-decision kinds': hostile ref / repo path values are escaped EXACTLY ONCE
// by the renderer (the broadcast mention is inert, the ampersand becomes a single &amp;,
// nothing is double-escaped). It renders a hand-built body, so it is not proof that a
// producer passes raw (unescaped) text; that is pinned in the producers' tests.
func TestNotificationBlocksEscapesUntrustedBodyOnce(t *testing.T) {
	for _, body := range []string{
		// ci_autofix_halted / mr_rework_halted shape: a hostile branch ref.
		"Automatic CI fix stopped on feat/<!channel>&*x*: reached the 2-attempt limit.",
		// guardrail_override_decided shape: a hostile repo path.
		"An instance admin rejected your request to allow grp/<proj>&<!here> through the guardrail.",
	} {
		blocks, fallback := notificationBlocks(notifyEvent{emoji: "🛑", title: "CI auto-fix stopped", body: body})
		_, section := blockSummary(blocks)

		for _, live := range []string{"<!channel>", "<!here>", "<proj>"} {
			if strings.Contains(section, live) || strings.Contains(fallback, live) {
				t.Fatalf("untrusted %q reached Slack unescaped: section %q fallback %q", live, section, fallback)
			}
		}
		for _, dbl := range []string{"&amp;lt;", "&amp;gt;", "&amp;amp;"} {
			if strings.Contains(section, dbl) || strings.Contains(fallback, dbl) {
				t.Fatalf("double-escaped %q in section %q / fallback %q", dbl, section, fallback)
			}
		}
		if !strings.Contains(section, "&lt;") || !strings.Contains(section, "&amp;") {
			t.Fatalf("want the single-escaped &lt; and &amp; in %q", section)
		}
	}
}

// A forge-supplied pipeline URL carrying `|` (or `<`, `>`, whitespace, a control char,
// a non-http scheme, a relative path) would break out of the <url|label> markup, so
// the notifier drops the link block entirely rather than render it.
func TestNotificationBlocksDropsUnsafeLink(t *testing.T) {
	for _, bad := range []string{
		"https://forge.example/p/1|Open in uzi> <!channel",
		"https://forge.example/p/<1>",
		"https://forge.example/p/1 2",
		"https://forge.example/p/1\n2",
		"https://forge.example/p/\x7f",
		"javascript:alert(1)",
		"/p/1",
		"https:///p/1",
	} {
		blocks, _ := notificationBlocks(notifyEvent{title: "CI auto-fix stopped", link: bad, linkLabel: "Open the pipeline"})
		if hasLinkBlock(blocks) {
			t.Fatalf("unsafe link %q must yield no link block, got %q", bad, contextText(blocks))
		}
	}
}

// LinkLabel is plumbed from the SlackRender through PublishNotification to the link
// block; an unset label keeps the historical "Open in uzi" text byte-for-byte.
func TestNotificationLinkLabelPlumbedWithDefault(t *testing.T) {
	n := NewNotifier(&fakeNotifStore{}, &fakePoster{}, fixedBase, nil)
	const url = "https://forge.example/grp/proj/-/pipelines/9"

	n.PublishNotification(uuid.New(), notifysvc.SlackRender{Title: "CI auto-fix stopped", Link: url, LinkLabel: "Open the pipeline"})
	n.PublishNotification(uuid.New(), notifysvc.SlackRender{Title: "judge review ready", Link: url})

	labelled := <-n.notifyCh
	blocks, _ := notificationBlocks(labelled)
	if got := contextText(blocks); !strings.Contains(got, "🔗 <"+url+"|Open the pipeline>") {
		t.Fatalf("custom label not rendered: %q", got)
	}

	plain := <-n.notifyCh
	blocks, _ = notificationBlocks(plain)
	if got := contextText(blocks); got != "🔗 <"+url+"|Open in uzi>" {
		t.Fatalf("default label changed: %q", got)
	}
}

// A custom LinkLabel that could break out of the <url|label> markup (a `|`, `<`, `>`,
// a newline or other control character, or an invisible Unicode format character) falls
// back to the default label, so a future dynamic label cannot inject markup or a second
// link. Today every LinkLabel is a fixed caller constant; this guards the day one is not.
func TestNotificationBlocksUnsafeLinkLabelFallsBackToDefault(t *testing.T) {
	const url = "https://forge.example/grp/proj/-/pipelines/9"
	for _, bad := range []string{
		"Open|<https://evil.example|click>",
		"Open the pipeline|x",
		"Open <!channel>",
		"Open > here",
		"Open\nthe pipeline",
		"Open\x07the pipeline",
		"Open\u202Ethe pipeline",
		"Open\u200Bthe pipeline",
	} {
		blocks, _ := notificationBlocks(notifyEvent{title: "CI auto-fix stopped", link: url, linkLabel: bad})
		if got, want := contextText(blocks), "🔗 <"+url+"|"+defaultNotifyLinkLabel+">"; got != want {
			t.Errorf("label %q rendered %q, want the default %q", bad, got, want)
		}
	}
	// A plain label (with an ampersand, which is escaped, not rejected) still renders.
	blocks, _ := notificationBlocks(notifyEvent{title: "t", link: url, linkLabel: "Pipeline & logs"})
	if got, want := contextText(blocks), "🔗 <"+url+"|Pipeline &amp; logs>"; got != want {
		t.Errorf("safe label rendered %q, want %q", got, want)
	}
}
