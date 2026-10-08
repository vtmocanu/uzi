package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRunGetPlanCrossCheckSources(t *testing.T) {
	pin, worker, future := "pin", "worker default", "future\n\x1b[31m\u202e"
	for _, tc := range []struct {
		name                  string
		model, effort         *string
		wantModel, wantEffort string
	}{
		{"model pin", &pin, &worker, "pin", "worker default"},
		{"effort pin", &worker, &pin, "worker default", "pin"},
		{"legacy", nil, nil, "unknown", "unknown"},
		{"unrecognised", &future, &future, "unknown", "unknown"},
	} {
		for _, historical := range []bool{false, true} {
			t.Run(tc.name+boolStr(historical), func(t *testing.T) {
				hostile := "recorded\n\t\x1b[31m\u202e" + strings.Repeat("x", 10000)
				summary := &apitypes.PlanCrossCheckSummaryDTO{CheckerModelSource: tc.model,
					CheckerEffortSource: tc.effort, CheckerModel: &hostile, CheckerEffort: &hostile, Historical: historical}
				fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
					"r1": {ID: "r1", PlanCrossCheckSummary: summary},
				}}
				out, stderr, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
				if code != 0 {
					t.Fatalf("exit %d: %s", code, stderr)
				}
				label := "PLAN_CHECK"
				if historical {
					label = "EARLIER_PLAN_CHECK"
				}
				for _, want := range []string{label + "_MODEL_SOURCE " + tc.wantModel, label + "_EFFORT_SOURCE " + tc.wantEffort} {
					found := false
					for _, line := range strings.Split(out, "\n") {
						if strings.Join(strings.Fields(line), " ") == want {
							found = true
						}
					}
					if !found {
						t.Fatalf("missing %q: %s", want, out)
					}
				}
				if strings.ContainsAny(out, "\x1b\u202e") || strings.Contains(out, strings.Repeat("x", 201)) {
					t.Fatalf("unsafe model/effort output: %q", out)
				}
				out, stderr, code = runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
				var got apitypes.RunDTO
				if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got.PlanCrossCheckSummary == nil {
					t.Fatalf("JSON exit %d: %s", code, stderr)
				}
				actual, _ := json.Marshal(got.PlanCrossCheckSummary)
				expected, _ := json.Marshal(summary)
				if string(actual) != string(expected) {
					t.Fatalf("JSON changed recorded findings: %s", actual)
				}
			})
		}
	}
}
