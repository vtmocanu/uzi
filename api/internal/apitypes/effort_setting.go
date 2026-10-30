package apitypes

// EffortSettingCellDTO is one harness's cell of the ordinary reasoning-effort
// setting (PRD #3247), parallel to ModelSettingCellDTO. A setting's cells are an
// array, always ordered claude then codex. Effort is the stored preference token
// (null = Default, a level, or "runtime"); DefaultEffort/DefaultSource are what
// Default means (medium, builtin). ResolvedEffort is the level a claim sends, null
// exactly when Mode is "runtime_default" (no level is sent). Source is user_default
// or builtin; Mode is explicit or runtime_default. Labelled per surface by
// fixtures/model-settings/contract.json.
type EffortSettingCellDTO struct {
	Harness        string  `json:"harness"`
	Effort         *string `json:"effort"`
	DefaultEffort  string  `json:"default_effort"`
	DefaultSource  string  `json:"default_source"`
	ResolvedEffort *string `json:"resolved_effort"`
	Source         string  `json:"source"`
	Mode           string  `json:"mode"`
}
