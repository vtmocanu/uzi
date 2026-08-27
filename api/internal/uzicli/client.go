package uzicli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Client is the read surface the CLI's command stubs depend on. Commands take a
// Client, never a concrete HTTP client, so tests inject FakeClient and the live
// HTTPClient below serves the real requests. Every method returns the stdlib-only
// apitypes DTOs directly (PRD #64 M1/M7): the CLI decodes the exact shapes the
// handlers serialize, without importing internal/handler (which would drag pgx +
// chi into the binary — the go list -deps layering assertion).
type Client interface {
	Whoami(ctx context.Context) (apitypes.UserDTO, error)
	ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error)
	GetRun(ctx context.Context, id string) (apitypes.RunDTO, error)
	// RunLogs returns a run's persisted messages after seq (0 = from the start);
	// `uzi run logs` polls it with the highest seq seen (Decision: REST polling, not
	// a WebSocket — PRD #64 Out of scope). The live client pages the fetch internally
	// (issue #160), transparently to callers, and is ALL-OR-NOTHING: it returns the
	// complete history or an error, never a partial slice that looks complete.
	RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error)
	// RunReview returns the judge's review for a run, or nil when the run is visible
	// but unjudged (the endpoint answers 200 {"review":null}). A run that is absent
	// or not visible is a real 404 → *ExitError{ExitNotFound}. The two cases are
	// deliberately distinct: nil is exit 0 "not judged", 404 is exit 4.
	//
	// The SECOND return is the active judge run for this target (PRD #119), and it
	// exists because `review == nil` was answering two different questions with one
	// word. A nil review means either "no judge has ever run on this" or "a judge is
	// already in flight and a verdict is seconds away" — auto-judge enqueues one at
	// the terminal transition, so the second case is the COMMON one right after a run
	// finishes. The CLI printed "not judged" for both, which told a user a review was
	// missing at the exact moment one was being written. A non-nil pending judge
	// carries the server-normalized state ("scheduled" | "running") and its enqueue
	// time; it is orthogonal to the review and can accompany a non-nil one too (a
	// re-judge in flight over an existing verdict).
	RunReview(ctx context.Context, id string) (*apitypes.ReviewDTO, *apitypes.PendingJudgeDTO, error)
	// RunInputs returns a run's follow_up steer queue (newest-first) with delivery
	// status (PRD #95). Owner-only server-side (GetRunByIDForUser): a non-owner —
	// including an admin_ro token on another user's run — gets a 404 →
	// *ExitError{ExitNotFound}, never another user's steer text.
	RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error)
	// StreamRun subscribes to one run's live events over /api/ws (PRD #112 M2),
	// authenticating with the same Bearer CLI token the REST reads use (M1 opened
	// that route to it). The returned stream reconnects, replays what a drop
	// swallowed, and periodically re-reads authoritative run state — see RunStream
	// for why the last of those is required rather than defensive.
	//
	// It is on this INTERFACE, not only on *HTTPClient, so a consumer can take a
	// Client and still be testable: FakeClient implements it too.
	StreamRun(ctx context.Context, runID string) (*RunStream, error)
	ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error)
	// ListSecrets returns the caller's Anthropic tokens as metadata (labels, ids,
	// default flags — never values): GET /api/me/secrets, RequireUser (PRD #104 D8,
	// a list is safe from a CLI token; creating/rotating/deleting is web-only). Each
	// entry's value appears nowhere — there is no reveal endpoint.
	ListSecrets(ctx context.Context) ([]apitypes.SecretDTO, error)
	// SetTokenAutoEligible opts one of the caller's Anthropic tokens into or out of
	// the auto-selection pool (PRD #111 M2, D2): PATCH
	// /api/me/secrets/anthropic_token/{id}/auto-eligible {auto_eligible}.
	//
	// It takes an ID. The label→id resolution happens in the COMMAND, client-side —
	// deliberately NOT the shape SetWorkerBindMode uses, which sends the label for the
	// server to resolve. An earlier version of this comment claimed they were the
	// same; see cmd/uzi/token.go for the two ways they differ (Go vs Postgres case
	// folding, and a missing kind filter) and why this side is accepted for now.
	//
	// This is the ONE secrets write a CLI token can reach, and its own narrow route
	// is why (D13). Every other secrets write is cookie-only, because minting or
	// replacing a credential from a stolen uzc_ is the exposure PRD #104 D8 closes;
	// this one mints nothing and reveals nothing, it only re-points which of the
	// caller's OWN tokens the pool may spend. An unknown label is a usage error
	// resolved client-side (exit 3); an unknown id is a 404 (exit 4).
	SetTokenAutoEligible(ctx context.Context, id string, eligible bool) (apitypes.SecretDTO, error)
	// SetRunPriority expedites (expedite=true → priority bumped to the front) or clears
	// (expedite=false → manual override removed, back to the kind default) ONE queued run:
	// PATCH /api/runs/{id}/priority (PRD #320 D6/D7), RequireUser so a CLI token can reach
	// it. A non-queued run is a 409 → ExitConflict (5); a foreign/absent run is 404 → 4.
	SetRunPriority(ctx context.Context, id string, expedite bool) (apitypes.RunDTO, error)
	// SelfRateLimits returns the caller's OWN per-token rate-limit meters, each
	// carrying the server-computed auto-selection status: GET /api/me/rate-limits.
	//
	// RequireUser since PRD #111 D23. Before that this was cookie-only, which left
	// `uzi token pool <label> --on` a silent no-op from a script's point of view: it
	// could opt a token in and had no way to learn the token could never be picked
	// (no gauge reading, or one that aged out) — R7's hazard, reintroduced on the CLI
	// half. The status string is computed server-side by autoselect.Classify and is
	// RENDERED here, never re-derived (D21).
	SelfRateLimits(ctx context.Context) ([]apitypes.TokenRateLimitDTO, error)
	// GetMySettings returns the caller's own non-secret settings, including
	// sidebar_token_ids: GET /api/me/settings. RequireUser (the GET was split out
	// from the cookie-only /me/settings group so a uzc_ can read it).
	GetMySettings(ctx context.Context) (apitypes.UserSettingsDTO, error)
	ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error)
	// DeleteRepo removes a single repo: DELETE /api/repos/{id}, 204 on success
	// (PRD #357 M3). The server only removes a DISABLED repo and refuses one that
	// is enabled or has an in-flight run with 409 → ExitConflict; a foreign/absent
	// id is 404 → ExitNotFound. It is destructive, so the `uzi repo remove` command
	// gates it behind --force / a confirm prompt.
	DeleteRepo(ctx context.Context, id string) error
	// BuildInfo reads the server's build coordinates from the UNAUTHENTICATED
	// GET /api/version (PRD #175 M4).
	//
	// It goes through newRequest, so credentialSafeBase gates it, and that is
	// REQUIRED rather than merely consistent. The ROUTE needs no credential; the
	// REQUEST carries one anyway, because newRequest attaches
	// `Authorization: Bearer <token>` to every request whose client holds a token
	// and does not special-case this one. So skipping the https-or-loopback guard
	// here — on the reasonable-sounding grounds that the endpoint is public —
	// would put a real uzc_ token in cleartext on the first
	// `uzi version --url http://…`. Measured, not inferred: a loopback server sees
	// `Authorization: Bearer uzc_…` on this call.
	//
	// Pinned by TestBuildInfoIsGatedByCredentialSafeBase, whose third arm asserts
	// the token IS attached — the fact the guard's necessity rests on.
	BuildInfo(ctx context.Context) (apitypes.BuildInfoDTO, error)
	AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error)
	AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error)
	AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error)
	AdminListCLITokens(ctx context.Context) ([]apitypes.AdminCLITokenDTO, error)
	AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error)
	AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error)
	// GuardrailImpact reads the live, non-persisting guardrail pre-flight impact
	// count (PRD #66 M3): GET /api/admin/guardrail-impact.
	GuardrailImpact(ctx context.Context) (apitypes.GuardrailImpactDTO, error)
	// AdminBlockedRepos reads the admin cross-user blocked-repos list from the
	// stored privilege report (PRD #66 M9): GET /api/admin/blocked-repos.
	AdminBlockedRepos(ctx context.Context) (apitypes.AdminBlockedReposDTO, error)
	// AdminAgentSource reads the agent-source config + sync status + staged
	// snapshot (PRD #602 M6): GET /api/admin/agent-source. READ-ONLY — the sync/
	// apply writes stay web-only (cookie-only), so the CLI never triggers a fetch.
	AdminAgentSource(ctx context.Context) (apitypes.AgentSourceDTO, error)

	// StartCLIAuth begins a browser-brokered login: POST /api/auth/cli/start with the
	// PKCE S256 challenge and a client description. UNAUTH by design (the CLI has no
	// credential yet). Returns the request_id, the display user_code, the request
	// lifetime, and the poll cadence to honour.
	StartCLIAuth(ctx context.Context, challenge, clientDesc string) (CLIAuthStartResult, error)
	// PollCLIAuth polls POST /api/auth/cli/poll {request_id, verifier} once and
	// classifies the reply: 202 pending, 200 authorized (Token/User set, once), or 410
	// terminal (Reason set). POST not GET so the verifier never lands in an access log.
	PollCLIAuth(ctx context.Context, requestID, verifier string) (CLIAuthPollResult, error)
	// CreateRun queues an agent run on a repo's PRD issue: POST /api/repos/{id}/runs
	// {issue_iid, wait_on_limit?, plan_md?, agent_selection?}. Returns the created run.
	//
	// waitOnLimit is the PRD #35 usage-limit opt-in and is TRI-STATE, which is why it
	// is a pointer rather than a bool: nil OMITS the key, and the server then stamps
	// the run from the owner's own Settings default. A non-nil false is a different
	// statement — "this run, explicitly, must not park" — and overrides that default.
	// Collapsing the two would make every CLI-created run silently opt OUT the moment
	// this parameter shipped, which is the exact defect the server's own field comment
	// cites for taking a *bool.
	//
	// seed is PRD #209's optional seeded plan: nil OMITS both plan_md and
	// agent_selection (an ordinary run planned from the issue, byte-identical to
	// before), and a non-nil seed always carries a plan (the server rejects a
	// selection with no plan). The plan's size cap and empty-plan rejection are the
	// SERVER's (422) — the client forwards the bytes so those rules live in one place.
	CreateRun(ctx context.Context, repoID string, issueIID int64, waitOnLimit *bool, seed *CreateRunSeed) (apitypes.RunDTO, error)
	// CreateTaskRun queues an issue-less handoff/task run on a repo (PRD #400 M3):
	// POST /api/repos/{id}/task-runs {context, base_branch?, open_mr}. The server
	// names the branch (uzi/task/<run-id>) and the created-run response carries it in
	// Branch — which is what the CLI then pushes local HEAD to. context is the inline
	// instruction (reused as issue_description, so its 256 KiB cap + sanitization are
	// the server's); baseBranch is the source ref to branch from, OMITTED when empty
	// so the worker branches from the caller's seeded HEAD; openMR asks the worker to
	// open an MR at the end (a branch exempt from `uzi handoff rm`); reviewRequested
	// (--review) asks that a diff-review run be auto-created when the task completes,
	// producing structured findings fetched via GetTaskReview; thenFixRequested
	// (--then-fix) asks that, after that review, a chained fix run push fixes for its
	// findings to the same branch (--then-fix implies --review); interactive
	// (--interactive) asks the worker to keep the run alive after signal_done (parking in
	// awaiting_followup to iterate) rather than terminating.
	CreateTaskRun(ctx context.Context, repoID, context, baseBranch string, openMR, reviewRequested, thenFixRequested, interactive bool) (apitypes.RunDTO, error)
	// GetTaskReview fetches a handoff task's diff-review (PRD #400 M4a): GET
	// /api/runs/{id}/task-review, whose envelope is {"task_review": <dto>|null}. A visible
	// task with no review yet returns a nil DTO (the CLI prints "no review available yet");
	// a run the caller can't see is a 404 (exit 4). id is the TARGET (task) run id.
	GetTaskReview(ctx context.Context, id string) (*apitypes.TaskReviewDTO, error)
	// DispatchTaskRun stamps a task run's dispatch gate (PRD #400 Decision 6): POST
	// /api/runs/{id}/dispatch, empty body. The CLI calls it LAST — after the run is
	// created and local HEAD has been pushed to uzi/task/<id> — which is the moment
	// the worker may claim the run. Owner-scoped server-side: a foreign/absent run, a
	// non-task run, or an already-dispatched one is a 404 (exit 4).
	DispatchTaskRun(ctx context.Context, runID string) (apitypes.RunDTO, error)
	// SubmitRunInput submits a steering input: POST /api/runs/{id}/inputs
	// {kind, body, selection}. kind ∈ {approve_plan, reject_plan, cancel, follow_up}.
	// sel is legal only with approve_plan; the server validates it against the run's
	// real roster (the client never composes the worker-bound body itself).
	SubmitRunInput(ctx context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error)
	// DeleteWorker removes one of the caller's workers: DELETE /api/workers/{id}
	// (204 No Content on success). A worker with active runs is a 409 (exit 5); an
	// unknown/foreign id is a 404 (exit 4). Minting a worker stays a webui action —
	// there is no create counterpart here.
	DeleteWorker(ctx context.Context, id string) error
	// SetWorkerBindMode sets HOW a worker chooses its Anthropic credential:
	// PATCH /api/workers/{id} {anthropic_bind_mode, anthropic_token}.
	//
	//   - "pinned" + a LABEL (the name a human knows, not a secret id) → that token;
	//   - "default" → the caller's default token;
	//   - "auto"    → the selector picks from the caller's opted-in pool per claim.
	//
	// Unlike minting a worker this yields no credential the caller lacks, so it is
	// RequireUser and reachable from a CLI token (PRD #104 D8). An unknown label or
	// an illegal mode is a 400 (exit 3); an unknown/foreign worker is a 404 (exit 4).
	// The change lands on the worker's next claim — no restart, no re-minted join
	// token.
	SetWorkerBindMode(ctx context.Context, id, mode, label string) (apitypes.WorkerDTO, error)
	// ListMemory returns the caller's agent memory across all repos (PRD #90):
	// GET /api/me/memory. Each entry carries its repo + provenance so the owner can
	// see what a future run would read back.
	ListMemory(ctx context.Context) ([]apitypes.AgentMemoryDTO, error)
	// DeleteMemory purges one of the caller's memory entries: DELETE
	// /api/me/memory/{id} (204 on success). An unknown/foreign id is a 404 (exit 4).
	DeleteMemory(ctx context.Context, id string) error
	// SetDisposition records a triage verdict on a judge recommendation (PRD #94
	// Decision 10): PUT /api/runs/{id}/review/recommendations/{recID}/disposition
	// {status, reason?}. status ∈ {done, dismissed}; reason (∈ {wont_do,
	// not_an_issue}) is sent only for a dismissal and omitted otherwise. Owner-only
	// on RequireUser (Decision 5): a uza_ read-only token mutating another user's
	// review gets a 404 (exit 4). 204 → nil.
	SetDisposition(ctx context.Context, runID, recID, status, reason string) error
	// DeleteDisposition undoes a recommendation's disposition (PRD #94 Decision 6):
	// DELETE the same path. A 404 means no disposition existed — returned as the
	// sentinel ErrNoDisposition (a plain error, NOT an *ExitError) so the command
	// can soften it to "already undone" and exit 0. Every other failure propagates
	// as an *ExitError with the documented exit code.
	DeleteDisposition(ctx context.Context, runID, recID string) error
	// JudgeStats returns the caller's all-time triage totals across every run (PRD
	// #94 Decision 8): GET /api/me/judge/stats. Owner-scoped; the reply is an
	// unenveloped TriageDTO.
	JudgeStats(ctx context.Context) (apitypes.TriageDTO, error)
	// JudgeBacklog returns the caller's all-time recommendation backlog deduped by
	// (category, target) (PRD #98 M1): GET /api/me/judge/recommendations. Owner-scoped
	// on RequireUser, read-only, no token spend; the reply is an unenveloped
	// JudgeBacklogDTO carrying the groups, the canonical triage tally and `truncated`.
	//
	// bucket is passed through VERBATIM and an empty bucket OMITS the parameter, so the
	// SERVER's default (todo) applies. The valid set is deliberately not restated on this
	// side: the CLI never compares against a bucket value, it only forwards one, so there
	// is no predicate to fail silently — an unknown bucket comes back as the server's own
	// 400 ("invalid bucket") and maps to the usage exit code. Restating the enum here
	// would add a second definition that could drift from the enforcing one; forwarding
	// keeps the validator the only place the set is written down.
	// runAnchor is the ?run= coordinate semi-join (PRD #98 Decision 1). It is the ONLY
	// predicate pushed into SQL before the row cap — bucket filtering happens in Go on the
	// already-cut rows — so it is the only parameter that can narrow a pull below
	// JudgeBacklogMaxRows and thereby answer a `truncated` warning. Empty omits it.
	// category is the ?category= recommendation-label filter (PRD #235 M3), forwarded
	// VERBATIM as the server's comma-separated `?category=a,b` wire form and validated
	// server-side: the CLI holds no category predicate, so an unknown label comes back as
	// the server's own 400 → the usage exit code, never a silently empty list. Empty omits
	// the parameter, so the server's "all labels" default applies. Restating the enum on
	// this side would add a second definition that could drift from the enforcing one, so
	// (like bucket) the value is only forwarded, never compared.
	JudgeBacklog(ctx context.Context, bucket, runAnchor, category string) (apitypes.JudgeBacklogDTO, error)
	// BulkSetDispositions applies one triage verdict to every member coordinate of the
	// given groups (PRD #98 M2, Decision 3): PUT
	// /api/me/judge/recommendations/disposition. status ∈ {done, dismissed}; reason
	// (∈ {wont_do, not_an_issue}) rides only a dismissal.
	//
	// The request's `scope` is left at its zero value, which the server reads as the
	// default open scope — settle what is open, never re-assert a settled member. The CLI
	// exposes no scope choice, so it names no scope: sending "" is what makes the server's
	// default the single definition of that behaviour.
	//
	// There is NO 404 on this route and none should be expected. It is owner-only by
	// construction (the service resolves members under `user_id = caller`), and
	// coordinates are not ids, so a coordinate that does not exist and one belonging to
	// another user are BOTH a 200 with Updated == 0 — #94 Decision 5's no-existence-oracle
	// rule. A caller learning "0 written" learns only that none of THEIR rows matched, and
	// must render that as "nothing was written" rather than as success.
	BulkSetDispositions(ctx context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error)

	// ListSchedules returns the caller's run schedules, newest first (PRD #241 M6):
	// GET /api/me/schedules. Owner-scoped on RequireUser, so a CLI token works. The
	// reply is a BARE top-level array of ScheduleDTO (no envelope), unlike the run
	// verbs above.
	ListSchedules(ctx context.Context) ([]apitypes.ScheduleDTO, error)
	// CreateSchedule creates a schedule on a repo the caller owns: POST
	// /api/repos/{id}/schedules (201 ScheduleDTO). repoID is passed straight through
	// as the path id, exactly as CreateRun does with --repo. The per-target/timing
	// field invariants are validated SERVER-side (400/422); the CLI validates only the
	// one-of constraints client-side so a bad invocation fails before any request.
	CreateSchedule(ctx context.Context, repoID string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error)
	// GetSchedule returns one owner-scoped schedule: GET /api/schedules/{id}. A
	// foreign/absent id is a 404 → *ExitError{ExitNotFound}.
	GetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error)
	// SetScheduleEnabled pauses (false) or resumes (true) a schedule: PATCH
	// /api/schedules/{id} with the minimal body {enabled}. Sending ONLY `enabled` hits
	// the handler's pause/resume path (onlyEnabled), which flips the flag without
	// re-validating or recomputing the schedule's config.
	SetScheduleEnabled(ctx context.Context, id string, enabled bool) (apitypes.ScheduleDTO, error)
	// PatchSchedule edits a schedule's mutable config: PATCH /api/schedules/{id} with a
	// full ScheduleRequest (200 ScheduleDTO). Unlike SetScheduleEnabled's minimal body,
	// this restates the whole config, so the caller MUST seed it from the fetched schedule
	// (the server's mergeSchedule takes max_issues/guidance straight from the request —
	// nil clears them — while keeping the rest on empty). Enabled is left nil here so the
	// pause flag is untouched; a config PATCH re-activates a terminal/parked schedule.
	PatchSchedule(ctx context.Context, id string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error)
	// DeleteSchedule removes an owner-scoped schedule: DELETE /api/schedules/{id} (204).
	// A foreign/absent id is a 404 (exit 4). Run history is preserved server-side (the
	// runs.schedule_id FK is ON DELETE SET NULL), so this deletes the schedule only.
	DeleteSchedule(ctx context.Context, id string) error
	// RunScheduleNow fires a schedule immediately through the shared run-creation seam,
	// WITHOUT advancing its cadence: POST /api/schedules/{id}/run-now (202
	// RunNowResponse). A benign dedup skip fires nothing and reports Created:0.
	RunScheduleNow(ctx context.Context, id string) (apitypes.RunNowResponse, error)
	// ListScheduleCatalog returns the builtin default-schedule catalog plus the caller's
	// per-repo enablement state (PRD #589 M3): GET /api/schedule-catalog. Owner-scoped on
	// RequireUser, so a CLI token works; the reply is the unenveloped ScheduleCatalogResponse.
	ListScheduleCatalog(ctx context.Context) (apitypes.ScheduleCatalogResponse, error)
	// EnableCatalogSchedule enables one builtin default job on a repo the caller owns (PRD
	// #589 M3): POST /api/repos/{id}/schedule-catalog/{slug}. Idempotent per (user, repo,
	// slug) server-side — a repeat enable returns the existing row rather than a duplicate.
	// The bool reports whether the row was CREATED (HTTP 201) versus already-enabled (200),
	// so the CLI can render a per-repo created/already-enabled result. This is the per-repo
	// call the CLI's multi-repo `catalog enable` fans out over CLIENT-side (one call per
	// --repo), so a partial retry is safe.
	EnableCatalogSchedule(ctx context.Context, repoID, slug string) (apitypes.ScheduleDTO, bool, error)
	// ResetSchedule restores a default-origin schedule's editable fields to the builtin
	// catalog defaults and clears its customized flag (PRD #589 M3): POST
	// /api/schedules/{id}/reset. A user-origin row is a 409 (nothing to reset to).
	ResetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error)
	// CloneSchedule copies a schedule into a new fully-editable user-origin row (PRD #589
	// M3): POST /api/schedules/{id}/clone. An empty repoID clones into the source's own
	// repo; a non-empty one clones into that (owned) repo — the replication path. Cloning a
	// default schedule lifts the catalog prompt lock (the server bakes the resolved
	// prompt/labels/guidance into the new row).
	CloneSchedule(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error)
	// AddScheduleRepo replicates a user-origin schedule's current config onto another repo
	// the caller owns as a new grouped sibling (PRD #636 M4, Decision 5): POST
	// /api/schedules/{id}/add-repo with body {"repo_id"}. The server, in one transaction,
	// ensures the source carries a display-only sibling_group_id (allocated race-safely) and
	// stamps the returned group id on the new sibling row, so source + new render as one
	// group. Owner-scoped (a foreign source or target repo is a 404 → ExitNotFound); a repo
	// that already holds a sibling in the group conflicts on the unique index → 409
	// (ExitConflict), which the CLI treats as an idempotent no-op. Returns the new sibling DTO.
	AddScheduleRepo(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error)

	// CheckRepoLabels reports which of the given selector labels do NOT exist on a repo the
	// caller owns (PRD #589 M4): POST /api/repos/{id}/labels/check. It is the sweep-label
	// WARN primitive — the caller resolves the selector (a sweep default's catalog labels,
	// or a custom sweep's form labels) and gets back the missing subset so it can warn and
	// offer to create them. Owner-scoped + forge-limited server-side; returns the missing
	// labels (empty = all present).
	CheckRepoLabels(ctx context.Context, repoID string, labels []string) ([]string, error)
	// EnsureRepoLabels creates any of the given labels absent on a repo the caller owns (PRD
	// #589 M4): POST /api/repos/{id}/labels/ensure. It is the sweep-label CONFIRM primitive,
	// paired with CheckRepoLabels — idempotent by name server-side (an existing label is
	// left untouched). Owner-scoped + forge-limited.
	EnsureRepoLabels(ctx context.Context, repoID string, labels []string) error

	// ListFindings returns the caller's owner-scoped incidental-findings backlog, deduped
	// by (repo, location) across every run a coordinate recurs in (PRD #333 M6): GET
	// /api/findings. Owner-scoped on RequireUser (so `uzi findings list` works from a CLI
	// token), read-only, no forge write and no token spend. The reply is the unenveloped
	// IncidentalFindingBacklogDTO carrying the coordinate rows and the open-findings count.
	//
	// bucket/repo/run are forwarded VERBATIM and an empty value OMITS the parameter, so the
	// SERVER's default applies (to_file for bucket, no filter for repo/run). The valid set is
	// deliberately not restated here: the CLI never compares against a value, it only forwards
	// one, so there is no predicate to fail silently — an unknown bucket comes back as the
	// server's own 400 → the usage exit code, and a well-formed but foreign/unknown repo or run
	// is an EMPTY list (no existence oracle), never a 404.
	ListFindings(ctx context.Context, bucket, repo, run string) (apitypes.IncidentalFindingBacklogDTO, error)
	// FileFinding files a forge issue from one finding coordinate (PRD #333 M6): POST
	// /api/findings/{id}/issue with an empty body, so the server files with the STORED,
	// already-sanitised template and the server-mandated marker label (the CLI is not the rich
	// editor — the web is, D4/D5). Returns the created forge issue (iid/web_url) and any
	// created-with-warning note. An unknown/foreign id is a 404 (exit 4); an already-filed or
	// mid-filing coordinate is a 409 (exit 5) — both come straight from statusError.
	FileFinding(ctx context.Context, id string) (apitypes.IncidentalFindingFileResultDTO, error)
	// DismissFinding triages one finding coordinate to `dismissed` with a reason (PRD #333 M6):
	// POST /api/findings/{id}/dismiss {reason}. reason is the wire enum (wont_do|not_an_issue),
	// mapped from the hyphenated flag and validated by the COMMAND before this is reached. An
	// unknown/foreign id is a 404 (exit 4); a non-open coordinate (filed/filing/already
	// dismissed) is a 409 (exit 5). 200 → nil.
	DismissFinding(ctx context.Context, id, reason string) error
	// GetReviewIssueDraft fetches the server-templated issue draft for one judge
	// recommendation (PRD #365 M2): GET
	// /api/runs/{runID}/review/recommendations/{recID}/issue-draft. Owner-or-admin to READ
	// the recommendation, reachable from a CLI token on RequireUser. The reply is the
	// {"draft": IssueDraftDTO} envelope; this returns the unwrapped draft carrying the
	// default repo, title, description and picker note. A foreign/unknown run or rec is a
	// 404 (exit 4).
	GetReviewIssueDraft(ctx context.Context, runID, recID string) (apitypes.IssueDraftDTO, error)
	// FileReviewIssue files a forge issue from one judge recommendation (PRD #365 M2): POST
	// /api/runs/{runID}/review/recommendations/{recID}/issue with {repo_id,title,description}
	// (all three required — unlike FileFinding's empty body, the CLI sends the draft defaults
	// it just fetched). Owner-or-admin to READ the recommendation, caller-owns-repo to WRITE.
	// Returns the created forge issue (iid/web_url/title) and any created-with-warning note.
	// A foreign/unknown run or rec is a 404 (exit 4); an already-filed or mid-filing
	// recommendation is a 409 (exit 5) — both come straight from statusError.
	FileReviewIssue(ctx context.Context, runID, recID, repoID, title, description string) (ReviewIssueFileResult, error)

	// GetProjectSyncStatus reads a repo's GitHub Projects v2 sync health (PRD #576
	// M7): GET /api/repos/{id}/github-project-sync. RequireUser since M7 moved the
	// route out of the cookie-only group, so a uzc_ Bearer reaches it (issue #428
	// class). Owner-or-admin server-side (GetRepoForUser preflight) — a
	// foreign/unknown repo is a 404 → *ExitError{ExitNotFound}, which the command
	// softens to "not linked" (a repo with NO link row is the SAME 404, "not sync
	// enabled"): both are the not-linked case for a read the CLI treats as normal
	// output, never an error.
	GetProjectSyncStatus(ctx context.Context, repoID string) (ProjectSyncStatus, error)
	// ResyncProjectSync re-seeds an already-linked board, picking up newly-added
	// Status options (PRD #576 M7): POST /api/repos/{id}/github-project-sync/resync.
	// RequireUser (moved with the status read) so the CLI Bearer is accepted. A
	// foreign/unknown repo, or one with no link row, is a 404 (exit 4) straight from
	// statusError.
	ResyncProjectSync(ctx context.Context, repoID string) error
}

