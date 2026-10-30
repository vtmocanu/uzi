package workersvc

import (
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// LeadSettingsInput is what the lead resolver reads: the run's harness, the model frozen onto the
// run at create (runs.model), the owner's model lane for that harness and the owner's per-harness
// effort column. The caller reads them; the resolver is pure.
type LeadSettingsInput struct {
	Harness    Harness
	RunModel   pgtype.Text // runs.model, frozen at create
	LaneModel  pgtype.Text // the user's lane for Harness
	UserEffort pgtype.Text // the user's per-harness effort column
}

// LeadSettings is the resolved lead model and effort with the provenance the claim ships.
type LeadSettings struct {
	// Model is invalid when no default_model is shipped (the worker decides).
	Model pgtype.Text
	// ModelSource is empty exactly when Model is invalid.
	ModelSource leadcfg.ModelSource
	// Effort is as resolveHarnessEffortPtr returns it: always non-nil.
	Effort       *string
	EffortSource leadcfg.EffortSource
	// DroppedLaneModel is the trimmed Claude-lane Codex-role token the #3245 guard discarded, for
	// its existing fallback note.
	DroppedLaneModel pgtype.Text
	// DroppedRunModel is the (untrimmed) incompatible runs.model that was ignored, for its
	// existing fallback note.
	DroppedRunModel pgtype.Text
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
	if in.RunModel.Valid {
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
	out.Effort = resolveHarnessEffortPtr(in.Harness, in.UserEffort)
	out.EffortSource = leadEffortSource(in.Harness, in.UserEffort)
	return out
}

// leadEffortSource says whether the effort the claim ships came from the owner's column
// (user_default) or from the uzi default (builtin). The test is the one
// agenttmpl.ResolveDefaultEffort applies: a valid, non-blank (after trimming) value is the
// owner's choice. The harness is accepted so the next resolver (per-harness defaults, child B)
// extends this function rather than adding a second one.
func leadEffortSource(_ Harness, userEffort pgtype.Text) leadcfg.EffortSource {
	if userEffort.Valid && strings.TrimSpace(userEffort.String) != "" {
		return leadcfg.EffortSourceUserDefault
	}
	return leadcfg.EffortSourceBuiltin
}
