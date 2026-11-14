package apitypes

import "time"

// RepoActivityDTO is one repo's line in the admin repo-activity list (issue #4334).
// LastRunAt is the created_at of the repo's newest run of ANY kind, and is null
// (present-as-null, no omitempty) for a repo that never ran.
type RepoActivityDTO struct {
	ID                string     `json:"id"`
	PathWithNamespace string     `json:"path_with_namespace"`
	Enabled           bool       `json:"enabled"`
	LastRunAt         *time.Time `json:"last_run_at"`
}

// AdminRepoActivityDTO is the GET /api/admin/repo-activity envelope (issue #4334):
// every repo across ALL users that is enabled or has a listed-active run. Repos is
// always an array: the handler seeds an empty slice so an instance with no such
// repo serializes [] and never null.
type AdminRepoActivityDTO struct {
	Repos []RepoActivityDTO `json:"repos"`
}
