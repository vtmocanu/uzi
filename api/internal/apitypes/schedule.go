package apitypes

import "time"

// ScheduleRequest is the create/patch input for a run schedule (PRD #241 M4).
//
// On CREATE, omitted pointer fields take their documented defaults (auto_approve=true
// per Decision 4, wait_on_limit=false, enabled=true). On PATCH the pointers carry
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
	Target      string     `json:"target"`
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
	Model     *string   `json:"model"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// NextFires is the live "next N fires" preview (up to 3), computed from the same
	// cron/next-fire logic the modal preview endpoint uses so the list and the modal
	// agree.
	NextFires []time.Time `json:"next_fires"`
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

// RunNowResponse reports the outcome of a manual run-now fire: how many runs were
// created and their ids (empty when a benign dedup skip fired nothing).
type RunNowResponse struct {
	Created int      `json:"created"`
	RunIDs  []string `json:"run_ids"`
}
