package apitypes

// Agent-source admin read DTOs (PRD #602 M6, CLI half). These mirror the wire
// shape of the GET /api/admin/agent-source response the handler emits (see
// handler/agent_source.go: agentSourceDTO and its nested types). Only the config
// and status projections are consumed by the read-only CLI subcommands (`uzi admin
// agent-source get|status`); the staged-snapshot review + the sync/apply writes
// stay web-only per ADR 0602, so the CLI never triggers a fetch or a write. The
// staged snapshot is decoded here too so a raw `--json` dump round-trips the full
// envelope without dropping fields.

// AgentSourceConfigDTO is the agent-source config projection: where roles are
// pulled from and whether the puller is on. CredentialConfigured reports only
// WHETHER a credential is set, never its value.
type AgentSourceConfigDTO struct {
	URL                  string `json:"url"`
	Ref                  string `json:"ref"`
	Folder               string `json:"folder"`
	Enabled              bool   `json:"enabled"`
	Interval             string `json:"interval"`
	CredentialConfigured bool   `json:"credential_configured"`
}

// AgentSourceCountsDTO folds the {staged, changed, failed} tallies the sync engine
// records for a fetch.
type AgentSourceCountsDTO struct {
	Staged  int `json:"staged"`
	Changed int `json:"changed"`
	Failed  int `json:"failed"`
}

// AgentSourceStatusDTO is the sync-status projection: the last fetch's time/sha/
// status/error and the last apply's time/sha. Fields are omitempty on the wire, so
// an absent value means "never happened", not zero.
type AgentSourceStatusDTO struct {
	LastSyncAt     string                `json:"last_sync_at,omitempty"`
	LastSyncSHA    string                `json:"last_sync_sha,omitempty"`
	LastSyncStatus string                `json:"last_sync_status,omitempty"`
	LastSyncError  string                `json:"last_sync_error,omitempty"`
	LastAppliedAt  string                `json:"last_applied_at,omitempty"`
	LastAppliedSHA string                `json:"last_applied_sha,omitempty"`
	Counts         *AgentSourceCountsDTO `json:"counts,omitempty"`
	// Derived server-side from the persisted remote facts + live config (PRD #702 M4,
	// Decision 6); the CLI does no egress. UpdateAvailable is the badge; LatestRef
	// names the newer semver tag on a tag-pinned update; UpdateCheckedAt is the
	// RFC3339 time of the last update check.
	UpdateAvailable bool   `json:"update_available"`
	LatestRef       string `json:"latest_ref,omitempty"`
	UpdateCheckedAt string `json:"update_checked_at,omitempty"`
}

// AgentSourceRoleDTO is one staged role in the review snapshot. The CLI does not
// render this (staged review is web-only), but it is decoded so a `--json` dump of
// the read endpoint round-trips the whole envelope.
type AgentSourceRoleDTO struct {
	Name          string   `json:"name"`
	OK            bool     `json:"ok"`
	Reason        string   `json:"reason,omitempty"`
	Description   string   `json:"description,omitempty"`
	Model         string   `json:"model,omitempty"`
	Tools         []string `json:"tools,omitempty"`
	PromptBody    string   `json:"prompt_body,omitempty"`
	BodySanitized bool     `json:"body_sanitized"`
	Notes         []string `json:"notes,omitempty"`
}

// AgentSourceDiffDTO is one entry in the staged snapshot's apply-diff.
type AgentSourceDiffDTO struct {
	Name   string `json:"name"`
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// AgentSourceStagedDTO is the staged snapshot pending review. Pending is true when
// the snapshot has not yet been applied (its SHA differs from the last-applied SHA).
type AgentSourceStagedDTO struct {
	FetchedAt  string               `json:"fetched_at,omitempty"`
	FetchedSHA string               `json:"fetched_sha"`
	SourceURL  string               `json:"source_url"`
	SourceRef  string               `json:"source_ref"`
	Roles      []AgentSourceRoleDTO `json:"roles"`
	Diff       []AgentSourceDiffDTO `json:"diff"`
	Counts     AgentSourceCountsDTO `json:"counts"`
	Pending    bool                 `json:"pending"`
}

// AgentSourceDTO is the GET /api/admin/agent-source view: config + sync status +
// the staged snapshot (nil when nothing has been staged yet).
type AgentSourceDTO struct {
	Config AgentSourceConfigDTO  `json:"config"`
	Status AgentSourceStatusDTO  `json:"status"`
	Staged *AgentSourceStagedDTO `json:"staged"`
}
