package workersvc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestResolvePlanPhase(t *testing.T) {
	const planSrc = "user_planning_default"
	cases := []struct {
		name string
		in   LeadSettingsInput
		want *ClaimPhaseSettings
	}{
		{"nothing set ships no plan_phase",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), UserEffort: txt("medium")}, nil},
		{"planning model and effort over lane and user effort (example 1)",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), UserEffort: txt("medium"), PlanningModel: txt("opus"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("opus"), ModelSource: planSrc, Effort: strp("high"), EffortSource: planSrc}},
		{"planning model only: effort inherits the implement pair",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), UserEffort: txt("low"), PlanningModel: txt("opus")},
			&ClaimPhaseSettings{Model: strp("opus"), ModelSource: planSrc, Effort: strp("low"), EffortSource: "user_default"}},
		{"planning effort only: model inherits the lane",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningEffort: txt("max")},
			&ClaimPhaseSettings{Model: strp("sonnet"), ModelSource: "user_default", Effort: strp("max"), EffortSource: planSrc}},
		{"planning effort only with the Runtime default implement effort still ships the planning effort",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), UserEffort: txt("runtime"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("sonnet"), ModelSource: "user_default", Effort: strp("high"), EffortSource: planSrc}},
		{"schedule pin wins the model in both phases, planning effort still applies (example 3)",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("opus"), RunModel: txt("fable"), RunModelSource: txt("schedule_pin"),
				UserEffort: txt("medium"), PlanningModel: txt("opus"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("fable"), ModelSource: "schedule_pin", Effort: strp("high"), EffortSource: planSrc}},
		{"schedule pin with only a planning model: nothing from the planning lane",
			LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("fable"), RunModelSource: txt("schedule_pin"), PlanningModel: txt("opus")}, nil},
		{"run_override model and effort win both phases",
			LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("sonnet"), RunModelSource: txt("run_override"),
				RunEffort: txt("low"), RunEffortSource: txt("run_override"), PlanningModel: txt("opus"), PlanningEffort: txt("high")}, nil},
		{"run_override effort wins, planning model still applies",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), RunEffort: txt("low"), RunEffortSource: txt("run_override"),
				PlanningModel: txt("opus"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("opus"), ModelSource: planSrc, Effort: strp("low"), EffortSource: "run_override"}},
		{"empty lane: the planning model is always shipped, the implement side ships none (example 2)",
			LeadSettingsInput{Harness: HarnessClaude, PlanningModel: txt("fable")},
			&ClaimPhaseSettings{Model: strp("fable"), ModelSource: planSrc, Effort: strp("medium"), EffortSource: "builtin"}},
		{"planning values are trimmed",
			LeadSettingsInput{Harness: HarnessClaude, PlanningModel: txt("  opus "), PlanningEffort: txt(" high ")},
			&ClaimPhaseSettings{Model: strp("opus"), ModelSource: planSrc, Effort: strp("high"), EffortSource: planSrc}},
		{"blank planning values are unset", LeadSettingsInput{Harness: HarnessClaude, PlanningModel: txt("  "), PlanningEffort: txt("")}, nil},
		{"a Codex role id as the planning model is treated as unset",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningModel: txt("gpt-6-sol")}, nil},
		{"a Codex role id planning model does not hide a planning effort",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningModel: txt("gpt-6-sol"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("sonnet"), ModelSource: "user_default", Effort: strp("high"), EffortSource: planSrc}},
		{"the preference-only runtime token as the planning effort is treated as unset",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningModel: txt("opus"), PlanningEffort: txt("runtime")},
			&ClaimPhaseSettings{Model: strp("opus"), ModelSource: planSrc, Effort: strp("medium"), EffortSource: "builtin"}},
		{"a junk planning effort alone ships no plan_phase",
			LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningEffort: txt("bogus")}, nil},
		{"Codex foreign alias is ignored independently of planning effort",
			LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6-luna"), PlanningModel: txt("opus"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("gpt-6-luna"), ModelSource: "user_default", Effort: strp("high"), EffortSource: planSrc}},
		{"Codex custom planning model with Runtime default implement effort",
			LeadSettingsInput{Harness: HarnessCodex, LaneModel: txt("gpt-6.1-sol"), UserEffort: txt("runtime"), PlanningModel: txt("custom-plan"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("custom-plan"), ModelSource: planSrc, Effort: strp("high"), EffortSource: planSrc}},
		{"Codex planning model inherits omitted implement effort",
			LeadSettingsInput{Harness: HarnessCodex, UserEffort: txt("runtime"), PlanningModel: txt("gpt-6-astra")},
			&ClaimPhaseSettings{Model: strp("gpt-6-astra"), ModelSource: planSrc, EffortSource: "user_default"}},
		{"Codex schedule freezes model independently",
			LeadSettingsInput{Harness: HarnessCodex, RunModel: txt("gpt-6-sol"), RunModelSource: txt("schedule_pin"), PlanningModel: txt("custom-plan"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("gpt-6-sol"), ModelSource: "schedule_pin", Effort: strp("high"), EffortSource: planSrc}},
		{"Codex effort override freezes effort independently",
			LeadSettingsInput{Harness: HarnessCodex, RunEffort: txt("low"), RunEffortSource: txt("run_override"), PlanningModel: txt("custom-plan"), PlanningEffort: txt("high")},
			&ClaimPhaseSettings{Model: strp("custom-plan"), ModelSource: planSrc, Effort: strp("low"), EffortSource: "run_override"}},
		{"Codex both frozen values suppress plan phase",
			LeadSettingsInput{Harness: HarnessCodex, RunModel: txt("gpt-6-sol"), RunModelSource: txt("run_override"), RunEffort: txt("low"), RunEffortSource: txt("run_override"), PlanningModel: txt("custom-plan"), PlanningEffort: txt("high")}, nil},
		{"Codex invalid stored model is ignored",
			LeadSettingsInput{Harness: HarnessCodex, PlanningModel: txt("bad model")}, nil},
		{"Codex runtime planning effort is ignored",
			LeadSettingsInput{Harness: HarnessCodex, PlanningEffort: txt("runtime")}, nil},
		{"unhonorable run override model ships nothing",
			LeadSettingsInput{Harness: HarnessClaude, RunModel: txt("gpt-6-sol"), RunModelSource: txt("run_override"), PlanningEffort: txt("high")}, nil},
		{"unhonorable run override effort ships nothing",
			LeadSettingsInput{Harness: HarnessClaude, RunEffort: txt("bogus"), RunEffortSource: txt("run_override"), PlanningModel: txt("opus")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			impl := resolveLeadSettings(tc.in)
			if !reflect.DeepEqual(impl.Plan, tc.want) {
				t.Errorf("resolveLeadSettings().Plan = %s, want %s", planString(impl.Plan), planString(tc.want))
			}
			if got := resolvePlanPhase(tc.in, impl); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resolvePlanPhase = %s, want %s", planString(got), planString(tc.want))
			}
		})
	}
}

