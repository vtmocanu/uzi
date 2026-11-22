package handler

import (
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
)

// settingsFieldError is a decode or validation failure that names the request field it
// belongs to. PutMySettings renders only Message (through httpx.Error, so its body stays
// {"error": ...} exactly as before the extraction); PUT /api/me/model-settings also renders
// Field. The Message strings, including their inconsistent field prefixes, are the
// pre-extraction text: they are the wire contract the shared decoders must not change.
type settingsFieldError struct {
	Field   string
	Message string
}

// decodeNullableText unmarshals one present tri-state value (a JSON string or null) into *string.
// A value of any other JSON type is the caller's "invalid <field>" error.
func decodeNullableText(raw json.RawMessage, field string) (*string, *settingsFieldError) {
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, &settingsFieldError{Field: field, Message: "invalid " + field}
	}
	return v, nil
}

// decodeLegacyDefaultModelField validates the deprecated single-lane default_model (PUT
// /api/me/settings only; the model-settings route does not accept it).
func decodeLegacyDefaultModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "default_model"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateModel(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

// decodeDefaultClaudeModelField validates the Claude worker-model lane, including the
// closed-list guard that keeps a known Codex role id out of it. Custom ids outside that
// shared set remain provider-validated.
func decodeDefaultClaudeModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "default_claude_model"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateModel(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	if err := rejectCodexIDInClaudeLane(field, val); err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

// decodeDefaultCodexModelField validates the Codex worker-model lane (PRD #1551 D3): a known
// Claude alias must not sit in it; a validated custom id passes.
func decodeDefaultCodexModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "default_codex_model"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateModel(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	if err := rejectClaudeAliasInCodexLane(field, val); err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

// decodeDefaultEffortField validates an ordinary effort preference (default_effort and
// default_codex_effort share the closed enum, including the explicit runtime mode).
func decodeDefaultEffortField(raw json.RawMessage, field string) (pgtype.Text, *settingsFieldError) {
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateDefaultEffort(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

// decodePrefixedModelField is the shared shape of the judge and summary lanes: the decode
// error is "invalid <field>" and a validator error is prefixed "<field>: ".
func decodePrefixedModelField(raw json.RawMessage, field string, validate func(*string) (pgtype.Text, error)) (pgtype.Text, *settingsFieldError) {
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validate(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: field + ": " + err.Error()}
	}
	return val, nil
}

func decodeJudgeModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "judge_model", validateClaudeJudgeModel)
}

func decodeCodexJudgeModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "codex_judge_model", validateCodexJudgeModel)
}

func decodeSummaryModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "summary_model", validateClaudeJudgeModel)
}

func decodeCodexSummaryModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "codex_summary_model", validateCodexSummaryModel)
}

// decodeTaskReviewModelField validates the Claude task-review model (PRD #1570): the Claude-lane
// rules of the judge lane (shared model rules plus the Codex role id refusal). Blank clears to NULL,
// so the review runs the built-in (no model, the SDK decides).
func decodeTaskReviewModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "task_review_model", validateClaudeJudgeModel)
}

// decodeCodexTaskReviewModelField validates the Codex task-review model (PRD #1570): only an
// agenttmpl.CodexRoleModels id, because the worker's advice renderer drops any other id and would run
// the provider default instead. Blank clears to NULL (the built-in review model).
func decodeCodexTaskReviewModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "codex_task_review_model", validateCodexSummaryModel)
}

// decodePlanningClaudeModelField validates the Claude planning model (PRD #3252): the same
// Claude-lane rules as the judge lane (shared model rules plus the Codex role id refusal), with
// the 400 naming planning_claude_model. Blank after trim clears to NULL.
func decodePlanningClaudeModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePrefixedModelField(raw, "planning_claude_model", validateClaudeJudgeModel)
}

// decodePlanningCodexModelField accepts validated custom IDs and rejects Claude aliases.
func decodePlanningCodexModelField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "planning_codex_model"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateModel(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: field + ": " + err.Error()}
	}
	if err := rejectClaudeAliasInCodexLane(field, val); err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

func decodePlanningCodexEffortField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePlanningEffort(raw, "planning_codex_effort")
}

// decodePlanningEffortField validates the Claude planning effort (PRD #3252): only the five
// agenttmpl.EffortLevels; the preference-only `runtime` token is rejected. Blank clears.
func decodePlanningEffortField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	return decodePlanningEffort(raw, "planning_claude_effort")
}

func decodePlanningEffort(raw json.RawMessage, field string) (pgtype.Text, *settingsFieldError) {
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	if v == nil {
		return pgtype.Text{}, nil
	}
	e, err := agenttmpl.ValidateEffort(*v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: field + ": " + err.Error()}
	}
	if e == "" {
		return pgtype.Text{}, nil
	}
	return pgtype.Text{String: e, Valid: true}, nil
}

// decodeDefaultHarnessField validates default_harness (PUT /api/me/settings only).
func decodeDefaultHarnessField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "default_harness"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateHarness(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}

// decodeJudgeRunsOnField validates judge_runs_on: null (same as the run), claude or codex.
func decodeJudgeRunsOnField(raw json.RawMessage) (pgtype.Text, *settingsFieldError) {
	const field = "judge_runs_on"
	v, ferr := decodeNullableText(raw, field)
	if ferr != nil {
		return pgtype.Text{}, ferr
	}
	val, err := validateJudgeRunsOn(v)
	if err != nil {
		return pgtype.Text{}, &settingsFieldError{Field: field, Message: err.Error()}
	}
	return val, nil
}
