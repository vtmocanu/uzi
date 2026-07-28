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
	// StopKind is the server-stamped stop signal (PRD #33, widened by #108 M5):
	// "cancelled" or "plan_rejected" for a deliberate HUMAN stop, "auto_stopped" when
	// the SERVER stopped a run whose updates could not be saved, null for every other
	// run. It — not the failure_reason text — is what clients read.
	//
	// Consumers must NOT treat the three alike, and the web's isStoppedRun is the
	// worked example: it styles the two human kinds calm/neutral because a deliberate
	// stop is not breakage, and deliberately leaves "auto_stopped" looking like the
	// breakage it is. `uzi run get` renders it as its own STOP_KIND row for the same
	// reason — on the live-poller half the worker overwrites failure_reason with its
	// own "run cancelled", so this field is the ONLY thing that distinguishes an
	// auto-stop from a user cancel.
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
	// Which Anthropic credential this run's claim actually spent (PRD #111 M1) —
	// the answer to "which account paid for this run?", which run_usage alone could
	// never give. Both null for a run claimed before the feature landed, and for a
	// run that has not been claimed yet.
	//
	// The two are NOT redundant and they go null independently. The label is a
	// SNAPSHOT taken at claim time, so it survives the token being renamed or
	// deleted; the id is live and goes null (ON DELETE SET NULL, migration 00086)
	// when the token is deleted. So `id == null, label != null` is the normal state
	// of a historical run whose credential is gone — render the label, and treat the
	// id as a link target only when present.
	//
	// The label is USER-SUPPLIED text, so any consumer writing it to a terminal must
	// sanitize; the CLI routes it through cellText and the web through
	// lib/sanitizeLabel for exactly this.
	//
	// This used to say "validateSecretLabel permits Unicode Cf, including bidi
	// overrides". PRD #111 M2 made that false: the validator now rejects Cf on write.
	// The obligation is unchanged, because the validator governs writes and not
	// history — labels stored before it landed are never re-validated, and nothing
	// re-validates on read.
	//
	// This DTO is owner-or-admin scoped throughout (ListRuns owner-only,
	// AdminListRuns admin-only, GetRun owner-or-admin), which is why the label rides
	// unconditionally here as failure_reason does. The SHARED board is a different
	// struct with a different rule — a token label names another user's billing
	// account, so it must not reach latestRunDTO without that struct's IsMine gate.
	AnthropicSecretID    *string `json:"anthropic_secret_id"`
	AnthropicSecretLabel *string `json:"anthropic_secret_label"`
	// AnthropicSelectReason is the MODE that named that credential, and it is why
	// the label alone was never enough (PRD #111 M5, D20). An auto pick and a default
	// fallback can name the SAME token, so "which account paid" and "why that account"
	// are different questions — and PRD #104's compatibility path creates a row
	// labelled literally `default`, so the label is not even a reliable hint.
	//
	// One of eight server-generated values (autoselect.Reason, closed by migration
	// 00089's CHECK): default, pinned, judge, auto, best_of_pool, pool_empty,
	// pool_stale, open_failed. Null for a run claimed before M1.
	//
	// A CLOSED SERVER ENUM, not free text: it describes the OWNER'S OWN configuration
	// and can carry no cross-tenant content, which is why it rides this DTO under the
	// owner-or-admin scoping already in force rather than needing a gate of its own.
	//
	// Clients must render an UNRECOGNISED value honestly rather than dropping or
	// guessing at it — the API is deployed separately, so a newer server can ship a
	// ninth reason this client has never heard of.
	AnthropicSelectReason *string `json:"anthropic_select_reason"`
	// AnthropicHeadroomPct is the measured headroom of an AUTO pick, in percentage
	// points, and null on every other lane because nothing measured them.
	//
	// The RAW min(100-five, 100-seven) — deliberately not the in-flight-penalised
	// rank the selector actually ordered on. The raw number is the one a user can see
	// in their own meters; the rank is an internal ordering key that appears nowhere
	// else in the product and moves when somebody else's run starts.
	//
	// It is also null on D14's retry, where the pick would not open and the fallback
	// was spent: the reading described the credential that FAILED, and attaching it to
	// the one that succeeded would attribute a measurement to a token nothing measured.
	//
	// Derived from the owner's own gauge rows, so it carries no cross-tenant content
	// either; an admin reading it is consistent with the admin rate-limits view.
	AnthropicHeadroomPct *int `json:"anthropic_headroom_pct"`
	// Usage is the run's rolled-up token/cost totals (PRD #40), present only when the
	// run has usage rows — null for a pre-feature run so the UI shows nothing rather
	// than a fabricated 0. Populated on the list (ListRuns) and detail (GetRun) reads;
	// nil on the create/worker DTO paths, which never render usage.
	//
	// Since PRD #111 M1 it can finally be read TOGETHER with the two fields above:
	// what a run cost, and which credential it cost it against.
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

// RunEventDTO is one /api/ws frame: the live-channel counterpart to the REST
// reads above, shared by the web, the hub that emits it, and the uzi CLI.
//
// It lives here rather than in internal/hub because M1 (PRD #112) moved /api/ws
// onto the RequireUser routes, which is this package's stated membership rule —
// and because a parallel decode struct in the CLI is the failure this package
// exists to prevent: nothing would catch tag-set drift between two definitions of
// one live wire contract. hub.Event is an ALIAS of this type, so there is exactly
// one definition and the server cannot emit a shape the client does not decode.
//
// The socket is a live channel only. Every frame it carries was already persisted
// (messages) or applied (state) before the hub was poked, so a dropped or missed
// frame is recovered by the client's REST replay — this is never the source of
// truth. See uzicli.StreamRun for the recovery contract that depends on that.
type RunEventDTO struct {
	// Type is a CLOSED set: message | state | health | input. "message" carries a
	// persisted run message (rendered directly, deduped by seq); the other three are
	// signals to re-read over REST, since the socket never carries authoritative run
	// state. A consumer must treat an unrecognised Type as inert — see
	// uzicli.NormalizeRunEvent.
	Type string `json:"type"`
	// Seq is set on "message" frames ONLY. state/health/input carry none, which is
	// why seq-gap detection cannot recover a dropped one of those.
	Seq  int32  `json:"seq,omitempty"`
	Kind string `json:"kind,omitempty"`
	// Agent is the emitting agent's name; Kind the message kind. Both are OPEN sets
	// written by the separately-deployed worker, so a value this binary does not
	// recognise is expected and must be preserved, not normalised away.
	Agent *string `json:"agent,omitempty"`
	// AgentInstance/AgentLabel are the PRD #99 subagent invocation id + task
	// label. The browser lanes a live frame off these without a REST re-read, so
	// they must ride the frame exactly as Agent does. Absent when the frame
	// carried no parent_tool_use_id (which is NOT the same as Agent == "lead").
	AgentInstance *string         `json:"agent_instance,omitempty"`
	AgentLabel    *string         `json:"agent_label,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CreatedAt     *time.Time      `json:"created_at,omitempty"`
	// Status is set on "state" frames and is a CLOSED set enforced by a database
	// CHECK constraint (runs.status, created by 00020_workers_runs.sql and widened by
	// 00091_run_awaiting_input.sql): queued, claimed, running, awaiting_approval,
	// awaiting_input, completed, failed, cancelled. It is the field that decides
	// whether a run reads as still live, so an unrecognised value must never reach a
	// consumer as-is.
	Status string `json:"status,omitempty"` // set on "state" frames
}
