package apitypes

import (
	"encoding/json"
	"time"
)

// Milestone is one entry of a run's milestone list (PRD #122): a stable machine id
// and a human-readable title. The id is a strict key (validated server-side at the
// worker state-report chokepoint); the title is UNTRUSTED display text a consumer
// writing it to a terminal must sanitize, the same obligation RepoAgent.Description
// carries.
//
// The definition lives here so the run DTO that embeds it stays a stdlib-only leaf
// (PRD #64 M1). workersvc.Milestone is a type alias of this, keeping the workersvc
// validators and every reference compiling unchanged.
type Milestone struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// RunSummaryDelta is one entry of a run's plan-summary deltas list (PRD #362): how
// the proposed plan diverged from the original ask. Kind is a closed enum
// {added, changed, dropped} (validated-and-rejected on persist, Decision 6); Text is
// UNTRUSTED, model-authored display text a consumer writing it to a terminal must
// sanitize — the deltas are advisory and the human still reads the real plan.
type RunSummaryDelta struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// RunDTO is the web view of a run. session_id and last_seq are intentionally
// omitted — they are worker-internal (resume plumbing), not browser state.
type RunDTO struct {
	ID string `json:"id"`
	// RepoID is null for a chat run (PRD #39): a chat has no repo. Non-null for
	// issue/ci_fix runs.
	RepoID *string `json:"repo_id"`
	// ForgeType is the run's forge ("gitlab"|"forgejo"|"github"), so the web picks the
	// per-run MR/PR noun and reference sigil (PRD #65 D2). "" on the worker/create
	// DTO paths, which never render the MR affordance in a browser; set on the
	// list/detail reads (ListRuns/AdminListRuns/GetRun) from the run's connection.
	ForgeType string `json:"forge_type"`
	// Kind is issue|ci_fix|chat|judge|self_improve|prompt|task|mr_rework. IssueIID is
	// null for the issue-less kinds (ci_fix, chat, judge, prompt, task, mr_rework) and
	// set for the issue-shaped kinds (issue, self_improve); the ci_fix fields below carry
	// pipeline context, chat carries Title, and a task (uzi handoff) carries
	// Branch/BaseBranch/OpenMr set at create.
	Kind             string `json:"kind"`
	IssueIID         *int64 `json:"issue_iid"`
	IssueTitle       string `json:"issue_title"`
	IssueDescription string `json:"issue_description"`
	// HasPRDLink is server-computed PRD presence for the runs view (PRD #764),
	// derived label-independently from the run's snapshotted issue description via
	// the same detector the board card uses (forgesvc.HasPRDLink). False for
	// issue-less run kinds (chat/ci_fix/etc.) whose description carries no prds link.
	HasPRDLink bool `json:"has_prd_link"`
	// Title is the chat conversation's display title (PRD #39), null for
	// issue/ci_fix runs. ResumeOfRunID points a Continue chat at the ended chat it
	// resumes (Decision 11), null otherwise.
	Title          *string `json:"title"`
	ResumeOfRunID  *string `json:"resume_of_run_id"`
	Status         string  `json:"status"`
	RequeueCount   int32   `json:"requeue_count"`
	IterationCount int32   `json:"iteration_count"`
	// IsPlanning is a server-computed display predicate (issue #321): true only while a
	// run is in its pre-approval PLANNING turn (status running, iteration_count 0, no
	// persisted plan yet; chat/judge excluded). Derived, not stored — no new column or
	// status value. A pre-feature api pod omits it; absent reads as not-planning.
	IsPlanning  bool `json:"is_planning"`
	AutoApprove bool `json:"auto_approve"`
	// TriggerSource records what/how/who started the run (issue #857): one of
	// manual, autopilot, schedule, self_improve, ci_fix, mr_rework, chat, task,
	// task_review, then_fix, judge, judge_rerun, resume. Always set (NOT NULL column,
	// DEFAULT 'manual'); historical rows carry a best-effort backfilled value.
	TriggerSource string `json:"trigger_source"`
	// Milestones is the run's FROZEN, human-approved milestone list (PRD #122 M1),
	// decoded from runs.milestones_frozen. A nil slice marshals to JSON null, which is
	// the back-compat contract: a run with no milestones (every pre-feature run, and
	// every run that never proposed a list) reads null and the web keeps rendering
	// today's plain badge. Populated only where the store is in reach (the run reads);
	// nil on the create/worker DTO paths. The pre-approval CANDIDATE list is never
	// exposed here — only the frozen list is served.
	Milestones []Milestone `json:"milestones"`
	// MilestonesCandidate is the run's PRE-APPROVAL candidate milestone list (PRD #122 M3),
	// decoded from runs.milestones_candidate. It is read-only and additive — the same
	// {id,title} objects as Milestones, but the breakdown the worker PROPOSED, before a
	// human approved it. A nil slice marshals to JSON null; the web renders it ONLY at the
	// plan gate (status awaiting_approval), so the human approves the proposed breakdown,
	// and ignores it everywhere else — a frozen run reads its approved list off Milestones.
	// The titles are UNTRUSTED display text a consumer writing them to a terminal must
	// sanitize, the same obligation Milestones and RepoAgent.Description carry. Populated
	// wherever the store row is decoded through the shared runToDTO — the browser run reads
	// AND the worker's own awaiting_approval report echo (which serialises the candidate the
	// worker just submitted back to it); nil on the create DTO path, which has no store row.
	MilestonesCandidate []Milestone `json:"milestones_candidate"`
	// MilestonesCompleted and MilestonesInProgress are the run's live progress (PRD #122
	// M2), decoded from runs.milestones_completed / _in_progress: arrays of frozen
	// milestone IDS (not {id,title} objects — the titles live on Milestones above; these
	// reference into it). completed is the server-unioned monotone done set; in_progress
	// the latest-reported snapshot. A nil slice marshals to JSON null — the back-compat
	// contract, so a run whose lead never reported progress reads null and the web derives
	// nothing. Populated only on the run reads (where the store is in reach); nil on the
	// create/worker DTO paths.
	MilestonesCompleted  []string `json:"milestones_completed"`
	MilestonesInProgress []string `json:"milestones_in_progress"`
	// BudgetMaxIterations and BudgetWallSeconds are the run's EFFECTIVE budget (PRD #122
	// M2, Decision 5/5b), derived server-side from the frozen milestone count at freeze
	// and persisted. Each is null when the run is on the GLOBAL default (a 0/1-milestone
	// run, byte-for-byte today). This is load-bearing on the state-report ack, not just
	// display: the worker reads these off the {run: RunDTO} response to learn its scaled
	// turn ceiling and wall clock mid-run.
	BudgetMaxIterations *int `json:"budget_max_iterations"`
	BudgetWallSeconds   *int `json:"budget_wall_seconds"`
	// ScopeCeiling is the operator scope ceiling (PRD #634 M2): the count of milestones the
	// run may complete over the immutable frozen list; NULL = unbounded. Like the budget
	// fields it rides the running-report ACK and the claim payload (both built by runToDTO),
	// so the worker honors it at the loop top and across a re-claim.
	ScopeCeiling *int    `json:"scope_ceiling"`
	WorkerID     *string `json:"worker_id"`
	Branch       *string `json:"branch"`
	// BaseBranch and OpenMr are the task/handoff columns (PRD #400), meaningful only
	// for a kind='task' run. BaseBranch is the source ref the task branched from (null
	// when it inherited the caller's local HEAD, and on every non-task run); OpenMr is
	// whether the worker opens an MR at the end (false by default and for every
	// non-task run — a plain handoff produces commits on the branch, not an MR).
	// For a handoff created without --base (issue #403 F3), BaseBranch is the resolved
	// SEED COMMIT sha (the local HEAD the CLI pushed as the branch tip), which the
	// auto-review uses as its diff base so it covers only the worker's commits, not the
	// user's own seeded HEAD.
	BaseBranch *string `json:"base_branch"`
	OpenMr     bool    `json:"open_mr"`
	// Interactive marks a long-lived, conversational task run (PRD #517 M1): the worker
	// keeps it alive (parking in awaiting_followup) after signal_done rather than
	// terminating. Set at create from --interactive; false by default and for every
	// non-task run. Always on the wire, like OpenMr above.
	Interactive bool `json:"interactive"`
	// DispatchedAt is when the CLI stamped a task run's dispatch gate (PRD #400
	// Decision 6) — the moment it became claimable, after its uzi/task/<id> branch was
	// seeded. Null on every non-task run and on a task run not yet dispatched. Mapped
	// like ClaimedAt (a nullable timestamp), read-only.
	DispatchedAt *time.Time `json:"dispatched_at"`
	MrIID        *int64     `json:"mr_iid"`
	// MrWebURL is the forge-supplied MR/PR web URL persisted by the worker at MR
	// creation (PRD #65 D8), null on runs created before it landed. The web renders
	// it directly through isHttpsUrl and only falls back to the legacy GitLab URL
	// reconstruction for those null rows — it is the only correct link on Forgejo.
	MrWebURL *string `json:"mr_web_url"`
	// BranchHasActiveRun and BranchHasOpenMr are server-computed `uzi handoff rm` preconditions
	// (issue #403 F1/F6), stamped ONLY on the owner/admin GetRun detail read for a kind='task'
	// run — false on every non-task run, on the list/create/worker DTO paths, and on a
	// pre-feature api pod. A handoff's original task, its auto-review and its --then-fix fix run
	// all share one uzi/task/<id> branch, so the CLI keys `rm` on these BRANCH-wide facts, not
	// the passed run's own row: BranchHasActiveRun is true while ANY run on the branch is
	// non-terminal (rm would race a live push); BranchHasOpenMr is true when the branch's owning
	// task opened an MR (rm exempt — the MR needs its source branch). Always on the wire (bool).
	BranchHasActiveRun bool `json:"branch_has_active_run"`
	BranchHasOpenMr    bool `json:"branch_has_open_mr"`
	// IssueWebURL is the forge-supplied issue web URL (PRD #411), nil for issue-less
	// runs or when the issue is no longer cached; rendered through isHttpsUrl on the web.
	IssueWebURL *string `json:"issue_web_url"`
	// MrState is the last merge-request state the PRD #24 watcher observed for
	// mr_iid (opened|closed|merged|locked), null when never observed. Display-only
	// and best-effort (PRD #33 Decision 1): the chip treats merged/closed distinctly
	// and everything else as the plain open chip. Frozen per run — a superseded
	// run's value can be stale, so freshness is scoped to the board card in the UI.
	MrState       *string `json:"mr_state"`
	FailureReason *string `json:"failure_reason"`
	// StopKind is the server-stamped stop signal (PRD #33, widened by #108 M5 and #517 M4):
	// "cancelled" or "plan_rejected" for a deliberate HUMAN stop, "auto_stopped" when
	// the SERVER stopped a run whose updates could not be saved, "stopped" for a graceful
	// `uzi run stop` of an interactive task run, null for every other run. It — not the
	// failure_reason text — is what clients read.
	//
	// A "stopped" run's happy path lands `completed` (the worker finalizes — push + MR iff
	// open_mr — and reports completed); on the edge where that finalize throws (or a
	// cancel-then-stop let the cancel win) the worker reports `failed` and the server routes
	// it to `cancelled`, never `agent_failure` and never judged. So a "stopped" stop_kind
	// rides a `completed` or a `cancelled` run, not a `failed` one.
	//
	// Consumers must NOT treat the three alike, and the web's isStoppedRun is the
	// worked example: it styles the two human kinds calm/neutral because a deliberate
	// stop is not breakage, and deliberately leaves "auto_stopped" looking like the
	// breakage it is. `uzi run get` renders it as its own STOP_KIND row for the same
	// reason — on the live-poller half the worker overwrites failure_reason with its
	// own "run cancelled", so this field is the ONLY thing that distinguishes an
	// auto-stop from a user cancel.
	StopKind *string `json:"stop_kind"`
	// StopReason is the operator's OPTIONAL free-text cancel reason (PRD #503 M3),
	// captured beside StopKind on the cancel paths and surfaced here on the read path
	// (issue #525). Null when no reason was given. Free text — treated like FailureReason,
	// not like the StopKind enum.
	StopReason *string `json:"stop_reason"`
	// Run health (PRD #47). This DTO is owner-scoped (ListRuns owner-only,
	// AdminListRuns admin-only, GetRun owner/admin), so health_reason rides
	// unconditionally here, matching failure_reason — the owner-gating that the shared
	// board applies is unnecessary. Health is the flag enum; HealthSince (when it was
	// raised) drives the run-view "stuck for Xm".
	Health       string     `json:"health"`
	HealthReason *string    `json:"health_reason"`
	HealthSince  *time.Time `json:"health_since"`
	PlanMd       *string    `json:"plan_md"`
	// PlanSource is where plan_md came from (PRD #209): "agent" for a normal run whose
	// worker wrote the plan at the gate, "seeded" for a run created WITH a user-authored
	// plan that skips planning + the approval gate. NOT NULL DEFAULT 'agent' in the DB
	// (store.Run.PlanSource is a plain string), so it is always on the wire and never a
	// pointer — a pre-feature run reads "agent". The SPA's SeededPlanPanel keys on it to
	// surface a seeded run's plan, which the approval UI would otherwise never render.
	PlanSource string `json:"plan_source"`
	// Plain-English run summaries (PRD #362), all null until the worker generates and
	// posts them (and null forever on any generation failure — summaries are advisory
	// and never block a run). SummaryIntent ("what this run will implement") lands early
	// in `running`; SummaryPlan ("what the proposed plan will do") + SummaryDeltas (how
	// the plan diverged from the ask) land at the plan gate. SummaryDeltas is
	// tolerated-with-fallback on READ (Decision 6): a malformed stored value renders as
	// nil ("no deltas"), never a crash — runToDTO logs and drops it. A nil slice
	// marshals to JSON null, the back-compat contract for every pre-feature run. The
	// delta Text is UNTRUSTED display text (see RunSummaryDelta).
	SummaryIntent *string           `json:"summary_intent"`
	SummaryPlan   *string           `json:"summary_plan"`
	SummaryDeltas []RunSummaryDelta `json:"summary_deltas"`
	// ci_fix context (PRD #6), all null for an issue run: the failing ref, the
	// failing pipeline's web URL (from the frozen snapshot), and the fix verdict
	// (verified|fix_failed|not_code|null-while-unverified).
	PipelineRef    *string `json:"pipeline_ref"`
	PipelineWebURL *string `json:"pipeline_web_url"`
	FixVerdict     *string `json:"fix_verdict"`
	// ReportOnly marks a completed run that intentionally opened NO merge request
	// (issue #279): its deliverable was a report/command-output/verification result,
	// not a code change. False for a normal MR completion and every pre-feature run
	// (the column is NOT NULL DEFAULT false). ReportMd is the lead's persisted findings
	// summary, already scrubbed server-side; nil unless report_only.
	ReportOnly bool    `json:"report_only"`
	ReportMd   *string `json:"report_md"`
	// PreservedPatch is the agent's diff preserved on a workflow_scope_missing `failed`
	// run (PRD #377 M1): the branch touched .github/workflows/** the bot PAT cannot push,
	// so the work is surfaced here for a human to land instead of being discarded. Null on
	// every other run (the column is nullable, set only on that failed path).
	PreservedPatch *string `json:"preserved_patch"`
	// PrdDonePath is the repo-relative path the run declared it moved a PRD to when it
	// archived a completed PRD (e.g. prds/done/72-x.md), null for a run that moved none.
	// Read-only surfacing of the runs.prd_done_path column so the issue's PRD link can be
	// reconciled after the run's merge request lands.
	PrdDonePath *string `json:"prd_done_path"`
	// PrdPatchSettledAt is when the PRD-link patch lifecycle settled (patched, no-match,
	// MR closed unmerged, or superseded); null while the patch is still pending — which,
	// with prd_done_path set, is the "declared a move, not yet reconciled" state.
	PrdPatchSettledAt *time.Time `json:"prd_patch_settled_at"`
	ClaimedAt         *time.Time `json:"claimed_at"`
	StartedAt         *time.Time `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
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
	// Anthropic usage-limit park (PRD #35). A run that exhausts the owner's
	// subscription window is parked at status "limit_wait" rather than failed, and
	// resumes once the window reopens.
	//
	// WaitOnLimit is the run's opt-in, resolved at creation from the owner's default
	// or an explicit override. It is on the DTO rather than inferred from the status
	// because it is meaningful BEFORE any park — it is what a "will retry on limit"
	// affordance renders, and what a per-run toggle reads back.
	WaitOnLimit bool `json:"wait_on_limit"`
	// MrReworkEnabled is the run's per-run MR-rework override (PRD #841 M1), tri-state:
	// null = inherit the owner default, true/false = explicit override. Unlike
	// WaitOnLimit (a resolved bool snapshotted at creation), this is nullable and read
	// LIVE — the candidate query resolves COALESCE(run, owner) IS NOT FALSE — so null on
	// the wire means "no per-run opinion", which the web renders as the effective
	// inherited value. It is what the per-run checkbox / `uzi run mr-rework` read back.
	MrReworkEnabled *bool `json:"mr_rework_enabled"`
	// LimitResetsAt is when the exhausted window reopens, as REPORTED by the worker
	// off the SDK frame. RetryNotBefore is when the server will actually promote the
	// run back to queued.
	//
	// The two are separate fields because they are separate facts and they routinely
	// differ: RetryNotBefore carries jitter, is bounded by RUN_LIMIT_MAX_PARK, is
	// cross-checked against the owner's own rate-limit gauge, and is pool-aware — a
	// user with a second credential that still has headroom is promoted early, so
	// RetryNotBefore can be far EARLIER than LimitResetsAt. Render the countdown off
	// RetryNotBefore (that is when work resumes) and LimitResetsAt only as context.
	// Both null for a run that has never parked.
	//
	// "Bounded by", not "clamped to", and the distinction is user-visible: PRD #35
	// Decision 4 says a computed stamp beyond RUN_LIMIT_MAX_PARK FAILS the run with a
	// clear reason rather than being pulled back to the ceiling. So a client will never
	// see a RetryNotBefore sitting exactly at now+RUN_LIMIT_MAX_PARK as the result of a
	// clamp — that run is `failed`, not parked. (Corrected 2026-07-27: this comment
	// said "clamped", which would have a client render a wait that never happens.)
	LimitResetsAt  *time.Time `json:"limit_resets_at"`
	RetryNotBefore *time.Time `json:"retry_not_before"`
	// LimitWaitCount is how many times this run has parked, capped server-side by
	// RUN_LIMIT_MAX_WAITS. 0 for a run that has never parked.
	//
	// The CAP is deliberately NOT on this DTO: it is one server constant, and
	// repeating it on every row of a list response is the wrong shape. Render
	// "attempt N"; if the denominator is wanted, it belongs on /api/me.
	LimitWaitCount int32 `json:"limit_wait_count"`
	// RateLimitType is which window rejected the run ("five_hour", "seven_day", …),
	// already allowlisted against the SDK union server-side with anything
	// unrecognised coerced to "unknown" — so it is a safe enum by the time it is
	// here, never worker free text. Null for a run that has never parked.
	//
	// Clients must still render an unrecognised value honestly rather than dropping
	// it: the vocabulary is the SDK's and a newer server can ship a member this
	// client has not heard of. Same rule as AnthropicSelectReason above.
	RateLimitType *string `json:"rate_limit_type"`
	// Model is the model frozen onto the run at fire time by the schedule that created it
	// (PRD #300): nil means the run inherited the owner's per-user Worker default. Surfaced
	// read-only so a scheduled run's model is confirmable.
	Model *string `json:"model"`
	// OverrideSubagentModel is the "apply model also to agents" flag frozen onto the run
	// at fire time by the schedule that created it (PRD #305): true means the run's model
	// was applied to every subagent (overriding pins), false is today's default. Surfaced
	// read-only so a scheduled run's fleet-wide model choice is confirmable.
	OverrideSubagentModel bool `json:"override_subagent_model"`
	// Priority is the run's DISPLAY priority class (PRD #320 D8), a non-empty enum in
	// {normal, background, expedited, restored}, computed by the ONE SQL function
	// fn_run_priority_class from the same demotion predicate ClaimRun's ORDER BY ranks
	// by — so the pill and the claim order can never disagree. NOT omitempty: it is
	// always a real value (never ""), so a client reads it unconditionally. `background`
	// is a demoted judge/self_improve run still yielding; `restored` the same run past
	// RUN_BACKGROUND_GRACE (fails open to normal rank); `expedited` a manual bump;
	// `normal` everything else. runToDTO takes it as an explicit param and stays pure —
	// no now()/config reaches into the mapper (D8).
	Priority string `json:"priority"`
	// The run's inferred/hinted scheduling requirements (PRD #84): RequiredCapabilities is
	// the claim-gating capability set (M2 repo hint UNION-merged with M4 plan-time
	// inference), RequiredTools the DISPLAY-ONLY provisionable toolchain families (M4 4b),
	// and SizeClass the clamped s/m/l estimate (M4 4b). All three are surfaced RAW so the
	// web/CLI (4d) derive the readiness/mismatch display from them plus the worker caps they
	// already fetch — there is deliberately no server-computed "capability_block" field; the
	// authoritative enforcement is the 409 the approval gate returns. Capability/tool slices
	// are non-nil ([] over null) via capsOrEmpty; SizeClass is "" for a run whose plan-time
	// inference never set it (the column is NOT NULL DEFAULT '').
	RequiredCapabilities []string `json:"required_capabilities"`
	RequiredTools        []string `json:"required_tools"`
	SizeClass            string   `json:"size_class"`
	// PlanChangedFiles is the git-status list surfaced at the approval gate (PRD #212).
	// [] (never omitempty) like required_tools: a pre-#212 run's NULL column and a clean
	// plan turn both map to []; renderers show the section only when non-empty.
	PlanChangedFiles []string `json:"plan_changed_files"`
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
	// IsRevising is a server-computed display predicate (issue #750): true when the run's
	// latest plan-ish message ({plan, plan_revising}) is a plan_revising — i.e. a "revise"
	// replan is in flight. Like IsPlanning it is derived, not stored (no new column or
	// status value), and meaningful only while status == "awaiting_approval": a plan
	// revise leaves runs.status at awaiting_approval, so surfaces classifying from status
	// alone need this to keep the run out of the human-attention grouping. It is
	// deliberately NOT on RunDTO (the detail page keeps its own derivePlanRevision panel).
	// A pre-feature api pod omits it; clients treat absent as false.
	IsRevising bool `json:"is_revising"`
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
	// OverrideCapabilities is the PRD #84 M4 4c user override ("run without the
	// capability", Decision 12), meaningful ONLY with approve_plan and default false. When
	// true the server clears the run's inferred/hinted required_capabilities before
	// approving, so a plan the capability gate would otherwise 409-BLOCK (its owning worker
	// cannot satisfy an inferred requirement) is approved anyway — the deliberate
	// false-positive-inference correction. No runtime security boundary is bypassed: the
	// §300 guardrail still denies docker USE on a daemon-less worker at run time.
	OverrideCapabilities bool `json:"override_capabilities"`
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

// SteerInputDTO is one steer-queue entry (PRD #95, #634), served by
// GET /api/runs/{id}/inputs (owner-only): the message body plus its delivery status.
// The queue now spans two kinds. For a Kind=="follow_up" row, state is derived from
// ConsumedAt (NULL ⇒ Queued, the worker has not drained it; set ⇒ Delivered, the
// worker consumed it for its next turn). For a Kind=="scope" operator scope directive
// (PRD #634), state is the Disposition — applied/declined/superseded — because a scope
// row is never consumed (it is excluded from ConsumeRunInputs, so its ConsumedAt is
// always NULL); a nil Disposition means the ceiling is still pending on a live run.
// Body is a pointer for the JSON-null vs value convention, though both kinds carry one.
// This is a DISTINCT struct from the worker-facing workersvc.InputDTO, which has no
// consumed_at.
type SteerInputDTO struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Body        *string    `json:"body"`
	CreatedAt   time.Time  `json:"created_at"`
	ConsumedAt  *time.Time `json:"consumed_at"`
	Disposition *string    `json:"disposition"`
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
	// CHECK constraint (runs.status, created by 00020_workers_runs.sql, widened with
	// 'limit_wait' by 00091_run_limit_wait.sql, with 'awaiting_input' by
	// 00092_run_awaiting_input.sql, and with 'awaiting_followup' by
	// 00146_interactive_task_runs.sql): queued, claimed, running, awaiting_approval,
	// limit_wait, awaiting_input, awaiting_followup, completed, failed, cancelled —
	// TEN values. It is the field that decides whether a run reads as still live, so
	// an unrecognised value must never reach a consumer as-is.
	Status string `json:"status,omitempty"` // set on "state" frames
}
