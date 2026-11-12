package apitypes

import (
	"bytes"
	"encoding/json"
)

// The effective-settings wire (PRD #3254): what model, effort and harness a run would use.
// Tokens only on the wire; the labels live in fixtures/model-settings/contract.json. No
// credential id, label or value appears in any of these types.

// EffectiveSettingsVersion is the wire version of every effective-settings response.
const EffectiveSettingsVersion = 1

// EffectiveIgnoredDTO is a value that was supplied but does not apply (a schedule's model pin on
// the other harness), with where it came from.
type EffectiveIgnoredDTO struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// EffectiveModelCellDTO is one harness's model cell of a setting: the shared settings cell
// (embedded unchanged, so its fields flatten into the object) plus which setting it is, when the
// value is fixed (create: frozen when the run is created or fired; claim: read when a worker
// picks the run up), an ignored supplied value and note codes. resolved_model "" means the
// runtime decides (note sdk_default).
type EffectiveModelCellDTO struct {
	ModelSettingCellDTO
	Setting string               `json:"setting"`
	FixedAt string               `json:"fixed_at"`
	Ignored *EffectiveIgnoredDTO `json:"ignored"`
	Notes   []string             `json:"notes"`
}

// EffectiveEffortCellDTO is one harness's effort cell: the shared effort cell (embedded
// unchanged, including a Codex catalog_default) plus the setting, when it is fixed and note
// codes. resolved_effort null means nothing is sent.
type EffectiveEffortCellDTO struct {
	EffortSettingCellDTO
	Setting string   `json:"setting"`
	FixedAt string   `json:"fixed_at"`
	Notes   []string `json:"notes"`
}

// CredentialDTO says which credential a run would use and when that is fixed, never which one.
// Kind is anthropic or codex; Source is claim_selection, chosen_at_create, auto_selection,
// owner_default, token_override or codex_default_credential; AuthMode (codex only) is
// subscription or api_key.
type CredentialDTO struct {
	Kind     string  `json:"kind"`
	Source   string  `json:"source"`
	FixedAt  string  `json:"fixed_at"`
	AuthMode *string `json:"auth_mode"`
}

// HarnessValueDTO is a predicted harness. Value is null with source chosen_at_create when a
// creation-time policy (a note: automatic_weekly_reset or overflow) chooses it at creation.
// The judge harness cell (PRD #3257) adds source same_as_run: value is null because the judge
// follows the harness of the run it reviews, which is known only when that run exists.
type HarnessValueDTO struct {
	Value   *string  `json:"value"`
	Source  string   `json:"source"`
	FixedAt string   `json:"fixed_at"`
	Notes   []string `json:"notes"`
}

// EffectiveHarnessDTO is one harness taken as an explicit choice: whether it is usable, the
// refusal otherwise, and its credential.
type EffectiveHarnessDTO struct {
	Harness       string         `json:"harness"`
	Usable        bool           `json:"usable"`
	Refusal       *string        `json:"refusal"`
	RefusalDetail *string        `json:"refusal_detail"`
	Credential    *CredentialDTO `json:"credential"`
}

// EffectiveStartChoiceDTO is what one start choice (default, claude or codex) resolves to when no
// Anthropic token override is sent. Harness is null only on a refusal.
type EffectiveStartChoiceDTO struct {
	Choice                   string           `json:"choice"`
	Harness                  *HarnessValueDTO `json:"harness"`
	Refusal                  *string          `json:"refusal"`
	RefusalDetail            *string          `json:"refusal_detail"`
	AnthropicOverrideAllowed bool             `json:"anthropic_override_allowed"`
}

// EffectiveCrossCheckDTO holds the cross-check cells, plan before code and Claude before Codex.
type EffectiveCrossCheckDTO struct {
	Models  []EffectiveModelCellDTO  `json:"models"`
	Efforts []EffectiveEffortCellDTO `json:"efforts"`
}

