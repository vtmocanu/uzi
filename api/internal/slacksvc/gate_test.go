package slacksvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/slack-go/slack"
)

// approveButtonIDs returns the button action ids in the gate's action block, in
// order, plus the concatenated section text.
func gateSummary(blocks []slack.Block) ([]string, string) {
	return blockSummary(blocks)
}

// No detected roster (nil or []) renders the single legacy Approve button (source
// own), byte-identical to the pre-M7 shape.
func TestGateBlocksNoRosterSingleApprove(t *testing.T) {
	for _, names := range [][]string{nil, {}} {
		ids, section := gateSummary(gateBlocks(uuid.New(), "https://uzi.example", names))
		want := []string{ActionGateApprove, ActionGateRequestChanges, ActionGateReject, ActionGateOpen}
		if len(ids) != len(want) {
			t.Fatalf("names=%v: buttons = %v, want %v", names, ids, want)
		}
		for i, w := range want {
			if ids[i] != w {
				t.Fatalf("names=%v: button %d = %q, want %q", names, i, ids[i], w)
			}
		}
		if strings.Contains(section, "Repo agents:") {
			t.Fatalf("names=%v: no roster line should render: %q", names, section)
		}
	}
}

// A detected roster renders two approve buttons (repo primary, own) with the names
// listed in the body — and NEVER any description text.
func TestGateBlocksRosterTwoApproves(t *testing.T) {
	names := []string{"coder", "reviewer", "tester", "auditor"}
	blocks := gateBlocks(uuid.New(), "https://uzi.example", names)
	ids, section := gateSummary(blocks)

	want := []string{ActionGateApproveRepo, ActionGateApproveOwn, ActionGateRequestChanges, ActionGateReject, ActionGateOpen}
	if len(ids) != len(want) {
		t.Fatalf("buttons = %v, want %v", ids, want)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("button %d = %q, want %q (all %v)", i, ids[i], w, ids)
		}
	}
	for _, n := range names {
		if !strings.Contains(section, n) {
			t.Fatalf("roster name %q missing from the body: %q", n, section)
		}
	}
	// The repo approve button labels the count; the primary button is repo.
	repoBtn := firstButton(blocks, ActionGateApproveRepo)
	if repoBtn == nil || repoBtn.Style != slack.StylePrimary {
		t.Fatalf("the repo approve must be the primary button")
	}
	if !strings.Contains(repoBtn.Text.Text, "4") {
		t.Fatalf("the repo approve button must show the count: %q", repoBtn.Text.Text)
	}
	// Every source-scoped approve carries a confirm dialog (the opt-in record).
	if repoBtn.Confirm == nil {
		t.Fatalf("the repo approve must carry a confirm dialog")
	}
}

// Over the cap, the roster line truncates to the first N names with a "+K more"
// tail — the message stays bounded regardless of a 16-agent repo.
func TestGateBlocksRosterTruncates(t *testing.T) {
	names := make([]string, 14)
	for i := range names {
		names[i] = "agent-" + string(rune('a'+i))
	}
	_, section := gateSummary(gateBlocks(uuid.New(), "https://uzi.example", names))
	if !strings.Contains(section, "+4 more") {
		t.Fatalf("expected a '+4 more' tail past the cap of %d: %q", maxGateAgentNames, section)
	}
	// The 11th name (index 10) is past the cap and must not appear.
	if strings.Contains(section, names[10]) {
		t.Fatalf("a name past the cap leaked into the body: %q", section)
	}
}

// revisePendingBlocks prompts for a threaded "what should change" reply and carries
// NO buttons (unlike reject-pending's escape hatch) — the feedback text is required.
func TestRevisePendingBlocksPromptNoButtons(t *testing.T) {
	ids, section := gateSummary(revisePendingBlocks(uuid.New()))
	if len(ids) != 0 {
		t.Fatalf("revise-pending must carry no buttons: %v", ids)
	}
	if !strings.Contains(strings.ToLower(section), "what should change") {
		t.Fatalf("revise-pending must prompt for the change: %q", section)
	}
}

// The plan-in-thread render routes the WHOLE plan blob through SlackMrkdwn (PRD #292
// M3), which owns its own &<>-escaping: any <, >, <@Uxxx> mention, or spoofed
// <https://evil|Open> link a hostile plan embeds is rendered INERT, while the genuine
// "full plan in uzi" deep link stays raw and clickable in its own block.
func TestPlanThreadBlocksEscapesHostilePlan(t *testing.T) {
	runID := uuid.New()
	plan := "Do <b> a thing & ping <@U123> then click <https://evil.example|Open in uzi>"
	blocks := planThreadBlocks(runID, plan, "https://uzi.example")
	_, section := gateSummary(blocks)

	if strings.Contains(section, "<@U123>") || strings.Contains(section, "<https://evil.example|Open in uzi>") {
		t.Fatalf("hostile plan markup survived un-escaped: %q", section)
	}
	if !strings.Contains(section, "&lt;@U123&gt;") || !strings.Contains(section, "&amp;") {
		t.Fatalf("plan blob was not mrkdwn-escaped: %q", section)
	}
	// The genuine deep link (trusted base + uuid) stays raw and clickable in its own
	// context block, OUTSIDE the escaped plan blob.
	if link := contextText(blocks); !strings.Contains(link, "<https://uzi.example/runs/"+runID.String()+"|Open the full plan in uzi>") {
		t.Fatalf("the trusted plan deep link must survive raw outside the escaped blob: %q", link)
	}
}