// ProjectSyncStatus mirrors the handler's getGithubProjectSyncStatusResponse JSON
// (PRD #576 M7): a repo's project link health snapshot. LastSyncedAt/LastError are
// pointers so a never-synced or healthy link decodes them as nil rather than a zero
// value; UnmatchedColumns is always a JSON array (never null) server-side, so `uzi
// project-sync status` can range it unconditionally.
type ProjectSyncStatus struct {
	ProjectNumber    int64      `json:"project_number"`
	OwnedByUzi       bool       `json:"owned_by_uzi"`
	LastSyncedAt     *time.Time `json:"last_synced_at"`
	LastError        *string    `json:"last_error"`
	ItemCount        int        `json:"item_count"`
	UnmatchedColumns []string   `json:"unmatched_columns"`
	NoDoneOption     bool       `json:"no_done_option"`
}

// ErrNoDisposition is returned by DeleteDisposition when the recommendation had
// no disposition to undo (the endpoint answers 404). It is a plain error, NOT an
// *ExitError, so `uzi review undo` can treat it as "already undone" (a friendly
// message, exit 0) instead of a hard not-found failure.
var ErrNoDisposition = errors.New("no disposition to undo")

// maxRespBytes caps how much of a response body the client reads, so a broken or
// hostile endpoint cannot make the CLI allocate without bound. 32 MiB is far above
// any real run-detail or message-history payload.
const maxRespBytes = 32 << 20

