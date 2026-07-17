package apitypes

import (
	"encoding/json"
	"time"
)

// RunDTO is the web view of a run. session_id and last_seq are intentionally
// omitted — they are worker-internal (resume plumbing), not browser state.
type RunDTO struct {
	ID string `json:"id"`
	// RepoID is null for a chat run (PRD #39): a chat has no repo. Non-null for
	// issue/ci_fix runs.
	RepoID *string `json:"repo_id"`
	// Kind is issue|ci_fix|chat. IssueIID is null for ci_fix (no issue) and chat
	// runs; the ci_fix fields below carry pipeline context, chat carries Title.
	Kind             string `json:"kind"`
	IssueIID         *int64 `json:"issue_iid"`
	IssueTitle       string `json:"issue_title"`
	IssueDescription string `json:"issue_description"`
	// Title is the chat conversation's display title (PRD #39), null for
	// issue/ci_fix runs. ResumeOfRunID points a Continue chat at the ended chat it
	// resumes (Decision 11), null otherwise.
	Title          *string `json:"title"`
	ResumeOfRunID  *string `json:"resume_of_run_id"`
	Status         string  `json:"status"`
	RequeueCount   int32   `json:"requeue_count"`
	IterationCount int32   `json:"iteration_count"`
	AutoApprove    bool    `json:"auto_approve"`
	WorkerID       *string `json:"worker_id"`
	Branch         *string `json:"branch"`
	MrIID          *int64  `json:"mr_iid"`
	// MrState is the last merge-request state the PRD #24 watcher observed for
	// mr_iid (opened|closed|merged|locked), null when never observed. Display-only
	// and best-effort (PRD #33 Decision 1): the chip treats merged/closed distinctly
	// and everything else as the plain open chip. Frozen per run — a superseded
	// run's value can be stale, so freshness is scoped to the board card in the UI.
	MrState       *string `json:"mr_state"`
	FailureReason *string `json:"failure_reason"`
	// StopKind is the server-stamped deliberate-stop signal (PRD #33): "cancelled"
	// or "plan_rejected", null for every other run. It — not the failure_reason
	// text — is what the client's isStoppedRun reads to style a stop as calm/neutral.
	StopKind *string `json:"stop_kind"`
	// Run health (PRD #47). This DTO is owner-scoped (ListRuns owner-only,
	// AdminListRuns admin-only, GetRun owner/admin), so health_reason rides
	// unconditionally here, matching failure_reason — the owner-gating that the shared
	// board applies is unnecessary. Health is the flag enum; HealthSince (when it was
	// raised) drives the run-view "stuck for Xm".
	Health       string     `json:"health"`
	HealthReason *string    `json:"health_reason"`
	HealthSince  *time.Time `json:"health_since"`
	PlanMd       *string    `json:"plan_md"`
	// ci_fix context (PRD #6), all null for an issue run: the failing ref, the
	// failing pipeline's web URL (from the frozen snapshot), and the fix verdict
	// (verified|fix_failed|not_code|null-while-unverified).
	PipelineRef    *string    `json:"pipeline_ref"`
	PipelineWebURL *string    `json:"pipeline_web_url"`
	FixVerdict     *string    `json:"fix_verdict"`
	ClaimedAt      *time.Time `json:"claimed_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// Per-run agent selection (PRD #37). RepoAgents is the roster the worker
	// detected in the clone's .claude/agents/: null when no worker ever reported
	// (a pre-feature run), `[]` when detection ran and found none. The plan gate
	// distinguishes those two — an inert repo card vs. a live one — so do not
	// collapse them. AgentSource/AgentExclusions stay null until a selection is
	// made, at the gate or by an autopilot run's self-resolved default.
	//
	// The names and descriptions here are REPO-SUPPLIED text. The gate panel
	// renders them as plain JSX, never through <Markdown>: an attacker-authored
	// description must not put a clickable link inside an approval dialog.
	RepoAgents      []RepoAgent `json:"repo_agents"`
	AgentSource     *string     `json:"agent_source"`
	AgentExclusions []string    `json:"agent_exclusions"`
	// OwnAgents is the OWN-source subagent roster (name + description) the run's
	// owner would run: exactly what ListClaimAgentTemplates delivers to a claim,
	// minus the lead. It is the single source of truth the plan-gate "My agent
	// templates" picker reads, so an excludable chip always matches what the
	// approve validator accepts and the count is exact (PRD #37 M4-fix). Populated
	// only on the run-detail read (GetRun), where the store is in reach; null (a Go
	// nil slice) on the list/create/worker DTOs, which never drive the picker.
	OwnAgents []RepoAgent `json:"own_agents"`
	// Usage is the run's rolled-up token/cost totals (PRD #40), present only when the
	// run has usage rows — null for a pre-feature run so the UI shows nothing rather
	// than a fabricated 0. Populated on the list (ListRuns) and detail (GetRun) reads;
	// nil on the create/worker DTO paths, which never render usage.
	Usage *UsageDTO `json:"usage,omitempty"`
}

// RunListItemDTO is a run row for the Runs index and the admin Agents-status
// overview: the run plus display context (repo path, worker name) and, for the
// admin view, the owning user's email.
type RunListItemDTO struct {
	RunDTO
	RepoPath   string  `json:"repo_path"`
	WorkerName *string `json:"worker_name"`
	OwnerEmail *string `json:"owner_email,omitempty"`
}

// MessageDTO is one persisted run message (the replay source a reconnecting
// browser reads before going live). Payload is the raw per-kind JSON, forwarded
// verbatim.
type MessageDTO struct {
	Seq       int32           `json:"seq"`
	Kind      string          `json:"kind"`
	Agent     *string         `json:"agent"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

// RunInputRequest is the POST /api/runs/{id}/inputs body: a steering input
// (approve/reject/follow-up/cancel).
type RunInputRequest struct {
	Kind string `json:"kind"`
	Body string `json:"body"`
	// Selection is the PRD #37 agent choice, sent STRUCTURED and legal only with
	// approve_plan. The client never composes the worker-bound input body itself:
	// the server validates this against the run's real roster and writes its own
	// canonical JSON encoding into that body.
	Selection *AgentSelection `json:"selection"`
}

// RunInputResponse is the POST /api/runs/{id}/inputs reply: server_side reports
// whether the input was applied server-side (cancel/plan-rejection with no live
// poller) rather than queued for the worker.
type RunInputResponse struct {
	ServerSide bool `json:"server_side"`
}
