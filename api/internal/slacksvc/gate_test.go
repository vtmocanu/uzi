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
		want := []string{ActionGateApprove, ActionGateReject, ActionGateOpen}
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

	want := []string{ActionGateApproveRepo, ActionGateApproveOwn, ActionGateReject, ActionGateOpen}
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