// logsPageSize bounds each /messages request RunLogs makes, so a large run's
// history is fetched in fixed-size pages instead of one unbounded response that
// could die mid-body (issue #160). Paging is transparent to callers: RunLogs
// reassembles the full slice before returning, so the Client interface and every
// caller see the same all-or-nothing "complete history or an error" contract.
const logsPageSize = 200

// maxLogsMessages is a hostile/compromised-server backstop for RunLogs: the loop
// stops on an EMPTY page, so a server that always returns a full page with
// strictly-increasing seqs never terminates and would grow the accumulator until
// OOM. This caps the aggregate — the per-page 32 MiB LimitReader and 30s timeout
// already bound each request, this bounds the whole fetch. It is sized ~1000x above
// any real run (the largest observed in issue #160 was 887 messages), so it never
// truncates a legitimate history — only a misbehaving server hits it. A var, not a
// const, purely so the backstop test can lower it and restore it in a defer.
var maxLogsMessages = 1_000_000

// HTTPClient is the live API client. It talks only to leaf packages (apitypes +
// net/http + stdlib), keeping cmd/uzi off the server stack.
type HTTPClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	// StreamRun tuning, zero = the package defaults. Unexported so they are not part
	// of the CLI's configuration surface: they exist so the stream tests can drive
	// the reconnect/reconcile timing deterministically instead of sleeping for real
	// seconds, which is the difference between pinning the recovery contract and
	// hoping for it.
	streamReconcile   time.Duration
	streamBackoffBase time.Duration
	streamBackoffMax  time.Duration
}

