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
	// RequiredCapabilities is the static per-repo capability hint (PRD #84 M2): the
	// non-provisionable capabilities (e.g. docker) every run on this repo is routed to
	// require. Copied onto each new issue run at enqueue and matched at claim time.
	// Server-owned vocabulary — an unknown name is Filter-ed out before it is stored.
	// Empty (the default) means no requirement, so runs claim anywhere.
	RequiredCapabilities []string `json:"required_capabilities"`
	// Pipeline is the repo's default-branch CI status (PRD #6), null when there is
	// no cached default-branch pipeline (no CI configured, MR-only pipelines, or not
	// yet synced). Set by the list handlers, which enrich from the pipeline cache.
	Pipeline *PipelineDTO `json:"pipeline"`
	// GuardrailOverride is the admin per-repo #66 guardrail override metadata (D8),
	// null when no override is active (guardrail_override_reason IS NULL). M8 exposes
	// it so M9 can render the badge; M8 itself adds no new UI control.
	GuardrailOverride *GuardrailOverrideDTO `json:"guardrail_override"`
	// GuardrailBlocked is the authoritative, server-computed "would a run be refused
	// on this repo right now" (PRD #66 M9, D8): the repo's stored privilege_report
	// findings run through the SINGLE shared privcheck.DowngradeOverridden (so the
	// override is applied identically to the live gates, never re-derived in the web)
	// and then RepoReport.Blocks(). The web reads this boolean for the badge STATE and
	// never re-implements the waivable-set rule. False on a connection with no report
	// yet (never swept, or UZI_PRIVILEGE_CHECK_INTERVAL=0) is "unknown, not safe": the
	// enable/run gates still fail closed live (M4-M6); the admin blocked-repos list is
	// where that unknown is surfaced explicitly (R1).
	GuardrailBlocked bool `json:"guardrail_blocked"`
}

// GuardrailOverrideDTO is the audit metadata for an active admin per-repo guardrail
// override (PRD #66 D8): the reason the admin gave, the actor's user id, and when it
// was set. By is the raw actor uuid string — M9 may resolve a display name; M8 keeps
// it simple.
type GuardrailOverrideDTO struct {
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
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
