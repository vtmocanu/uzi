package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetPlanCrossCheckReason(t *testing.T) {
	reason, cleared := "model_timeout", ""
	for _, tc := range []struct {
		name, harness, status, want string
		required                    bool
		reason                      *string
	}{
		{"actual claude", "claude", "awaiting_approval", "plan cross-check: model timeout", true, &reason},
		{"actual codex", "codex", "awaiting_approval", "plan cross-check: model timeout", true, &reason},
		{"ordinary", "claude", "awaiting_approval", "", false, &reason},
		{"default off", "claude", "awaiting_approval", "", false, nil},
		{"nil after revision", "codex", "awaiting_approval", "", true, nil},
		{"cleared after revision", "claude", "awaiting_approval", "", true, &cleared},
		{"approved", "claude", "running", "", true, &reason},
		{"rejected", "codex", "failed", "", true, &reason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
				"r1": {ID: "r1", Kind: "issue", Harness: tc.harness, Status: tc.status,
					PlanCrossCheckRequired: tc.required, PlanCrossCheckGateReason: tc.reason},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("run get exit %d: %s", code, stderr)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, out)
			}
			if tc.want == "" && strings.Contains(out, "PLAN_CROSS_CHECK") {
				t.Errorf("stale cross-check reason in:\n%s", out)
			}
			if strings.Contains(out, "checker unavailable") || strings.Contains(out, "Codex lead") || strings.Contains(out, "EARLIER_PLAN") {
				t.Errorf("invented cross-check status in:\n%s", out)
			}
		})
	}
}

func TestRunGetPlanCrossCheckSummary(t *testing.T) {
	model, effort, reason, child := "recorded-model", "high", "revise", "checker-child"
	hostile := "visible\n\t\x1b]8;;https://evil.example\x07link\x1b]8;;\x07\u202e"
	for _, historical := range []bool{false, true} {
		for _, usage := range []*apitypes.UsageDTO{
			nil,
			{CostStatus: "metered", CostUSD: 1.25, InputTokens: 123, OutputTokens: 45},
			{CostStatus: "subscription", InputTokens: 123},
			{CostStatus: "unreported", InputTokens: 123},
		} {
			s := &apitypes.PlanCrossCheckSummaryDTO{
				Verdict: "revise", ReasonClass: &reason, Historical: historical,
				CheckerModel: &model, CheckerEffort: &effort, Usage: usage,
				Findings: &apitypes.PlanCrossCheckFindingsDTO{Summary: hostile,
					Items: []apitypes.PlanCrossCheckFindingDTO{{File: hostile, Severity: hostile, Summary: hostile, Rationale: hostile}}},
			}
			if !historical {
				s.CheckerRunID = &child
			}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
				"r1": {ID: "r1", Kind: "issue", Status: "awaiting_approval", PlanCrossCheckSummary: s},
			}}
			out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			for _, want := range []string{model, effort, "revise", "visible"} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %s", want, out)
				}
			}
			if strings.ContainsAny(out, "\x1b\x07\u202e") {
				t.Fatalf("unsafe terminal output: %q", out)
			}
			if strings.Contains(out, "Earlier-plan findings") != historical {
				t.Fatalf("historical labeling: %s", out)
			}
			if strings.Contains(out, child) == historical {
				t.Fatalf("deleted/live child identity: %s", out)
			}
			wantCost := "cost unavailable"
			if usage != nil {
				switch usage.CostStatus {
				case "metered":
					wantCost = "$1.25"
				case "subscription":
					wantCost = "subscription"
				}
			}
			if !strings.Contains(out, wantCost) || strings.Contains(out, "$0.00") {
				t.Fatalf("dishonest child cost: %s", out)
			}
			if strings.Contains(out, "PLAN_CROSS_CHECK") {
				t.Fatalf("summary invented current gate reason: %s", out)
			}
		}
	}
}

func TestRunGetPlanCrossCheckBounds(t *testing.T) {
	items := make([]apitypes.PlanCrossCheckFindingDTO, 21)
	for i := range items {
		items[i] = apitypes.PlanCrossCheckFindingDTO{File: strings.Repeat("x", 10000), Severity: "block", Summary: "bounded", Rationale: "bounded"}
	}
	items[20].Summary = "excluded-finding"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Kind: "issue", PlanCrossCheckSummary: &apitypes.PlanCrossCheckSummaryDTO{
			Findings: &apitypes.PlanCrossCheckFindingsDTO{Items: items},
		}},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(out, "excluded-finding") || !strings.Contains(out, "1 findings omitted") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unbounded findings: %s", out)
	}
}
