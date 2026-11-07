package apitypes

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
// start choices, the 8 model cells, the 4 effort cells and the 8 cross-check cells.
type EffectiveSettingsDTO struct {
	Version    int                       `json:"version"`
	Harnesses  []EffectiveHarnessDTO     `json:"harnesses"`
	Start      []EffectiveStartChoiceDTO `json:"start"`
	Models     []EffectiveModelCellDTO   `json:"models"`
	Efforts    []EffectiveEffortCellDTO  `json:"efforts"`
	CrossCheck EffectiveCrossCheckDTO    `json:"cross_check"`
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