// NewHTTPClient builds the live client from resolved settings.
func NewHTTPClient(s Settings) *HTTPClient {
	return &HTTPClient{
		BaseURL: s.URL,
		Token:   s.Token,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			// Refuse every redirect. The https guard in newRequest only vets the
			// INITIAL URL; without this, Go's default policy would follow a 3xx
			// and replay `Authorization: Bearer <uzc_/uza_>` across a same-host
			// scheme downgrade (https→http), handing the token to a cleartext
			// endpoint. A legitimate uzi /api/* endpoint never redirects, so a
			// redirect is refused (surfaces as a clean transport failure, exit 6),
			// never followed — mirroring the worker's redirect:"error" posture.
			CheckRedirect: refuseRedirect,
		},
	}
}

// refuseRedirect is the CheckRedirect policy for the live client: it rejects any
// redirect hop rather than let net/http forward the bearer token to the redirect
// target. Returning a non-nil error makes Client.Do fail (and, per net/http, the
// intermediate response body is already closed) instead of following the hop.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing to follow redirect to %s: uzi API endpoints never redirect", req.URL.Redacted())
}

var _ Client = (*HTTPClient)(nil)

// newRequest builds a request to base+path with an optional JSON body, rejecting a
// non-https base URL before any credential is attached. This is a credential-leak
// guard, not a nicety: --url / $UZI_URL / config flow verbatim into BaseURL, and
// every authenticated method attaches `Authorization: Bearer <uzc_/uza_>` — so a
// plaintext or attacker-controlled URL would leak the token in the clear (and the
// login flow's PKCE verifier rides these requests too). https everywhere, with an
// http exception ONLY for the loopback compose stack (127.0.0.1/localhost). Mirrors
// the server's https-only FORGE_ALLOWED_BASE_URLS.
func (c *HTTPClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	u, err := c.credentialSafeBase()
	if err != nil {
		return nil, err
	}
	full := strings.TrimRight(u.String(), "/") + path
	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, Exitf(ExitGeneric, "build request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

// credentialSafeBase parses and vets BaseURL, returning it only when it is safe to
// attach a credential to. It is the SINGLE gate for that decision: newRequest uses
// it for every REST call and StreamRun uses it for the WebSocket dial (PRD #112 M2),
// because the socket carries the same `Authorization: Bearer <uzc_/uza_>` header and
// a second copy of this check is a second thing that can drift. A transport added
// later must call this before it sends the token, not re-derive the rule.
func (c *HTTPClient) credentialSafeBase() (*url.URL, error) {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		return nil, Exitf(ExitUsage, "no uzi API URL configured: pass --url or set $UZI_URL")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return nil, Exitf(ExitUsage, "invalid uzi API URL %q", base)
	}
	if u.Scheme != "https" && !isLoopbackURL(u) {
		return nil, Exitf(ExitUsage,
			"refusing to send credentials to %q: only https (or http on 127.0.0.1/localhost) is allowed — a plaintext URL leaks your token",
			base)
	}
	return u, nil
}

// isLoopbackURL reports whether the URL's host is loopback (127.0.0.0/8, ::1) or
// the literal "localhost" — the only hosts the http scheme is permitted for.
func isLoopbackURL(u *url.URL) bool {
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// doJSONRead performs one request with an optional JSON body and returns the
// response plus its (capped) body, already drained and closed. Every transport
// failure is returned as an *ExitError (ExitUnreachable): a raw error leaking to
// main would be misclassified as a usage error (see ExitCodeFor). The caller owns
// HTTP-status classification, since some callers (the poll loop) branch on
// non-error statuses like 202/410 that others treat as failures.
func (c *HTTPClient) doJSONRead(ctx context.Context, method, path string, reqBody any) (*http.Response, []byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, nil, Exitf(ExitGeneric, "encode request: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Any error from Do is a transport failure (dial refused, DNS, TLS,
		// timeout, context deadline): the server is effectively unreachable.
		return nil, nil, Exitf(ExitUnreachable, "cannot reach uzi at %s: %v", c.BaseURL, transportMsg(err))
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, nil, Exitf(ExitUnreachable, "reading response from uzi: %v", err)
	}
	return resp, respBody, nil
}

// get executes a GET and, on a 2xx, decodes the body into out (out may be nil to
// discard). Every failure — transport, HTTP status, or decode — is returned as an
// *ExitError carrying the right process exit code.
func (c *HTTPClient) get(ctx context.Context, path string, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// postJSON executes a POST with a JSON body and, on a 2xx, decodes the reply into
// out (out may be nil). Non-2xx maps to the documented exit code via statusError.
func (c *HTTPClient) postJSON(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// put executes a PUT with a JSON body and, on a 2xx, decodes the reply into out
// (out may be nil — the disposition endpoint answers 204). Non-2xx maps to the
// documented exit code via statusError. Mirrors postJSON with http.MethodPut.
func (c *HTTPClient) put(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPut, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// patch executes a PATCH with a JSON body and, on a 2xx, decodes the reply into
// out. Mirrors put with http.MethodPatch — PATCH is the verb for a partial update
// whose absent fields mean "leave alone" (PRD #104 M3's worker rebind).
func (c *HTTPClient) patch(ctx context.Context, path string, reqBody, out any) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodPatch, path, reqBody)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, out)
}

// del executes a DELETE and treats any 2xx (the endpoint answers 204) as success,
// discarding the (empty) body. Non-2xx maps to the documented exit code.
func (c *HTTPClient) del(ctx context.Context, path string) error {
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	return decode2xx(resp, body, path, nil)
}

// decode2xx maps a non-2xx status to an *ExitError and otherwise decodes the body
// into out (nil to discard). Shared by get and postJSON so success/error handling
// is identical across verbs.
func decode2xx(resp *http.Response, body []byte, path string, out any) error {
	if resp.StatusCode/100 != 2 {
		return statusError(resp.StatusCode, body)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return Exitf(ExitGeneric, "malformed response from uzi (%s): %v", path, err)
		}
	}
	return nil
}

// transportMsg unwraps a *url.Error so the message reads as the underlying cause
// (dial/DNS/TLS/timeout) rather than repeating the request URL.
func transportMsg(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// statusError maps a non-2xx status to an *ExitError with the documented exit
// code, folding in the server's {"error": "..."} message when present.
func statusError(status int, body []byte) *ExitError {
	msg := serverErrMsg(body)
	switch {
	case status == http.StatusBadRequest:
		if msg == "" {
			msg = "bad request"
		}
		return Exitf(ExitUsage, "%s", msg)
	case status == http.StatusUnauthorized:
		if msg == "" {
			msg = "authentication required or token invalid — run `uzi auth token` or set $UZI_TOKEN"
		}
		return Exitf(ExitAuth, "%s", msg)
	case status == http.StatusForbidden:
		// The only 403 a CLI read verb hits is the admin_ro scope gate
		// (RequireAdminRO); owner-scoped reads 404 a foreign resource, never 403.
		if msg == "" {
			msg = "forbidden"
		}
		return Exitf(ExitAuth, "%s: your token lacks the required scope (admin views need an admin-scoped token)", msg)
	case status == http.StatusNotFound:
		if msg == "" {
			msg = "not found"
		}
		return Exitf(ExitNotFound, "%s", msg)
	case status == http.StatusConflict:
		if msg == "" {
			msg = "conflict"
		}
		return Exitf(ExitConflict, "%s", msg)
	case status >= 500:
		if msg == "" {
			msg = fmt.Sprintf("server error (%d)", status)
		}
		return Exitf(ExitUnreachable, "%s", msg)
	default:
		if msg == "" {
			msg = fmt.Sprintf("request failed (%d)", status)
		}
		return Exitf(ExitGeneric, "%s", msg)
	}
}

// serverErrMsg extracts the message from the API's {"error": "..."} body, or ""
// when the body is not that shape.
func serverErrMsg(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		return strings.TrimSpace(e.Error)
	}
	return ""
}

func (c *HTTPClient) Whoami(ctx context.Context) (apitypes.UserDTO, error) {
	var env struct {
		User apitypes.UserDTO `json:"user"`
	}
	if err := c.get(ctx, "/api/auth/me", &env); err != nil {
		return apitypes.UserDTO{}, err
	}
	return env.User, nil
}

func (c *HTTPClient) ListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) GetRun(ctx context.Context, id string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id), &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

