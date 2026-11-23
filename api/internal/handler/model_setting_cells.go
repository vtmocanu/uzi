package handler

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// judgeModelCells builds the judge setting's cells (PRD #3245), always ordered
// claude then codex. userClaude/userCodex are the stored user lanes (nil = Default);
// instance maps the instance model keys to their InstanceModels values. It reads no
// settings itself: the caller passes its own snapshot, so every surface resolves
// from one read.
func judgeModelCells(userClaude, userCodex *string, instance map[string]string) []apitypes.ModelSettingCellDTO {
	return []apitypes.ModelSettingCellDTO{
		judgeModelCell("claude", userClaude, instance[settings.KeyJudgeModel]),
		judgeModelCell("codex", userCodex, instance[settings.KeyCodexJudgeModel]),
	}
}

func judgeModelCell(harness string, user *string, instance string) apitypes.ModelSettingCellDTO {
	stored := ""
	if user != nil {
		stored = *user
	}
	r := agenttmpl.ResolveJudgeModel(harness, stored, instance)
	return apitypes.ModelSettingCellDTO{
		Harness:       harness,
		Model:         user,
		DefaultModel:  r.DefaultModel,
		DefaultSource: r.DefaultSource,
		ResolvedModel: r.Model,
		Source:        r.Source,
	}
}

// summaryModelCells builds the run-summary setting's cells (PRD #3248), always ordered
// claude then codex, mirroring judgeModelCells: userClaude/userCodex are the stored
// user lanes (nil = Default) and instance maps the instance model keys to their
// InstanceModels values from the caller's single snapshot.
func summaryModelCells(userClaude, userCodex *string, instance map[string]string) []apitypes.ModelSettingCellDTO {
	return []apitypes.ModelSettingCellDTO{
		summaryModelCell("claude", userClaude, instance[settings.KeySummaryModel]),
		summaryModelCell("codex", userCodex, instance[settings.KeyCodexSummaryModel]),
	}
}

func summaryModelCell(harness string, user *string, instance string) apitypes.ModelSettingCellDTO {
	stored := ""
	if user != nil {
		stored = *user
	}
	r := agenttmpl.ResolveSummaryModel(harness, stored, instance)
	return apitypes.ModelSettingCellDTO{
		Harness:       harness,
		Model:         user,
		DefaultModel:  r.DefaultModel,
		DefaultSource: r.DefaultSource,
		ResolvedModel: r.Model,
		Source:        r.Source,
	}
}

// taskReviewModelCells builds the task-review model setting's cells (PRD #1570), always ordered
// claude then codex. userClaude/userCodex are the stored user lanes (nil = Default). Each cell
// resolves through workersvc.ResolveTaskReview, the rule the review claim applies: a valid stored
// model is user_default, anything else the built-in (Claude: no model, the SDK decides).
func taskReviewModelCells(userClaude, userCodex *string) []apitypes.ModelSettingCellDTO {
	return []apitypes.ModelSettingCellDTO{
		taskReviewModelCell(workersvc.HarnessClaude, userClaude),
		taskReviewModelCell(workersvc.HarnessCodex, userCodex),
	}
}

func taskReviewModelCell(harness workersvc.Harness, user *string) apitypes.ModelSettingCellDTO {
	r := workersvc.ResolveTaskReview(harness, workersvc.TaskReviewInput{Model: user})
	return apitypes.ModelSettingCellDTO{
		Harness:       string(harness),
		Model:         user,
		DefaultModel:  r.BuiltinModel,
		DefaultSource: agenttmpl.WorkerDefaultSourceBuiltin,
		ResolvedModel: r.Model,
		Source:        r.ModelSource,
	}
}

// taskReviewEffortCells builds the task-review effort setting's cells (PRD #1570), always ordered
// claude then codex, from the stored review preferences and the stored ordinary worker efforts.
func taskReviewEffortCells(reviewClaude, reviewCodex, workerClaude, workerCodex *string) []apitypes.EffortSettingCellDTO {
	return []apitypes.EffortSettingCellDTO{
		taskReviewEffortCell(workersvc.HarnessClaude, reviewClaude, workerClaude),
		taskReviewEffortCell(workersvc.HarnessCodex, reviewCodex, workerCodex),
	}
}

func taskReviewEffortCell(harness workersvc.Harness, review, worker *string) apitypes.EffortSettingCellDTO {
	r := workersvc.ResolveTaskReview(harness, workersvc.TaskReviewInput{Effort: review, WorkerEffort: worker})
	return reviewEffortCellFrom(string(harness), r.StoredEffort, r.WorkerEffort, r.Effort)
}