// EffectiveSettingsDTO is the body of GET /api/me/effective-settings: both harnesses, the three
// start choices, the 8 model cells, the 4 effort cells, the 8 cross-check cells and the judge harness.
type EffectiveSettingsDTO struct {
	Version    int                       `json:"version"`
	Harnesses  []EffectiveHarnessDTO     `json:"harnesses"`
	Start      []EffectiveStartChoiceDTO `json:"start"`
	Models     []EffectiveModelCellDTO   `json:"models"`
	Efforts    []EffectiveEffortCellDTO  `json:"efforts"`
	CrossCheck EffectiveCrossCheckDTO    `json:"cross_check"`
	// JudgeHarness is the harness a judge run would use (PRD #3257), always present: an
	// explicit "Judge runs on" choice is value claude|codex with source user_default; unset is
	// value null with source same_as_run (the judge follows the run it reviews). fixed_at is
	// create in both cases.
	JudgeHarness HarnessValueDTO `json:"judge_harness"`
}

// PreviewCredentialOverrideRequest is an Anthropic token choice in a preview request: mode is
// inherit, auto, default or pinned, and secret_id (a uuid) names the pinned token.
type PreviewCredentialOverrideRequest struct {
	Mode     string  `json:"mode"`
	SecretID *string `json:"secret_id,omitempty"`
}

// StartPreviewRequest is the body of POST /api/me/effective-settings/start-preview.
type StartPreviewRequest struct {
	Harness            *string                           `json:"harness"`
	CredentialOverride *PreviewCredentialOverrideRequest `json:"credential_override"`
}

// StartPreviewAlternativeDTO is one harness a creation-time policy might choose, computed as if
// that harness were chosen. Credential is null on a refused branch, which carries its own
// refusal and refusal_detail; both are null on a valid branch.
type StartPreviewAlternativeDTO struct {
	Harness       string         `json:"harness"`
	Credential    *CredentialDTO `json:"credential"`
	Refusal       *string        `json:"refusal"`
	RefusalDetail *string        `json:"refusal_detail"`
}

// StartPreviewDTO is the body of POST /api/me/effective-settings/start-preview. When the
// harness is chosen at creation, Credential is null and Alternatives lists Claude then Codex,
// neither marked as chosen. Refusal is set only for a failure before a harness is decided, or when
// every alternative is refused for the same reason.
type StartPreviewDTO struct {
	Version       int                          `json:"version"`
	Harness       *HarnessValueDTO             `json:"harness"`
	Refusal       *string                      `json:"refusal"`
	RefusalDetail *string                      `json:"refusal_detail"`
	Credential    *CredentialDTO               `json:"credential"`
	Alternatives  []StartPreviewAlternativeDTO `json:"alternatives,omitempty"`
}

// OptionalText distinguishes an omitted string field from an explicit null and a value, like
// OptionalHarness: a plain *string collapses omitted and null.
//
//   - key absent         -> Present=false (as stored)
//   - "model": null      -> Present=true, Value=nil (clear)
//   - "model": "sonnet"  -> Present=true, Value=&"sonnet"
type OptionalText struct {
	Present bool
	Value   *string
}

// UnmarshalJSON records that the key was present (even for an explicit null).
func (o *OptionalText) UnmarshalJSON(b []byte) error {
	o.Present = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// MarshalJSON emits the value, or null when Present without one. A Present=false wrapper is
// omitted by IsZero and the omitzero tag.
func (o OptionalText) MarshalJSON() ([]byte, error) {
	if o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.Value)
}

// IsZero drives the omitzero tag: an omitted wrapper is dropped from the encoded body.
func (o OptionalText) IsZero() bool { return !o.Present }

// OptionalPreviewCredentialOverride keeps presence for a preview's credential_override like
// OptionalCredentialOverride (omitted is as stored, null is inherit), and decodes the object
// strictly, so an unknown member is rejected as it is in the rest of the preview body.
type OptionalPreviewCredentialOverride struct {
	Present bool
	Value   *PreviewCredentialOverrideRequest
}

