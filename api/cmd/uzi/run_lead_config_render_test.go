package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func renderLeadDetail(t *testing.T, run apitypes.RunDTO) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), run); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRunDetailLeadRows(t *testing.T) {
	opus, medium := "opus", "medium"
	run := apitypes.RunDTO{ID: "lead", Kind: "issue", Status: "awaiting_approval", Harness: "claude", LeadConfig: &apitypes.RunLeadConfigDTO{
		Applicable: true, ClaimGeneration: 1, Current: true,
		Phases: []apitypes.LeadPhaseConfigDTO{{
			Phase: "plan", PhaseEntry: 1, Seq: 1, Harness: "claude",
			RequestedModel: &opus, RequestedModelSource: "user_default",
			RequestedEffort: &medium, RequestedEffortMode: "explicit", RequestedEffortSource: "builtin",
			ReportedAt: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC),
		}},
	}}
	out := renderLeadDetail(t, run)
	for _, want := range []string{
		"opus · medium",
		"opus: your Claude default; medium: built-in; runtime: not reported yet; effort: not reported yet; This is the lead agent; subagents with their own pin may differ.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "LEAD ") || !strings.Contains(out, "LEAD_SOURCE") {
		t.Errorf("missing LEAD / LEAD_SOURCE rows:\n%s", out)
	}
	if strings.Contains(out, "lead: opus") {
		t.Errorf("LEAD must drop the chip's lead: prefix:\n%s", out)
	}
}

func TestRunDetailLeadRowsSanitizeModel(t *testing.T) {
	hostile := "op\x1b[31mus"
	run := apitypes.RunDTO{ID: "lead", Kind: "issue", Status: "running", Harness: "claude", LeadConfig: &apitypes.RunLeadConfigDTO{
		Applicable: true, ClaimGeneration: 1, Current: true,
		Phases: []apitypes.LeadPhaseConfigDTO{{
			Phase: "implement", PhaseEntry: 1, Seq: 1, Harness: "claude",
			RequestedModel: &hostile, RequestedModelSource: "user_default", RequestedEffortMode: "runtime_default", RequestedEffortSource: "builtin",
		}},
	}}
	if out := renderLeadDetail(t, run); strings.Contains(out, "\x1b") {
		t.Errorf("escape byte reached the terminal:\n%q", out)
	}
}

func TestRunDetailNoLeadRowsForNonLeadRun(t *testing.T) {
	out := renderLeadDetail(t, apitypes.RunDTO{ID: "plain", Kind: "chat", Status: "running", Harness: "claude"})
	if strings.Contains(out, "LEAD") {
		t.Errorf("non-lead run must not print LEAD rows:\n%s", out)
	}
}