func planString(p *ClaimPhaseSettings) string {
	if p == nil {
		return "<nil>"
	}
	d := func(s *string) string {
		if s == nil {
			return "nil"
		}
		return *s
	}
	return "{model=" + d(p.Model) + "/" + p.ModelSource + " effort=" + d(p.Effort) + "/" + p.EffortSource + "}"
}

// TestResolvePlanPhaseEmptyImplementModelHasNoSource pins the inherited side: with no planning model
// and no implement model the plan model is nil and its source empty (the worker resolves the template).
func TestResolvePlanPhaseEmptyImplementModelHasNoSource(t *testing.T) {
	in := LeadSettingsInput{Harness: HarnessClaude, PlanningEffort: txt("high")}
	got := resolveLeadSettings(in).Plan
	if got == nil || got.Model != nil || got.ModelSource != "" {
		t.Fatalf("plan = %s, want a nil model with an empty source", planString(got))
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "model") {
		t.Fatalf("inherited unset model serialized as %s, want no model or model_source key", raw)
	}
}

// TestResolvePlanPhaseLateEffortMatchesFullInput pins the assembleClaim shape: the lead block
// resolves without the user effort, then re-derives Plan with the late-read effort. The result must
// equal resolveLeadSettings over the full input, for every effort source.
func TestResolvePlanPhaseLateEffortMatchesFullInput(t *testing.T) {
	for _, userEffort := range []pgtype.Text{{}, txt("low"), txt("runtime"), txt("max")} {
		for _, planningEffort := range []pgtype.Text{{}, txt("high")} {
			base := LeadSettingsInput{Harness: HarnessClaude, LaneModel: txt("sonnet"), PlanningModel: txt("opus"), PlanningEffort: planningEffort}
			full := base
			full.UserEffort = userEffort
			want := resolveLeadSettings(full)

			early := resolveLeadSettings(base)
			early.Effort, early.EffortSource, _ = leadEffortFor(base.Harness, base.RunEffort, base.RunEffortSource, userEffort)
			late := base
			late.UserEffort = userEffort
			early.Plan = resolvePlanPhase(late, early)
			if !reflect.DeepEqual(early.Plan, want.Plan) {
				t.Errorf("userEffort=%+v planningEffort=%+v: late Plan %s, full-input Plan %s", userEffort, planningEffort, planString(early.Plan), planString(want.Plan))
			}
		}
	}
}
