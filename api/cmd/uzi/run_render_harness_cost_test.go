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
// same safe bucket.
func TestRenderRunDetailCostUnreported(t *testing.T) {
	for _, status := range []string{"unreported", ""} {
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
// terse CLI twin of the web HarnessBadge convention).
func TestRunListHarnessColumn(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Harness: "codex", IssueTitle: "codex run"}},
		{RunDTO: apitypes.RunDTO{ID: "r2", Kind: "issue", Status: "running", Harness: "claude", IssueTitle: "claude run"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HARNESS") {
		t.Fatalf("expected a HARNESS column header, got:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	var codexLine, claudeLine string
	for _, ln := range lines {
		if strings.Contains(ln, "codex run") {
			codexLine = ln
		}
		if strings.Contains(ln, "claude run") {
			claudeLine = ln
		}
	}
	if !strings.Contains(codexLine, "codex") {
		t.Errorf("a codex-harness run should be marked explicitly, got row: %q", codexLine)
	}
	if claudeLine == "" {
		t.Fatalf("expected a row for the claude run, got:\n%s", out)
	}
}
