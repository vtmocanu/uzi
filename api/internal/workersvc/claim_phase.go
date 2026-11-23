package workersvc

import (
	"slices"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// ClaimPhaseSettings is the claim's plan_phase (PRD #3252): the lead model and effort for every
// turn before plan approval, with the same provenance tokens as default_model_source /
// default_effort_source. It is omitted from the wire (nil) unless the owner's planning lane
// contributed at least one of the two values, so every other claim stays byte-identical.
type ClaimPhaseSettings struct {
	// Model is the plan-phase model. nil only when the plan side inherits an implement side that
	// ships no default_model (the worker resolves the template's model then).
	Model *string `json:"model,omitempty"`
	// Effort is the plan-phase effort. nil when it inherits the owner's Runtime default.
	Effort *string `json:"effort,omitempty"`
	// ModelSource is leadcfg.ModelSourceUserPlanningDefault for a planning-lane value, else the
	// implement model's source (empty when neither ships a model).
	ModelSource string `json:"model_source,omitempty"`
	// EffortSource is leadcfg.EffortSourceUserPlanningDefault for a planning-lane value, else the
	// implement effort's source.
	EffortSource string `json:"effort_source,omitempty"`
}

// resolvePlanPhase is the pure plan-phase rule (PRD #3252 section 2), called from
// resolveLeadSettings with the implement result it has just computed. Per setting, independently:
// a frozen run value (a run_override or schedule_pin model, a run_override effort) wins in both
// phases, so the plan value is the implement value; otherwise a set planning value applies to the
// plan phase; otherwise the plan value equals the implement value. A planning model that is a
// known foreign-harness token (or an invalid Codex model), or a planning effort outside
// agenttmpl.EffortLevels (the preference-only `runtime`
// token, junk), is treated as unset. Safe custom Codex model IDs are accepted. The planning model is
// always shipped when selected, even when the implement side has no default_model.
//
// It returns nil when the harness is unknown, the run override is unhonorable, or no plan
// value came from the planning lane.
func resolvePlanPhase(in LeadSettingsInput, implement LeadSettings) *ClaimPhaseSettings {
	if (in.Harness != HarnessClaude && in.Harness != HarnessCodex) || implement.Unhonorable != nil {
		return nil
	}
	plan := &ClaimPhaseSettings{}
	fromPlanning := false

	frozenModel := implement.ModelSource == leadcfg.ModelSourceRunOverride || implement.ModelSource == leadcfg.ModelSourceSchedulePin
	planModel := strings.TrimSpace(in.PlanningModel.String)
	validModel := !agenttmpl.CodexRoleModels[planModel]
	if in.Harness == HarnessCodex {
		m, err := agenttmpl.ValidateModel(planModel)
		validModel = err == nil && m != "" && !agenttmpl.KnownClaudeAliases[m]
	}
	switch {
	case !frozenModel && in.PlanningModel.Valid && planModel != "" && validModel:
		plan.Model = &planModel
		plan.ModelSource = string(leadcfg.ModelSourceUserPlanningDefault)
		fromPlanning = true
	default:
		if implement.Model.Valid {
			m := implement.Model.String
			plan.Model = &m
		}
		plan.ModelSource = string(implement.ModelSource)
	}

	frozenEffort := implement.EffortSource == leadcfg.EffortSourceRunOverride
	planEffort := strings.TrimSpace(in.PlanningEffort.String)
	switch {
	case !frozenEffort && in.PlanningEffort.Valid && slices.Contains(agenttmpl.EffortLevels, planEffort):
		plan.Effort = &planEffort
		plan.EffortSource = string(leadcfg.EffortSourceUserPlanningDefault)
		fromPlanning = true
	default:
		if implement.Effort != nil {
			e := *implement.Effort
			plan.Effort = &e
		}
		plan.EffortSource = string(implement.EffortSource)
	}

	if !fromPlanning {
		return nil
	}
	return plan
}
