package workersvc

import (
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// LeadSettingsInput is what the lead resolver reads: the run's harness, the model and effort frozen
// onto the run at create (runs.model / model_source, runs.effort / effort_source), the owner's model
// lane for that harness and the owner's per-harness effort column. The caller reads them; the
// resolver is pure.
type LeadSettingsInput struct {
	Harness         Harness
	RunModel        pgtype.Text // runs.model, frozen at create
	RunModelSource  pgtype.Text // runs.model_source: run_override, schedule_pin or NULL
	RunEffort       pgtype.Text // runs.effort, frozen at create
	RunEffortSource pgtype.Text // runs.effort_source: run_override or NULL
	LaneModel       pgtype.Text // the user's lane for Harness
	UserEffort      pgtype.Text // the user's per-harness effort column
}

// RunOverrideUnhonorable names the start-time run_override choice the claim cannot honor. It is
// never a fallback: the claim parks the run instead of running it on something the owner did not pick.
type RunOverrideUnhonorable struct {
	Setting string // "model" or "effort"
	Value   string
	Harness Harness
}

// LeadSettings is the resolved lead model and effort with the provenance the claim ships.
type LeadSettings struct {
	// Model is invalid when no default_model is shipped (the worker decides).
	Model pgtype.Text
	// ModelSource is empty exactly when Model is invalid.
	ModelSource leadcfg.ModelSource
	// Effort is the claim's default_effort; nil means the owner chose the runtime default (no
	// default_effort is shipped).
	Effort       *string
	EffortSource leadcfg.EffortSource
	// DroppedLaneModel is the trimmed Claude-lane Codex-role token the #3245 guard discarded, for
	// its existing fallback note.
	DroppedLaneModel pgtype.Text
	// DroppedRunModel is the (untrimmed) incompatible runs.model that was ignored, for its
	// existing fallback note.
	DroppedRunModel pgtype.Text
	// Unhonorable is set when a run_override model or effort cannot be honored (model checked first).
	// The other fields are not meaningful then, and DroppedRunModel is never set for a run_override.
	Unhonorable *RunOverrideUnhonorable
}

// resolveLeadSettings is the one server-side home of lead precedence (PRD #3244 section 3). It is
// the behavior-preserving extraction of assembleClaim's inline lane/schedule block: the
// per-harness lane is read, a Claude lane holding a Codex role token is cleared (#3245), then a
// compatible runs.model wins over the lane and an incompatible one is dropped. The caller owns
// the gate (never for a checker or Codex review claim) and the fallback notes.
func resolveLeadSettings(in LeadSettingsInput) LeadSettings {
	var out LeadSettings
	model := in.LaneModel
	if in.Harness != HarnessCodex {
		if model.Valid && agenttmpl.CodexRoleModels[strings.TrimSpace(model.String)] {
			out.DroppedLaneModel = pgtype.Text{String: strings.TrimSpace(model.String), Valid: true}
			model = pgtype.Text{}
		}
	}
	if model.Valid {
		out.ModelSource = leadcfg.ModelSourceUserDefault
	}
	runOverride := in.RunModel.Valid && in.RunModelSource.Valid && in.RunModelSource.String == string(leadcfg.ModelSourceRunOverride)
	switch {
	case runOverride:
		if !runOverrideModelHonorable(in.Harness, in.RunModel.String) {
			out.Unhonorable = &RunOverrideUnhonorable{Setting: "model", Value: in.RunModel.String, Harness: in.Harness}
			return out
		}
		model = in.RunModel
		out.ModelSource = leadcfg.ModelSourceRunOverride
	case in.RunModel.Valid:
		if harnessModelCompatible(in.Harness, in.RunModel.String) {
			model = in.RunModel
			out.ModelSource = leadcfg.ModelSourceSchedulePin
		} else {
			out.DroppedRunModel = in.RunModel
		}
	}
	out.Model = model
	if !model.Valid {
		out.ModelSource = ""
	}
	var unhonorable bool
	out.Effort, out.EffortSource, unhonorable = leadEffortFor(in.Harness, in.RunEffort, in.RunEffortSource, in.UserEffort)
	if unhonorable {
		out.Unhonorable = &RunOverrideUnhonorable{Setting: "effort", Value: in.RunEffort.String, Harness: in.Harness}
	}
	return out
}

// leadEffortFor is the effort precedence: a start-time run_override effort that is exactly one of
// agenttmpl.EffortLevels wins over the owner's column (including the `runtime` choice) with source
// run_override; a run_override effort outside the set is unhonorable (the other results are then
// meaningless); everything else is resolveLeadEffort over the owner's column.
func leadEffortFor(harness Harness, runEffort, runEffortSource, userEffort pgtype.Text) (effort *string, source leadcfg.EffortSource, unhonorable bool) {
	if runEffort.Valid && runEffortSource.Valid && runEffortSource.String == string(leadcfg.EffortSourceRunOverride) {
		if !slices.Contains(agenttmpl.EffortLevels, runEffort.String) {
			return nil, "", true
		}
		level := runEffort.String
		return &level, leadcfg.EffortSourceRunOverride, false
	}
	effort, source = resolveLeadEffort(harness, userEffort)
	return effort, source, false
}

// resolveLeadEffort is the single home of the lead effort value and its source. assembleClaim
// reads the effort column after the model block (statement order is deliberate), so it calls this
// again with the late read instead of repeating the two expressions; resolveLeadSettings calls it
// for LeadSettingsInput.UserEffort.
//
// It makes one agenttmpl.ResolveHarnessEffort call (#3247) and maps the EffortResolution: the
// level is the claim's default_effort (nil for the owner's `runtime` choice, so the worker
// records requested_effort_mode runtime_default), and the resolution's source is the
// default_effort_source (NULL, blank or a corrupt stored value is builtin; a level or `runtime`
// is user_default). The value mapping is shared with resolveHarnessEffortPtr.
func resolveLeadEffort(harness Harness, userEffort pgtype.Text) (effort *string, source leadcfg.EffortSource) {
	r := agenttmpl.ResolveHarnessEffort(string(harness), textPtr(userEffort))
	return effortResolutionPtr(r), leadcfg.EffortSource(r.Source)
}
