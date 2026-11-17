package apitypes

import "time"

// ModelEntryDTO is one mapped gateway model of a provider (PRD #4320).
type ModelEntryDTO struct {
	Model         string `json:"model"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

// ModelMapDTO maps the Claude aliases to gateway models. Haiku is required (the SDK's background
// calls use the small/fast slot); the others are optional.
type ModelMapDTO struct {
	Opus   *ModelEntryDTO `json:"opus,omitempty"`
	Sonnet *ModelEntryDTO `json:"sonnet,omitempty"`
	Haiku  ModelEntryDTO  `json:"haiku"`
	Fable  *ModelEntryDTO `json:"fable,omitempty"`
}

// ProviderProbeDTO is the latest stored probe result of a provider's current revision. The two
// results are separate vocabulary strings; neither certifies full compatibility.
type ProviderProbeDTO struct {
	AuthResult string    `json:"auth_result"`
	ToolResult string    `json:"tool_result,omitempty"`
	ProbedAt   time.Time `json:"probed_at"`
}

// ProviderDTO is one user-defined model provider with its current revision. It never carries the
// key: HasKey and KeyEnabled are the only key state a client sees.
type ProviderDTO struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Engine          string            `json:"engine"`
	Retired         bool              `json:"retired"`
	CurrentRevision int               `json:"current_revision"`
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	AuthStyle       string            `json:"auth_style"`
	ModelMap        ModelMapDTO       `json:"model_map"`
	HasKey          bool              `json:"has_key"`
	KeyEnabled      bool              `json:"key_enabled"`
	Probe           *ProviderProbeDTO `json:"probe"`
}

// ProviderListDTO is the GET /api/me/providers response. Enabled is false when the operator has
// not configured UZI_PROVIDER_ALLOWED_BASE_URLS: the feature is off and the list is empty.
type ProviderListDTO struct {
	Enabled   bool          `json:"enabled"`
	Providers []ProviderDTO `json:"providers"`
}
