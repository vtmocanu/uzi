package apitypes

import "time"

// EffortSettingCellDTO is one harness's cell of the ordinary reasoning-effort
// setting (PRD #3247), parallel to ModelSettingCellDTO. A setting's cells are an
// array, always ordered claude then codex. Effort is the stored preference token
// (null = Default, a level, or "runtime"); DefaultEffort/DefaultSource are what
// Default means (medium, builtin). ResolvedEffort is the level a claim sends, null
// exactly when Mode is "runtime_default" (no level is sent). Source is user_default
// or builtin; Mode is explicit or runtime_default. Labelled per surface by
// fixtures/model-settings/contract.json.
//
// CatalogDefault (issue #3251) is carried on the codex cell only, always: the level the
// Codex catalog would use for the user's Codex model when Mode is runtime_default. The
// Claude cell never carries it.
type EffortSettingCellDTO struct {
	Harness        string             `json:"harness"`
	Effort         *string            `json:"effort"`
	DefaultEffort  string             `json:"default_effort"`
	DefaultSource  string             `json:"default_source"`
	ResolvedEffort *string            `json:"resolved_effort"`
	Source         string             `json:"source"`
	Mode           string             `json:"mode"`
	CatalogDefault *CatalogDefaultDTO `json:"catalog_default,omitempty"`
}

// CatalogDefaultDTO is the Codex catalog default reasoning effort a new Codex run
// would see (issue #3251). State is known, unknown or varies (workers on different
// runtime versions reported different levels). Effort, Model, RuntimeVersion and
// ObservedAt are null exactly when State is unknown. Effort "none" means the catalog
// requests no reasoning level.
type CatalogDefaultDTO struct {
	State          string     `json:"state"`
	Effort         *string    `json:"effort"`
	Model          *string    `json:"model"`
	RuntimeVersion *string    `json:"runtime_version"`
	ObservedAt     *time.Time `json:"observed_at"`
}
