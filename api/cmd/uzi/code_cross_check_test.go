package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func codeCheckCommand(t *testing.T, s *apitypes.CodeCrossCheck, args ...string) string {
	t.Helper()
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Kind: "issue", CodeCrossCheckRequired: true, CodeCrossCheckSummary: s},
	}}
	out, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"run", "get", "r1"}, args...)...)
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	return out
}

func TestRunGetCodeCrossCheckStates(t *testing.T) {
	now := time.Now()
	badHead := "not-a-sha"
	for _, tc := range []struct {
		name string
		s    *apitypes.CodeCrossCheck
		want string
	}{
		{"missing", nil, "Pending"},
		{"pending", &apitypes.CodeCrossCheck{Outcome: "pending"}, "Pending"},
		{"both NULL failed", &apitypes.CodeCrossCheck{Outcome: "failed"}, "Incomplete"},
		{"malformed head", &apitypes.CodeCrossCheck{Outcome: "failed", HeadCommit: &badHead}, "Incomplete"},
		{"no findings", &apitypes.CodeCrossCheck{Outcome: "completed", Findings: json.RawMessage("[]")}, "No findings recorded"},
		{"historical", &apitypes.CodeCrossCheck{Outcome: "completed", InterruptedAt: &now, Findings: json.RawMessage("[]")}, "Earlier attempt evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := codeCheckCommand(t, tc.s)
			if !strings.Contains(out, tc.want) || strings.Contains(out, "CHECKED_SHA") {
				t.Fatalf("wrong evidence: %s", out)
			}
			if tc.name == "historical" && (strings.Contains(out, "Completed") || strings.Contains(out, "\nCODE_CHECK")) {
				t.Fatalf("history presented as active: %s", out)
			}
		})
	}
}

func TestRunGetCodeCrossCheckMetadata(t *testing.T) {
	family, model, effort, child, head := "codex", "recorded-model", "high", "checker-child", strings.Repeat("a", 40)
	for _, usage := range []*apitypes.UsageDTO{nil, {CostStatus: "metered", CostUSD: 1.25}, {CostStatus: "subscription"}, {CostStatus: "unreported"}} {
		for _, deleted := range []bool{false, true} {
			s := &apitypes.CodeCrossCheck{Outcome: "completed", CheckerHarness: &family, CheckerModel: &model, CheckerEffort: &effort, HeadCommit: &head, Usage: usage, Findings: json.RawMessage("[]")}
			if !deleted {
				s.CheckerRunID = &child
			}
			out := codeCheckCommand(t, s)
			wantCost := "cost unavailable"
			if usage != nil && usage.CostStatus == "metered" {
				wantCost = "$1.25"
			}
			if usage != nil && usage.CostStatus == "subscription" {
				wantCost = "subscription"
			}
			for _, want := range []string{family, model, effort, head, wantCost} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q: %s", want, out)
				}
			}
			if strings.Contains(out, child) == deleted || strings.Contains(out, "$0.00") {
				t.Fatalf("invented metadata: %s", out)
			}
		}
	}
}

func TestRunGetCodeCrossCheckHostileBounds(t *testing.T) {
	hostile := "visible\nFORGED_ROW\t\x1b[31m\x07\u202e"
	items := make([]map[string]any, 21)
	for i := range items {
		items[i] = map[string]any{"id": fmt.Sprintf("f%d", i), "severity": hostile, "path": hostile, "line": 12, "title": hostile, "detail": strings.Repeat("x", 9000)}
	}
	items[0]["detail"] = hostile
	items[1]["id"] = "f0"
	items[2]["id"] = "bad\nkey"
	items[3]["id"] = "bad\x1bkey"
	items[4]["id"] = "bad\u202ekey"
	items[20]["title"] = "excluded-finding"
	// Keep the serialized payload under the parse bound while testing field caps.
	for i := 1; i < 20; i++ {
		items[i]["detail"] = strings.Repeat("x", 500)
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	s := &apitypes.CodeCrossCheck{Outcome: "completed", Findings: raw, CheckerModel: &hostile, CheckerHarness: &hostile, CheckerEffort: &hostile, CheckerRunID: &hostile, ReasonClass: &hostile}
	out := codeCheckCommand(t, s)
	if strings.ContainsAny(out, "\x1b\x07\u202e") || strings.Contains(out, "\nFORGED_ROW") || strings.Contains(out, "excluded-finding") || strings.Contains(out, strings.Repeat("x", 201)) {
		t.Fatalf("unsafe or unbounded output: %q", out)
	}
	if strings.Count(out, "ID unavailable") != 4 || !strings.Contains(out, "1 findings omitted") {
		t.Fatalf("ID/cap handling: %s", out)
	}
	for _, invalid := range []json.RawMessage{json.RawMessage("null"), json.RawMessage("{}"), json.RawMessage("broken"), json.RawMessage("[" + strings.Repeat(" ", 65536) + "]")} {
		s.Findings = invalid
		if got := codeCheckCommand(t, s); !strings.Contains(got, "Findings unavailable") {
			t.Fatalf("legacy/raw cap: %s", got)
		}
	}
}

func TestRunGetCodeCrossCheckJSONUnchanged(t *testing.T) {
	s := &apitypes.CodeCrossCheck{Outcome: "pending", Findings: json.RawMessage("[]")}
	out := codeCheckCommand(t, s, "--json")
	var got apitypes.RunDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.CodeCrossCheckSummary == nil || got.CodeCrossCheckSummary.Outcome != "pending" || string(got.CodeCrossCheckSummary.Findings) != "[]" {
		t.Fatalf("JSON changed: %s", out)
	}
}