// reviewEffortCellFrom is the one cell shape of the task-review effort, shared by the Settings
// cells and the effective view. stored is the owner's review preference (nil = inherits); worker is
// the worker effort's resolution, what a reset selects (DefaultEffort carries its level, or the
// `runtime` token when it sends none); final is what the review resolves to.
func reviewEffortCellFrom(harness string, stored *string, worker, final agenttmpl.EffortResolution) apitypes.EffortSettingCellDTO {
	cell := apitypes.EffortSettingCellDTO{
		Harness: harness, Effort: stored,
		DefaultEffort: worker.Level, DefaultSource: worker.Source,
		Source: final.Source, Mode: final.Mode,
	}
	if worker.Mode == agenttmpl.EffortModeRuntimeDefault {
		cell.DefaultEffort = agenttmpl.EffortRuntime
	}
	if final.Mode == agenttmpl.EffortModeExplicit {
		level := final.Level
		cell.ResolvedEffort = &level
	}
	return cell
}

// effortSettingCells builds the ordinary effort setting's cells (PRD #3247), always
// ordered claude then codex, from the stored preference tokens of one row read.
// catalog (issue #3251) is attached to the codex cell only; the claude cell never
// carries it.
func effortSettingCells(claude, codex *string, catalog *apitypes.CatalogDefaultDTO) []apitypes.EffortSettingCellDTO {
	codexCell := effortSettingCell("codex", codex)
	codexCell.CatalogDefault = catalog
	return []apitypes.EffortSettingCellDTO{
		effortSettingCell("claude", claude),
		codexCell,
	}
}

// planningSourceUserPlanningDefault is the provenance token of a stored planning value (PRD #3252).
const planningSourceUserPlanningDefault = "user_planning_default"

// planningModelCells builds the read-only planning model cells, Claude then Codex (PRD #3252). planning is the
// stored planning model (nil = "Same as the run"). The default pair is the implement lane's
// resolution: the user's harness-specific model lane (user_default), else the lead template's
// model (template), else the harness built-in. A set planning value resolves with
// user_planning_default; otherwise the cell resolves to the default pair.
func planningModelCells(planning, claudeLane, leadClaudePin, codexPlanning, codexLane, leadCodexPin *string) []apitypes.ModelSettingCellDTO {
	return []apitypes.ModelSettingCellDTO{
		planningModelSettingCell("claude", planning, claudeLane, leadClaudePin, nil),
		planningModelSettingCell("codex", codexPlanning, codexLane, nil, leadCodexPin),
	}
}

func planningModelSettingCell(harness string, planning, lane, claudePin, codexPin *string) apitypes.ModelSettingCellDTO {
	defModel, defSource := agenttmpl.ResolveWorkerDefaultModel(harness, lane, claudePin, codexPin)
	cell := apitypes.ModelSettingCellDTO{
		Harness: harness, Model: planning,
		DefaultModel: defModel, DefaultSource: defSource,
		ResolvedModel: defModel, Source: defSource,
	}
	if planning != nil {
		m := strings.TrimSpace(*planning)
		valid := !agenttmpl.CodexRoleModels[m]
		if harness == "codex" {
			_, err := agenttmpl.ValidateModel(m)
			valid = err == nil && !agenttmpl.KnownClaudeAliases[m]
		}
		if m != "" && valid {
			cell.ResolvedModel, cell.Source = m, planningSourceUserPlanningDefault
		}
	}
	return cell
}

// planningEffortCellFromOrdinary builds the read-only per-harness planning effort cell (PRD #3252). The
// default pair is the ordinary harness effort cell's resolution (including Runtime default mode,
// where no level is sent and DefaultEffort carries the preference token); a stored planning level
// resolves with user_planning_default and explicit mode.
func planningEffortCellFromOrdinary(planning *string, ordinary apitypes.EffortSettingCellDTO) apitypes.EffortSettingCellDTO {
	harness := ordinary.Harness
	cell := apitypes.EffortSettingCellDTO{
		Harness: harness, Effort: planning,
		DefaultSource:  ordinary.Source,
		ResolvedEffort: ordinary.ResolvedEffort,
		Source:         ordinary.Source,
		Mode:           ordinary.Mode,
	}
	if ordinary.ResolvedEffort != nil {
		cell.DefaultEffort = *ordinary.ResolvedEffort
	} else {
		cell.DefaultEffort = agenttmpl.EffortRuntime
	}
	if planning != nil {
		if r := agenttmpl.ResolveHarnessEffort(harness, planning); r.Mode == agenttmpl.EffortModeExplicit && r.Source == agenttmpl.EffortSourceUserDefault {
			level := r.Level
			cell.ResolvedEffort, cell.Source, cell.Mode = &level, planningSourceUserPlanningDefault, agenttmpl.EffortModeExplicit
		}
	}
	return cell
}

