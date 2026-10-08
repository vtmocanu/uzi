package apitypes

// AdminDockerAllowlistRepoDTO identifies a repository for instance Docker trust.
type AdminDockerAllowlistRepoDTO struct {
	ID                string `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	Enabled           bool   `json:"enabled"`
	OwnerEmail        string `json:"owner_email"`
	ConnectionID      string `json:"connection_id"`
	ForgeType         string `json:"forge_type"`
	BaseURL           string `json:"base_url"`
}

// AdminDockerAllowlistReposDTO is the admin repository picker envelope.
type AdminDockerAllowlistReposDTO struct {
	Repos []AdminDockerAllowlistRepoDTO `json:"repos"`
}
