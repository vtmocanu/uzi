// Package leadcfg is the dependency-free vocabulary of the per-claim lead-config record (PRD #3244):
// the closed sets of phases, sources, modes and evidence tokens, the one lane predicate, the derived
// fixed_at, request validation and the run-level label table. It imports no store or handler package so
// the server resolver, the write path, the read overlay and the presenters share one definition. The
// vocabulary is pinned to fixtures/lead-config/contract.json by this package's tests, and the SQL
// constraints of migration 00379 by a LiveDB test in the store package.
package leadcfg

// Phase is the lead agent's phase a row belongs to.
type Phase string

const (
	PhasePlan      Phase = "plan"
	PhaseImplement Phase = "implement"
)

// ModelSource says who decided the requested model.
type ModelSource string

const (
	ModelSourceRunOverride         ModelSource = "run_override"
	ModelSourceSchedulePin         ModelSource = "schedule_pin"
	ModelSourceUserPlanningDefault ModelSource = "user_planning_default"
	ModelSourceUserDefault         ModelSource = "user_default"
	ModelSourceTemplate            ModelSource = "template"
	ModelSourceBuiltin             ModelSource = "builtin"
)

// EffortSource says who decided the requested effort.
type EffortSource string

const (
	EffortSourceRunOverride         EffortSource = "run_override"
	EffortSourceUserPlanningDefault EffortSource = "user_planning_default"
	EffortSourceUserDefault         EffortSource = "user_default"
	EffortSourceTemplate            EffortSource = "template"
	EffortSourceBuiltin             EffortSource = "builtin"
)

// EffortMode says whether an effort is sent on the wire.
type EffortMode string

const (
	EffortModeExplicit       EffortMode = "explicit"
	EffortModeRuntimeDefault EffortMode = "runtime_default"
)

// ModelEvidence says how the observed model was learned.
type ModelEvidence string

const (
	ModelEvidenceClaudeInit          ModelEvidence = "claude_init"
	ModelEvidenceCodexThreadResponse ModelEvidence = "codex_thread_response"
	ModelEvidenceUnknown             ModelEvidence = "unknown"
)

// EffortEvidence says how the observed effort was learned.
type EffortEvidence string

const (
	EffortEvidenceClaudeInit          EffortEvidence = "claude_init"
	EffortEvidenceClaudeGetSettings   EffortEvidence = "claude_get_settings"
	EffortEvidenceCodexSent           EffortEvidence = "codex_sent"
	EffortEvidenceCodexRuntimeDefault EffortEvidence = "codex_runtime_default"
	EffortEvidenceUnknown             EffortEvidence = "unknown"
)

// Harnesses are the two agent harnesses a lead run can use.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
)

// The vocabulary accessors return fresh slices so a caller cannot mutate the closed sets.

// Phases returns the closed set of phases.
func Phases() []Phase { return []Phase{PhasePlan, PhaseImplement} }

// ModelSources returns the closed set of requested-model sources.
func ModelSources() []ModelSource {
	return []ModelSource{ModelSourceRunOverride, ModelSourceSchedulePin, ModelSourceUserPlanningDefault,
		ModelSourceUserDefault, ModelSourceTemplate, ModelSourceBuiltin}
}

// EffortSources returns the closed set of requested-effort sources.
func EffortSources() []EffortSource {
	return []EffortSource{EffortSourceRunOverride, EffortSourceUserPlanningDefault, EffortSourceUserDefault,
		EffortSourceTemplate, EffortSourceBuiltin}
}

// EffortModes returns the closed set of requested-effort modes.
func EffortModes() []EffortMode { return []EffortMode{EffortModeExplicit, EffortModeRuntimeDefault} }

// ModelEvidences returns the closed set of observed-model evidence tokens.
func ModelEvidences() []ModelEvidence {
	return []ModelEvidence{ModelEvidenceClaudeInit, ModelEvidenceCodexThreadResponse, ModelEvidenceUnknown}
}

// EffortEvidences returns the closed set of observed-effort evidence tokens.
func EffortEvidences() []EffortEvidence {
	return []EffortEvidence{EffortEvidenceClaudeInit, EffortEvidenceClaudeGetSettings, EffortEvidenceCodexSent,
		EffortEvidenceCodexRuntimeDefault, EffortEvidenceUnknown}
}

// RequestedEfforts returns the closed set of requested effort levels (the levels uzi can send).
func RequestedEfforts() []string { return []string{"low", "medium", "high", "xhigh", "max"} }

// ObservedEfforts returns the closed set of observed effort levels: the requested levels plus the Codex
// levels none, minimal, ultra, and other (a level the reporter does not recognize).
func ObservedEfforts() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "other"}
}

// ObservedEffortOther is the stored token for an unrecognized observed level.
const ObservedEffortOther = "other"

// FixedAt says when a requested value became fixed: "create" for a source decided when the run was created
// (a per-run override or a schedule pin), "claim" for every source resolved at claim time (D17). It is
// derived, never stored. Any other token reads as "claim".
func FixedAt(source string) string {
	switch source {
	case string(ModelSourceRunOverride), string(ModelSourceSchedulePin):
		return "create"
	default:
		return "claim"
	}
}

// LeadLane reports whether a run has a lead agent whose configuration is recorded: kinds issue, ci_fix,
// prompt, self_improve, mr_rework and task, excluding a review run (a task with a review target) and an
// isolated-lane run (bound to an egress profile), which the isolated runner executes without a lead
// executor (D1). It is the one predicate for claim provenance, the GetRun overlay and, mirrored in SQL by
// InsertRunLeadConfigFenced, the write fence.
func LeadLane(kind string, reviewTarget, egressProfileBound bool) bool {
	if reviewTarget || egressProfileBound {
		return false
	}
	switch kind {
	case "issue", "ci_fix", "prompt", "self_improve", "mr_rework", "task":
		return true
	default:
		return false
	}
}