// catalogDefaultDTO projects the service result onto the wire shape; unknown carries
// no value fields.
func catalogDefaultDTO(c workersvc.CatalogDefault) *apitypes.CatalogDefaultDTO {
	if c.State != workersvc.CatalogStateKnown && c.State != workersvc.CatalogStateVaries {
		return &apitypes.CatalogDefaultDTO{State: workersvc.CatalogStateUnknown}
	}
	effort, model, version, observed := c.Effort, c.Model, c.RuntimeVersion, c.ObservedAt
	return &apitypes.CatalogDefaultDTO{State: c.State, Effort: &effort, Model: &model, RuntimeVersion: &version, ObservedAt: &observed}
}

func effortSettingCell(harness string, stored *string) apitypes.EffortSettingCellDTO {
	def := agenttmpl.ResolveHarnessEffort(harness, nil)
	r := agenttmpl.ResolveHarnessEffort(harness, stored)
	cell := apitypes.EffortSettingCellDTO{
		Harness:       harness,
		Effort:        stored,
		DefaultEffort: def.Level,
		DefaultSource: def.Source,
		Source:        r.Source,
		Mode:          r.Mode,
	}
	if r.Mode == agenttmpl.EffortModeExplicit {
		level := r.Level
		cell.ResolvedEffort = &level
	}
	return cell
}

// leadEffortSettingCell is the worker-effort cell of an effective-settings view, built from the
// lead prediction so it shows what a claim ships (issue #3258 N3). It is effortSettingCell over the
// owner's stored preference, except when the prediction resolved the lead template's effort (source
// template: the preference is NULL or blank): then the cell resolves to that level with source
// template. DefaultEffort/DefaultSource describe what clearing the preference (NULL) resolves to:
// the lead template's effort with source template when it has a valid one, else the builtin.
// effortSettingCell itself stays preference-only for the Settings preference cells.
func leadEffortSettingCell(harness string, lead workersvc.LeadPrediction) apitypes.EffortSettingCellDTO {
	cell := effortSettingCell(harness, lead.UserEffort)
	if lead.TemplateEffort != nil {
		cell.DefaultEffort = *lead.TemplateEffort
		cell.DefaultSource = string(leadcfg.EffortSourceTemplate)
	}
	if lead.EffortSource == string(leadcfg.EffortSourceTemplate) && lead.Effort != nil {
		level := *lead.Effort
		cell.ResolvedEffort = &level
		cell.Source = lead.EffortSource
		cell.Mode = agenttmpl.EffortModeExplicit
	}
	return cell
}

// validateCodexJudgeModel validates a user Codex judge lane value: nil or blank clears
// to NULL; anything else must be a curated Codex id after trimming, and is stored trimmed.
func validateCodexJudgeModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	if !agenttmpl.CuratedCodexModels[m] {
		return pgtype.Text{}, fmt.Errorf("%q is not a supported Codex model", m)
	}
	return pgtype.Text{String: m, Valid: true}, nil
}

// validateClaudeJudgeModel validates the user Claude judge lane: the shared model rules
// plus a refusal of any Codex role id.
func validateClaudeJudgeModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateClaudeLaneModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	return pgtype.Text{String: m, Valid: true}, nil
}

// validateCodexSummaryModel validates a user Codex summary lane value: nil or blank
// clears to NULL; anything else must be a Codex role id after trimming, and is stored
// trimmed.
func validateCodexSummaryModel(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	m, err := agenttmpl.ValidateModel(*raw)
	if err != nil {
		return pgtype.Text{}, err
	}
	if m == "" {
		return pgtype.Text{}, nil
	}
	if !agenttmpl.CodexRoleModels[m] {
		return pgtype.Text{}, fmt.Errorf("%q is not a supported Codex model", m)
	}
	return pgtype.Text{String: m, Valid: true}, nil
}
