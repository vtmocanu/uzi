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
// ci_autofix_halted is forge-supplied. These tests drive the REAL notificationBlocks so
// the escaping contract is proven where it is enforced.

// hasLinkBlock reports whether the render carries the deep-link context block.
func hasLinkBlock(blocks []slack.Block) bool {
	for _, b := range blocks {
		if cb, ok := b.(*slack.ContextBlock); ok && cb.BlockID == "slack_notify_link" {
			return true
		}
	}
	return false
}

// Hostile ref / repo path values are escaped EXACTLY ONCE: the broadcast mention is
// inert, the ampersand becomes a single &amp;, and nothing is double-escaped
// (&amp;lt; / &amp;amp; would mean a producer pre-escaped before the notifier did).
func TestNotificationBlocksHaltKindsEscapeUntrustedOnce(t *testing.T) {
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
