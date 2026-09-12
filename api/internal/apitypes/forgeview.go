package apitypes

import "time"

// forgeview.go holds the wire DTOs for the read-only forge views (PRD #1255 M2a):
// open pulls (list + drill-in), their checks/reviews/merge state, and CI runs (list +
// drill-in) with jobs and steps. Every string field is FORGE-AUTHORED UNTRUSTED text
// (titles, branch/check/job/step names, logins, descriptions, URLs — D7); the api
// never persists any of it (D4, read-through). Times marshal RFC3339; a *T field
// emits null when absent. These are exercised by the api⇄SPA contract test
// (contract_test.go) against recorded fixtures, mirrored by web/src/lib/apiTypes.ts.

// PullDTO is one open merge/pull request as the `pulls` list route returns it (PRD
// #1255 D4/D5) — the MergeRequestSummary wire form plus the newest uzi run that opened
// it. Conflicts is a pointer because "unknown" is a real third state (GitHub computes
// mergeability lazily). RunID is nil when no uzi run has this repo+mr_iid. The list
// route deliberately carries NO per-PR checks — check detail appears only on
// PullDetailDTO (D4: the cold list must not fan out a checks call per PR).
type PullDTO struct {
	IID            int64     `json:"iid"`
	Title          string    `json:"title"`
	Author         string    `json:"author"`
	SourceBranch   string    `json:"source_branch"`
	TargetBranch   string    `json:"target_branch"`
	HeadSHA        string    `json:"head_sha"`
	Draft          bool      `json:"draft"`
	Conflicts      *bool     `json:"conflicts"`
	ReviewDecision string    `json:"review_decision"`
	WebURL         string    `json:"web_url"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	Commits        int       `json:"commits"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// RunID is the newest uzi run opened against this repo+mr_iid (the `↳ run` link),
	// nil when none. It is a run UUID as a string, matching RunDTO.ID.
	RunID *string `json:"run_id"`
}

// CheckDTO is one status/check for a PR's head sha (PRD #1255 D5), the neutral
// forge.Check wire form. Status is the run phase (queued/in_progress/completed);
// Conclusion is meaningful once completed (success/failure/neutral/…/"" while
// running). Source is the reporting app's slug or the literal "status" for a
// commit-status surface. Name and Description are untrusted forge text.
type CheckDTO struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	Description string    `json:"description"`
	WebURL      string    `json:"web_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Source      string    `json:"source"`
}

// PullReviewDTO is one per-reviewer review on a PR (PRD #1255 D6): the reviewer login,
// the review's RAW forge state (approved/changes_requested/commented/dismissed/… —
// passed through verbatim, forge-specific vocabulary), and when it was submitted. Named
// PullReviewDTO (not ReviewDTO — that is the run-judge review in review.go). Author and
// State are untrusted forge text.
type PullReviewDTO struct {
	Author      string    `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// MergeStateDTO is a PR's merge readiness for the drill-in MERGE panel (PRD #1255 D3).
// Conflicts is the tri-state (nil == unknown). MergeableState is the raw coarse forge
// state and may be "" (the neutral list summary does not expose it in M2a).
// BlockedReason is DERIVED by the handler from conflicts / review decision / checks (in
// that priority), "" when nothing blocks. RequiredChecksPassed is best-effort — no
// failing and no pending check — until branch-protection "required" detail lands (run 2).
type MergeStateDTO struct {
	Conflicts            *bool  `json:"conflicts"`
	MergeableState       string `json:"mergeable_state"`
	BlockedReason        string `json:"blocked_reason"`
	RequiredChecksPassed bool   `json:"required_checks_passed"`
}

// PullDetailDTO is the PR drill-in (PRD #1255 D3): the PullDTO scalars plus the head
// sha's checks, the per-reviewer reviews, and the derived merge state. checks/reviews
// are non-nil only when the forge returned rows.
type PullDetailDTO struct {
	PullDTO
	Checks  []CheckDTO      `json:"checks"`
	Reviews []PullReviewDTO `json:"reviews"`
	Merge   MergeStateDTO   `json:"merge"`
}

// CIRunDTO is one CI run for the `ci` list route (PRD #1255 D5), the neutral
// forge.WorkflowRun wire form: a GitHub Actions workflow run, a GitLab pipeline, or a
// Forgejo Actions run. Status carries the run phase and Conclusion the terminal
// outcome where the forge splits them (GitHub); GitLab/Forgejo fold everything into
// Status and leave Conclusion "". JobsDone/JobsTotal are best-effort (0 unless the
// route filled them for a running row). Name, Title, Actor, Branch are untrusted text.
type CIRunDTO struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Number     int64     `json:"number"`
	Event      string    `json:"event"`
	Branch     string    `json:"branch"`
	SHA        string    `json:"sha"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	Title      string    `json:"title"`
	Actor      string    `json:"actor"`
	WebURL     string    `json:"web_url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	StartedAt  time.Time `json:"started_at"`
	JobsDone   int       `json:"jobs_done"`
	JobsTotal  int       `json:"jobs_total"`
}

// CIStepDTO is one step of a CI job (PRD #1255 D5), GitHub Actions only (GitLab/Forgejo
// jobs expose no steps). Number is the 1-based step index. Name is untrusted text.
type CIStepDTO struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	Number      int       `json:"number"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// CIJobDTO is one job of a CI run (PRD #1255 D5), the neutral forge.Job wire form with
// its steps. Steps is non-nil only on GitHub Actions. Name is untrusted text.
type CIJobDTO struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	WebURL     string      `json:"web_url"`
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at"`
	Steps      []CIStepDTO `json:"steps"`
}

// CIRunDetailDTO is the CI-run drill-in (PRD #1255 D3): the CIRunDTO scalars plus the
// run's jobs (with steps). Unsupported is a non-empty sentence only on the
// ErrForgeVersionUnsupported degrade path (a forge version without the Actions
// endpoint), where Jobs is empty and the TUI renders the sentence instead of an error.
type CIRunDetailDTO struct {
	CIRunDTO
	Jobs        []CIJobDTO `json:"jobs"`
	Unsupported string     `json:"unsupported"`
}
