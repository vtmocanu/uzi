package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1429 M5: `uzi run get`/`run list` render the run's actual harness and an honest
// per-run cost reading (D7). These pin the exact rendered text per cost_status.
// renderDetail is the shared `run get` render helper, defined in render_test.go.

// TestRenderRunDetailHarness: HARNESS always renders the run's actual harness, unconditionally
// (like KIND/STATUS), including an unrecognised future value rendered honestly rather than
// dropped or special-cased.
func TestRenderRunDetailHarness(t *testing.T) {
	for _, h := range []string{"claude", "codex", "some-future-harness"} {
		out := renderDetail(t, apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "running", Harness: h})
		if !strings.Contains(out, "HARNESS") || !strings.Contains(out, h) {
			t.Errorf("harness %q: expected a HARNESS row naming it, got:\n%s", h, out)
		}
	}
}

// TestRenderRunDetailCostMetered: a metered run renders the honest dollar figure via
// fmtCostCents.
func TestRenderRunDetailCostMetered(t *testing.T) {
	out := renderDetail(t, apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "completed",
		Usage: &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 4.20, InputTokens: 100},
	})
	if !strings.Contains(out, "COST") || !strings.Contains(out, "$4.20") {
		t.Errorf("a metered run should render COST $4.20, got:\n%s", out)
	}
}

// TestRenderRunDetailCostSubscription: a subscription-status run renders the word
// "subscription" — never a dollar figure, never $0 — even at cost_usd == 0.
func TestRenderRunDetailCostSubscription(t *testing.T) {
	out := renderDetail(t, apitypes.RunDTO{
		ID: "run-1", Kind: "issue", Status: "completed",
		Usage: &apitypes.UsageDTO{CostStatus: "subscription", CostUSD: 0, InputTokens: 500},
	})
	if !strings.Contains(out, "COST") || !strings.Contains(out, "subscription") {
		t.Errorf("a subscription-status run should render COST subscription, got:\n%s", out)
	}
	if strings.Contains(out, "$0") {
		t.Errorf("a subscription-status run must never render a dollar figure:\n%s", out)
	}
}

// TestRenderRunDetailCostUnreported: an unreported-status run renders "cost unavailable" with
// its token count, never a guessed dollar figure. The pre-M1 empty cost_status folds to the
// same safe bucket. "future-v2" stands in for a cost_status value this build has never heard
// of — a NEWER server can extend the wire enum without this build knowing, and it must fold
// to the SAME safe cost-unavailable bucket rather than being guessed as metered/subscription
// (mirrors web costStatus.ts's hostile-enum coverage, D7).
func TestRenderRunDetailCostUnreported(t *testing.T) {
	for _, status := range []string{"unreported", "", "future-v2"} {
		out := renderDetail(t, apitypes.RunDTO{
			ID: "run-1", Kind: "issue", Status: "completed",
			Usage: &apitypes.UsageDTO{CostStatus: status, CostUSD: 0, InputTokens: 500, OutputTokens: 300},
		})
		if !strings.Contains(out, "cost unavailable") {
			t.Errorf("cost_status %q: expected 'cost unavailable', got:\n%s", status, out)
		}
		if strings.Contains(out, "$") {
			t.Errorf("cost_status %q: must never render a dollar figure:\n%s", status, out)
		}
	}
}

// TestRenderRunDetailCostAbsentWithoutUsage: a run with no usage bundle at all renders no COST
// row (pre-#40 / unclaimed run), matching the emit-only-when-present convention of its
// neighbours.
func TestRenderRunDetailCostAbsentWithoutUsage(t *testing.T) {
	out := renderDetail(t, apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "queued"})
	if strings.Contains(out, "COST") {
		t.Errorf("a run with no Usage must render no COST row, got:\n%s", out)
	}
}

// TestRunListHarnessColumn: `run list` marks Codex explicitly and leaves Claude unmarked (the
// terse CLI twin of the web HarnessBadge convention). The HARNESS cell is extracted by its
// column position (between the HARNESS and TITLE header labels, per newRunListCmd's fixed
// column order ID/KIND/STATUS/AGE/HARNESS/TITLE) rather than a whole-row Contains, because the
// TITLE cell right after it literally contains the word "claude" ("claude run") — a
// harnessListCell regression that marked Claude explicitly would still pass a bare
// strings.Contains(claudeLine, "claude") check, since that substring is already present via
// the title.
func TestRunListHarnessColumn(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Harness: "codex", IssueTitle: "codex run"}},
		{RunDTO: apitypes.RunDTO{ID: "r2", Kind: "issue", Status: "running", Harness: "claude", IssueTitle: "claude run"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !strings.Contains(lines[0], "HARNESS") {
		t.Fatalf("expected a HARNESS column header, got:\n%s", out)
	}
	header := lines[0]
	harnessStart := strings.Index(header, "HARNESS")
	titleStart := strings.Index(header, "TITLE")
	if harnessStart < 0 || titleStart < 0 || titleStart <= harnessStart {
		t.Fatalf("expected a HARNESS column before a TITLE column in the header, got: %q", header)
	}

	var codexLine, claudeLine string
	for _, ln := range lines[1:] {
		if strings.Contains(ln, "codex run") {
			codexLine = ln
		}
		if strings.Contains(ln, "claude run") {
			claudeLine = ln
		}
	}
	if codexLine == "" {
		t.Fatalf("expected a row for the codex run, got:\n%s", out)
	}
	if claudeLine == "" {
		t.Fatalf("expected a row for the claude run, got:\n%s", out)
	}

	// tabwriter aligns every row's columns to the same start offsets as the header, so slicing
	// [harnessStart:titleStart] on a data row yields exactly the HARNESS cell (padded with
	// trailing spaces to the column width).
	harnessCell := func(line string) string {
		end := titleStart
		if end > len(line) {
			end = len(line)
		}
		if harnessStart >= end {
			return ""
		}
		return strings.TrimSpace(line[harnessStart:end])
	}

	if got := harnessCell(codexLine); got != "codex" {
		t.Errorf("a codex-harness run's HARNESS cell should be exactly \"codex\", got %q in row: %q", got, codexLine)
	}
	if got := harnessCell(claudeLine); got != "" {
		t.Errorf("a claude-harness run's HARNESS cell must be blank (unmarked), got %q in row: %q", got, claudeLine)
	}
}
