package workersvc

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// modelSource records where a run's frozen runs.model came from (PRD #3250). Each public create
// entrypoint passes it explicitly to createRun; it is never inferred from triggerSource. The empty
// value means no model was supplied and leaves runs.model_source NULL.
type modelSource string

const (
	modelSourceNone        modelSource = ""
	modelSourceRunOverride modelSource = "run_override"
	modelSourceSchedulePin modelSource = "schedule_pin"
)

// effortSourceRunOverride is the only runs.effort_source value: effort is only ever chosen at start.
const effortSourceRunOverride = "run_override"

// scheduleModelSource is the source a schedule producer stamps: a schedule pin exactly when it
// supplies a model, the same condition under which createRun writes runs.model (and the one the
// CreatePromptRun and CreateSelfImproveRun statements apply in SQL).
func scheduleModelSource(model *string) modelSource {
	if model != nil {
		return modelSourceSchedulePin
	}
	return modelSourceNone
}

// runOverrideSource is the source a manual request stamps: run_override whenever the request
// carried a model, none otherwise.
func runOverrideSource(model *string) modelSource {
	if model != nil {
		return modelSourceRunOverride
	}
	return modelSourceNone
}

// effortSourceFor stamps effort_source run_override exactly when an effort was supplied.
func effortSourceFor(effort *string) pgtype.Text {
	if effort == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: effortSourceRunOverride, Valid: true}
}

// ErrInvalidRunEffort refuses a start-time effort outside agenttmpl.EffortLevels. The handler's shape
// step rejects it first; createRun re-checks so no caller can persist an unvalidated effort.
var ErrInvalidRunEffort = errors.New("effort must be one of low, medium, high, xhigh, max")

// validRunEffort reports whether effort is exactly one of the canonical levels (no trimming).
func validRunEffort(effort string) bool {
	e, err := agenttmpl.ValidateEffort(effort)
	return err == nil && e != "" && e == effort
}

// ErrModelIncompatibleWithHarness is the sentinel a run-start model refusal matches: the chosen
// run_override model cannot be served by the harness the create resolved to.
var ErrModelIncompatibleWithHarness = errors.New("model is incompatible with the run's harness")

// ModelIncompatibleError carries the refused model and the harness the create resolved to, so the
// handler can emit the harness structurally. Is ties it to ErrModelIncompatibleWithHarness.
type ModelIncompatibleError struct {
	Model   string
	Harness Harness
}

// Error is surface-neutral: the CLI and the web print it unparsed. The Codex list comes from the
// curated set so it cannot drift.
func (e *ModelIncompatibleError) Error() string {
	if e.Harness == HarnessCodex {
		return fmt.Sprintf("%s is not a model a Codex run can start with, and this run resolved to the codex harness; start it on the Claude harness or choose one of: %s",
			e.Model, strings.Join(codexCuratedModelsSlice(), ", "))
	}
	return fmt.Sprintf("%s cannot run on the Claude harness, and this run resolved to the claude harness; start it on the Codex harness or choose a Claude model", e.Model)
}

func (e *ModelIncompatibleError) Is(target error) bool {
	return target == ErrModelIncompatibleWithHarness
}

// RunOverrideModelHonorable reports whether harness can serve a start-time run_override model.
// It is exported for the handler's fast-fail beside the authoritative in-transaction check.
// Schedule pins keep harnessModelCompatible. On Claude both refuse every Codex role id; this
// predicate additionally refuses a blank, whitespace-bearing or ValidateModel-invalid value
// (harnessModelCompatible trims and accepts blank), and Codex accepts only the curated set.
func RunOverrideModelHonorable(harness Harness, model string) bool {
	return runOverrideModelHonorable(harness, model)
}

func runOverrideModelHonorable(harness Harness, model string) bool {
	if strings.TrimSpace(model) == "" || strings.IndexFunc(model, unicode.IsSpace) >= 0 {
		return false
	}
	switch harness {
	case HarnessClaude:
		m, err := agenttmpl.ValidateClaudeLaneModel(model)
		return err == nil && m != "" && m == model
	case HarnessCodex:
		return agenttmpl.CuratedCodexModels[model]
	default:
		return false
	}
}
