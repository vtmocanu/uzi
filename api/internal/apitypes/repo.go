package apitypes

import "time"

// RepoDTO is the browser/CLI view of a connected repo.
type RepoDTO struct {
	ID                string  `json:"id"`
	ConnectionID      string  `json:"connection_id"`
	ForgeProjectID    int64   `json:"forge_project_id"`
	PathWithNamespace string  `json:"path_with_namespace"`
	WebURL            string  `json:"web_url"`
	DefaultBranch     *string `json:"default_branch"`
	Enabled           bool    `json:"enabled"`
	RepoSkillsEnabled bool    `json:"repo_skills_enabled"`
	// RepoClaudemdEnabled is the trusted-repo instructions opt-in (PRD #246): when
	// true, the lead reads the clone's root CLAUDE.md as a nonce-fenced,
	// UNTRUSTED/ADVISORY block. Independent of RepoSkillsEnabled (both sit under the
	// UI's "Trusted repo" grouping). Default false.
	RepoClaudemdEnabled bool `json:"repo_claudemd_enabled"`
	// RepoDevboxOptIn is the tier-2 opt-in (PRD #18 M5): when true, a run on this
	// repo also unions the packages from the repo's own devbox.json (packages-only,
	// never its hooks/scripts). Default false.
	RepoDevboxOptIn bool `json:"repo_devbox_opt_in"`
	// Pipeline is the repo's default-branch CI status (PRD #6), null when there is
	// no cached default-branch pipeline (no CI configured, MR-only pipelines, or not
	// yet synced). Set by the list handlers, which enrich from the pipeline cache.
	Pipeline *PipelineDTO `json:"pipeline"`
}

// PipelineDTO is the CI-badge payload (PRD #6) a repo row, board header, or card
// carries: the raw GitLab pipeline status (the web layer maps it to a tone), the
// pipeline's web URL, its id, and when uzi last synced it (badge staleness). A
// null pipeline on a DTO means "no CI, or not yet synced" for that ref.
type PipelineDTO struct {
	// Ref is the watched ref this pipeline is for (default branch or an agent
	// branch) — what the Fix CI trigger POSTs to fix this pipeline (PRD #6).
	Ref        string    `json:"ref"`
	Status     string    `json:"status"`
	WebURL     string    `json:"web_url"`
	PipelineID int64     `json:"pipeline_id"`
	SyncedAt   time.Time `json:"synced_at"`
}