// RunLogs fetches a run's messages after `after` in bounded pages (?after=&limit=)
// and reassembles them, so a large history cannot die mid-body on one unbounded
// request (issue #160). Paging is internal and transparent: callers still get one
// complete slice or an error. The contract is ALL-OR-NOTHING — if any page fails,
// everything accumulated so far is discarded and (nil, err) is returned, so a
// caller never mistakes a partial history for a complete one.
//
// The loop stops ONLY on an empty page, never on a merely-short one: the server
// clamps ?limit= to its own maxRunMessagesPage, so a short page can mean the server
// capped the request below logsPageSize rather than "this is the last page". Treating
// a short page as terminal would silently truncate the history yet return it with a
// nil error — a partial that looks complete, which this contract forbids. Stopping on
// empty is correct regardless of how the server clamps limit, at the cost of one extra
// trailing (empty) request. maxLogsMessages is the backstop: a hostile server that
// never returns an empty page cannot grow the accumulator without bound.
func (c *HTTPClient) RunLogs(ctx context.Context, id string, after int32) ([]apitypes.MessageDTO, error) {
	var all []apitypes.MessageDTO
	seq := after
	for {
		var env struct {
			Messages []apitypes.MessageDTO `json:"messages"`
		}
		path := fmt.Sprintf("/api/runs/%s/messages?after=%d&limit=%d", url.PathEscape(id), seq, logsPageSize)
		if err := c.get(ctx, path, &env); err != nil {
			// All-or-nothing: discard partial history so an error can never look
			// like a complete fetch.
			return nil, err
		}
		if len(env.Messages) == 0 {
			// An empty page is the last page — robust to the server clamping ?limit=
			// below logsPageSize, where a short page is NOT necessarily the end.
			return all, nil
		}
		all = append(all, env.Messages...)
		if len(all) > maxLogsMessages {
			// Hostile/compromised-server backstop: a server that always returns a full
			// page with strictly-increasing seqs never returns an empty page and keeps
			// advancing. Abort all-or-nothing rather than accumulate toward OOM.
			return nil, Exitf(ExitGeneric, "run logs: history exceeded %d messages; aborting", maxLogsMessages)
		}
		// Advance past the last message. On the real server seq is gapless and the
		// query is seq > @after, so max seq is the last element's; the guard below
		// only trips against a misbehaving server that would otherwise loop forever.
		next := env.Messages[len(env.Messages)-1].Seq
		if next <= seq {
			return nil, Exitf(ExitGeneric, "run logs: server returned a page that did not advance past seq %d", seq)
		}
		seq = next
	}
}

func (c *HTTPClient) RunReview(ctx context.Context, id string) (*apitypes.ReviewDTO, *apitypes.PendingJudgeDTO, error) {
	// The envelope is {"review": <dto>|null, "pending_judge": <dto>|null} — both keys
	// always present, either nullable (PRD #119 M1). A 200 with review:null is a
	// visible-but-unjudged run — return a nil review so the command exits 0 rather
	// than inventing a 404, which arrives here as *ExitError{ExitNotFound} from get.
	//
	// pending_judge is decoded as a SECOND return rather than folded into the review
	// because review:null is two states, not one: a run nobody ever judged, and a run
	// whose auto-enqueued judge is still queued or running. The CLI printed "not
	// judged" for both. A pre-#119 server omits the key, which decodes to nil — the
	// same value it sends for "no judge in flight", and the same output as before.
	var env struct {
		Review       *apitypes.ReviewDTO       `json:"review"`
		PendingJudge *apitypes.PendingJudgeDTO `json:"pending_judge"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/review", &env); err != nil {
		return nil, nil, err
	}
	return env.Review, env.PendingJudge, nil
}

func (c *HTTPClient) RunInputs(ctx context.Context, id string) ([]apitypes.SteerInputDTO, error) {
	var env struct {
		Inputs []apitypes.SteerInputDTO `json:"inputs"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/inputs", &env); err != nil {
		return nil, err
	}
	return env.Inputs, nil
}

