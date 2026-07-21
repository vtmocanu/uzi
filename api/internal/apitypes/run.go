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
	// ForgeType is the run's forge ("gitlab"|"forgejo"), so the web picks the
	// per-run MR/PR noun and reference sigil (PRD #65 D2). "" on the worker/create
	// DTO paths, which never render the MR affordance in a browser; set on the
	// list/detail reads (ListRuns/AdminListRuns/GetRun) from the run's connection.
	ForgeType string `json:"forge_type"`
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
	// MrWebURL is the forge-supplied MR/PR web URL persisted by the worker at MR
	// creation (PRD #65 D8), null on runs created before it landed. The web renders
	// it directly through isHttpsUrl and only falls back to the legacy GitLab URL
	// reconstruction for those null rows — it is the only correct link on Forgejo.
	MrWebURL *string `json:"mr_web_url"`
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
	// Judge badge (PRD #98 M4, Decision 7). JudgeVerdict is the run's review verdict
	// (ideal|ok|issues), nil when the run was never judged — rendered as NO badge, never
	// as a neutral one, since "unjudged" and "judged fine" are different facts.
	//
	// JudgeTodoCount is the run's still-to-triage recommendation count, bucketed in Go
	// through the shared BucketOf (never a SQL ladder — #94 Decision 2). It is 0 both for
	// an unjudged run and for a fully-triaged one; the row appends it to the badge only
	// when > 0, so the single grammar is "verdict, optionally a count" (⚖ issues · 2,
	// ⚖ ideal) rather than two competing grammars.
	//
	// The two travel together but come from DIFFERENT mechanisms on purpose: the verdict
	// via a safe UNIQUE join in the list query, the count via a separate bounded fetch
	// bucketed in Go. See queries/runtime.sql for why the count cannot ride the join.
	JudgeVerdict   *string `json:"judge_verdict"`
	JudgeTodoCount int     `json:"judge_todo_count"`
}

// MessageDTO is one persisted run message (the replay source a reconnecting
// browser reads before going live). Payload is the raw per-kind JSON, forwarded
// verbatim.
type MessageDTO struct {
	Seq   int32   `json:"seq"`
	Kind  string  `json:"kind"`
	Agent *string `json:"agent"`
	// AgentInstance is the subagent INVOCATION id (the SDK's per-frame
	// parent_tool_use_id, PRD #99) and AgentLabel the task description that
	// invocation was given. Both null when the frame carried no
	// parent_tool_use_id — the orchestrator's own turns, infra frames, and every
	// pre-migration message. That is NOT the same as Agent == "lead": a repo may
	// ship an agent NAMED lead, which is a real subagent and does carry an id.
	// Consumers fall back to Agent.
	AgentInstance *string         `json:"agent_instance"`
	AgentLabel    *string         `json:"agent_label"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     time.Time       `json:"created_at"`
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
// poller) rather than queued for the worker. ID + CreatedAt are the created steer
// row (PRD #95 S2), present ONLY on a follow_up write so the web's optimistic queue
// entry can adopt the real id + timestamp; omitted (nil) for approve/cancel/reject,
// which surface no queue row.
type RunInputResponse struct {
	ServerSide bool       `json:"server_side"`
	ID         *int64     `json:"id,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
}

// SteerInputDTO is one follow_up steer-queue entry (PRD #95), served by
// GET /api/runs/{id}/inputs (owner-only): the message body plus its delivery status.
// ConsumedAt NULL ⇒ Queued (the worker has not drained it), set ⇒ Delivered (the
// worker consumed it for its next turn). Body is a pointer for the JSON-null vs value
// convention, though a follow_up always carries one. This is a DISTINCT struct from
// the worker-facing workersvc.InputDTO, which has no consumed_at.
type SteerInputDTO struct {
	ID         int64      `json:"id"`
	Body       *string    `json:"body"`
	CreatedAt  time.Time  `json:"created_at"`
	ConsumedAt *time.Time `json:"consumed_at"`
}
