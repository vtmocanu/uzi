package apitypes

import "time"

// ScheduleRequest is the create/patch input for a run schedule (PRD #241 M4).
//
// On CREATE, omitted pointer fields take their documented defaults (auto_approve=true
// per Decision 4, wait_on_limit=true, enabled=true). On PATCH the pointers carry
// "omitted = keep the current value" semantics: a nil AutoApprove/WaitOnLimit/Enabled
// leaves that flag untouched, so a caller can toggle one field without restating the
// rest. Timing/target-specific fields (CronExpr, RunAt, Timezone, IssueIID, Labels,
// Prompt) are validated per target/timing by the handler's pure validateScheduleConfig.
//
// MaxIssues is the sweep fan-out cap (PRD #274 M2, sweep target ONLY): the max number of
// issues one sweep fire starts, oldest-first. A nil/absent value means unlimited; a new
// sweep defaults to 10. It carries "present, even to clear" tri-state — the web sends an
// explicit null to clear a sweep back to unlimited (see mergeSchedule's replace-semantics).
//
// Guidance is optional owner-authored steering (PRD #274 M3, issue/sweep targets ONLY):
// injected into the run instruction as a clearly delineated "how" section after the issue
// body, without changing WHAT the task is or any eligibility gate. A nil/absent OR blank
// value means none; the prompt target rejects it (a prompt carries its own text). It
// carries the same "present, even to clear" tri-state as MaxIssues — an explicit null (or
// empty/whitespace, normalized to nil) clears stored guidance.
//
// Model is the per-schedule model override (PRD #300, all targets): the model a schedule
// fires its runs on, frozen onto each run at fire time and taking precedence over the
// owner's per-user Worker model at claim assembly. A nil/absent OR blank value means
// inherit the owner default (NULL in the DB). Validated later via agenttmpl.ValidateModel;
// it carries the same "present, even to clear" replace-semantics on PATCH (see mergeSchedule).
type ScheduleRequest struct {
	Target string `json:"target"`
	// RepoID repoints a schedule to another repo on PATCH (Feature A, PRD #344). It is
	// honored ONLY on PATCH: CreateSchedule takes the repo from the URL and ignores a
	// body repo_id, so a create caller has no reason to send it. Empty = keep the current
	// repo (keep-on-empty in mergeSchedule), NOT replace-semantics.
	RepoID      string     `json:"repo_id"`
	IssueIID    *int64     `json:"issue_iid"`
	Labels      []string   `json:"labels"`
	Prompt      string     `json:"prompt"`
	Timing      string     `json:"timing"`
	CronExpr    string     `json:"cron_expr"`
	RunAt       *time.Time `json:"run_at"`
	Timezone    string     `json:"timezone"`
	AutoApprove *bool      `json:"auto_approve"`
	WaitOnLimit *bool      `json:"wait_on_limit"`
	Enabled     *bool      `json:"enabled"`
	MaxIssues   *int       `json:"max_issues"`
	Guidance    *string    `json:"guidance"`
	Model       *string    `json:"model"`
	// OverrideSubagentModel is the per-schedule "apply model also to agents" opt-in (PRD
	// #305, all targets): when true, a fired run's resolved model overrides every
	// subagent's own model pin too, not just the lead. Boolean, not tri-state (Decision 5):
	// nil ≡ false (off) under replace semantics — an omitted value turns it off, which the
	// web avoids by always sending the full config. Default off is byte-identical to today.
	OverrideSubagentModel *bool `json:"override_subagent_model"`
}