// The plan blob is now RENDERED by SlackMrkdwn (PRD #292 M3): a markdown heading/bold
// becomes *bold*, a list becomes • bullets, and an https link becomes <url|label>.
func TestPlanThreadBlocksRendersMarkdown(t *testing.T) {
	runID := uuid.New()
	plan := "## Plan\n\n**do** this\n\n- step one\n- step two\n\nsee [docs](https://x)"
	blocks := planThreadBlocks(runID, plan, "https://uzi.example")
	_, section := gateSummary(blocks)

	if !strings.Contains(section, "*Plan*") {
		t.Errorf("a heading must render as a bold line, got %q", section)
	}
	if !strings.Contains(section, "*do*") {
		t.Errorf("**do** must render as *do*, got %q", section)
	}
	if !strings.Contains(section, "• step one") || !strings.Contains(section, "• step two") {
		t.Errorf("a markdown list must render as • bullets, got %q", section)
	}
	if !strings.Contains(section, "<https://x|docs>") {
		t.Errorf("an https link must render as <url|label>, got %q", section)
	}
}

// contextText concatenates the mrkdwn of every context block (blockSummary only
// captures section + action blocks).
func contextText(blocks []slack.Block) string {
	out := ""
	for _, b := range blocks {
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

// A plan over the 3000-char section cap is truncated on a RUNE boundary (multi-byte
// runes never split), and the deep link — a separate block outside the truncated
// region — always survives (PRD #41 Decision 10).
func TestPlanThreadBlocksTruncatesOnRuneBoundary(t *testing.T) {
	runID := uuid.New()
	// A long run of a 3-byte rune ('世'), well past the section cap.
	plan := strings.Repeat("世", maxSlackSectionRunes+500)
	blocks := planThreadBlocks(runID, plan, "https://uzi.example")
	_, section := gateSummary(blocks)

	if len([]rune(section)) > maxSlackSectionRunes+2 { // +2 for the "\n…" tail
		t.Fatalf("section not bounded to the cap: %d runes", len([]rune(section)))
	}
	if !strings.Contains(section, "…") {
		t.Fatalf("an over-long plan must be visibly truncated: %q", section[:60])
	}
	// Every rune in the truncated section must be intact (no split multi-byte rune →
	// no U+FFFD replacement char).
	if strings.ContainsRune(section, '�') {
		t.Fatalf("truncation split a multi-byte rune (found U+FFFD)")
	}
	// The deep link lives in its own block and survives regardless of plan length.
	link := ""
	for _, b := range blocks {
		if cb, ok := b.(*slack.ContextBlock); ok && cb.ContextElements.Elements != nil {
			for _, el := range cb.ContextElements.Elements {
				if txt, ok := el.(*slack.TextBlockObject); ok {
					link += txt.Text
				}
			}
		}
	}
	if !strings.Contains(link, "/runs/"+runID.String()) {
		t.Fatalf("the deep link must survive outside the truncated region: %q", link)
	}
}

// truncateForSlackSection is a no-op under the cap and rune-safe over it.
func TestTruncateForSlackSection(t *testing.T) {
	short := "just a short plan"
	if got := truncateForSlackSection(short); got != short {
		t.Fatalf("under-cap text must pass through unchanged: %q", got)
	}
	long := strings.Repeat("é", maxSlackSectionRunes+100) // 2-byte rune
	got := truncateForSlackSection(long)
	if strings.ContainsRune(got, '�') {
		t.Fatalf("truncation split a multi-byte rune")
	}
	if len([]rune(got)) > maxSlackSectionRunes+2 {
		t.Fatalf("truncated text exceeds the cap: %d runes", len([]rune(got)))
	}
}

// firstButton finds the first button element with the given action id.
func firstButton(blocks []slack.Block, actionID string) *slack.ButtonBlockElement {
	for _, b := range blocks {
		if ab, ok := b.(*slack.ActionBlock); ok && ab.Elements != nil {
			for _, el := range ab.Elements.ElementSet {
				if btn, ok := el.(*slack.ButtonBlockElement); ok && btn.ActionID == actionID {
					return btn
				}
			}
		}
	}
	return nil
}
