package apitypes

import "time"

// ModelEntryDTO is one mapped gateway model of a provider (PRD #4320).
type ModelEntryDTO struct {
	Model         string `json:"model"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

// ModelMapDTO carries the engine-specific model map. Claude requires haiku; Codex requires
// default. Optional entries retain their exact uzi-visible model IDs.
type ModelMapDTO struct {
	Default    *ModelEntryDTO `json:"default,omitempty"`
	GPT6Astra  *ModelEntryDTO `json:"gpt-6-astra,omitempty"`
	GPT56Sol   *ModelEntryDTO `json:"gpt-5.6-sol,omitempty"`
	GPT6Sol    *ModelEntryDTO `json:"gpt-6-sol,omitempty"`
	GPT61Sol   *ModelEntryDTO `json:"gpt-6.1-sol,omitempty"`
	GPT62Sol   *ModelEntryDTO `json:"gpt-6.2-sol,omitempty"`
	GPT62Astra *ModelEntryDTO `json:"gpt-6.2-astra,omitempty"`
	GPT6Luna   *ModelEntryDTO `json:"gpt-6-luna,omitempty"`
	Opus       *ModelEntryDTO `json:"opus,omitempty"`
	Sonnet     *ModelEntryDTO `json:"sonnet,omitempty"`
	Haiku      ModelEntryDTO  `json:"haiku,omitzero"`
	Fable      *ModelEntryDTO `json:"fable,omitempty"`
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
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Engine          string      `json:"engine"`
	Retired         bool        `json:"retired"`
	CurrentRevision int         `json:"current_revision"`
	Protocol        string      `json:"protocol"`
	BaseURL         string      `json:"base_url"`
	AuthStyle       string      `json:"auth_style"`
	ModelMap        ModelMapDTO `json:"model_map"`
	// CredentialMode is null for a personal provider and "per_user_key" for an admin-shared
	// definition (PRD #4324): HasKey, KeyEnabled and Probe then describe the CALLER's own key only.
	CredentialMode *string           `json:"credential_mode"`
	HasKey         bool              `json:"has_key"`
	KeyEnabled     bool              `json:"key_enabled"`
	Probe          *ProviderProbeDTO `json:"probe"`
}

// AdminSharedProviderDTO is an admin-shared provider definition as an admin reads it (PRD #4324):
// the definition fields plus two counts. It carries no key and no per-user Test evidence.
type AdminSharedProviderDTO struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	Engine            string      `json:"engine"`
	Retired           bool        `json:"retired"`
	CurrentRevision   int         `json:"current_revision"`
	Protocol          string      `json:"protocol"`
	BaseURL           string      `json:"base_url"`
	AuthStyle         string      `json:"auth_style"`
	ModelMap          ModelMapDTO `json:"model_map"`
	CredentialMode    string      `json:"credential_mode"`
	SelectedUserCount int64       `json:"selected_user_count"`
	AttachedKeyCount  int64       `json:"attached_key_count"`
}

// AdminSharedProviderListDTO is the GET /api/admin/providers response.
type AdminSharedProviderListDTO struct {
	Providers []AdminSharedProviderDTO `json:"providers"`
}

// ProviderCreateRequest is the POST /api/me/providers body. It never carries a key: the key is
// added from the web Settings only.
type ProviderCreateRequest struct {
	Name      string      `json:"name"`
	Engine    string      `json:"engine"`
	Protocol  string      `json:"protocol"`
	BaseURL   string      `json:"base_url"`
	AuthStyle string      `json:"auth_style"`
	ModelMap  ModelMapDTO `json:"model_map"`
}

// ProviderPatchRequest is the PATCH /api/me/providers/{id} body: a rename and/or a wholesale
// model-map replacement. Nil fields are left alone.
type ProviderPatchRequest struct {
	Name     *string      `json:"name,omitempty"`
	ModelMap *ModelMapDTO `json:"model_map,omitempty"`
}

// ProviderListDTO is the GET /api/me/providers response. Enabled is false when the operator has
// not configured UZI_PROVIDER_ALLOWED_BASE_URLS: the feature is off and the list is empty.
// AllowedBaseURLs echoes the operator's exact allowlist entries so a user can pick one; it is
// an empty array when the feature is off.
type ProviderListDTO struct {
	Enabled         bool          `json:"enabled"`
	Providers       []ProviderDTO `json:"providers"`
	AllowedBaseURLs []string      `json:"allowed_base_urls"`
}