// ScheduleDTO is the response view of a run schedule (PRD #241 M4). All fields are
// snake_case JSON. NextFires is a computed preview (up to 3 upcoming fires) for a
// recurring schedule; it is empty for a once/terminal schedule. RepoPath is a
// best-effort display value and may be "" when the repo can no longer be resolved
// (disconnected or no longer owned).
type ScheduleDTO struct {
	ID          string     `json:"id"`
	RepoID      string     `json:"repo_id"`
	RepoPath    string     `json:"repo_path"`
	Target      string     `json:"target"`
	IssueIID    *int64     `json:"issue_iid"`
	Labels      []string   `json:"labels"`
	Prompt      string     `json:"prompt"`
	Timing      string     `json:"timing"`
	CronExpr    string     `json:"cron_expr"`
	RunAt       *time.Time `json:"run_at"`
	Timezone    string     `json:"timezone"`
	NextFireAt  *time.Time `json:"next_fire_at"`
	LastFiredAt *time.Time `json:"last_fired_at"`
	AutoApprove bool       `json:"auto_approve"`
	WaitOnLimit bool       `json:"wait_on_limit"`
	Enabled     bool       `json:"enabled"`
	// MaxIssues is the sweep fan-out cap (PRD #274 M2, sweep target only): nil means
	// unlimited (NULL in the DB), a value is the oldest-first per-fire limit. New sweeps
	// default to 10.
	MaxIssues *int `json:"max_issues"`
	// Guidance is optional owner-authored steering (PRD #274 M3, issue/sweep targets only):
	// injected into the run instruction as a delineated "how" section. nil means none (NULL
	// or empty in the DB).
	Guidance *string `json:"guidance"`
	// Model is the per-schedule model override (PRD #300, all targets): nil means inherit
	// the owner's per-user Worker model (NULL or empty in the DB), a value is the model a
	// schedule fires its runs on. Validated via agenttmpl.ValidateModel; replace-semantics
	// on PATCH (see mergeSchedule).
	Model *string `json:"model"`
	// OverrideSubagentModel is the per-schedule "apply model also to agents" opt-in (PRD
	// #305, all targets): true means a fired run's resolved model overrides every subagent's
	// own model pin too, not just the lead. Boolean, not tri-state (Decision 5): nil ≡ false
	// (off) under replace semantics. Default off is byte-identical to PRD #300's behaviour.
	OverrideSubagentModel *bool     `json:"override_subagent_model"`
	Status                string    `json:"status"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
	// NextFires is the live "next N fires" preview (up to 3), computed from the same
	// cron/next-fire logic the modal preview endpoint uses so the list and the modal
	// agree.
	NextFires []time.Time `json:"next_fires"`
	// LastFire is the structured summary of this schedule's most recent persisted fire
	// (PRD #308 M3), unmarshalled straight from the run_schedules.last_fire jsonb column
	// (its json tags mirror schedsvc's package-internal persisted wire shape exactly). A
	// nil value means the schedule has never fired — or, since only the success/benign
	// advance path persists, that a parked-or-transient fire left the prior summary (or
	// none) in place rather than overwriting it.
	LastFire *LastFire `json:"last_fire"`
}

// LastFireStarted is one run a persisted fire actually created (PRD #308 M3). Its json
// tags mirror schedsvc's package-internal lastFireStarted exactly, since apitypes.LastFire
// is json.Unmarshalled straight from the persisted last_fire jsonb bytes.
type LastFireStarted struct {
	IssueIID *int64 `json:"issue_iid"` // nil for a prompt schedule
	RunID    string `json:"run_id"`    // uuid string
	Title    string `json:"title"`
	// WebURL is the forge issue's web URL snapshotted at fire time (PRD #411). Empty for
	// prompt schedules, for skips that never fetched the issue, and for pre-#411 persisted
	// summaries (which lack the key → unmarshals to "").
	WebURL string `json:"web_url"`
}

// LastFireSkip is one candidate a persisted fire considered but started nothing for, with
// its typed reason (a SkipReason string, never free text). Tags mirror schedsvc's
// lastFireSkip exactly.
type LastFireSkip struct {
	IssueIID *int64 `json:"issue_iid"`
	Title    string `json:"title"`
	Reason   string `json:"reason"`
	// WebURL is the forge issue's web URL snapshotted at fire time (PRD #411). Empty for
	// prompt schedules, for skips that never fetched the issue, and for pre-#411 persisted
	// summaries (which lack the key → unmarshals to "").
	WebURL string `json:"web_url"`
}

// LastFire is the top-level persisted summary of a schedule's last fire (PRD #308 M3).
// FiredAt is the advance instant; Matched == len(Started) + len(Skips) balances (Decision
// 4). Tags mirror schedsvc's lastFireRecord exactly so the raw jsonb column unmarshals
// straight into this struct.
type LastFire struct {
	FiredAt time.Time         `json:"fired_at"`
	Matched int               `json:"matched"`
	Capped  bool              `json:"capped"`
	Started []LastFireStarted `json:"started"`
	Skips   []LastFireSkip    `json:"skips"`
}

// SchedulePreviewRequest asks for a live "next fires" preview from a timing spec that
// need not correspond to a stored schedule (the modal computes it as the user types).
// N is clamped to [1,10] by the handler (default 3).
type SchedulePreviewRequest struct {
	Timing   string     `json:"timing"`
	CronExpr string     `json:"cron_expr"`
	RunAt    *time.Time `json:"run_at"`
	Timezone string     `json:"timezone"`
	N        int        `json:"n"`
}

// SchedulePreviewResponse carries the computed fire instants (UTC), earliest first.
type SchedulePreviewResponse struct {
	Fires []time.Time `json:"fires"`
}

// RunNowResponse reports the outcome of a manual run-now fire (PRD #308 M3). Created and
// RunIDs are retained for back-compat (Created == len(Started), RunIDs are the started
// runs' ids) and are derivable from Started; the structured Matched/Capped/Started/Skips
// fields carry the full per-candidate outcome so a caller can render it without a second
// fetch. Started/Skips are non-nil empty slices, matching the persisted convention.
type RunNowResponse struct {
	Created int               `json:"created"`
	RunIDs  []string          `json:"run_ids"`
	Matched int               `json:"matched"`
	Capped  bool              `json:"capped"`
	Started []LastFireStarted `json:"started"`
	Skips   []LastFireSkip    `json:"skips"`
}
