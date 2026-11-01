package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
)

// PRD #3708: plan_cross_check_gated survives the list/summary copies and is omitted when false,
// so an older client or server sees no new key.
func TestPlanCrossCheckGatedSurvivesRunCopies(t *testing.T) {
	for _, gated := range []bool{false, true} {
		item := RunListItemDTO{}
		item.PlanCrossCheckGated = gated
		item.PlanCrossCheckSummary = &PlanCrossCheckSummaryDTO{Rounds: []PlanCrossCheckRoundDTO{{Round: 1, Response: PlanCrossCheckRoundResponseDTO{State: PlanCrossCheckResponseNotReported}}}}
		summary := RunSummaryOf(item)
		if summary.PlanCrossCheckGated != gated || summary.ListItem().PlanCrossCheckGated != gated {
			t.Fatalf("gated=%v lost across RunSummaryOf/ListItem", gated)
		}
		if got := summary.ListItem().PlanCrossCheckSummary; got == nil || len(got.Rounds) != 1 || got.Rounds[0].Response.State != PlanCrossCheckResponseNotReported {
			t.Fatalf("rounds lost across copies: %+v", got)
		}
		for name, v := range map[string]any{"run": item.RunDTO, "list item": item, "summary": summary} {
			wire, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if has := strings.Contains(string(wire), `"plan_cross_check_gated":true`); has != gated {
				t.Fatalf("%s gated=%v wire=%v", name, gated, has)
			}
			if !gated && strings.Contains(string(wire), `"plan_cross_check_gated"`) {
				t.Fatalf("%s: false must omit the key", name)
			}
		}
	}
}

func TestPlanCrossCheckSummaryRoundsWireOmittedWhenAbsent(t *testing.T) {
	wire, err := json.Marshal(PlanCrossCheckSummaryDTO{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), `"rounds"`) || strings.Contains(string(wire), `"deadline_at"`) {
		t.Fatalf("absent rounds/deadline must be omitted: %s", wire)
	}
}