func (c *HTTPClient) ListWorkers(ctx context.Context) ([]apitypes.WorkerDTO, error) {
	var env struct {
		Workers []apitypes.WorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

func (c *HTTPClient) ListSecrets(ctx context.Context) ([]apitypes.SecretDTO, error) {
	var env struct {
		Secrets []apitypes.SecretDTO `json:"secrets"`
	}
	if err := c.get(ctx, "/api/me/secrets", &env); err != nil {
		return nil, err
	}
	return env.Secrets, nil
}

func (c *HTTPClient) SetTokenAutoEligible(ctx context.Context, id string, eligible bool) (apitypes.SecretDTO, error) {
	body := struct {
		AutoEligible bool `json:"auto_eligible"`
	}{AutoEligible: eligible}
	var env struct {
		Secret apitypes.SecretDTO `json:"secret"`
	}
	if err := c.patch(ctx, "/api/me/secrets/anthropic_token/"+url.PathEscape(id)+"/auto-eligible", body, &env); err != nil {
		return apitypes.SecretDTO{}, err
	}
	return env.Secret, nil
}

func (c *HTTPClient) SetRunPriority(ctx context.Context, id string, expedite bool) (apitypes.RunDTO, error) {
	body := struct {
		Expedite bool `json:"expedite"`
	}{Expedite: expedite}
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.patch(ctx, "/api/runs/"+url.PathEscape(id)+"/priority", body, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) SelfRateLimits(ctx context.Context) ([]apitypes.TokenRateLimitDTO, error) {
	var env struct {
		Tokens []apitypes.TokenRateLimitDTO `json:"tokens"`
	}
	if err := c.get(ctx, "/api/me/rate-limits", &env); err != nil {
		return nil, err
	}
	return env.Tokens, nil
}

func (c *HTTPClient) GetMySettings(ctx context.Context) (apitypes.UserSettingsDTO, error) {
	var env struct {
		Settings apitypes.UserSettingsDTO `json:"settings"`
	}
	if err := c.get(ctx, "/api/me/settings", &env); err != nil {
		return apitypes.UserSettingsDTO{}, err
	}
	return env.Settings, nil
}

func (c *HTTPClient) DeleteWorker(ctx context.Context, id string) error {
	return c.del(ctx, "/api/workers/"+url.PathEscape(id))
}

func (c *HTTPClient) DeleteRepo(ctx context.Context, id string) error {
	return c.del(ctx, "/api/repos/"+url.PathEscape(id))
}

func (c *HTTPClient) SetWorkerBindMode(ctx context.Context, id, mode, label string) (apitypes.WorkerDTO, error) {
	// An empty label sends JSON null, which is what a non-pinned mode requires —
	// distinct from omitting the field, which would mean "leave it alone". *string is
	// what makes the two expressible on the wire, and the server REFUSES a label
	// alongside default/auto rather than quietly dropping one of them.
	body := struct {
		AnthropicBindMode string  `json:"anthropic_bind_mode"`
		AnthropicToken    *string `json:"anthropic_token"`
	}{AnthropicBindMode: mode}
	if label != "" {
		body.AnthropicToken = &label
	}
	var env struct {
		Worker apitypes.WorkerDTO `json:"worker"`
	}
	if err := c.patch(ctx, "/api/workers/"+url.PathEscape(id), body, &env); err != nil {
		return apitypes.WorkerDTO{}, err
	}
	return env.Worker, nil
}

func (c *HTTPClient) ListMemory(ctx context.Context) ([]apitypes.AgentMemoryDTO, error) {
	var env struct {
		Memories []apitypes.AgentMemoryDTO `json:"memories"`
	}
	if err := c.get(ctx, "/api/me/memory", &env); err != nil {
		return nil, err
	}
	return env.Memories, nil
}

func (c *HTTPClient) DeleteMemory(ctx context.Context, id string) error {
	return c.del(ctx, "/api/me/memory/"+url.PathEscape(id))
}

// dispositionPath builds the disposition endpoint path for a (run, rec) pair,
// escaping both id segments.
func dispositionPath(runID, recID string) string {
	return "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/disposition"
}

func (c *HTTPClient) SetDisposition(ctx context.Context, runID, recID, status, reason string) error {
	reqBody := map[string]string{"status": status}
	// reason is required iff dismissed; omit it otherwise so a "done" PUT never
	// carries a stray (and server-rejected) reason field.
	if reason != "" {
		reqBody["reason"] = reason
	}
	return c.put(ctx, dispositionPath(runID, recID), reqBody, nil)
}

func (c *HTTPClient) DeleteDisposition(ctx context.Context, runID, recID string) error {
	// The command resolves recID against the current review before calling, so the
	// run and recommendation exist; a 404 here therefore means "no disposition on
	// this coordinate" — softened to ErrNoDisposition (a plain error) so undo can
	// report "already undone" and exit 0, per Decision 6. Any other non-2xx keeps
	// its real exit code.
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, dispositionPath(runID, recID), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoDisposition
	}
	return decode2xx(resp, body, dispositionPath(runID, recID), nil)
}

func (c *HTTPClient) JudgeStats(ctx context.Context) (apitypes.TriageDTO, error) {
	var out apitypes.TriageDTO
	if err := c.get(ctx, "/api/me/judge/stats", &out); err != nil {
		return apitypes.TriageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) JudgeBacklog(ctx context.Context, bucket, runAnchor, category string) (apitypes.JudgeBacklogDTO, error) {
	path := "/api/me/judge/recommendations"
	// All omitted rather than sent empty when unset: the handler's `== ""` branches are what
	// apply its defaults, and an explicit empty value would take the same branch only by
	// coincidence. Escaped because all are user input off a flag.
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if runAnchor != "" {
		q.Set("run", runAnchor)
	}
	if category != "" {
		// Forwarded verbatim as the comma-separated `?category=a,b` list the server splits
		// and normalizes; the CLI never parses it into labels of its own.
		q.Set("category", category)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.JudgeBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.JudgeBacklogDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) BulkSetDispositions(ctx context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error) {
	// Scope is left zero on purpose — see the interface doc. Reason is likewise sent as
	// "" for a `done`, which is what the shared validator requires (done carries no
	// reason); this is a struct, not the omit-when-empty map SetDisposition builds, and
	// the two agree because "" IS the legal value there.
	reqBody := apitypes.JudgeBulkDispositionRequest{Items: items, Status: status, Reason: reason}
	var out apitypes.JudgeDispositionResultDTO
	if err := c.put(ctx, "/api/me/judge/recommendations/disposition", reqBody, &out); err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) ListRepos(ctx context.Context) ([]apitypes.RepoDTO, error) {
	var env struct {
		Repos []apitypes.RepoDTO `json:"repos"`
	}
	if err := c.get(ctx, "/api/repos", &env); err != nil {
		return nil, err
	}
	return env.Repos, nil
}

// BuildInfo reads GET /api/version. The response is decoded into the SHARED
// apitypes.BuildInfoDTO rather than a CLI-local struct, which is what keeps
// "unknown" distinguishable from zero for the fields that carry that distinction:
// Commits and UptimeSeconds are pointers, and Commit and BuiltAt are omitempty, so
// an unstamped server's absent values stay absent through `uzi version --json`. A
// local struct with plain int fields would render a dev server as commits 0 and
// uptime 0 — a build claiming to know things it does not, which is precisely what
// the server side of this PRD spent a pointer to prevent.
//
// FOUNDED IS THE EXCEPTION, and the boundary is worth stating because the sentence
// above is otherwise one field too broad. Founded has no omitempty — deliberately,
// since the server always sends it and TestBuildInfoDTOTags pins version+founded as
// the always-present pair. Against a PRE-#175 server, which sends neither, the
// decode leaves it "" and re-marshalling emits `"founded": ""`: present-but-empty,
// the one place this response conflates unknown with empty. Adding omitempty would
// fix the rollout-window cosmetic by changing the server's contract, which is the
// wrong trade. The window closes when the server is upgraded.
//
// Confined to --json. The text path skips empty values (serverRows in
// cmd/uzi/version.go), so it already prints nothing for an unknown founded.
// Measured against a server returning `{"version":"0.11.11"}`: --json emits
// `"founded": ""` while the text output shows only the version line.
func (c *HTTPClient) BuildInfo(ctx context.Context) (apitypes.BuildInfoDTO, error) {
	var out apitypes.BuildInfoDTO
	if err := c.get(ctx, "/api/version", &out); err != nil {
		return apitypes.BuildInfoDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AdminListUsers(ctx context.Context) ([]apitypes.UserDTO, error) {
	var env struct {
		Users []apitypes.UserDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/users", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}

func (c *HTTPClient) AdminListRuns(ctx context.Context) ([]apitypes.RunListItemDTO, error) {
	var env struct {
		Runs []apitypes.RunListItemDTO `json:"runs"`
	}
	if err := c.get(ctx, "/api/admin/runs", &env); err != nil {
		return nil, err
	}
	return env.Runs, nil
}

func (c *HTTPClient) AdminListWorkers(ctx context.Context) ([]apitypes.AdminWorkerDTO, error) {
	var env struct {
		Workers []apitypes.AdminWorkerDTO `json:"workers"`
	}
	if err := c.get(ctx, "/api/admin/workers", &env); err != nil {
		return nil, err
	}
	return env.Workers, nil
}

// AdminListCLITokens reads the factory-wide standing-credential inventory. The
// response carries no token value and no hash — see apitypes.AdminCLITokenDTO.
func (c *HTTPClient) AdminListCLITokens(ctx context.Context) ([]apitypes.AdminCLITokenDTO, error) {
	var env struct {
		Tokens []apitypes.AdminCLITokenDTO `json:"tokens"`
	}
	if err := c.get(ctx, "/api/admin/cli-tokens", &env); err != nil {
		return nil, err
	}
	return env.Tokens, nil
}

// GuardrailImpact reads the live guardrail pre-flight impact count. The endpoint
// persists nothing; it re-sweeps the forge and returns how many enabled repos
// would be refused under the new guardrail (PRD #66 M3).
func (c *HTTPClient) GuardrailImpact(ctx context.Context) (apitypes.GuardrailImpactDTO, error) {
	var out apitypes.GuardrailImpactDTO
	if err := c.get(ctx, "/api/admin/guardrail-impact", &out); err != nil {
		return apitypes.GuardrailImpactDTO{}, err
	}
	return out, nil
}

// AdminBlockedRepos reads the admin cross-user blocked-repos list from the STORED
// privilege report (PRD #66 M9). ChecksUnknown on the envelope is true when a
// connection was never checked — an empty list is then "unknown", not "none blocked".
func (c *HTTPClient) AdminBlockedRepos(ctx context.Context) (apitypes.AdminBlockedReposDTO, error) {
	var out apitypes.AdminBlockedReposDTO
	if err := c.get(ctx, "/api/admin/blocked-repos", &out); err != nil {
		return apitypes.AdminBlockedReposDTO{}, err
	}
	return out, nil
}

// AdminAgentSource reads the agent-source config + sync status + staged snapshot
// (PRD #602 M6): GET /api/admin/agent-source. Read-only — the "Sync now" and
// approve-and-apply writes are cookie-only, so there is nothing here for a uza_
// (admin_ro) token to trigger. The response envelope is {"agent_source": {...}}.
func (c *HTTPClient) AdminAgentSource(ctx context.Context) (apitypes.AgentSourceDTO, error) {
	var env struct {
		AgentSource apitypes.AgentSourceDTO `json:"agent_source"`
	}
	if err := c.get(ctx, "/api/admin/agent-source", &env); err != nil {
		return apitypes.AgentSourceDTO{}, err
	}
	return env.AgentSource, nil
}

func (c *HTTPClient) AdminUsage(ctx context.Context) (apitypes.AdminUsageDTO, error) {
	var out apitypes.AdminUsageDTO
	if err := c.get(ctx, "/api/admin/usage", &out); err != nil {
		return apitypes.AdminUsageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AdminRateLimits(ctx context.Context) ([]apitypes.AdminRateLimitRowDTO, error) {
	var env struct {
		Users []apitypes.AdminRateLimitRowDTO `json:"users"`
	}
	if err := c.get(ctx, "/api/admin/rate-limits", &env); err != nil {
		return nil, err
	}
	return env.Users, nil
}

func (c *HTTPClient) StartCLIAuth(ctx context.Context, challenge, clientDesc string) (CLIAuthStartResult, error) {
	reqBody := map[string]string{"code_challenge": challenge, "client_desc": clientDesc}
	var out struct {
		RequestID string `json:"request_id"`
		UserCode  string `json:"user_code"`
		ExpiresIn int    `json:"expires_in"`
		Interval  int    `json:"interval"`
	}
	if err := c.postJSON(ctx, "/api/auth/cli/start", reqBody, &out); err != nil {
		return CLIAuthStartResult{}, err
	}
	return CLIAuthStartResult{
		RequestID: out.RequestID,
		UserCode:  out.UserCode,
		ExpiresIn: out.ExpiresIn,
		Interval:  out.Interval,
	}, nil
}

func (c *HTTPClient) PollCLIAuth(ctx context.Context, requestID, verifier string) (CLIAuthPollResult, error) {
	reqBody := map[string]string{"request_id": requestID, "verifier": verifier}
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, "/api/auth/cli/poll", reqBody)
	if err != nil {
		return CLIAuthPollResult{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// 200 {token, user} — approved and minted, returned once. Store, never print.
		var out struct {
			Token string           `json:"token"`
			User  apitypes.UserDTO `json:"user"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return CLIAuthPollResult{}, Exitf(ExitGeneric, "malformed login response from uzi: %v", err)
		}
		return CLIAuthPollResult{Status: CLIAuthAuthorized, Token: out.Token, User: out.User}, nil
	case http.StatusAccepted:
		// 202 {status:"pending"} — keep polling.
		return CLIAuthPollResult{Status: CLIAuthPending}, nil
	case http.StatusGone:
		// 410 {status:"expired"|"denied"|"consumed"} — terminal, stop.
		return CLIAuthPollResult{Status: CLIAuthTerminal, Reason: pollStatusField(body)}, nil
	default:
		// 400/401/429/5xx etc. — map to the documented exit code (auth/usage/...).
		return CLIAuthPollResult{}, statusError(resp.StatusCode, body)
	}
}

// pollStatusField pulls {"status": "..."} from a poll reply, defaulting to
// "expired" when absent — the request_id is not a secret, so a missing/opaque
// terminal body reads as expired rather than leaking existence.
func pollStatusField(body []byte) string {
	var s struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(body, &s) == nil && strings.TrimSpace(s.Status) != "" {
		return s.Status
	}
	return "expired"
}

// CreateRunSeed is PRD #209's optional seeded plan for CreateRun: an
// externally-authored plan and the run's subagent roster. Nil ⇒ an ordinary run
// planned from the issue. It mirrors the server's workersvc.SeededPlan and keeps the
// coupling STRUCTURAL — a Selection cannot ride without a plan, which is the server's
// own rule ("agent_selection requires plan_md"): a nil-vs-set seed carries the choice,
// so no CreateRun caller can express the rejected combination in the first place.
type CreateRunSeed struct {
	// PlanMD is the plan text, forwarded verbatim. Empty is a valid value to SEND —
	// the server returns 422 on an empty/whitespace plan, and letting it decide keeps
	// the scrub-and-empty rule in one place rather than duplicated client-side.
	PlanMD string
	// Selection is the run's roster, or nil for "no selection" (the worker then applies
	// its default: repo agents when the clone has a roster, else own — Success Criterion 5).
	Selection *apitypes.AgentSelection
	// PlannedCommit is the commit the plan was written against (PRD #209 M4), forwarded on
	// `--planned-commit`. Empty ⇒ not sent, so the worker's staleness compare stays inert.
	PlannedCommit string
	// RequireBase makes a base-commit divergence FAIL the run rather than warn
	// (`--require-base`). Only meaningful with a PlannedCommit; the CLI and the server both
	// reject it otherwise.
	RequireBase bool
}

func (c *HTTPClient) CreateRun(ctx context.Context, repoID string, issueIID int64, waitOnLimit *bool, seed *CreateRunSeed) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// A struct rather than the map this used to build, because the map could not
	// express the tri-state: `omitempty` on a POINTER omits only nil, so a non-nil
	// false still marshals as `"wait_on_limit": false`. That is the whole contract —
	// omitted means "inherit my default", present-and-false means "explicitly not this
	// run". A `map[string]any` with an `if != nil` guard would work too and is worse:
	// it puts the rule in a conditional instead of in the type.
	//
	// plan_md/agent_selection stay `omitempty` so a nil seed sends NEITHER key and the
	// body is byte-identical to a pre-#209 create (Success Criterion 2). A non-nil seed
	// always sets plan_md (via &PlanMD, so even an empty plan is SENT and the server's
	// 422 fires) and rides the selection only when one was chosen.
	//
	// planned_commit/require_base (M4) are `omitempty` for the same reason — a nil seed,
	// AND a seed that set neither, sends neither key, so the byte-identical guarantee
	// holds for a plain --plan-file create too. planned_commit rides only when non-empty
	// (a *string so omitempty drops it); require_base only when true.
	reqBody := struct {
		IssueIID      int64                    `json:"issue_iid"`
		WaitOnLimit   *bool                    `json:"wait_on_limit,omitempty"`
		PlanMD        *string                  `json:"plan_md,omitempty"`
		Selection     *apitypes.AgentSelection `json:"agent_selection,omitempty"`
		PlannedCommit *string                  `json:"planned_commit,omitempty"`
		RequireBase   bool                     `json:"require_base,omitempty"`
	}{IssueIID: issueIID, WaitOnLimit: waitOnLimit}
	if seed != nil {
		reqBody.PlanMD = &seed.PlanMD
		reqBody.Selection = seed.Selection
		if seed.PlannedCommit != "" {
			reqBody.PlannedCommit = &seed.PlannedCommit
		}
		reqBody.RequireBase = seed.RequireBase
	}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/runs", reqBody, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) CreateTaskRun(ctx context.Context, repoID, taskContext, baseBranch string, openMR, reviewRequested, thenFixRequested, interactive bool) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	// base_branch is `omitempty` so an unset --base sends no key and the worker
	// branches from the caller's seeded HEAD — the CreateRun tri-state convention: an
	// omitted field means "use the default", present means "this, explicitly". open_mr,
	// review_requested and then_fix_requested are plain bools (a task defaults to no MR,
	// no review, no fix; false is the common, correct value to send).
	reqBody := struct {
		Context          string `json:"context"`
		BaseBranch       string `json:"base_branch,omitempty"`
		OpenMr           bool   `json:"open_mr"`
		Interactive      bool   `json:"interactive"`
		ReviewRequested  bool   `json:"review_requested"`
		ThenFixRequested bool   `json:"then_fix_requested"`
	}{Context: taskContext, BaseBranch: baseBranch, OpenMr: openMR, Interactive: interactive, ReviewRequested: reviewRequested, ThenFixRequested: thenFixRequested}
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/task-runs", reqBody, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) GetTaskReview(ctx context.Context, id string) (*apitypes.TaskReviewDTO, error) {
	// The envelope is {"task_review": <dto>|null} — a 200 with task_review:null is a
	// visible-but-unreviewed task, so a nil DTO is returned (the command exits 0 and
	// prints the "no review yet" hint); a 404 arrives as *ExitError{ExitNotFound} from get.
	var env struct {
		TaskReview *apitypes.TaskReviewDTO `json:"task_review"`
	}
	if err := c.get(ctx, "/api/runs/"+url.PathEscape(id)+"/task-review", &env); err != nil {
		return nil, err
	}
	return env.TaskReview, nil
}

func (c *HTTPClient) DispatchTaskRun(ctx context.Context, runID string) (apitypes.RunDTO, error) {
	var env struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/dispatch", nil, &env); err != nil {
		return apitypes.RunDTO{}, err
	}
	return env.Run, nil
}

func (c *HTTPClient) SubmitRunInput(ctx context.Context, runID, kind, body string, sel *apitypes.AgentSelection) (apitypes.RunInputResponse, error) {
	reqBody := apitypes.RunInputRequest{Kind: kind, Body: body, Selection: sel}
	var out apitypes.RunInputResponse
	if err := c.postJSON(ctx, "/api/runs/"+url.PathEscape(runID)+"/inputs", reqBody, &out); err != nil {
		return apitypes.RunInputResponse{}, err
	}
	return out, nil
}

// The schedule endpoints (PRD #241 M6) return their DTOs BARE — the handler writes
// the value directly (httpx.JSON), not under a `{"schedule": …}` key — so these
// decode into the DTO itself rather than an envelope struct.

func (c *HTTPClient) ListSchedules(ctx context.Context) ([]apitypes.ScheduleDTO, error) {
	var out []apitypes.ScheduleDTO
	if err := c.get(ctx, "/api/me/schedules", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *HTTPClient) CreateSchedule(ctx context.Context, repoID string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/schedules", req, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) GetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.get(ctx, "/api/schedules/"+url.PathEscape(id), &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) SetScheduleEnabled(ctx context.Context, id string, enabled bool) (apitypes.ScheduleDTO, error) {
	// A struct carrying ONLY `enabled`, so the handler's onlyEnabled() pause/resume path
	// fires: the config is left untouched and just the flag flips. A full ScheduleRequest
	// here would re-submit empty target/timing fields and be rejected on re-validation.
	reqBody := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	var out apitypes.ScheduleDTO
	if err := c.patch(ctx, "/api/schedules/"+url.PathEscape(id), reqBody, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) PatchSchedule(ctx context.Context, id string, req apitypes.ScheduleRequest) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.patch(ctx, "/api/schedules/"+url.PathEscape(id), req, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) DeleteSchedule(ctx context.Context, id string) error {
	return c.del(ctx, "/api/schedules/"+url.PathEscape(id))
}

func (c *HTTPClient) RunScheduleNow(ctx context.Context, id string) (apitypes.RunNowResponse, error) {
	var out apitypes.RunNowResponse
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/run-now", nil, &out); err != nil {
		return apitypes.RunNowResponse{}, err
	}
	return out, nil
}

func (c *HTTPClient) ListScheduleCatalog(ctx context.Context) (apitypes.ScheduleCatalogResponse, error) {
	var out apitypes.ScheduleCatalogResponse
	if err := c.get(ctx, "/api/schedule-catalog", &out); err != nil {
		return apitypes.ScheduleCatalogResponse{}, err
	}
	return out, nil
}

func (c *HTTPClient) EnableCatalogSchedule(ctx context.Context, repoID, slug string) (apitypes.ScheduleDTO, bool, error) {
	// Call doJSONRead directly (not postJSON) so the 201-vs-200 status survives: the server
	// answers 201 for a fresh enable and 200 for an idempotent repeat, and the CLI renders
	// created vs already-enabled off that distinction.
	path := "/api/repos/" + url.PathEscape(repoID) + "/schedule-catalog/" + url.PathEscape(slug)
	resp, body, err := c.doJSONRead(ctx, http.MethodPost, path, nil)
	if err != nil {
		return apitypes.ScheduleDTO{}, false, err
	}
	var out apitypes.ScheduleDTO
	if err := decode2xx(resp, body, path, &out); err != nil {
		return apitypes.ScheduleDTO{}, false, err
	}
	return out, resp.StatusCode == http.StatusCreated, nil
}

func (c *HTTPClient) ResetSchedule(ctx context.Context, id string) (apitypes.ScheduleDTO, error) {
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/reset", nil, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CloneSchedule(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	// An empty repoID clones into the source's own repo, so the body is nil (no repo_id
	// key); a non-empty one sends {"repo_id": …} to clone into that owned repo.
	var body any
	if strings.TrimSpace(repoID) != "" {
		trimmed := strings.TrimSpace(repoID)
		body = apitypes.ScheduleCloneRequest{RepoID: &trimmed}
	}
	var out apitypes.ScheduleDTO
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/clone", body, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) AddScheduleRepo(ctx context.Context, id, repoID string) (apitypes.ScheduleDTO, error) {
	// postJSON maps a non-2xx through statusError, so a foreign source/repo 404→ExitNotFound
	// and a duplicate-sibling 409→ExitConflict reach the caller as documented exit codes; the
	// add-repo command special-cases the 409 into a clean no-op.
	var out apitypes.ScheduleDTO
	body := apitypes.AddScheduleRepoRequest{RepoID: strings.TrimSpace(repoID)}
	if err := c.postJSON(ctx, "/api/schedules/"+url.PathEscape(id)+"/add-repo", body, &out); err != nil {
		return apitypes.ScheduleDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) CheckRepoLabels(ctx context.Context, repoID string, labels []string) ([]string, error) {
	var out apitypes.LabelCheckResponse
	path := "/api/repos/" + url.PathEscape(repoID) + "/labels/check"
	if err := c.postJSON(ctx, path, apitypes.LabelCheckRequest{Labels: labels}, &out); err != nil {
		return nil, err
	}
	return out.Missing, nil
}

func (c *HTTPClient) EnsureRepoLabels(ctx context.Context, repoID string, labels []string) error {
	var out apitypes.LabelEnsureResponse
	path := "/api/repos/" + url.PathEscape(repoID) + "/labels/ensure"
	return c.postJSON(ctx, path, apitypes.LabelEnsureRequest{Labels: labels}, &out)
}

func (c *HTTPClient) ListFindings(ctx context.Context, bucket, repo, run string) (apitypes.IncidentalFindingBacklogDTO, error) {
	path := "/api/findings"
	// Each omitted rather than sent empty when unset: the handler's `== ""` branches are what
	// apply its defaults, and an explicit empty value would take the same branch only by
	// coincidence. Escaped because all are user input off a flag.
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if run != "" {
		q.Set("run", run)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.IncidentalFindingBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) FileFinding(ctx context.Context, id string) (apitypes.IncidentalFindingFileResultDTO, error) {
	// An empty JSON object body: the file endpoint resolves title/description from the STORED,
	// already-sanitised finding row (D4) and assembles labels server-side (D5), so the CLI sends
	// no edits — the web is the rich editor, the CLI files defaults. postJSON routes a non-2xx
	// through statusError, so 404→ExitNotFound and 409→ExitConflict come for free.
	var out apitypes.IncidentalFindingFileResultDTO
	if err := c.postJSON(ctx, "/api/findings/"+url.PathEscape(id)+"/issue", struct{}{}, &out); err != nil {
		return apitypes.IncidentalFindingFileResultDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) DismissFinding(ctx context.Context, id, reason string) error {
	// reason is the wire enum, already mapped and validated by the command. postJSON discards
	// the (200) body via a nil out and maps a non-2xx through statusError (404→4, 409→5).
	return c.postJSON(ctx, "/api/findings/"+url.PathEscape(id)+"/dismiss", map[string]string{"reason": reason}, nil)
}

// ReviewFiledIssueDTO / ReviewIssueFileResult mirror the review file handler's wire shape
// (POST .../review/recommendations/{recID}/issue): the created forge issue plus an optional
// created-with-warning note. Defined here (not in apitypes) because the handler's response is
// a handler-local type; the shape is pinned by TestFileIssueClientWireRoundtripLiveDB (decodes
// a real server fileIssueResponse into this type) and TestReviewFileClientWireRoundtrip
// (marshal/unmarshal json-tag parity incl. warning), both in api/internal/handler.
type ReviewFiledIssueDTO struct {
	IID    int64  `json:"iid"`
	WebURL string `json:"web_url"`
	Title  string `json:"title"`
}

type ReviewIssueFileResult struct {
	Issue   ReviewFiledIssueDTO `json:"issue"`
	Warning string              `json:"warning,omitempty"`
}

func (c *HTTPClient) GetReviewIssueDraft(ctx context.Context, runID, recID string) (apitypes.IssueDraftDTO, error) {
	// The handler wraps the DTO in a {"draft": ...} envelope, so decode into a local envelope
	// and hand back the unwrapped draft. Both ids are escaped — they are user input off the
	// positionals. A non-2xx (404 for a foreign/unknown run or rec) maps through statusError.
	var env struct {
		Draft apitypes.IssueDraftDTO `json:"draft"`
	}
	path := "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/issue-draft"
	if err := c.get(ctx, path, &env); err != nil {
		return apitypes.IssueDraftDTO{}, err
	}
	return env.Draft, nil
}

func (c *HTTPClient) FileReviewIssue(ctx context.Context, runID, recID, repoID, title, description string) (ReviewIssueFileResult, error) {
	// Unlike FileFinding's empty body, the review file endpoint decodes fileIssueRequest
	// {repo_id,title,description} — all three required — so the CLI sends the draft defaults it
	// just fetched (the web is the rich editor). postJSON routes a non-2xx through statusError,
	// so 404→ExitNotFound and 409→ExitConflict come for free.
	body := map[string]string{"repo_id": repoID, "title": title, "description": description}
	path := "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/issue"
	var out ReviewIssueFileResult
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return ReviewIssueFileResult{}, err
	}
	return out, nil
}

func (c *HTTPClient) GetProjectSyncStatus(ctx context.Context, repoID string) (ProjectSyncStatus, error) {
	var out ProjectSyncStatus
	if err := c.get(ctx, "/api/repos/"+url.PathEscape(repoID)+"/github-project-sync", &out); err != nil {
		return ProjectSyncStatus{}, err
	}
	return out, nil
}

func (c *HTTPClient) ResyncProjectSync(ctx context.Context, repoID string) error {
	return c.postJSON(ctx, "/api/repos/"+url.PathEscape(repoID)+"/github-project-sync/resync", nil, nil)
}