// UnmarshalJSON records that the key was present and decodes a non-null object strictly.
func (o *OptionalPreviewCredentialOverride) UnmarshalJSON(b []byte) error {
	o.Present = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v PreviewCredentialOverrideRequest
	if err := dec.Decode(&v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// MarshalJSON emits the object, or null when Present without one.
func (o OptionalPreviewCredentialOverride) MarshalJSON() ([]byte, error) {
	if o.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// IsZero drives the omitzero tag: an omitted wrapper is dropped from the encoded body.
func (o OptionalPreviewCredentialOverride) IsZero() bool { return !o.Present }

// ScheduleSettingsPreviewRequest is the body of POST /api/me/effective-settings/schedule-preview.
// Without schedule_id it is a draft and target is required; with schedule_id it supplies fields
// that replace the stored ones (an absent field means as stored). harness, model and
// credential_override keep presence: omitted is as stored, null clears.
type ScheduleSettingsPreviewRequest struct {
	ScheduleID            *string                           `json:"schedule_id,omitempty"`
	Target                *string                           `json:"target,omitempty"`
	Harness               OptionalHarness                   `json:"harness,omitzero"`
	Model                 OptionalText                      `json:"model,omitzero"`
	OverrideSubagentModel *bool                             `json:"override_subagent_model,omitempty"`
	CredentialOverride    OptionalPreviewCredentialOverride `json:"credential_override,omitzero"`
}

// EffectiveSubagentPinDTO is one own subagent whose template pins a model for the harness.
type EffectiveSubagentPinDTO struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// EffectiveSubagentsDTO says how a run's subagents pick a model. mode is follow_lead (every
// subagent runs the lead's model) or own_pins (each pinned subagent keeps its pin; the rest
// follow the lead). lead_value "" means the SDK decides. pinned lists at most 32 subagents sorted
// by name; pinned_truncated says there were more.
type EffectiveSubagentsDTO struct {
	Mode            string                    `json:"mode"`
	LeadValue       string                    `json:"lead_value"`
	Pinned          []EffectiveSubagentPinDTO `json:"pinned"`
	PinnedTruncated bool                      `json:"pinned_truncated"`
	// Notes are labels: repo_agents_not_predicted always, since agents a cloned repository
	// defines are not predicted.
	Notes []string `json:"notes"`
}

// ScheduleAlternativeDTO is one harness a creation-time policy might choose for a schedule fire,
// computed as if that harness were chosen. A refused branch carries its own refusal and
// refusal_detail, and worker_model, worker_effort, credential and subagents are all null.
type ScheduleAlternativeDTO struct {
	Harness       string                  `json:"harness"`
	WorkerModel   *EffectiveModelCellDTO  `json:"worker_model"`
	WorkerEffort  *EffectiveEffortCellDTO `json:"worker_effort"`
	Credential    *CredentialDTO          `json:"credential"`
	Subagents     *EffectiveSubagentsDTO  `json:"subagents"`
	Refusal       *string                 `json:"refusal"`
	RefusalDetail *string                 `json:"refusal_detail"`
}

// ScheduleSettingsPreviewDTO is the body of POST /api/me/effective-settings/schedule-preview:
// what firing the schedule now and claiming the run would use. Refusal is set for a save-step
// failure, a failure before the harness is decided, a fire-rule refusal on a known harness, or
// when every alternative is refused for the same reason. When the harness is chosen at creation,
// harness.value is null, the four harness-dependent fields are null and Alternatives lists
// Claude then Codex, neither marked as chosen.
type ScheduleSettingsPreviewDTO struct {
	Version       int                      `json:"version"`
	Harness       *HarnessValueDTO         `json:"harness"`
	Refusal       *string                  `json:"refusal"`
	RefusalDetail *string                  `json:"refusal_detail"`
	WorkerModel   *EffectiveModelCellDTO   `json:"worker_model"`
	WorkerEffort  *EffectiveEffortCellDTO  `json:"worker_effort"`
	Credential    *CredentialDTO           `json:"credential"`
	Subagents     *EffectiveSubagentsDTO   `json:"subagents"`
	Notes         []string                 `json:"notes"`
	Alternatives  []ScheduleAlternativeDTO `json:"alternatives,omitempty"`
}
