// Package workersvc is the agent-runtime service: it owns the worker protocol
// (register/heartbeat/claim/messages/state/inputs), the web-facing worker and
// run CRUD, and the background sweeper's DB work. It is the sibling of forgesvc
// — the handlers and the sweeper goroutine both call it, so the run-lifecycle
// rules live in exactly one place.
//
// The one secret-bearing path is Claim: it decrypts the user's bot PAT and
// Anthropic token and returns them in the claim payload (the only delivery
// channel, per PRD #2/#3's server-holds-the-keys model). Those plaintexts are
// never logged and never persisted here.
package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/planpolicy"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/toolprofile"
	"github.com/vtmocanu/uzi/api/internal/toolseed"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// MaxIssueDescriptionBytes bounds the snapshotted issue description carried in a
// run: generous for a PRD-shaped body, a secondary guard under the 1 MiB whole-body
// cap DecodeJSON already enforces. Enforced once inside createRun so BOTH the
// manual-start (handler) and autopilot (poller) paths share the exact cap — the
// single source of truth that replaced the two mirrored consts (PRD #19 M5).
const MaxIssueDescriptionBytes = 256 * 1024

// MaxSeededPlanBytes bounds a create-time seeded plan (PRD #209 D5), mirroring
// MaxIssueDescriptionBytes: a seeded plan is untrusted external input on the same
// footing as a snapshotted issue body, and the same 256 KiB bound is generous for a
// plan document while staying a secondary guard under DecodeJSON's whole-body cap.
// Checked on the RAW input before the secret scrub, so an oversize body is rejected
// (422) rather than scrubbed-then-measured.
const MaxSeededPlanBytes = 256 * 1024

// planSourceAgent / planSourceSeeded are the two runs.plan_source values (00095).
// A run planned inside the worker (or predating the column) is 'agent'; a run whose
// plan was supplied at create time over the API is 'seeded'. Named constants so the
// plan_approved third disjunct (assembleClaim) and the create path cannot drift on a
// bare string literal.
const (
	planSourceAgent  = "agent"
	planSourceSeeded = "seeded"
)

// plannedCommitRe validates a create-time --planned-commit (PRD #209 M4): hex, between
// git's default abbreviation floor (7) and a full sha256 (64). See ErrInvalidPlannedCommit
// for why the bound matters (a shorter value makes the worker's prefix-tolerant --require-base
// compare inert). The CLI carries the same pattern as a pre-flight; this is the authority.
var plannedCommitRe = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// Terminal run statuses. A run in any of these is finished and immutable.
var terminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// Sentinel errors mapped to HTTP status codes by the handlers.
var (
	ErrRunNotFound   = errors.New("run not found")
	ErrRunNotOwned   = errors.New("run not owned by worker")
	ErrRepoNotFound  = errors.New("repo not found")
	ErrIssueNotFound = errors.New("issue not found")
	// ErrNotPRDIssue rejects a run on an issue that does not carry the configured
	// run-eligibility label (PRD #764 M1: the uzi_label) → 422. This is now the SINGLE
	// eligibility gate — an issue without the uzi_label is not uzi's to run, PRD link
	// or no. (The name is retained for the sentinel's existing consumers; the message
	// surfaces reworded to tell the user to add the uzi label.)
	ErrNotPRDIssue         = errors.New("issue does not carry the uzi label")
	ErrDescriptionTooLarge = errors.New("issue description is too large to run")
	// ErrPlanTooLarge rejects a create-time seeded plan over MaxSeededPlanBytes (PRD
	// #209 D5) → 422. Mirrors ErrDescriptionTooLarge; checked on the raw input.
	ErrPlanTooLarge = errors.New("seeded plan is too large to run")
	// ErrPlanEmpty rejects a seeded plan that is empty or whitespace-only at create
	// time (PRD #209 D8) → 422. (The check runs on the secret-scrubbed text, but the
	// scrub only ADDS the "[redacted]" marker and never deletes, so it cannot itself
	// make a non-empty plan empty — see createRun.) Storing a blank plan_md as 'seeded'
	// is the entry half of the D8 armed-fall-through hole: the run would reach the
	// worker plan_approved=true with nothing to implement, plan and gate, and come back
	// armed. Rejecting at create time closes that entry path; the plan_source='agent'
	// write in SetRunAwaitingApproval closes every other one.
	ErrPlanEmpty = errors.New("seeded plan is empty")
	// ErrPlanUnsafe rejects a create-time SEEDED plan whose text names a
	// bright-line infrastructure-reconnaissance target (cloud instance metadata
	// endpoint, the kube-apiserver ClusterIP, or the in-pod service-account token
	// mount) → 422 (issue #280). Deterministic defense-in-depth: a seeded plan
	// skips the approval gate (plan_source='seeded' sets plan_approved), so this
	// is the one automated control between such a plan and implementation. The
	// matched category is wrapped into the returned error for the 422 message.
	// It runs only on SEEDED plans; an ordinary issue-planned run is never
	// screened (it still goes through the human approval gate unchanged).
	ErrPlanUnsafe = errors.New("seeded plan names a prohibited target")
	// ErrInvalidPlannedCommit rejects a create-time --planned-commit that is not a
	// plausible git commit sha (PRD #209 M4) → 400. The compare in the worker is
	// prefix-tolerant with no floor, so a 1-2 char value would spuriously match almost
	// any base and silently make --require-base inert (a user who asked to FAIL on
	// divergence would get a silent proceed — the unsafe direction); and there is no
	// other length/charset bound on the field, so garbage would persist and re-deliver on
	// every claim. Requiring hex of git's abbrev floor (7) up to a full sha256 (64) closes
	// both. This is the authoritative gate; the CLI mirrors it as a pre-flight usage error.
	ErrInvalidPlannedCommit = errors.New("planned base commit is not a valid commit sha (hex, 7-64 chars)")
	ErrActiveRunExists      = errors.New("a non-terminal run already exists for this issue")
	ErrRunTerminal          = errors.New("run has already finished")
	// ErrStopNotInteractive rejects a `stop` on a run that is not an interactive task
	// (PRD #517 M4) → 409. A graceful stop is honored ONLY by the interactive-task park:
	// no other run kind reads the stop flag, so a stop on a plan-gated / chat /
	// non-interactive task run would stamp a permanent, spurious stop_kind='stopped' and
	// return a misleading "stop sent" success while nothing actually winds the run down.
	// Gated on kind+interactive alone, NOT on status: a stop on a RUNNING interactive task
	// (not yet parked) is still valid. It is a run-state conflict, hence 409 (the CLI maps
	// 409 → ExitConflict).
	ErrStopNotInteractive = errors.New("run stop applies only to interactive task runs")
	// ErrScopeNotMilestoneRun rejects a `scope` (PRD #634 M2) on a run that is not a
	// milestone-structured issue run (kind='issue' with a non-empty frozen milestone
	// list) → 409. A scope ceiling bounds how many of the frozen milestones the run may
	// complete, so it is meaningless on a run with no frozen milestones or a non-issue kind.
	ErrScopeNotMilestoneRun = errors.New("scope applies only to milestone-structured issue runs")
	// ErrInvalidScopeCeiling rejects a `scope` whose body does not parse as an integer
	// ceiling (PRD #634 M2) → 400. The value is clamped, never rejected, when out of range —
	// only a non-integer body is a caller error. (Named to avoid colliding with the unrelated
	// judge-bulk ErrInvalidScope.)
	ErrInvalidScopeCeiling = errors.New("scope ceiling must be an integer")
	// ErrReviseCapReached rejects a revise_plan once the run has hit
	// PLAN_MAX_REVISIONS persisted revisions (PRD #41). Counted over ALL
	// revise_plan rows for the run (a consumed revise still counts), so the cap is
	// the lifetime number of revisions requested, not the pending backlog. → 409.
	ErrReviseCapReached = errors.New("plan revision limit reached")
	ErrInvalidState     = errors.New("invalid run state")
	// ErrRunNotAwaitingInput rejects an `answer` for a run that is not parked on a
	// clarification question (PRD #88 M1) → 409. Its sibling ErrStaleAnswer covers the
	// run that IS parked but on a DIFFERENT question — the two are separated because
	// they mean different things to a user: "nothing is being asked right now" versus
	// "you answered a question that has already moved on".
	ErrRunNotAwaitingInput = errors.New("run is not waiting for an answer")
	// ErrStaleAnswer rejects an answer naming a question other than the run's currently
	// open one (PRD #88 M1) → 409. The common cause is benign and expected: a Slack
	// reply written against question N that arrives after the lead has already asked
	// N+1. Applying it to N+1 would silently answer the wrong question.
	ErrStaleAnswer = errors.New("answer does not match the run's open question")
	// ErrInvalidAnswer covers a malformed `answer` body (PRD #88 M1) → 400. Rejected
	// rather than defaulted: an answer that cannot say what it answers has no safe
	// interpretation.
	ErrInvalidAnswer  = errors.New("invalid answer")
	ErrInvalidMessage = errors.New("invalid run message")
	// ErrUnstorableMessage is a batch the DATABASE refused for a reason that can
	// never succeed on retry (PRD #108 M2) → 400, joining ErrInvalidMessage.
	//
	// The status code is the retry contract, and this is the load-bearing half of
	// the fix. A permanent failure returned as 500 is what converted one bad
	// payload into a 27-minute, 239-message wedge: the worker's batcher treats any
	// throw as retryable and re-posts the identical batch at ~2 Hz. With this,
	// incomplete sanitation degrades to "that batch is rejected and the run fails
	// with a clear reason" instead of "the run wedges silently" — which is why the
	// PRD is not titled "strip NUL bytes".
	ErrUnstorableMessage = errors.New("run message cannot be stored")
	// ErrInvalidSelection covers both PRD #37 payloads: a worker-reported repo
	// agent roster that breaks a cap, and a browser-submitted agent selection that
	// names an agent the run does not have. Both map to 400.
	ErrInvalidSelection = errors.New("invalid agent selection")
	// ErrCapabilityUnmet is the sentinel for the PRD #84 M4 4c approval gate: a plan being
	// approved requires capabilities its OWNING worker cannot satisfy, so the approve is
	// blocked → 409. It is carried by a *CapabilityUnmetError, which names the unmet set for
	// the handler to render; errors.Is against this sentinel matches (Unwrap), and errors.As
	// on the struct extracts the names. Mirrors ErrInvalidSelection's role as the stable
	// match target the handler switches on.
	ErrCapabilityUnmet = errors.New("run requires capabilities the owning worker cannot satisfy")
	ErrWorkerNotFound  = errors.New("worker not found")
	// ErrWorkerHasActiveRuns rejects deletion of a worker that still owns a
	// non-terminal run: the FK is ON DELETE SET NULL, so deleting would orphan the
	// run past every sweep (and the one-active-run index would then block re-runs).
	ErrWorkerHasActiveRuns = errors.New("worker has active runs")
	// ErrUnknownSecretLabel is a token label that names none of the user's
	// credentials (PRD #104 M3) → 400. Case-insensitive, matching the unique index
	// on lower(label), so it means "you have no token by that name" and never "you
	// spelled it with the wrong capitalization".
	ErrUnknownSecretLabel = errors.New("unknown anthropic token label")
	// ErrSecretNotOwned is a secret id that is not the caller's (PRD #104 D11) → 404,
	// never 403: a 403 would confirm the id names a real credential belonging to
	// someone else. The composite FK refuses the same binding independently, so this
	// exists to produce the right status code, not to be the security control.
	ErrSecretNotOwned = errors.New("anthropic secret not found for this user")
	// ErrInvalidBindMode is a bind mode outside the closed set (PRD #111 M3) → 400.
	// The database CHECK (00088) refuses the same value independently; this exists so
	// the caller gets a status naming the legal modes rather than a 500 from a
	// constraint violation.
	ErrInvalidBindMode = errors.New("invalid anthropic bind mode")

	// Agent-memory sentinels (PRD #90), mapped to HTTP by the worker handlers.
	// ErrMemoryNoRepo rejects a save/read on a repo-less run (runs.repo_id is
	// nullable — a chat/self-improve run has no repo, so it has no (user,repo)
	// memory scope) → 409. ErrMemoryTooLarge is an oversize title/body → 400.
	// ErrMemoryWriteCap is the per-run write cap tripped → 429.
	ErrMemoryNoRepo   = errors.New("run has no repo for agent memory")
	ErrMemoryTooLarge = errors.New("agent memory title or body too large")
	ErrMemoryWriteCap = errors.New("per-run agent memory write cap reached")
	ErrMemoryEmpty    = errors.New("agent memory title and body must be non-empty")

	// ErrForgeNoRepo rejects a forge read on a repo-less run (runs.repo_id is
	// nullable — a chat/self-improve run has no repo, hence no forge connection to
	// read from) → 409 (PRD #158 M1). Distinct from ErrRunNotOwned (404): the run
	// exists and is held by this worker, it just has nothing to read against.
	ErrForgeNoRepo = errors.New("run has no repo for forge read")

	// ErrGuardrailBlocked is the sentinel the #66 default-branch guardrail refuses a
	// run with (D1 layer 2). Returned wrapped in a *GuardrailBlockedError so callers
	// can errors.Is it for the 422 mapping and errors.As it to read the block
	// findings.
	ErrGuardrailBlocked = errors.New("run refused by the default-branch guardrail")
)

// GuardrailBlockedError is returned by the run-create paths when the #66 default-
// branch guardrail refuses (D1 layer 2). Findings carries the block-finding
// messages for the 422 body. It wraps ErrGuardrailBlocked so callers can errors.Is
// it, and errors.As it to read Findings.
type GuardrailBlockedError struct{ Findings []string }

func (e *GuardrailBlockedError) Error() string { return ErrGuardrailBlocked.Error() }
func (e *GuardrailBlockedError) Unwrap() error { return ErrGuardrailBlocked }

// CapabilityUnmetError is returned by capabilityGate when the PRD #84 M4 4c approval gate
// blocks: Unmet carries the run's required capabilities the owning worker cannot satisfy,
// in stable capability order, for the handler's 409 body. It wraps ErrCapabilityUnmet so
// callers can errors.Is it for the status mapping and errors.As it to read Unmet — exactly
// the GuardrailBlockedError pattern above.
type CapabilityUnmetError struct{ Unmet []string }

func (e *CapabilityUnmetError) Error() string {
	return fmt.Sprintf("%s: %s", ErrCapabilityUnmet.Error(), strings.Join(e.Unmet, ", "))
}
func (e *CapabilityUnmetError) Unwrap() error { return ErrCapabilityUnmet }

// Agent-memory caps (PRD #90, OQ-C). Server-enforced (not client-trusted) and the
// single Go source of truth the SDK tool schema mirrors: at most
// MemoryMaxTitleBytes/MemoryMaxBodyBytes/MemoryMaxEvidenceBytes per entry (all
// measured as UTF-8 byte length via len() on the sanitized string),
// MemoryMaxPerRun writes per run (spam bound), and MemoryMaxPerUserRepo entries
// per (user,repo) with the oldest evicted on insert. Evidence shares the title's
// 200-byte bound: it is a single-line pointer, mirroring the client's own 200-byte
// cap (PRD #266).
const (
	MemoryMaxTitleBytes    = 200
	MemoryMaxBodyBytes     = 2048
	MemoryMaxEvidenceBytes = 200
	MemoryMaxPerRun        = 5
	MemoryMaxPerUserRepo   = 20
)

// Store is the narrow set of generated queries workersvc uses. *store.Queries
// satisfies it; tests embed it and override only the methods they exercise.
type Store interface {
	// Workers.
	CreateWorker(ctx context.Context, arg store.CreateWorkerParams) (store.Worker, error)
	GetWorkerByID(ctx context.Context, id uuid.UUID) (store.Worker, error)
	GetWorkerByIDForUser(ctx context.Context, arg store.GetWorkerByIDForUserParams) (store.Worker, error)
	ListWorkersByUser(ctx context.Context, userID uuid.UUID) ([]store.ListWorkersByUserRow, error)
	// RegisterWorker returns a Row rather than a Worker because the query is a CTE
	// (it clears the INV-5 ceiling anchor in the same round trip). The Row is
	// field-identical to store.Worker and is converted at the call site.
	RegisterWorker(ctx context.Context, arg store.RegisterWorkerParams) (store.RegisterWorkerRow, error)
	// UpsertWorkerRollHealth records a controller roll-health report (PRD #113 M4).
	// Returns rows affected: 0 means the worker is unknown or external, which the
	// query enforces rather than the caller.
	UpsertWorkerRollHealth(ctx context.Context, arg store.UpsertWorkerRollHealthParams) (int64, error)
	// GetWorkerUpgradeSummaryForUser is user-scoped BY CONSTRUCTION: roll health has no
	// user_id, so the query joins through `workers`. See the query's own comment.
	GetWorkerUpgradeSummaryForUser(ctx context.Context, userID uuid.UUID) ([]store.GetWorkerUpgradeSummaryForUserRow, error)
	HeartbeatWorker(ctx context.Context, arg store.HeartbeatWorkerParams) (store.Worker, error)
	DeleteWorkerForUser(ctx context.Context, arg store.DeleteWorkerForUserParams) (int64, error)
	// DeleteEphemeralWorkerForRun tears down the ephemeral worker bound to a now-terminal
	// run, guarded (embedded) on the worker holding no non-terminal run (PRD #529 M4).
	DeleteEphemeralWorkerForRun(ctx context.Context, arg uuid.UUID) (int64, error)
	CountWorkerNonTerminalRuns(ctx context.Context, arg store.CountWorkerNonTerminalRunsParams) (int64, error)
	MarkStaleWorkersOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)

	// Runs.
	CreateRun(ctx context.Context, arg store.CreateRunParams) (store.Run, error)
	// CI-fix runs (PRD #6).
	CreateCIFixRun(ctx context.Context, arg store.CreateCIFixRunParams) (store.Run, error)
	// MR review-watcher rework runs (PRD #700 M3): the create path + its create-time
	// cross-kind branch guard.
	CreateAutoMRReworkRun(ctx context.Context, arg store.CreateAutoMRReworkRunParams) (store.Run, error)
	// Self-improvement runs (PRD #46 Decision 10).
	CreateSelfImproveRun(ctx context.Context, arg store.CreateSelfImproveRunParams) (store.Run, error)
	// Scheduled prompt runs (PRD #241).
	CreatePromptRun(ctx context.Context, arg store.CreatePromptRunParams) (store.Run, error)
	// Task/handoff runs (PRD #400).
	CreateTaskRun(ctx context.Context, arg store.CreateTaskRunParams) (store.Run, error)
	// DispatchTaskRun stamps a task run's dispatch gate after the CLI seeds its branch
	// (PRD #400 Decision 6), making it claimable; 0 rows → pgx.ErrNoRows.
	DispatchTaskRun(ctx context.Context, arg store.DispatchTaskRunParams) (store.Run, error)
	// Task-review runs + their persisted findings (PRD #400 M4a): the auto-enqueued
	// diff-review run, its review-run-scoped POST authz, the atomic header+findings
	// upsert, and the CLI/panel read side.
	CreateTaskReviewRun(ctx context.Context, arg store.CreateTaskReviewRunParams) (store.Run, error)
	// CreateThenFixRun inserts the chained fix run for a --then-fix handoff (PRD #400 M5):
	// a NORMAL task (review_target_run_id NULL) on the original's branch; 23505 → the
	// one-active-fix-per-target index tripped.
	CreateThenFixRun(ctx context.Context, arg store.CreateThenFixRunParams) (store.Run, error)
	GetActiveTaskReviewRunForWorkerTarget(ctx context.Context, arg store.GetActiveTaskReviewRunForWorkerTargetParams) (store.Run, error)
	UpsertTaskReviewWithFindings(ctx context.Context, arg store.UpsertTaskReviewWithFindingsParams) (uuid.UUID, error)
	GetTaskReviewForTarget(ctx context.Context, targetRunID uuid.UUID) (store.TaskReview, error)
	ListTaskReviewFindings(ctx context.Context, reviewID uuid.UUID) ([]store.TaskReviewFinding, error)
	CountActiveRunsWithBranch(ctx context.Context, arg store.CountActiveRunsWithBranchParams) (int64, error)
	CountActiveCIFixForRef(ctx context.Context, arg store.CountActiveCIFixForRefParams) (int64, error)
	GetRunByIDForUser(ctx context.Context, arg store.GetRunByIDForUserParams) (store.Run, error)
	GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error)
	// GetRunMilestoneFreezeSnapshot reads the live milestone freeze state at the approve
	// instant so submitApproval can log what CreateApprovePlanInput saw (issue #260).
	GetRunMilestoneFreezeSnapshot(ctx context.Context, id uuid.UUID) (store.GetRunMilestoneFreezeSnapshotRow, error)
	ListRunsForUser(ctx context.Context, arg store.ListRunsForUserParams) ([]store.ListRunsForUserRow, error)
	ListActiveRunsAll(ctx context.Context, backgroundGraceCutoff pgtype.Timestamptz) ([]store.ListActiveRunsAllRow, error)
	ListAllWorkers(ctx context.Context) ([]store.ListAllWorkersRow, error)
	GetRunOwnedByWorker(ctx context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error)
	// GetRunForgeConnForWorker returns the forge connection facts for a run the
	// worker holds (PRD #158 M1): forge_project_id + the connection. Worker-scoped
	// by construction (its predicate carries r.worker_id), so a run the worker does
	// not hold — or a repo-less run — returns pgx.ErrNoRows.
	GetRunForgeConnForWorker(ctx context.Context, arg store.GetRunForgeConnForWorkerParams) (store.GetRunForgeConnForWorkerRow, error)
	ClaimRun(ctx context.Context, arg store.ClaimRunParams) (store.Run, error)
	GetRunClaimContext(ctx context.Context, runID uuid.UUID) (store.GetRunClaimContextRow, error)
	// Run judge (PRD #46 M3): terminal-funnel enqueue, judge-run-scoped trace/review
	// authz, the command-not-found scan input, and the review upsert.
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	CreateJudgeRun(ctx context.Context, arg store.CreateJudgeRunParams) (store.Run, error)
	// Per-user judge spend guards (PRD #69 M5 Decision 9, Gate 5). Read-only count
	// queries feeding the cooldown and daily-budget backstops in maybeEnqueueJudge.
	// LastJudgeEnqueuedAt returns a NULLABLE timestamp (NULL ⇒ user never judged).
	LastJudgeEnqueuedAt(ctx context.Context, userID uuid.UUID) (pgtype.Timestamptz, error)
	CountJudgesSince(ctx context.Context, arg store.CountJudgesSinceParams) (int64, error)
	GetActiveJudgeRunForWorkerTarget(ctx context.Context, arg store.GetActiveJudgeRunForWorkerTargetParams) (store.Run, error)
	ListToolTraceForRun(ctx context.Context, arg store.ListToolTraceForRunParams) ([]store.ListToolTraceForRunRow, error)
	// ListKnownImproveUziTargetsForUser is the owner's existing improve_uzi target menu
	// carried on a judge claim (issue #232): frequency-ranked, canonical-deduped, capped,
	// so the judge reuses an exact coordinate instead of inventing new phrasing.
	ListKnownImproveUziTargetsForUser(ctx context.Context, arg store.ListKnownImproveUziTargetsForUserParams) ([]string, error)
	// RecentSelfImproveMRRunsForRepo is the repo's recent MR-bearing self_improve runs,
	// bounded (PRD #686 D12). Feeds the self_improve claim's open-MR picker context (D11):
	// each candidate's open-state is resolved LIVE from the forge, never from runs.mr_state.
	RecentSelfImproveMRRunsForRepo(ctx context.Context, arg store.RecentSelfImproveMRRunsForRepoParams) ([]store.RecentSelfImproveMRRunsForRepoRow, error)
	ListRunInputsForRun(ctx context.Context, arg store.ListRunInputsForRunParams) ([]store.RunUserInput, error)
	UpsertRunReviewWithRecommendations(ctx context.Context, arg store.UpsertRunReviewWithRecommendationsParams) (uuid.UUID, error)
	// Judge review read side (PRD #46 M4): the run-page verdict + recommendations panel.
	GetRunReviewForTarget(ctx context.Context, targetRunID uuid.UUID) (store.RunReview, error)
	// The judge run's timing + usage for the reviewed run's panel (PRD #69 M6): the
	// judge run's claim/start/finish stamps and its run_usage_totals rollup, NULL usage
	// for a pre-feature judge that posted no result frame.
	GetJudgeRunUsageForTarget(ctx context.Context, targetRunID uuid.UUID) (store.GetJudgeRunUsageForTargetRow, error)
	// The ACTIVE judge run for a target (PRD #119 M1): the panel's pending-judge signal,
	// on the same owner-or-admin gate as the review read. Its predicate is the
	// one-active-judge-per-target index's partial WHERE with the indexed column
	// (target_run_id) spelled out as an equality, so "pending" and "a click would 23505"
	// are the same set of states.
	GetActiveJudgeRunForTarget(ctx context.Context, targetRunID pgtype.UUID) (store.GetActiveJudgeRunForTargetRow, error)
	ListRecommendationsForReview(ctx context.Context, reviewID uuid.UUID) ([]store.ReviewRecommendation, error)
	ListFiledIssuesForReview(ctx context.Context, reviewID uuid.UUID) ([]store.RecommendationFiledIssue, error)
	// Judge review triage (PRD #94 M1): the coordinate-keyed disposition upsert/undo,
	// the per-review list, and the global flat join feeding the Go bucketer.
	UpsertRecommendationDisposition(ctx context.Context, arg store.UpsertRecommendationDispositionParams) (store.RecommendationDisposition, error)
	// SystemDismissDeniedCLIRecommendation is the deterministic net (issue #167): a
	// non-clobbering (ON CONFLICT DO NOTHING) system auto-dismissal of a denylisted-CLI
	// recommendation, stamped set_via='denied_cli'. Distinct from the human upsert above.
	SystemDismissDeniedCLIRecommendation(ctx context.Context, arg store.SystemDismissDeniedCLIRecommendationParams) (int64, error)
	DeleteRecommendationDisposition(ctx context.Context, arg store.DeleteRecommendationDispositionParams) (int64, error)
	ListDispositionsForReview(ctx context.Context, reviewID uuid.UUID) ([]store.RecommendationDisposition, error)
	ListJudgeTriageRowsForUser(ctx context.Context, userID uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error)
	// Judge menu grouped read (PRD #98 M1): the wider per-recommendation join the
	// (category, target) dedup groups. Same spine as the triage row above, plus the
	// runs join, the verdict/confidence/filed projection, the pushed-down ?run= anchor
	// and a hard row cap.
	ListJudgeRecommendationRowsForUser(ctx context.Context, arg store.ListJudgeRecommendationRowsForUserParams) ([]store.ListJudgeRecommendationRowsForUserRow, error)
	// Judge menu bulk-disposition resolve (PRD #98 M2): the owner-scoped lookup of a set
	// of (category, target) coordinates' member recommendations. It is the security
	// boundary of the fan-out — the disposition is written off the rows it returns, never
	// off the request body.
	// /runs judge badge (PRD #98 M4): per-recommendation triage facts for the runs on one
	// page, bucketed in Go by the shared BucketOf — never counted in SQL.
	ListJudgeTriageRowsForRuns(ctx context.Context, arg store.ListJudgeTriageRowsForRunsParams) ([]store.ListJudgeTriageRowsForRunsRow, error)
	// /runs + board plan-revise flag (issue #750): the plan-ish message rows for a page of
	// runs, folded into the is_revising set in Go by planRevisingSet — never in SQL.
	ListPlanRevisionStateForRuns(ctx context.Context, runIds []uuid.UUID) ([]store.ListPlanRevisionStateForRunsRow, error)
	ListOwnedRecommendationsForCoords(ctx context.Context, arg store.ListOwnedRecommendationsForCoordsParams) ([]store.ListOwnedRecommendationsForCoordsRow, error)
	// The fan-out write itself: ONE multi-row upsert over the RESOLVED coordinates, so a
	// bulk call is a single round-trip that cannot half-apply (PRD #98 M2, audit NB-A).
	UpsertDispositionsForResolvedCoords(ctx context.Context, arg store.UpsertDispositionsForResolvedCoordsParams) (int64, error)
	// Incidental findings capture (PRD #333 M2): the per-run evidence insert + cap
	// count, and the coordinate-keyed `open` disposition upsert with its two follow-up
	// UPDATEs (re-open on a materially-different content_hash; refresh an already-open
	// row's last_title/hash). CreateFinding derives (user_id, repo_id) from the claimed
	// run and canonicalises + sanitises the text before any of these run.
	CountFindingsForRun(ctx context.Context, runID uuid.UUID) (int64, error)
	// The Findings backlog read (PRD #333 M4, D7/D8): the disposition-driven,
	// coordinate-deduped backlog and the D8 open-findings meta count. FindingsBacklog
	// assembles the DTO from these.
	ListFindingsBacklog(ctx context.Context, arg store.ListFindingsBacklogParams) ([]store.ListFindingsBacklogRow, error)
	CountOpenFindingsForUser(ctx context.Context, arg store.CountOpenFindingsForUserParams) (int64, error)
	InsertFinding(ctx context.Context, arg store.InsertFindingParams) (store.IncidentalFinding, error)
	UpsertOpenDisposition(ctx context.Context, arg store.UpsertOpenDispositionParams) (store.FindingDisposition, error)
	ReopenDispositionOnHashMismatch(ctx context.Context, arg store.ReopenDispositionOnHashMismatchParams) (int64, error)
	UpdateDispositionLastTitle(ctx context.Context, arg store.UpdateDispositionLastTitleParams) (int64, error)
	SetRunRunning(ctx context.Context, arg store.SetRunRunningParams) (int64, error)
	// ClearRunMilestonesCompleted (PRD #628 M4) is the only non-union writer of
	// milestones_completed: it resets the column to empty on a cross-worker re-claim that
	// reseeds from the DEFAULT branch (no committed work recovered), so pass-1's stale
	// milestones don't read as "done" while pass-2 re-implements them. Ownership+status
	// guarded in SQL; runs immediately BEFORE SetRunRunning in the running arm so the union
	// refills from empty. Returns rows-affected (0 when the ownership/status guard refuses).
	ClearRunMilestonesCompleted(ctx context.Context, arg store.ClearRunMilestonesCompletedParams) (int64, error)
	SetRunAwaitingApproval(ctx context.Context, arg store.SetRunAwaitingApprovalParams) (int64, error)
	// Plain-English run summaries (PRD #362 M1). Intent is a plain UPDATE (the
	// idempotent-on-set decision lives in the service); the plan write carries the
	// Decision 3 stale-write guard (updates only if plan_md still matches), returning
	// rows-affected so the service detects a stale (0-row) write.
	SetRunIntentSummary(ctx context.Context, arg store.SetRunIntentSummaryParams) (int64, error)
	SetRunPlanSummary(ctx context.Context, arg store.SetRunPlanSummaryParams) (int64, error)
	// SetRunAwaitingInput parks a run on a clarification question (PRD #88 M1) and
	// stamps the question's identity. It clears health on entry, which is what makes
	// leaving `awaiting_input` out of ListActiveRunsForHealth safe — see the query.
	SetRunAwaitingInput(ctx context.Context, arg store.SetRunAwaitingInputParams) (int64, error)
	// SetRunAwaitingFollowup parks an interactive task run in-process after signal_done
	// (PRD #517 M2/M3, Decision 3), holding the worker slot/clone/session for `uzi run
	// follow-up` to resume. Like SetRunAwaitingInput it clears health on entry (the same
	// reason awaiting_followup is safe to leave out of ListActiveRunsForHealth).
	SetRunAwaitingFollowup(ctx context.Context, arg store.SetRunAwaitingFollowupParams) (int64, error)
	SetRunCompleted(ctx context.Context, arg store.SetRunCompletedParams) (int64, error)
	SetRunFailed(ctx context.Context, arg store.SetRunFailedParams) (int64, error)
	ReconcileRunMR(ctx context.Context, arg store.ReconcileRunMRParams) (int64, error)
	// SetRunLimitWait parks a run until the owner's Anthropic usage window reopens
	// (PRD #35); PromoteLimitWaitRuns is the sweeper pass that brings it back. The
	// park's source guard is POSITIVE (status = 'running'), unlike every sibling
	// above, so a re-delivered or out-of-order report is a 0-row no-op rather than a
	// second park.
	SetRunLimitWait(ctx context.Context, arg store.SetRunLimitWaitParams) (int64, error)
	PromoteLimitWaitRuns(ctx context.Context, now pgtype.Timestamptz) ([]store.PromoteLimitWaitRunsRow, error)
	// The reactive-resume pass (PRD #754 M5): ListPoolWaitRuns is the pool_wait worklist
	// (oldest first), and PromotePoolWaitRun promotes ONE held run (pool_wait → queued),
	// owner-scoped so the sweeper passes each run's own user_id and resume-now cannot
	// promote a foreign run.
	ListPoolWaitRuns(ctx context.Context) ([]store.ListPoolWaitRunsRow, error)
	PromotePoolWaitRun(ctx context.Context, arg store.PromotePoolWaitRunParams) (int64, error)
	MarkRunFailedByID(ctx context.Context, arg store.MarkRunFailedByIDParams) (int64, error)
	CancelRunServerSide(ctx context.Context, arg store.CancelRunServerSideParams) (int64, error)
	// CancelRunByWorker is the LIVE-worker cancel transition (PRD #503 M1). SetState's
	// failed arm routes here (instead of SetRunFailed) when the loaded run carries
	// stop_kind='cancelled', so an operator cancel ends 'cancelled'/NULL fail_origin —
	// converging with CancelRunServerSide — rather than being mis-classified as
	// agent_failure. Worker-scoped because SetState holds a worker, not a user.
	CancelRunByWorker(ctx context.Context, arg store.CancelRunByWorkerParams) (int64, error)
	RejectRunServerSide(ctx context.Context, arg store.RejectRunServerSideParams) (int64, error)
	// FailRunAutoStop is the server-side half of PRD #108 M5's auto-stop. Unlike
	// its two neighbours it takes no user_id: it is driven by the sweeper, not by a
	// request from a user whose ownership must be proven.
	FailRunAutoStop(ctx context.Context, arg store.FailRunAutoStopParams) (int64, error)
	UpdateRunLastSeq(ctx context.Context, arg store.UpdateRunLastSeqParams) (int64, error)

	// Sweeper + register-time orphan recovery.
	SweepClaimedNeverStarted(ctx context.Context, cutoff pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error)
	// RequeueClaimedRunToQueued resets one just-claimed run to queued when its
	// owner's vault locked between the claim gate and the token open (PRD #32 M3).
	RequeueClaimedRunToQueued(ctx context.Context, id uuid.UUID) (int64, error)
	// SetRunPoolWait holds one just-claimed `auto` run whose token pool is genuinely
	// empty (PRD #754 M4). claimed → pool_wait, a non-locking hold; positive source
	// guard (status='claimed'), keeps worker_id for affinity, and does NOT touch the
	// usage-limit budget (an empty pool is not a usage park). M5 resumes it.
	SetRunPoolWait(ctx context.Context, arg store.SetRunPoolWaitParams) (int64, error)
	// HasActiveRunForIssue reports whether the issue already has a non-terminal run.
	// CreateRun uses it as the manual-path dedup pre-check that replaces the
	// uq_runs_one_active_per_issue index for pool_wait runs (PRD #754 M4 Decision 8):
	// the index no longer counts a held run as active, so the structural backstop it
	// gave the manual/board/Slack path is restored here (this SELECT still counts
	// pool_wait as active).
	HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error)
	SweepRunningTimeout(ctx context.Context, arg store.SweepRunningTimeoutParams) ([]store.SweepRunningTimeoutRow, error)
	FailRunsOfStaleWorkersOverCap(ctx context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) ([]store.FailRunsOfStaleWorkersOverCapRow, error)
	RequeueRunsOfStaleWorkers(ctx context.Context, arg store.RequeueRunsOfStaleWorkersParams) ([]store.RequeueRunsOfStaleWorkersRow, error)
	FailWorkerRunsOverCap(ctx context.Context, arg store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error)
	RequeueWorkerRuns(ctx context.Context, arg store.RequeueWorkerRunsParams) (int64, error)

	// Run-health detector (PRD #47): the per-tick active-run scan, the per-running-run
	// tool window (loop + in-flight), the single health writer, and the queued-run
	// worker-online count.
	ListActiveRunsForHealth(ctx context.Context) ([]store.ListActiveRunsForHealthRow, error)
	ListRunToolWindow(ctx context.Context, arg store.ListRunToolWindowParams) ([]store.ListRunToolWindowRow, error)
	SetRunHealth(ctx context.Context, arg store.SetRunHealthParams) (int64, error)
	CountOnlineWorkersForUser(ctx context.Context, userID uuid.UUID) (int64, error)
	CountOnlineWorkersWithFreeSlotForUser(ctx context.Context, userID uuid.UUID) (int64, error)
	// CountOnlineWorkersSatisfyingCaps backs PRD #84 M3: a queued run whose
	// required_capabilities are not a subset of ANY online worker's effective caps gets
	// a capability-specific "no eligible worker" reason. A per-run lookup like the two
	// counts above, and off the hot path for the same reason — it runs only for a queued
	// run already past its health threshold.
	CountOnlineWorkersSatisfyingCaps(ctx context.Context, arg store.CountOnlineWorkersSatisfyingCapsParams) (int64, error)
	// CountOnlineEligibleWorkersForRepo backs PRD #361's queued Docker-allowlist reason:
	// how many of the caller's online workers fn_worker_can_claim accepts for this repo/kind,
	// ignoring availability (free slots AND draining). Since issue #512 M2 it is capability-
	// aware (it threads the run's required_capabilities and the capability-aware flag through
	// fn_worker_can_claim), not just the pure docker→allowlist notion. It is an ELIGIBILITY
	// count, not an availability one — draining workers still count, so an all-draining roll
	// is not misattributed to the allowlist. A result of 0 with capable workers present means
	// the docker+capability fence persistently blocks the run (an all-Docker fleet fenced from
	// a non-allowlisted repo); the draining/busy axis is handled by CountOnlineWorkersWithFreeSlotForUser.
	CountOnlineEligibleWorkersForRepo(ctx context.Context, arg store.CountOnlineEligibleWorkersForRepoParams) (int64, error)
	// RunHasVerdictSinceGateOpened backs issue #182: an awaiting_approval run whose
	// owner already answered THIS gate reports waiting_worker rather than
	// approval_idle. A per-run lookup like ListRunToolWindow above, and for the same
	// reason — it runs only for runs already past the approval threshold.
	RunHasVerdictSinceGateOpened(ctx context.Context, arg store.RunHasVerdictSinceGateOpenedParams) (bool, error)
	// RunPriorityClassForRun backs PRD #320 D9: a queued run demoted by the
	// kind-derived priority reports a deprioritized/restored reason instead of the
	// generic wait. A per-run lookup like RunHasVerdictSinceGateOpened above, and for
	// the same reason — it runs only for runs already past the queued threshold.
	RunPriorityClassForRun(ctx context.Context, arg store.RunPriorityClassForRunParams) (string, error)

	// Messages + inputs.
	InsertRunMessage(ctx context.Context, arg store.InsertRunMessageParams) (int64, error)
	ListRunMessagesAfter(ctx context.Context, arg store.ListRunMessagesAfterParams) ([]store.RunMessage, error)
	ListRunMessagesAfterPage(ctx context.Context, arg store.ListRunMessagesAfterPageParams) ([]store.RunMessage, error)
	// UpsertRunUsage folds a delivered result frame's per-model usage into
	// run_usage (PRD #40 M2), GREATEST-merged so re-delivery never regresses.
	UpsertRunUsage(ctx context.Context, arg store.UpsertRunUsageParams) error
	// BumpRunLineageEpoch increments a run's lineage-epoch counter by one (PRD
	// #632), called once per newly-inserted resume_lineage_break status event so a
	// fresh SDK leg's run_usage rows are stamped with a higher epoch than the prior.
	BumpRunLineageEpoch(ctx context.Context, id uuid.UUID) error
	// Usage read rollups (PRD #40 M3), all over the run_usage_totals view.
	GetRunUsageTotal(ctx context.Context, runID uuid.UUID) (store.GetRunUsageTotalRow, error)
	SelfUsage(ctx context.Context, userID uuid.UUID) (store.SelfUsageRow, error)
	AdminUsageTotals(ctx context.Context) (store.AdminUsageTotalsRow, error)
	AdminUsagePerUser(ctx context.Context) ([]store.AdminUsagePerUserRow, error)
	CreateRunInput(ctx context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error)
	// CountRunReviseInputs is the read-only reporting view of the PRD #41 plan-revision
	// cap: all revise_plan rows for the run (no consumed_at filter), so a consumed
	// revise still counts. Enforcement itself rides CreateRunReviseInputIfUnderCap.
	CountRunReviseInputs(ctx context.Context, runID uuid.UUID) (int64, error)
	// CreateRunReviseInputIfUnderCap atomically enqueues a revise_plan only while the
	// run is under PlanMaxRevisions (PRD #41): the cap predicate rides the UPDATE that
	// bumps runs.revise_count, so concurrent submits can't both exceed the cap (#106).
	// pgx.ErrNoRows = capped.
	CreateRunReviseInputIfUnderCap(ctx context.Context, arg store.CreateRunReviseInputIfUnderCapParams) (store.RunUserInput, error)
	// ListFollowUpInputsForRun backs the web/CLI steer queue (PRD #95): kind='follow_up'
	// only, newest-first, uncapped. Owner-scoped by the run resolve, not the query.
	ListFollowUpInputsForRun(ctx context.Context, runID uuid.UUID) ([]store.RunUserInput, error)
	CreateStopVerdictInput(ctx context.Context, arg store.CreateStopVerdictInputParams) (store.RunUserInput, error)
	// CreateScopeCeilingInput sets runs.scope_ceiling AND writes the kind='scope' audit row
	// in one statement (PRD #634 M2). The audit row is excluded from ConsumeRunInputs; the
	// control travels as runs.scope_ceiling on the ACK/claim.
	CreateScopeCeilingInput(ctx context.Context, arg store.CreateScopeCeilingInputParams) (store.RunUserInput, error)
	// SettleScopeInputDisposition settles the still-pending scope audit row(s) at
	// completion (PRD #634 M4). Idempotent (WHERE disposition IS NULL); never overwrites an
	// already-settled ('superseded'/'applied') row.
	SettleScopeInputDisposition(ctx context.Context, arg store.SettleScopeInputDispositionParams) (int64, error)
	// CreateApprovePlanInput enqueues an approve_plan AND records the run's agent
	// selection atomically (PRD #37).
	CreateApprovePlanInput(ctx context.Context, arg store.CreateApprovePlanInputParams) (store.RunUserInput, error)
	// ClearRunRequiredCapabilities wipes a run's inferred/hinted required_capabilities
	// set (PRD #84 M4 4c), owner- and awaiting_approval-scoped. Backs the "run without the
	// capability" user override so a capability-gated approve is no longer fenced.
	ClearRunRequiredCapabilities(ctx context.Context, arg store.ClearRunRequiredCapabilitiesParams) (int64, error)
	// CreateRunAnswerInput enqueues an `answer` and records the question it answers
	// (PRD #88 M1). The question id is a column, not a JSON field, because
	// SetRunRunning's resume guard compares it in SQL.
	CreateRunAnswerInput(ctx context.Context, arg store.CreateRunAnswerInputParams) (store.RunUserInput, error)
	ConsumeRunInputs(ctx context.Context, runID uuid.UUID) ([]store.ConsumeRunInputsRow, error)

	// Agent memory (PRD #90): the worker-facing write/read of the per-(user,repo)
	// cross-run store. Identity is derived from the run claim, never the body; the
	// per-run write cap and the oldest-eviction that keep the set bounded are
	// enforced in the store service around these calls.
	InsertAgentMemory(ctx context.Context, arg store.InsertAgentMemoryParams) (store.AgentMemory, error)
	ListAgentMemoryForUserRepo(ctx context.Context, arg store.ListAgentMemoryForUserRepoParams) ([]store.AgentMemory, error)
	CountAgentMemoryForRun(ctx context.Context, runID pgtype.UUID) (int64, error)
	EvictAgentMemoryOverCap(ctx context.Context, arg store.EvictAgentMemoryOverCapParams) error

	// Chat runs (PRD #39): a third run kind riding the run machinery. The chat
	// claim lane (ClaimChatRun) is disjoint from ClaimRun (which now excludes chat);
	// GetChatRunClaimContext carries only the resume session (no repo/forge join).
	CreateChatRun(ctx context.Context, arg store.CreateChatRunParams) (store.Run, error)
	CreateChatContinueRun(ctx context.Context, arg store.CreateChatContinueRunParams) (store.Run, error)
	ListChatRunsForUser(ctx context.Context, userID uuid.UUID) ([]store.ListChatRunsForUserRow, error)
	GetLiveChatForUser(ctx context.Context, userID uuid.UUID) (store.Run, error)
	ClaimChatRun(ctx context.Context, arg store.ClaimChatRunParams) (store.Run, error)
	GetChatRunClaimContext(ctx context.Context, runID uuid.UUID) (pgtype.Text, error)
	CountChatFollowUps(ctx context.Context, runID uuid.UUID) (int64, error)
	SweepIdleChatRuns(ctx context.Context, cutoff pgtype.Timestamptz) ([]store.SweepIdleChatRunsRow, error)
	GetChatProposalForConfirm(ctx context.Context, arg store.GetChatProposalForConfirmParams) (store.GetChatProposalForConfirmRow, error)
	MarkProposalConfirmed(ctx context.Context, arg store.MarkProposalConfirmedParams) (store.IssueProposal, error)
	MarkProposalDismissed(ctx context.Context, id uuid.UUID) (store.IssueProposal, error)
	// M3 (PRD #39): proposal creation (worker) + claim-first confirm + the worker's
	// user_id-scoped chat read surface.
	CreateIssueProposal(ctx context.Context, arg store.CreateIssueProposalParams) (store.IssueProposal, error)
	CountPendingProposalsForRun(ctx context.Context, runID uuid.UUID) (int64, error)
	ClaimProposalForConfirm(ctx context.Context, arg store.ClaimProposalForConfirmParams) (store.ClaimProposalForConfirmRow, error)
	RevertProposalToPending(ctx context.Context, id uuid.UUID) (int64, error)
	SweepStuckConfirmingProposals(ctx context.Context, cutoff pgtype.Timestamptz) ([]uuid.UUID, error)
	GetRepoIDByPathForUser(ctx context.Context, arg store.GetRepoIDByPathForUserParams) (uuid.UUID, error)
	ListRunsForWorkerUser(ctx context.Context, arg store.ListRunsForWorkerUserParams) ([]store.ListRunsForWorkerUserRow, error)
	GetRunForWorkerUser(ctx context.Context, arg store.GetRunForWorkerUserParams) (store.GetRunForWorkerUserRow, error)
	ListRunMessagesForWorkerPage(ctx context.Context, arg store.ListRunMessagesForWorkerPageParams) ([]store.RunMessage, error)

	// Cross-cutting reads for run creation + claim.
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	GetIssueByIID(ctx context.Context, arg store.GetIssueByIIDParams) (store.Issue, error)
	ListBoardColumns(ctx context.Context, repoID uuid.UUID) ([]store.BoardColumn, error)
	GetUserSecretCiphertext(ctx context.Context, arg store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error)
	GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error)
	// Per-run credential attribution (PRD #111 M1): the two owner-scoped metadata
	// reads that give openAnthropic the id + label to record, and the write that
	// records them. Both reads return the label ALONGSIDE the id from one row —
	// resolving the id and then looking its label up separately would be an
	// unscoped cross-tenant read, so there is deliberately no such query.
	GetDefaultUserSecretMeta(ctx context.Context, arg store.GetDefaultUserSecretMetaParams) (store.GetDefaultUserSecretMetaRow, error)
	GetUserSecretMetaByID(ctx context.Context, arg store.GetUserSecretMetaByIDParams) (store.GetUserSecretMetaByIDRow, error)
	SetRunAnthropicSecret(ctx context.Context, arg store.SetRunAnthropicSecretParams) (int64, error)
	// Auto-selection (PRD #111 M4): every anthropic_token the user holds, with its
	// gauge reading and in-flight run count. NOT pre-filtered on auto_eligible — the
	// eligibility gate lives entirely in autoselect.Classify (D21), and the ranker
	// needs the unfiltered set to tell "you pooled nothing" from "you pooled tokens
	// that are all stale".
	ListAutoSelectCandidates(ctx context.Context, userID uuid.UUID) ([]store.ListAutoSelectCandidatesRow, error)
	// Park-time exhaustion writes (PRD #217 M1): mark the dead credential's named
	// window 100% consumed with source='limit_report', touching that one *_pct column
	// and nothing else. UPDATE-only, so a zero-row result is success (the token was
	// never polled). See the queries' own comments for why synced_at and the reset
	// columns are deliberately left alone (D3/D4).
	MarkFiveHourExhausted(ctx context.Context, userSecretID uuid.UUID) (int64, error)
	MarkSevenDayExhausted(ctx context.Context, userSecretID uuid.UUID) (int64, error)
	// Worker → token binding (PRD #104 M3): label resolution for the mint-time and
	// CLI-facing forms, and the id-keyed rebind itself.
	GetUserSecretIDByLabel(ctx context.Context, arg store.GetUserSecretIDByLabelParams) (uuid.UUID, error)
	SetWorkerAnthropicSecret(ctx context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error)
	// Judge-lane → token binding (PRD #104 M4): read at judge-claim time, written by
	// PUT /api/me/judge.
	GetUserJudgeAnthropicSecret(ctx context.Context, id uuid.UUID) (pgtype.UUID, error)
	SetUserJudgeAnthropicSecret(ctx context.Context, arg store.SetUserJudgeAnthropicSecretParams) (store.User, error)
	GetUserDefaultModel(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	// Per-user default reasoning effort (PRD #617): read at issue- and chat-run
	// claim assembly, keyed on the run owner. NULL ⇒ inherit (worker omits the SDK
	// effort key, so the SDK default `high` applies).
	GetUserDefaultEffort(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	// Per-user judge model override (PRD #69 M2): read at judge-claim assembly,
	// keyed on the run owner. NULL ⇒ inherit the instance judge_model.
	GetUserJudgeModel(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	// Per-user run-summary model override (PRD #362 M2): read at ISSUE-run claim
	// assembly, keyed on the run owner. NULL ⇒ inherit the instance summary_model.
	GetUserSummaryModel(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	// ListClaimAgentTemplates resolves template allocations for the run owner
	// (PRD #18 M7): only the builtin/global defaults ± the owner's overlay + the
	// owner's own allocated user templates ride the claim, not every template.
	ListClaimAgentTemplates(ctx context.Context, userID pgtype.UUID) ([]store.AgentTemplate, error)
	ListRunSkillAllocations(ctx context.Context, userID pgtype.UUID) ([]store.ListRunSkillAllocationsRow, error)
	// Tier-1 tool provisioning (PRD #18 M4): the run owner's per-repo package list
	// and the admin allowlist it is re-validated against at claim time.
	GetRepoToolProfile(ctx context.Context, arg store.GetRepoToolProfileParams) (store.RepoToolProfile, error)
	ListToolAllowlist(ctx context.Context) ([]store.ToolAllowlist, error)
}

// Params are the runtime knobs the service needs, mirrored from config.
type Params struct {
	RunTimeout     time.Duration
	RunIdleTimeout time.Duration
	// WorkerTaskIdleTimeout (PRD #517 M5, WORKER_TASK_IDLE_TIMEOUT) is the interactive-task
	// park's worker-side idle backstop. Mirrored from config and shipped in the claim (like
	// RunIdleTimeout) so the worker's own park idle timer matches what the server configured
	// (no drift). Delivered only on an interactive task claim; every other claim omits it.
	WorkerTaskIdleTimeout time.Duration
	RunMaxIterations      int
	// HandoffRunTimeout / HandoffRunMaxIterations (issue #785) are the dedicated default
	// wall-clock budget and iteration cap for a NON-interactive `uzi handoff` task run,
	// mirrored from config. Persisted on the run at create; interactive handoffs get NULL.
	HandoffRunTimeout       time.Duration
	HandoffRunMaxIterations int
	// PlanMaxRevisions caps how many times a run's plan may be revised at the
	// approval gate (PRD #41, PLAN_MAX_REVISIONS). Enforced server-side in
	// SubmitInput and shipped in the claim so the worker enforces the same limit.
	PlanMaxRevisions int
	// QuestionMax and QuestionTimeoutSeconds bound the PRD #88 clarification loop.
	// Both are enforced WORKER-side (a question is an in-process ask_user signal, so
	// unlike a revise_plan there is no input row for the server to count) but
	// CONFIGURED here and shipped in the claim, following plan_max_revisions rather
	// than a worker env var: controller/internal/kube/render.go renders exactly one
	// tuning var into a hosted worker pod, so an env knob would be unreachable on k8s.
	QuestionMax            int
	QuestionTimeoutSeconds int
	RunMaxRequeues         int
	WorkerHeartbeatStale   time.Duration
	WorkerAffinityGrace    time.Duration
	// WorkerAffinityCeiling (PRD #628 D3a): the run-lane affinity ceiling. ClaimRun pins
	// a promoted run to its prior worker only while that worker is a live, non-draining
	// claim target (the liveness leg); this ceiling bounds the live-but-wedged case. It is
	// the run lane's @affinity_cutoff, distinct from WorkerAffinityGrace which stays the
	// chat lane's grace (ClaimChatRun gets no liveness short-circuit in M1's scope).
	WorkerAffinityCeiling time.Duration
	// WorkerSpreadGrace (PRD #216): a queued run older than this is exempt from the
	// fleet-aware spread (fail-open), so a run can never be stranded by deferral.
	WorkerSpreadGrace time.Duration
	// WorkerBackgroundGrace (PRD #320): a demoted (judge/self_improve) run older than
	// this fails open to normal priority in ClaimRun's ORDER BY, so background work
	// can never be starved by interactive runs (minutes-scale run-age fail-open).
	WorkerBackgroundGrace time.Duration
	// ClaimGrace is the claimed-but-never-started reclaim window. It is not a
	// PRD env var (the PRD fixes it at 5m in prose); defaulted in New.
	ClaimGrace time.Duration
	// SkillMaxBytes / SkillsMaxPerRun are the skill caps (PRD #16), mirrored from
	// config. SkillsMaxPerRun bounds the per-run union at claim assembly; both ride
	// the claim so the worker enforces the same limits (no server/worker drift).
	SkillMaxBytes   int
	SkillsMaxPerRun int

	// Chat lifecycle knobs (PRD #39 Decision 3). ChatIdleTimeout is the SERVER idle
	// backstop the sweep applies (complete a chat whose last message is older than
	// this). ChatMaxTurns is the server-enforced turn cap AND is delivered in the
	// chat claim so the worker enforces the same value. WorkerChatIdleTimeout /
	// WorkerChatTurnTimeout ride the chat claim so the worker's own idle + per-turn
	// wall-clocks match what the server configured.
	ChatIdleTimeout       time.Duration
	ChatMaxTurns          int
	WorkerChatIdleTimeout time.Duration
	WorkerChatTurnTimeout time.Duration
	// ProposalConfirmStuckTimeout bounds how long an issue proposal may sit in the
	// transient 'confirming' state before the sweep reverts it to pending (M3): the
	// recovery for a confirm handler killed mid-flight. Must sit above the forge HTTP
	// timeout so a legitimately in-flight confirm is never reaped. 0 disables the sweep.
	ProposalConfirmStuckTimeout time.Duration
	// AutoStopEnabled is the operator kill switch for PRD #108 M5's auto-stop
	// (UZI_AUTOSTOP_ENABLED, default true), read once at boot.
	//
	// It is deliberately NOT the health toggle (Decision 8: an admin disabling
	// health must not silently disable loop protection) and deliberately NOT a
	// settings key: an automatic destructive behaviour needs an off switch that does
	// not depend on the database it might be misbehaving against. Same shape as
	// Phase 1's UZI_HOME_RECLAIM. It is a runtime escape hatch, NOT the PRD's "ship
	// M4, hold M5" fallback — see config.go for why that framing was retracted.
	//
	// NOTE the zero value is FALSE, so a Params literal that omits it has auto-stop
	// OFF. That is the fail-safe direction and it is why the default lives in
	// config.go (where the env is read) rather than in New.
	AutoStopEnabled bool

	// Autoselect is the operator-set auto-selection policy (PRD #111 D6/M4), built
	// by config.Config.AutoselectPolicy — the SAME constructor handler.autoStatus
	// reads. That sharing is the point and not a convenience: the settings page and
	// the selector classify the same token against the same thresholds, so they
	// cannot disagree about whether it is eligible. Two Params-side literals mapping
	// the four knobs independently would each be internally consistent and would drift
	// with nothing going red (D21).
	//
	// The ZERO VALUE is a coherent, deliberate policy rather than a hole: MaxStaleness
	// 0 means nothing is ever fresh, so every token classifies stale, Select returns
	// pool_stale and every auto worker resolves its non-auto binding. A Params literal
	// that forgets this field therefore behaves exactly like a factory with the poller
	// disabled (R2) — degraded, never wrong, and never a spend against a credential
	// nobody chose.
	Autoselect autoselect.Policy

	// Anthropic usage-limit park (PRD #35), mirrored from config. RunLimitMaxWaits
	// caps parks PER RUN; RunLimitMaxPark caps how far out ONE park may reach.
	//
	// NOTE the zero values are the SAFE direction and are not a hole, but they are
	// not the same kind of safe. RunLimitMaxWaits == 0 means "never park", so a
	// Params literal that omits it behaves exactly as uzi did before this feature —
	// the run fails on a limit, now with a better reason. RunLimitMaxPark == 0 makes
	// every computed stamp exceed the ceiling and therefore also fails the run, so
	// the two agree; the defaults live in config.go, where the envs are read.
	RunLimitMaxWaits int
	RunLimitMaxPark  time.Duration
}

// Broadcaster receives run events after they are persisted, for live fan-out to
// browsers. It is the seam onto the WS hub; nil in tests and any deployment that
// serves no live channel. Every method is best-effort and must never block or
// error the persistence path (the DB write is authoritative).
type Broadcaster interface {
	// PublishMessage forwards one newly-persisted run message. agentInstance and
	// agentLabel are the PRD #99 per-frame subagent invocation id + task label,
	// empty when the frame carried no parent_tool_use_id (not the same as
	// agent == "lead": a repo-authored agent may legitimately be NAMED lead).
	PublishMessage(runID uuid.UUID, seq int32, kind, agent, agentInstance, agentLabel string, payload []byte, createdAt time.Time)
	// PublishState signals that a run's status changed.
	PublishState(runID uuid.UUID, status string)
	// PublishHealth signals that a run's health flag changed (PRD #47) — raised,
	// changed, or self-cleared (health=="ok"). health is the flag enum and reason is
	// the fixed server-controlled template (empty when clearing). nudge is set ONLY
	// when the sweeper judged this event nudge-worthy (an ok→flagged transition past
	// the cooldown) and has already stamped health_notified_at — the notifier then
	// threads one DM; otherwise it only re-renders the root. The live hub maps the
	// event to a WS run-update (browsers re-read the run, picking up the owner-gated
	// reason over REST) and ignores nudge. Best-effort, never blocks the sweep.
	PublishHealth(runID uuid.UUID, health, reason string, nudge bool)
	// PublishInput signals that a run's follow_up steer queue changed (PRD #95) — the
	// worker consumed a follow-up, stamping consumed_at. Carries no data; the live hub
	// pokes browsers to re-read GET /runs/{id}/inputs (owner-gated), the Slack notifier
	// no-ops it (steer text never goes to Slack). Best-effort, never blocks the consume.
	PublishInput(runID uuid.UUID)
}

// MultiBroadcaster fans each event out to several Broadcasters — the WS hub AND
// the Slack notifier (PRD #25 M3). Each is best-effort and non-blocking by its
// own contract, so the fan-out is a plain iteration. A nil or empty value is a
// valid no-op broadcaster.
type MultiBroadcaster []Broadcaster

func (m MultiBroadcaster) PublishMessage(runID uuid.UUID, seq int32, kind, agent, agentInstance, agentLabel string, payload []byte, createdAt time.Time) {
	for _, b := range m {
		b.PublishMessage(runID, seq, kind, agent, agentInstance, agentLabel, payload, createdAt)
	}
}

func (m MultiBroadcaster) PublishState(runID uuid.UUID, status string) {
	for _, b := range m {
		b.PublishState(runID, status)
	}
}

func (m MultiBroadcaster) PublishHealth(runID uuid.UUID, health, reason string, nudge bool) {
	for _, b := range m {
		b.PublishHealth(runID, health, reason, nudge)
	}
}

func (m MultiBroadcaster) PublishInput(runID uuid.UUID) {
	for _, b := range m {
		b.PublishInput(runID)
	}
}

// RunLifecycle is notified after each run status write so the board's column
// automation can react (queued → In Progress, completed → Human Review,
// failed/cancelled → origin). It is the seam onto runlifecycle; nil in tests and
// deployments that run no automation. Notify is best-effort and must never block
// or error the persistence path — it is called on the request path, and the
// durable move marker is already stamped in the status write's transaction.
type RunLifecycle interface {
	// Notify reports that a run just transitioned to status.
	Notify(runID uuid.UUID, status string)
}

// SettingsReader is the narrow slice of the instance settings the judge machinery
// reads (PRD #46): the global kill-switch gates the terminal-funnel enqueue, the
// model rides the judge claim. *settings.Cache satisfies it. Optional (nil-safe);
// a nil reader means the judge feature is off (no enqueue, no model) — the default
// in tests and any deployment that never wired it.
type SettingsReader interface {
	JudgeEnabled(ctx context.Context) (bool, error)
	// JudgeEnforceAll reports whether the judge is enforced for every run (PRD #69),
	// bypassing the per-user judge_enabled opt-in gate. The kill-switch and token
	// presence still govern. A nil reader (or a best-effort error) reads as false, so
	// enforcement never turns on when settings are unavailable.
	JudgeEnforceAll(ctx context.Context) (bool, error)
	// JudgeCooldownSeconds / JudgeDailyBudget are the per-user judge spend guards (PRD
	// #69 M5 Decision 9), checked at Gate 5 in every mode. Cooldown 0 disables the
	// cooldown; budget 0 means unlimited. Best-effort: a read error fails OPEN (the
	// enqueue proceeds), since these are soft cost backstops, not correctness gates.
	JudgeCooldownSeconds(ctx context.Context) (int, error)
	JudgeDailyBudget(ctx context.Context) (int, error)
	JudgeModel(ctx context.Context) (string, error)
	// SummaryModel is the instance-default model the inline run-summary generator
	// runs on (PRD #362 Decision 8), the fallback when the run owner has no per-user
	// summary_model override. Falls back to the compiled-in default ("haiku").
	SummaryModel(ctx context.Context) (string, error)
	// UziLabel is the single run-eligibility label an issue must carry to be runnable
	// (PRD #764 M1). It is read here rather than passed in by each caller because the
	// gate is shared: the board handler, the scheduler, and the poller's autopilot must
	// all be answering the same question, and an operator renaming uzi_label must move
	// them all at once.
	//
	// It rides this interface rather than a new one despite the judge scope of its
	// siblings, because a deployment that wires settings at all wires *Cache, which
	// serves them all. A nil reader falls back to the compiled-in default ("uzi") and
	// the gate still runs — "settings unavailable" must not mean "unguarded".
	UziLabel(ctx context.Context) (string, error)
}

// DockerAllowlistReader is the narrow settings view the claim gate reads for the
// docker-worker repo allowlist (PRD #89 M-allow). *settings.Cache satisfies it.
// Kept its own interface (interface segregation, like SettingsReader/Settings) so a
// test exercises only what it uses. Optional (nil-safe): a nil reader means the
// allowlist is UNAVAILABLE, which the claim gate treats as fail-closed for a docker
// worker — it then claims no repo-bearing run. A non-docker worker never consults it.
type DockerAllowlistReader interface {
	DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error)
}

// CapabilityScheduleReader is the narrow settings view the claim gate reads for the
// capability-aware scheduling kill-switch (PRD #84 Decision 13). *settings.Cache
// satisfies it. Kept its own interface (interface segregation, like
// DockerAllowlistReader) so a test exercises only what it uses. Optional (nil-safe):
// a nil reader — or a read error — DEFAULTS the flag ON (capability_aware=true), the
// safe default, so an unconfigured or momentarily-unreadable setting routes rather
// than degrading to best-effort claiming. Existing tests construct the service
// without it and are unaffected.
type CapabilityScheduleReader interface {
	CapabilityAwareScheduling(ctx context.Context) (bool, error)
}

// Service holds the store, the secret cipher, and the runtime params.
type Service struct {
	q   Store
	box *secretbox.Box
	p   Params
	// now is time.Now in production; overridable in tests for deterministic
	// cutoffs.
	now func() time.Time
	// bcast fans persisted run events out to browser WS subscribers. Optional
	// (nil-safe); set via SetBroadcaster after construction so New's signature —
	// and every existing caller — stays unchanged.
	bcast Broadcaster
	// lifecycle reacts to status changes with board column moves. Optional
	// (nil-safe); set via SetLifecycle for the same reason as bcast.
	lifecycle RunLifecycle
	// vlt gates claims on the run owner's vault being unlocked and opens the
	// owner's Anthropic token at claim time (PRD #32). Optional (nil-safe); set via
	// SetVault. The SAME *vault.Vault instance backs the HTTP handlers, so a login
	// on the API and a claim by the worker share one DEK cache. Nil (tests, or a
	// deployment without the vault) disables the gate and falls back to opening the
	// token under the master box — the pre-vault behavior.
	vlt *vault.Vault
	// Two narrow settings views over the same *settings.Cache: `settings` =
	// judge/self-improve reads, `healthSettings` = run-health reads (interface
	// segregation — each feature's tests fake only what they use).
	//
	// settings reads the instance judge toggles/model (PRD #46). Optional (nil-safe);
	// set via SetSettings. Nil disables the judge terminal-funnel enqueue entirely.
	settings SettingsReader
	// healthSettings reads the runtime-tunable run-health thresholds (PRD #47).
	// Optional (nil-safe); set via SetHealthSettings. A nil healthSettings disables
	// the whole health detector, so tests that do not exercise it — and any
	// deployment without a settings cache — behave exactly as before.
	healthSettings Settings
	// dockerAllowlist reads the docker-worker repo allowlist the claim gate enforces
	// (PRD #89 M-allow). Optional (nil-safe); set via SetDockerAllowlist with the same
	// settings cache the HTTP handlers hold. Nil ⇒ a docker worker is fail-closed (it
	// claims no repo-bearing run); a non-docker worker never consults it, so tests and
	// deployments without a settings cache are unaffected.
	dockerAllowlist DockerAllowlistReader
	// capabilitySettings reads the capability-aware scheduling kill-switch the claim
	// gate threads into ClaimRun (PRD #84 Decision 13). Optional (nil-safe); set via
	// SetCapabilitySettings with the same settings cache the HTTP handlers hold. Nil ⇒
	// the flag DEFAULTS ON, so tests and deployments without a settings cache route
	// capability-aware exactly as a live instance whose admin left the default in place.
	capabilitySettings CapabilityScheduleReader
	// lastSlowClampWarn is the last health_slow_seconds value the read-time clamp
	// warned about (PRD #47), so the warning logs once per distinct misconfigured
	// value instead of on every 15s sweep. Touched only by the sweeper goroutine
	// (slowThreshold, reached via detectRunHealth ← Sweep), so it needs no lock.
	lastSlowClampWarn time.Duration
	// persistFail counts consecutive AppendMessages failures per run (PRD #108 M4),
	// the signal a persistence wedge cannot suppress because the wedge IS the event
	// being counted. Always non-nil (New constructs it).
	//
	// Unlike lastSlowClampWarn directly above, it is NOT sweeper-only: it is written
	// by every HTTP handler goroutine serving /messages and read by the sweeper, so
	// it carries its own mutex. Do not copy that field's lock-free reasoning here —
	// see persistfail.go.
	persistFail *persistFailTracker
	// forgeBaseURLAllowed is the SSRF gate for the M8 checkpoint-publish path (PRD
	// #122): it reports whether a run's forge base URL is on the configured
	// allowlist before the api will fetch/push against it. Set via
	// SetForgeBaseURLAllowed with config.Config.ForgeBaseURLAllowed. A nil gate is a
	// loud misconfiguration — Publish refuses (500) rather than fail-open — so a
	// deployment that never wired it can never broker a push to an arbitrary host.
	forgeBaseURLAllowed func(string) bool
	// publishFn is the go-git checkpoint publisher (PRD #122 M8). Defaults to
	// pushbroker.Publish (set in New); a setter lets handler/service tests stub it so
	// the whole Publish path is exercised without a real forge. Injected as a seam
	// rather than called directly so pushbroker stays the ONE place go-git lives.
	publishFn func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error)
	// forges builds a forge driver from a stored connection (PRD #191 Decision 8): the
	// seam the composite forge-write operations lifted out of the handlers
	// (ConfirmProposalForUser, StartRunForUser) reach the forge through. Optional
	// (nil-safe); set via SetForges. workersvc cannot import forgesvc directly
	// (forgesvc already imports workersvc — a cycle), so this mirrors the narrow
	// ForgeBuilder interface privcheck/selfimprove/runlifecycle already use. Nil ⇒ the
	// composite ops return ErrForgesUnavailable rather than panic (the pre-#191
	// deployments and every test that never wires it are unaffected).
	forges ForgeBuilder
	// guard is the #66 default-branch guardrail (D1 layer 2), a narrow RepoGuard
	// interface *privcheck.Service satisfies; set via SetRepoGuard. Nil ⇒ the
	// service-layer gate is skipped (guardDefaultBranch is a no-op) and layer 3 (the
	// claim backstop) remains the security net, so a wiring gap never fails all runs.
	guard RepoGuard
}

// SetSettings wires the instance settings reader (PRD #46). Call once at startup,
// before serving. A nil reader (tests, or a deployment that never enabled the judge)
// disables the terminal-funnel enqueue and leaves the judge model unset in claims.
func (s *Service) SetSettings(sr SettingsReader) { s.settings = sr }

// SetForges wires the forge builder (PRD #191 Decision 8), the seam
// ConfirmProposalForUser/StartRunForUser reach the forge through. Call once at
// startup, before serving; pass the same *forgesvc.Service the handlers hold. A nil
// builder leaves the composite ops returning ErrForgesUnavailable.
func (s *Service) SetForges(fb ForgeBuilder) { s.forges = fb }

// SetRepoGuard wires the #66 default-branch guardrail (D1 layer 2). Late-injected
// in main.go because the privcheck.Service is built after this Service. A nil guard
// leaves guardDefaultBranch a no-op — the claim backstop (M6, layer 3) is the net.
func (s *Service) SetRepoGuard(g RepoGuard) { s.guard = g }

// SetBroadcaster wires the live-event broadcaster (the WS hub). Call once at
// startup, before serving. A nil broadcaster disables live fan-out; the persisted
// message log still backs the browser's REST replay.
func (s *Service) SetBroadcaster(b Broadcaster) { s.bcast = b }

// SetLifecycle wires the run-lifecycle column automation. Call once at startup,
// before serving. A nil lifecycle disables auto-moves; the same-transaction move
// markers still accumulate and would be applied if one were later attached.
func (s *Service) SetLifecycle(l RunLifecycle) { s.lifecycle = l }

// SetVault wires the per-user vault (PRD #32 M3). Call once at startup, before
// serving, with the SAME instance the HTTP handlers hold. A nil vault disables
// the claim gate and opens the Anthropic token under the master box (pre-vault
// behavior), which is what the workersvc tests rely on.
func (s *Service) SetVault(v *vault.Vault) { s.vlt = v }

// SetHealthSettings wires the run-health settings reader (PRD #47). Call once at
// startup, before the sweeper runs, with the same settings cache the HTTP handlers
// hold. A nil healthSettings (the default) disables the health detector entirely.
func (s *Service) SetHealthSettings(cfg Settings) { s.healthSettings = cfg }

// SetDockerAllowlist wires the docker-worker repo-allowlist reader the claim gate
// enforces (PRD #89 M-allow). Call once at startup, before serving, with the same
// settings cache the HTTP handlers hold. Nil (the default in tests) makes a docker
// worker fail-closed — it claims no repo-bearing run — while leaving non-docker
// workers wholly unaffected.
func (s *Service) SetDockerAllowlist(r DockerAllowlistReader) { s.dockerAllowlist = r }

// SetCapabilitySettings wires the capability-aware scheduling kill-switch reader the
// claim gate threads into ClaimRun (PRD #84 Decision 13). Call once at startup, before
// serving, with the same settings cache the HTTP handlers hold. Nil (the default in
// tests) defaults the flag ON — capability matching is enforced — so the omission is
// safe rather than a silent disable.
func (s *Service) SetCapabilitySettings(r CapabilityScheduleReader) { s.capabilitySettings = r }

// SetForgeBaseURLAllowed wires the SSRF gate for the M8 checkpoint-publish path
// (PRD #122). Call once at startup with config.Config.ForgeBaseURLAllowed. Leaving
// it unset (nil) makes Publish refuse — it is a loud misconfiguration, never
// fail-open — so the broker can never be pointed at an un-allowlisted host.
func (s *Service) SetForgeBaseURLAllowed(fn func(string) bool) { s.forgeBaseURLAllowed = fn }

// SetPublishFn overrides the go-git checkpoint publisher (PRD #122 M8). Production
// leaves the pushbroker.Publish default New installs; tests stub it to exercise the
// service's derivation/authorization/skip mapping without a real forge.
func (s *Service) SetPublishFn(fn func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error)) {
	s.publishFn = fn
}

// notify fires the lifecycle hook if one is wired. It is a no-op otherwise, so
// every call site stays unconditional.
func (s *Service) notify(runID uuid.UUID, status string) {
	if s.lifecycle != nil {
		s.lifecycle.Notify(runID, status)
	}
}

// defaultClaimGrace is the claimed-but-never-started reclaim window (PRD: 5m).
const defaultClaimGrace = 5 * time.Minute

// New constructs a Service. box may be nil only in tests that never call Claim.
func New(q Store, box *secretbox.Box, p Params) *Service {
	if p.ClaimGrace <= 0 {
		p.ClaimGrace = defaultClaimGrace
	}
	return &Service{q: q, box: box, p: p, now: time.Now, persistFail: newPersistFailTracker(), publishFn: pushbroker.Publish}
}

// -------------------------------------------------------------------------
// Worker protocol
// -------------------------------------------------------------------------

// Register brings a worker online and recovers its orphaned runs. A registering
// worker has, by definition, just started fresh and is executing nothing, so any
// run it still holds (claimed/running/awaiting_approval/awaiting_input) is
// orphaned: over the re-queue budget it is failed, otherwise it is re-queued to
// this same worker (affinity) to be re-claimed and resumed from the persisted
// session. This is what makes `docker compose down && up` recover — the
// out-of-process worker's fresh-start signal, which the server cannot infer from
// heartbeats alone.
//
// awaiting_input (PRD #88) belongs in that set for a reason the stale-heartbeat
// sweeps cannot cover: RegisterWorker stamps last_heartbeat_at = now(), so a worker
// that restarts faster than WORKER_HEARTBEAT_STALE is NEVER stale and those sweeps
// never see it. A parked run missing from these two register sweeps would sit in
// awaiting_input forever, pointing at execution that no longer exists, with its
// worker-held answer deadline gone and no user-visible signal — on the ordinary
// restart path this comment already names.
func (s *Service) Register(ctx context.Context, wkr store.Worker, version, template string, maxConcurrentRuns *int, capabilities []string) (store.Worker, error) {
	max := int32(s.p.RunMaxRequeues)
	orphanFailed, err := s.q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		FailureReason: pgText("worker restarted; run orphaned and out of re-queue budget"),
		WorkerID:      pgUUID(wkr.ID),
		MaxRequeues:   max,
	})
	if err != nil {
		return store.Worker{}, err
	}
	// PRD #46 Decision 2: these orphaned-over-cap runs just committed to 'failed'
	// (worker lost) — the same worker-lost runs the sweeper's identical
	// FailRunsOfStaleWorkersOverCap funnels into the judge. Funnel them here too.
	// Best-effort, gated inside; never fails the register.
	for _, id := range orphanFailed {
		s.maybeEnqueueJudgeByID(ctx, id)
	}
	if _, err := s.q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{
		WorkerID:    pgUUID(wkr.ID),
		MaxRequeues: max,
	}); err != nil {
		return store.Worker{}, err
	}
	// template is the worker's self-reported image template (PRD #18); empty →
	// NULL (older image sends none). Soft signal only; never rejected here.
	// maxConcurrentRuns is the worker's advertised concurrency cap (PRD #42); nil →
	// NULL (an older image, or M3a before the M2 agent sends it). Observability
	// only — the server never enforces it, so it is stored exactly as reported.
	//
	// capabilities is the STORED set (PRD #84 M1): the union of the worker's
	// SELF-REPORTABLE caps and the template-derived caps. capability.SelfReportable
	// restricts the self-report side to names a worker is allowed to announce (today
	// only docker), so a base worker cannot spoof a template-only capability like jvm
	// by self-reporting it. The union is passed through Filter for vocabulary
	// validation, dedupe, and stable order — so the column stays authoritative and a
	// later milestone's peer subquery can read workers.capabilities directly.
	storedCaps := capability.Filter(append(capability.SelfReportable(capabilities), capability.TemplateCapabilities(template)...))
	row, err := s.q.RegisterWorker(ctx, store.RegisterWorkerParams{
		Version:           pgText(version),
		TemplateReported:  pgText(template),
		Capabilities:      storedCaps,
		MaxConcurrentRuns: pgIntPtr(maxConcurrentRuns),
		ID:                wkr.ID,
	})
	if err != nil {
		return store.Worker{}, err
	}
	// Field-identical structs; the conversion keeps Register's signature — and so
	// every handler and DTO builder above it — unchanged.
	return store.Worker(row), nil
}

// WorkerStats is a validated, clamped container resource sample (PRD #49). The
// handler decodes the untrusted worker report, validates + clamps it into this, and
// passes it here; the service only persists it. DISPLAY-ONLY (Decision 5) — no
// scheduling path ever reads the columns it writes. A nil *WorkerStats writes NULLs
// (the tick carried no stats), so a downgrade / collector error self-clears the gauge.
type WorkerStats struct {
	// CPUPct is finite and clamped to [0, MaxWorkerCPUPct]; nil when the worker
	// omitted it (the first tick after start, per Decision 2).
	CPUPct *float64
	// MemBytes is the working-set memory in bytes, non-negative.
	MemBytes int64
	// MemLimit is the cgroup memory limit in bytes (non-negative); nil when unlimited
	// (memory.max=max) or unknown (the process fallback).
	MemLimit *int64
	// Source is the validated enum: "cgroup" or "process".
	Source string
}

// Heartbeat refreshes liveness, overwrites the worker's latest resource sample (PRD
// #49), and returns the updated worker. A nil stats writes NULLs for every stats_
// column, so a worker that stops reporting self-clears its gauge.
func (s *Service) Heartbeat(ctx context.Context, wkr store.Worker, stats *WorkerStats) (store.Worker, error) {
	arg := store.HeartbeatWorkerParams{ID: wkr.ID}
	if stats != nil {
		arg.StatsCpuPct = pgFloat4Ptr(stats.CPUPct)
		arg.StatsMemBytes = pgtype.Int8{Int64: stats.MemBytes, Valid: true}
		arg.StatsMemLimitBytes = int8Param(stats.MemLimit)
		arg.StatsSource = pgText(stats.Source)
	}
	return s.q.HeartbeatWorker(ctx, arg)
}

// Claim atomically claims the oldest claimable run for the worker's user and
// assembles the full run payload (issue snapshot, repo + clone URL, decrypted
// credentials, structured agent templates, config caps, and resume fields). A
// nil payload with a nil error means the queue is idle (the handler answers
// 204). If the claimed run's credentials are missing or undecryptable, the run
// is failed immediately and idle is reported, so a broken run never wedges the
// worker in a claim loop.
//
// Fleet-aware spread (PRD #216): a busy worker DEFERS a queued run to a
// strictly-less-loaded, eligible, live peer of the same user rather than claiming
// it, so runs spread across a fleet instead of piling on whichever worker polls
// first. The deferral is a no-op at the caller level — the run simply isn't
// returned (nil payload / 204), so the worker just re-polls and a peer claims it.
func (s *Service) Claim(ctx context.Context, wkr store.Worker) (*ClaimPayload, error) {
	// Vault gate (PRD #32 M3): while the run owner's vault is locked (after a pod
	// restart, or a manual lock), do not claim any of their runs — report idle so
	// they stay queued as "waiting for vault unlock" instead of failing. This is a
	// pure in-memory check on the worker's own user, NOT a SQL filter: ClaimRun is
	// already scoped to wkr.UserID, and the DEK cache is per-process. A queued run
	// sits indefinitely by design (no sweep touches status='queued'); the next
	// unlock lets it claim within one poll cycle.
	if s.vlt != nil && !s.vlt.Unlocked(wkr.UserID) {
		return nil, nil // idle: owner locked
	}

	// Drain gate (PRD #422 M3): a cordoned worker finishes its in-flight runs but
	// claims nothing new, so the controller can roll it once idle. Like the vault
	// gate this reports idle (nil,nil), not an error — the worker keeps heartbeating
	// and reporting its running runs; only NEW claims are refused. draining_since is
	// cleared on the worker's next register (after its roll), which re-enables claims.
	if wkr.DrainingSince.Valid {
		return nil, nil // idle: worker draining/cordoned
	}

	// Docker-worker repo allowlist (PRD #89 M-allow): a docker-enabled worker may
	// claim ONLY runs whose repo is on the trusted allowlist. Repo-less JUDGE runs are
	// exempt (the SQL narrows the exemption to kind='judge', so a future repo-less kind
	// fail-closes until deliberately exempted) — safe NOT because "repo-less =
	// content-free" (a judge reasons over an untrusted trace) but because the repo-less
	// executor (agent/src/judge-runner.ts, and the chat lane's chat-executor.ts)
	// carries no daemon-reaching tool, so DOCKER_HOST is inert for it; an agent/
	// regression test pins that. This is the accepted-risk likelihood control for the non-rootless
	// DinD tier: the trigger is repo content, so the gate binds at claim, not at
	// provisioning. Fail-closed: a docker worker with no allowlist reader wired, or
	// an empty allowlist, claims no repo-bearing run.
	//
	// The fetch is UNCONDITIONAL (PRD #216): a non-docker claiming worker still needs
	// the real allowlist so the fleet-aware spread can evaluate whether a DOCKER peer
	// could claim a repo run — without it, spreading a repo run to a docker peer would
	// silently stop. A read error is only fatal for a docker claiming worker (its own
	// eligibility depends on it); a non-docker worker degrades gracefully to "don't
	// spread repo runs to docker peers this cycle" and still claims itself.
	isDocker := wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool
	allowlist := []uuid.UUID{}
	if s.dockerAllowlist != nil {
		al, aerr := s.dockerAllowlist.DockerRepoAllowlist(ctx)
		if aerr != nil {
			// A docker worker's OWN eligibility depends on the allowlist, so an
			// unconfirmable allowlist must fail-closed (never claim a repo run we
			// can't vet). A non-docker worker doesn't need it for its own claim;
			// it only needs it to know whether a DOCKER peer could claim a repo
			// run. If it can't be confirmed, simply don't spread repo runs to
			// docker peers this cycle (empty allowlist => docker peers ineligible
			// for repo runs) — the claiming worker still claims, never a strand.
			if isDocker {
				return nil, aerr
			}
		} else {
			allowlist = al
		}
	}

	// Capability-aware scheduling kill-switch (PRD #84 Decision 13). Threaded into
	// ClaimRun as @capability_aware: the extended fn_worker_can_claim reads its new
	// subset clause as `NOT capability_aware OR (required ⊆ caps)`, so a false flag makes
	// the capability match trivially true (best-effort claiming) while the docker
	// allowlist clause above stays enforced. capabilityAwareOn (shared with the health
	// detector's queued-reason path) is DEFAULT ON: a nil reader (tests, or a deployment
	// without a settings cache) or a read error both leave it true, so an unconfirmable
	// flag routes rather than silently degrading to a mid-run crash.
	capabilityAware := s.capabilityAwareOn(ctx)

	run, err := s.q.ClaimRun(ctx, store.ClaimRunParams{
		WorkerID:            pgUUID(wkr.ID),
		UserID:              wkr.UserID,
		AffinityCutoff:      pgTime(s.now().Add(-s.p.WorkerAffinityCeiling)),
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allowlist,
		WorkerCaps:          wkr.Capabilities,
		CapabilityAware:     capabilityAware,
		// PRD #529 Decision 4: an ephemeral worker may claim only its bound run.
		IsEphemeral:           wkr.Ephemeral,
		EphemeralRunID:        wkr.EphemeralRunID,
		SpreadCutoff:          pgTime(s.now().Add(-s.p.WorkerSpreadGrace)),
		BackgroundGraceCutoff: pgTime(s.now().Add(-s.p.WorkerBackgroundGrace)),
		HeartbeatCutoff:       pgTime(s.now().Add(-s.p.WorkerHeartbeatStale)),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // idle
		}
		return nil, err
	}

	payload, err := s.assembleClaim(ctx, wkr, run)
	if err != nil {
		return nil, s.recoverClaimAssembly(ctx, run, err)
	}
	return payload, nil
}

// recoverClaimAssembly turns a failed claim assembly (run OR chat lane) into the
// right terminal/transient outcome, always reporting idle (nil payload) to the
// caller so a broken claim never wedges the worker's poll loop:
//   - a locked vault is transient — reset the just-claimed run to queued (never
//     fail it), keeping worker_id for affinity and NOT bumping requeue_count, so a
//     persistently locked vault can't trip the requeue cap (mirrors
//     SweepClaimedNeverStarted);
//   - a missing/undecryptable credential (or a rejected tool package), and a #66
//     guardrail block at claim (D1 layer 3), are terminal — fail the run with the
//     safe (no-secret-bytes) reason and fire the failed notify;
//   - a vanished run (its forge connection cascade-deleted the repo → run) is dropped.
//
// The returned error is nil for every handled case (report idle) and non-nil only
// for an unexpected error the caller must propagate.
func (s *Service) recoverClaimAssembly(ctx context.Context, run store.Run, err error) error {
	switch {
	case errors.Is(err, errVaultLocked):
		if _, rerr := s.q.RequeueClaimedRunToQueued(ctx, run.ID); rerr != nil {
			return rerr
		}
		return nil // idle; the run is queued again, awaiting unlock
	case errors.Is(err, errAutoPoolEmpty):
		// An auto claim with a genuinely empty pool must not spend the non-pooled default
		// nor hard-fail (PRD #754). It is HELD in the non-locking pool_wait status (M4,
		// replacing M2's interim requeue): SetRunPoolWait keeps worker_id for affinity,
		// does not touch the usage-limit budget, and — unlike the requeue — does not churn
		// the queue. The run is idle (nil payload) exactly as before; M5 adds the reactive
		// + manual resume off pool_wait. Still "never spends the default, never hard-fails".
		// Positive source guard (status='claimed'), so a re-delivered claim is a no-op.
		if _, rerr := s.q.SetRunPoolWait(ctx, store.SetRunPoolWaitParams{
			ID:       run.ID,
			WorkerID: run.WorkerID,
		}); rerr != nil {
			return rerr
		}
		return nil // idle; the run is held in pool_wait, awaiting a pooled token
	case errors.Is(err, errCredentialUnavailable) || errors.Is(err, errToolPackagesRejected) || errors.Is(err, errGuardrailBlockedClaim):
		// A guardrail block at claim (D1 layer 3) is TERMINAL — fail-closed even on a
		// forge blip (R4; the user restarts after fixing protection), matching
		// errCredentialUnavailable rather than the transient errVaultLocked requeue.
		//
		// PRD #69 M7a: these three infra sentinels used to collapse into one failed
		// write, LOSING the origin. Split the class per sentinel so the judge sees a
		// TRUSTED origin (errVaultLocked never reaches here — it requeues above — and
		// errRunVanished is not a failed write, so neither owes a fail_origin).
		var failOrigin string
		switch {
		case errors.Is(err, errCredentialUnavailable):
			failOrigin = "credential_unavailable"
		case errors.Is(err, errToolPackagesRejected):
			failOrigin = "provisioning_failed"
		case errors.Is(err, errGuardrailBlockedClaim):
			failOrigin = "guardrail_blocked"
		}
		if _, ferr := s.q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{
			ID:            run.ID,
			FailureReason: pgText(err.Error()),
			FailOrigin:    pgText(failOrigin),
		}); ferr != nil {
			return ferr
		}
		s.notify(run.ID, "failed") // restore the origin column for the dead run (no-op for chat)
		return nil                 // idle; the run now shows failed in the UI
	case errors.Is(err, errRunVanished):
		return nil
	default:
		return err
	}
}

// errCredentialUnavailable marks a claim that cannot proceed because a required
// secret is absent or cannot be decrypted. Its message is safe to store as a
// run failure reason (it never includes secret bytes).
var errCredentialUnavailable = errors.New("credential unavailable")

// errRunVanished marks a claim whose run disappeared before its payload could be
// assembled (a cascading delete of the forge connection).
var errRunVanished = errors.New("run vanished before claim assembly")

// errGuardrailBlockedClaim marks a claim the #66 default-branch guardrail refused
// AT CLAIM (D1 layer 3, the security net): the bot can reach the repo's default
// branch, or that could not be verified (fail-closed). recoverClaimAssembly treats
// it as TERMINAL (like errCredentialUnavailable, not the transient errVaultLocked
// requeue), so the run is failed rather than pushing. Its message is safe to store
// as a run failure reason — it carries only the block finding messages, never any
// secret bytes.
var errGuardrailBlockedClaim = errors.New("run refused by the default-branch guardrail at claim")

// errVaultLocked marks a claim that cannot open the owner's DEK-sealed Anthropic
// token because their vault locked between the claim gate and the open. It must
// NOT be conflated with errCredentialUnavailable: that path fails the run
// (terminal), whereas a locked vault is transient and the run must go back to
// queued to be retried after the next unlock (PRD #32 success criteria 3 & 5).
var errVaultLocked = errors.New("vault locked during claim")

// errAutoPoolEmpty marks an auto claim that found genuinely nothing pooled to spend:
// an empty pool, or a resuming run whose only pooled token is the excluded dead
// credential (#754 M2). The auto lane must NEVER spend the non-pooled owner default,
// and it must NOT hard-fail a run for a transient/holdable condition — so this is
// HOLDABLE like errVaultLocked, and recoverClaimAssembly HOLDS the run in the
// non-locking pool_wait status (PRD #754 M4, via SetRunPoolWait) rather than failing
// it — M5 resumes it reactively or manually. Its message carries no secret bytes.
var errAutoPoolEmpty = errors.New("auto pool is empty")

// claimCred is the concrete Anthropic credential ONE claim spends: the identity to
// record on the run, the label to snapshot alongside it, and the plaintext to ship
// in the claim payload. It exists because openAnthropic used to hand back only
// []byte — the credential was chosen and then forgotten, which is exactly why a run
// could never answer "which account paid for this?" (PRD #111 M1, D8).
//
// Token is secret bytes. Nothing may log this struct whole.
type claimCred struct {
	ID    uuid.UUID
	Label string
	Token []byte
}

// Selection reasons recorded in runs.anthropic_select_reason — the MODE that named
// the credential, which is what a user actually needs to read (D20: an auto pick and
// a default fallback can name the same token, and PRD #104's compatibility path
// creates a row labelled literally "default", so the label alone answers nothing).
//
// These are ALIASES, not a second definition. The whole eight-value vocabulary lives
// in autoselect (see Reason there for why it hosts even the non-auto three), and
// migration 00089's CHECK is the same eight; these exist only so the claim path reads
// in its own idiom rather than saying string(autoselect.ReasonPinned) on every line.
// Aliasing means a rename upstream is a compile error here, which a second set of
// string literals would not be.
const (
	selectReasonDefault = string(autoselect.ReasonDefault)
	selectReasonPinned  = string(autoselect.ReasonPinned)
	selectReasonJudge   = string(autoselect.ReasonJudge)
)

// secretChoice is WHICH credential a claim should spend and WHY: the override
// openAnthropic takes (nil ⇒ the owner's default), the reason to record, and the
// measured headroom when a reading produced the choice.
//
// It replaced a bare *uuid.UUID because M4 made the answer two-dimensional. An auto
// pick and a default fallback can name the SAME token, so an id alone can no longer
// say what happened — and the fallback reasons (pool_empty, pool_stale) are carried
// by a choice whose id is nil, i.e. by exactly the value that used to mean "nothing
// to say".
//
// headroom is a pointer because NULL is a real answer: only an auto pick has a
// measured headroom, and 0 is a legal one (a fully-consumed token picked
// best-of-pool), so a zero value cannot stand in for absence.
type secretChoice struct {
	secretID *uuid.UUID
	reason   string
	headroom *int16
}

// autoLaneRetryable reports whether a credential the AUTO lane resolved and then
// failed to open earns ONE floor-retry onto ANOTHER pooled token (D14, reshaped by
// #754 M2). It is the precise gate on that retry, and it covers the three auto-lane
// picks that have a pooled alternative to fall to:
//
//   - a selector pick (auto / best_of_pool), and
//   - a floor pick (pool_stale) — #754 made the floor a real pooled spend, so a
//     floored token that will not open must ALSO get one retry onto the next pooled
//     token rather than dying terminally on the first undecryptable row.
//
// It deliberately EXCLUDES open_failed, which is the reason autoFloorRetry itself
// records: a second open failure therefore fails this gate on its REASON conjunct and
// is terminal by STRUCTURE — no counter, and no dependency on an invariant enforced
// three files away. It also excludes pinned / default / judge: the user named those
// credentials, and silently billing a different one is the R4 failure this PRD is
// otherwise built to avoid. The nil-secretID guard keeps the empty-pool hold
// (errAutoPoolEmpty, which never reaches an open) out of the retry entirely.
func (c secretChoice) autoLaneRetryable() bool {
	return c.secretID != nil &&
		(c.reason == string(autoselect.ReasonAuto) ||
			c.reason == string(autoselect.ReasonBestOfPool) ||
			c.reason == string(autoselect.ReasonPoolStale))
}

// staticChoice names the mode that produced a claim's secretID override for the two
// non-auto resolutions. It takes the OVERRIDE rather than the resolved credential on
// purpose: after resolution both cases are just an id, and "the owner's default" and
// "a binding that happens to name the default token" are different facts that a user
// reading the run view needs told apart.
//
// bound is the reason to use when the override is set — selectReasonPinned for a
// worker binding, selectReasonJudge for the judge lane. An UNSET override is
// selectReasonDefault either way, and that asymmetry is correct: a judge lane with no
// binding really did spend the owner's default, and saying "judge" would claim a
// binding chose it.
func staticChoice(secretID *uuid.UUID, bound string) secretChoice {
	if secretID == nil {
		return secretChoice{reason: selectReasonDefault}
	}
	return secretChoice{secretID: secretID, reason: bound}
}

// openAnthropic resolves AND opens the Anthropic credential for one run — the one
// secret the run lane, the judge lane and the chat lane all deliver, and the ONE
// place credential resolution happens. The vault-dispatch logic (dek needs unlock,
// legacy master opens regardless, nil vault → master box) lives in secretopen,
// shared with the rate-limit poller (PRD #53); this method maps its sentinels back
// to workersvc's domain errors, preserving the exact prior behavior: a lock
// surfaces as errVaultLocked (requeue, never fail), and a missing/undecryptable
// token as errCredentialUnavailable with its original failure-reason text (which
// never includes secret bytes).
//
// secretID is the binding-else-default seam (PRD #104 M1): nil resolves the user's
// default token, non-nil resolves that specific credential. The run lane passes a
// worker's anthropic_secret_id (M3) or the owner's judge binding for self_improve
// (M4), the judge lane its own, and the chat lane always nil (chat is deliberately
// not bindable, D5). Keeping every lane on this one function is what keeps
// resolution in one place instead of three copies drifting apart (R4). A bound id
// that is not the caller's is ErrNoSecret, i.e. errCredentialUnavailable, never
// another user's credential (D11).
//
// 🔴 IT NOW RESOLVES THE DEFAULT EXPLICITLY, AND THAT IS THE POINT (PRD #111 D8).
// The nil case used to hand the whole job to secretopen.Open, which resolves
// "the user's default of this kind" INSIDE its ciphertext query and returns only
// plaintext — so there was no id for the caller to record, and a run could not name
// what it spent. Now the default is resolved to (id, label) first and the open
// always goes by id, which makes the recorded id provably the opened one.
//
// The resolution is equivalent, not merely similar: GetDefaultUserSecretMeta's
// predicate (user_id AND kind AND is_default) is character-identical to
// GetUserSecretCiphertext's, both match at most one row under 00077's partial unique
// index, and the open then reads that row's own sealed_with and kind for the DEK AAD.
// Same row, same crypto.
//
// The one thing that DOES change is a race window, and it is deliberate: between
// resolving the id and opening it the user could set a different default, and this
// run now opens the id it resolved rather than whatever the default became. That is
// D8's entire purpose — recorded id == opened id — and it is the safer of the two
// orderings, so do not "fix" it. The narrow cost, accepted knowingly: if the token is
// DELETED inside that window the open now fails (errCredentialUnavailable, a terminal
// run failure) where the single-statement form would have opened the new default.
// PRD #111 D14 adds a retry for the auto lane specifically; a run whose owner deletes
// the credential mid-claim failing is the same outcome as deleting it a moment
// earlier.
//
// Both metadata lookups are OWNER-SCOPED IN THEIR OWN PREDICATE, and that is not
// decoration: they run BEFORE the open, so an unscoped by-id lookup would put another
// user's label in hand at exactly the point M1 records it — a claim that then fails
// on the open, having already leaked what it was going to record.
//
// TWO ROUND TRIPS PER CLAIM, AND THAT IS SETTLED, NOT PENDING (PRD #111 A7, decided
// in M4). The obvious tightening is to project `label` from secretopen's ciphertext
// query so one read serves both, which would also make the label provably come from
// the row that was decrypted — D8's own argument, one level down. Declined, for two
// reasons that are worth writing down because the idea recurs:
//
//   - The provenance gain is nil, unlike D8's. D8 closed a real gap: the default was
//     resolved INSIDE the ciphertext query and no id ever escaped, so there was
//     nothing to record. Here both reads name the SAME id under the same predicate
//     on a primary key, so they cannot return different rows. All the projection
//     would buy is a label read microseconds later — and the label is a point-in-time
//     SNAPSHOT that a later rename deliberately does not update anyway (00086).
//   - The cost lands on the wrong package. secretopen is shared with the rate-limit
//     poller; widening its return type would ripple into usagepoller's TokenOpener
//     seam for the claim lane's convenience, and it would make a function that
//     currently returns only secret bytes return a struct mixing plaintext with
//     safe-to-log metadata — a second claimCred-shaped thing to never log whole.
//
// M4's auto lane does not change this arithmetic, which was the reason the decision
// waited: it is the same shape as M3's pinned lane, not a third case.
//
// 🔴 A CORRECTION TO HOW THAT WAS FIRST WRITTEN, because the clause argued against
// its own conclusion. It read "it arrives here with a label already in hand from the
// ranking query" — offered as the reason the second read is harmless, when that is
// exactly what would make it redundant. The ranking query does select the label, and
// autoselect.Outcome carried it as far as M5, where it was found to have no reader
// and was DELETED rather than wired up.
//
// The positive reason the second read is right, which the original clause never gave:
// this one is SAME-CALL. The label and the ciphertext come out of consecutive reads
// of one row inside this function, so a rename between the ranking query and the open
// cannot make the run name an account it did not bill. The ranking query's copy is
// older and belongs to a different call. Spending same-call provenance to save a
// primary-key lookup would invert D8 on precisely the lane where the SELECTOR, not
// the user, chose the credential.
func (s *Service) openAnthropic(ctx context.Context, userID uuid.UUID, secretID *uuid.UUID) (claimCred, error) {
	var meta struct {
		ID    uuid.UUID
		Label string
	}
	var err error
	if secretID != nil {
		var row store.GetUserSecretMetaByIDRow
		row, err = s.q.GetUserSecretMetaByID(ctx, store.GetUserSecretMetaByIDParams{
			ID:     *secretID,
			UserID: userID,
		})
		meta.ID, meta.Label = row.ID, row.Label
	} else {
		var row store.GetDefaultUserSecretMetaRow
		row, err = s.q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{
			UserID: userID,
			Kind:   store.KindAnthropicToken,
		})
		meta.ID, meta.Label = row.ID, row.Label
	}
	if err != nil {
		// pgx.ErrNoRows here is "no such credential for this user", which is the
		// SAME fact secretopen.ErrNoSecret carried before and must keep producing
		// the identical failure-reason text: a token-less user's run has always
		// failed with this string, and it is read by e2e and handler assertions.
		// Anything else is a real lookup error, surfaced verbatim (no secret bytes).
		if errors.Is(err, pgx.ErrNoRows) {
			return claimCred{}, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
		}
		return claimCred{}, fmt.Errorf("anthropic credential lookup: %w", err)
	}

	tok, err := secretopen.OpenByID(ctx, s.q, s.vlt, s.box, userID, meta.ID)
	switch {
	case err == nil:
		return claimCred{ID: meta.ID, Label: meta.Label, Token: tok}, nil
	case errors.Is(err, secretopen.ErrVaultLocked):
		return claimCred{}, errVaultLocked
	case errors.Is(err, secretopen.ErrNoSecret):
		return claimCred{}, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
	case errors.Is(err, secretopen.ErrUndecryptable):
		return claimCred{}, fmt.Errorf("%w: Anthropic token could not be decrypted", errCredentialUnavailable)
	default:
		// A DB lookup/internal error, surfaced verbatim (carries no secret bytes).
		return claimCred{}, err
	}
}

// recordRunCredential persists WHICH credential a claim spent, on the run it was
// assembled for (PRD #111 M1). Called by all three lanes, always AFTER a successful
// open — an unopened credential was never spent and must never be recorded as if it
// were.
//
// A failure here fails the claim, deliberately. The alternative (log and carry on)
// would deliver a payload whose spend is attributable to nothing, which is the
// silent-wrong-attribution failure this milestone exists to remove; and it is not
// costly, because the run is 'claimed' with no payload delivered, which
// SweepClaimedNeverStarted already requeues at ClaimGrace. A 0-row result is the one
// case that is NOT an error: it means the run vanished under us (its forge
// connection cascade-deleted the repo → run), which every other claim-path reader
// treats as errRunVanished and drops.
func (s *Service) recordRunCredential(ctx context.Context, run store.Run, cred claimCred, choice secretChoice) error {
	// The headroom recorded is the RAW headroom of the pick — what the user's own
	// meters show — never the in-flight-penalised rank, which is an internal ordering
	// key that appears nowhere else in the product. NULL for every non-auto lane,
	// because there is no reading behind those choices; NULL also on D14's retry,
	// where the credential actually spent is the fallback and the measured headroom
	// described the one that would not open.
	headroom := pgtype.Int2{}
	if choice.headroom != nil {
		headroom = pgtype.Int2{Int16: *choice.headroom, Valid: true}
	}
	n, err := s.q.SetRunAnthropicSecret(ctx, store.SetRunAnthropicSecretParams{
		AnthropicSecretID:     pgUUID(cred.ID),
		AnthropicSecretLabel:  pgText(cred.Label),
		AnthropicSelectReason: pgText(choice.reason),
		AnthropicHeadroomPct:  headroom,
		ID:                    run.ID,
		UserID:                run.UserID,
	})
	if err != nil {
		return fmt.Errorf("record run anthropic credential: %w", err)
	}
	if n == 0 {
		return errRunVanished
	}
	return nil
}

// Worker Anthropic bind modes (PRD #111 M3, D1), mirroring migration 00088's CHECK.
// The set is CLOSED: BindModeDefault spends the owner's default credential,
// BindModePinned the one workers.anthropic_secret_id names, and BindModeAuto lets
// the selector choose from the owner's opted-in pool at claim time.
const (
	BindModeDefault = "default"
	BindModePinned  = "pinned"
	BindModeAuto    = "auto"
)

// ValidBindMode reports whether a string is one of the three modes. Exported
// because the handler validates a request body against the same closed set the
// database CHECK enforces, and two spellings of one vocabulary is how a 500 from a
// constraint violation replaces a 400 that could have named the legal values.
func ValidBindMode(mode string) bool {
	switch mode {
	case BindModeDefault, BindModePinned, BindModeAuto:
		return true
	}
	return false
}

// claimSecretID resolves WHICH credential a run-lane claim spends, and is the one
// place that decision is made (R4: three copies of resolution drift, and a wrong
// fallback spends the wrong account silently).
//
//   - self_improve → the owner's JUDGE binding (PRD #104 M4). This branch is not
//     cosmetic and it is not automatic: a self_improve run is repo-ful and rides
//     the ordinary run lane, NOT assembleJudgeClaim, so without it "self-improve
//     follows the judge binding" would simply be false while appearing to be
//     handled. It belongs with the judge because it is uzi reviewing and improving
//     itself — the same activity the judge binding exists to bill separately —
//     not work the user asked a particular worker to do. It is checked FIRST, so a
//     worker's bind mode — auto included — never applies to it.
//   - everything else on this lane (issue, ci_fix) → the claiming worker's BIND
//     MODE decides.
//
// Judge runs never reach here; they fork to assembleJudgeClaim earlier.
func (s *Service) claimSecretID(ctx context.Context, wkr store.Worker, run store.Run) (secretChoice, error) {
	if run.Kind == RunKindSelfImprove {
		id, err := s.judgeSecretID(ctx, run.UserID)
		if err != nil {
			return secretChoice{}, err
		}
		return staticChoice(id, selectReasonJudge), nil
	}
	if wkr.AnthropicBindMode == BindModeAuto {
		return s.autoChoice(ctx, run)
	}
	return staticChoice(workerSecretID(wkr), selectReasonPinned), nil
}

// claimExclude is the credential this claim must NOT resolve onto: the run's
// just-parked dead credential (PRD #217), but only WHILE it is not yet due to retry.
// retry_not_before is the run's retry CADENCE, not a proof the token's real Anthropic
// window has reopened — decideLimitPark can set it below the true reset (Decision 6e
// lowers it to a pooled alternative's availability; the report-less fallback is a
// 15m-doubling guess, limitwait.go). So once retry_not_before has passed — which is
// exactly what let PromoteLimitWaitRuns return the run to queued — the run is DUE for
// another attempt, and #754 M3 relaxes the exclusion so the resume re-picks or floors
// onto that very token instead of holding or switching accounts. That is how a
// single-pooled-token user "continues on cristi": each cadence it re-floors onto the
// token; if the window is genuinely still closed the worker re-parks with a fresh
// real-reset report, and the thrash converges in ~one cycle (bounded overall by
// RUN_LIMIT_MAX_WAITS, whose terminus is a failed run, not a mis-spend — the
// deliberate #754 tradeoff). A run with no dead credential (every non-resume claim)
// excludes nothing.
//
// The "still excluding" branch (a claimable run whose retry_not_before is still in the
// future) is DEFENSIVE: limit_wait's only production exit is PromoteLimitWaitRuns at
// retry_not_before <= now, so no normal resume reaches this function with a future
// stamp. It is kept because excluding is the safe answer should any future transition
// ever hand a not-yet-due run to the claim path, and the M2 exclusion tests inject a
// future stamp to exercise it.
func (s *Service) claimExclude(run store.Run) uuid.UUID {
	if !run.LimitDeadSecretID.Valid {
		return uuid.Nil
	}
	// Window still closed → keep excluding. Relax (Nil) once it has reopened, and also
	// when there is no reset stamp to wait on (nothing says the window is closed).
	if run.RetryNotBefore.Valid && run.RetryNotBefore.Time.After(s.now()) {
		return uuid.UUID(run.LimitDeadSecretID.Bytes)
	}
	return uuid.Nil
}

// autoChoice runs the selector for an `auto` worker (PRD #111 M4, #754 M2).
//
// It is BEHIND claimSecretID, never beside it. PRD #104's R4 is that three copies of
// credential resolution drift and a wrong fallback spends the wrong account silently;
// keeping the selector under the one function that answers "which credential" means
// openAnthropic and assembleClaim never learn that auto exists.
//
// The three impure steps live here and nowhere else: the query, the clock, and the
// policy. autoselect.Select is pure, which is what lets the whole ranking be tested
// against hand-written fixtures with no database.
//
// A query error FAILS the claim rather than degrading to the owner default, and that
// is deliberate in the same way judgeSecretID's is: "the database was unreachable for
// a moment" and "you have no pooled tokens" are different facts, and quietly treating
// the first as the second spends an account the user did not choose while raising
// nothing. The run is retried; a silent mis-spend is not retried, because nobody
// learns it happened.
//
// # The pooled-only invariant (#754)
//
// An auto worker NEVER spends a non-pooled credential — the owner default is NOT
// auto-eligible unless the user pooled it, and an auto lane that quietly bills it is
// the exact bug #754 fixes. That reshapes the old D7 fallback (which resolved
// workerSecretID(wkr) ⇒ the owner default) into a three-rung ladder, every rung of
// which stays inside the pool:
//
//   - Ranking exit (out.Picked): the selector named a measurable pooled token. Record
//     it with its reason and measured headroom. `exclude` (the run's just-parked
//     dead credential, PRD #217 M2) is passed to Select, which drops it from the
//     ranking so it can be neither picked nor the anchor.
//   - Floor (out not Picked, but a pooled token remains): the pool has tokens but none
//     is measurable — a measurable one would have been Picked as best_of_pool — so
//     autoselect.Floor spends the best pooled token anyway (stale/unmeasured
//     included), recorded as pool_stale with no headroom. Floor honours `exclude`
//     exactly as Select does, so the dead credential is never floored onto.
//   - Empty-pool hold (Floor.ok == false): there is genuinely nothing pooled to spend
//     — an empty pool, or the only pooled token is the excluded dead credential. Do NOT
//     spend the non-pooled default and do NOT hard-fail; signal errAutoPoolEmpty, which
//     recoverClaimAssembly holds in the non-locking pool_wait status (PRD #754 M4) so
//     the run waits rather than billing an account the user did not pool.
//
// Floor.ok, not Select's PoolNonEmpty, decides floor-vs-hold: they diverge in the
// excluded-sole-token case (PoolNonEmpty counts before the exclude skip, Floor.ok
// after), and the credential we may actually spend NOW is Floor's question.
func (s *Service) autoChoice(ctx context.Context, run store.Run) (secretChoice, error) {
	userID := run.UserID
	// exclude comes from claimExclude (window-aware): the just-parked dead credential
	// while its window is still closed, uuid.Nil once retry_not_before has reopened it
	// (#754 M3 exclude-relax) or when there is no dead credential.
	exclude := s.claimExclude(run)

	rows, err := s.q.ListAutoSelectCandidates(ctx, userID)
	if err != nil {
		return secretChoice{}, fmt.Errorf("auto-select candidates: %w", err)
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		cands = append(cands, autoselectrow.FromCandidateRow(row))
	}
	out := autoselect.Select(cands, exclude, s.p.Autoselect, s.now())
	if out.Picked {
		id := out.SecretID
		// The gauge is a SMALLINT 0..100 and headroom is derived from it by subtraction,
		// so the value is in range by construction and the narrowing cannot truncate.
		// runs.anthropic_headroom_pct carries a CHECK BETWEEN 0 AND 100 as the backstop.
		h := int16(out.Headroom)
		return secretChoice{secretID: &id, reason: string(out.Reason), headroom: &h}, nil
	}
	// NOT picked. The auto lane NEVER resolves the non-pooled owner default (#754).
	// Floor spends the best pooled token — always unmeasured here, since a measurable
	// one would have been Picked as best_of_pool — recorded as pool_stale, no headroom.
	if floorID, ok := autoselect.Floor(cands, exclude, s.now()); ok {
		id := floorID
		return secretChoice{secretID: &id, reason: string(autoselect.ReasonPoolStale)}, nil
	}
	// Genuinely nothing pooled to spend (empty pool, or the only pooled token is the
	// excluded dead credential). Do NOT spend the default and do NOT hard-fail — signal
	// an empty-pool hold that recoverClaimAssembly holds in pool_wait (PRD #754 M4).
	return secretChoice{}, errAutoPoolEmpty
}

// autoFloorRetry recomputes an auto credential after a picked (or floored) token
// failed to open (D14, #754 M2). It re-lists the user's pooled candidates, drops
// failedID (the token that just would not decrypt), and floors over the rest — still
// honouring the run's just-parked dead credential — so an undecryptable pooled pick
// falls to ANOTHER pooled token, NEVER the non-pooled owner default.
//
// It records reason=open_failed, which fails autoLaneRetryable, so a second open
// failure is terminal by structure. Returns errCredentialUnavailable when no other
// pooled token is available (terminal; the caller fails the run rather than spending
// the default). A candidate-query error propagates as-is — a DB blip is not "the pool
// is empty", the same reasoning autoChoice uses.
func (s *Service) autoFloorRetry(ctx context.Context, run store.Run, failedID uuid.UUID) (secretChoice, error) {
	// exclude comes from claimExclude (window-aware): the just-parked dead credential
	// while its window is still closed, uuid.Nil once retry_not_before has reopened it
	// (#754 M3 exclude-relax) or when there is no dead credential.
	exclude := s.claimExclude(run)
	rows, err := s.q.ListAutoSelectCandidates(ctx, run.UserID)
	if err != nil {
		return secretChoice{}, fmt.Errorf("auto-floor-retry candidates: %w", err)
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		c := autoselectrow.FromCandidateRow(row)
		if c.SecretID == failedID {
			continue // the pick that just failed to open must not be re-floored onto
		}
		cands = append(cands, c)
	}
	floorID, ok := autoselect.Floor(cands, exclude, s.now())
	if !ok {
		// No OTHER pooled token to spend — terminal. Never fall to the non-pooled
		// default (#754); recoverClaimAssembly fails the run on errCredentialUnavailable.
		return secretChoice{}, fmt.Errorf("%w: no other pooled Anthropic token after open failure", errCredentialUnavailable)
	}
	id := floorID
	return secretChoice{secretID: &id, reason: string(autoselect.ReasonOpenFailed)}, nil
}

// workerSecretID is a worker's Anthropic binding as openAnthropic's override: nil
// means "the owner's default", which is what a nil resolves to downstream.
//
// The MODE is what decides, and the id is read in exactly one of the three
// (PRD #111 M3):
//
//   - pinned → the named credential. A NULL id here is D9: it resolves as default,
//     which is what this function already did for an unset binding before the mode
//     column existed, so the rule is kept true rather than newly invented. It is
//     also not a hypothetical — 00078's FK nulls the id when the token is deleted
//     and deliberately leaves the mode, so every pinned worker whose credential is
//     removed lands here.
//   - default → nil, and the id is NOT read. A stale id left behind by a mode
//     change therefore cannot leak into a claim.
//   - auto → nil FOR NOW. M3 ships the mode; M4 fills in this arm with the
//     selector. Until then an auto worker behaves exactly as a default one, which
//     is also the state auto degrades to when the pool is empty or stale (D7/R2) —
//     so the interim behaviour is a supported outcome of the finished feature, not
//     a placeholder that does something the design forbids.
//
// An unrecognised mode is impossible through the API (00088's CHECK and
// ValidBindMode both reject it) and resolves as default if one ever appears, which
// is the safe direction: spending the owner's default is what every worker did
// before any of this existed.
func workerSecretID(wkr store.Worker) *uuid.UUID {
	if wkr.AnthropicBindMode != BindModePinned {
		return nil
	}
	if !wkr.AnthropicSecretID.Valid {
		return nil
	}
	id := uuid.UUID(wkr.AnthropicSecretID.Bytes)
	return &id
}

// assembleClaim builds the claim payload for an already-claimed run. It takes the
// CLAIMING worker, not just the run, because since PRD #104 M3 the credential a
// run spends can depend on which worker picked it up.
func (s *Service) assembleClaim(ctx context.Context, wkr store.Worker, run store.Run) (*ClaimPayload, error) {
	// Judge lane (PRD #46 Decision 1): a judge run has no repo and no forge
	// connection, so it MUST fork before GetRunClaimContext (which INNER-JOINs
	// repos → forge_connections and would treat a repo-less judge run as vanished)
	// and before the bot-PAT open. Its claim carries only the Anthropic token.
	if run.Kind == RunKindJudge {
		return s.assembleJudgeClaim(ctx, run)
	}

	rc, err := s.q.GetRunClaimContext(ctx, run.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errRunVanished
		}
		return nil, fmt.Errorf("claim context: %w", err)
	}

	// #66 D1 layer 3, the claim backstop: this is the single ForgePAT-attach choke
	// point (the PAT is decrypted just below and shipped at ~1790), reached ONLY by
	// PAT-bearing runs — the judge lane forked above, and chat is claimed on a
	// separate lane (ClaimChat/assembleChatClaim; ClaimRun excludes kind<>'chat'), so
	// no extra kind-guard is needed. This is the security net that SUBSUMES layer 2:
	// a run queued while main was protected and claimed after protection was removed
	// is refused HERE rather than pushing. Placed before box.Open so a blocked run is
	// never decrypted. A nil guard skips (same nil-safety as layer 2; production wires
	// it via SetRepoGuard). Overridden comes from the live guardrail_override_reason
	// column GetRunClaimContext now carries (M8): a non-NULL reason means the admin
	// per-repo override is active, so GuardRepo downgrades the waivable findings
	// post-evaluation — never protection_unreadable (D8/D3), so a queued-then-claimed
	// run whose protection read errors is still refused even on an overridden repo.
	if s.guard != nil {
		res := s.guard.GuardRepo(ctx, privcheck.GuardInput{
			ForgeType:       rc.ForgeType,
			BaseURL:         rc.BaseUrl,
			TokenCiphertext: rc.TokenCiphertext,
			Repo: privcheck.Repo{
				ID:             uuid.UUID(run.RepoID.Bytes).String(),
				Path:           rc.RepoPath,
				ForgeProjectID: rc.ForgeProjectID,
				DefaultBranch:  rc.DefaultBranch.String,
			},
			// Live per-repo override (M8): NULL reason ⇒ no override.
			Overridden: rc.GuardrailOverrideReason.Valid,
		})
		if res.Blocked {
			return nil, fmt.Errorf("%w: %s", errGuardrailBlockedClaim, strings.Join(res.BlockMessages(), "; "))
		}
	}

	botPAT, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		// box.Open errors carry no plaintext.
		return nil, fmt.Errorf("%w: bot PAT could not be decrypted", errCredentialUnavailable)
	}

	// Which credential this claim spends: the claiming worker's binding for ordinary
	// runs, the owner's judge binding for self_improve, the owner's default when
	// neither names one (PRD #104 D1). Resolution is per-claim, which is what makes a
	// rebind take effect on the worker's next claim with no restart and no re-minted
	// join token — the token has never ridden the worker, only each claim response.
	choice, err := s.claimSecretID(ctx, wkr, run)
	if err != nil {
		return nil, err
	}
	cred, err := s.openAnthropic(ctx, run.UserID, choice.secretID)
	if err != nil {
		// 🔴 D14, reshaped by #754 M2. Without this arm, "auto never fails a run" is
		// simply untrue. recoverClaimAssembly maps errCredentialUnavailable to
		// MarkRunFailedByID — a TERMINAL failure — so a token that passes the gauge gate
		// and then will not decrypt (a rotated UZI_SECRET_KEY, a corrupt row, a token
		// deleted between the ranking query and the open) kills a run another POOLED
		// token could have completed.
		//
		// #754: the retry NEVER lands on workerSecretID(wkr)/nil/the non-pooled owner
		// default. autoFloorRetry re-floors onto ANOTHER pooled token (excluding the
		// pick that just failed, and still honouring the run's dead-secret exclude); when
		// no other pooled token remains it returns errCredentialUnavailable and the run
		// fails terminally rather than spending the default.
		//
		// Scoped as tightly as it can be, on three axes:
		//   - only a credential the AUTO lane resolved (choice.autoLaneRetryable) — a
		//     selector pick or a floor pick, never pinned/default/judge, which the user
		//     named and whose failure is how they learn the token is broken;
		//   - only errCredentialUnavailable. NOT errVaultLocked: that path already
		//     requeues the run, which is transient and correct, and retrying it would
		//     convert a wait into a spend on the wrong account;
		//   - exactly ONCE, by STRUCTURE. autoFloorRetry records reason=open_failed, which
		//     fails autoLaneRetryable on its REASON conjunct whatever the id turns out to
		//     be — so a second open failure is terminal with no counter and no dependency
		//     on an invariant enforced three files away.
		if !choice.autoLaneRetryable() || !errors.Is(err, errCredentialUnavailable) {
			return nil, err
		}
		// autoLaneRetryable guarantees a non-nil secretID; read it BEFORE overwriting.
		failedID := *choice.secretID
		choice, err = s.autoFloorRetry(ctx, run, failedID)
		if err != nil {
			return nil, err
		}
		cred, err = s.openAnthropic(ctx, run.UserID, choice.secretID)
		if err != nil {
			return nil, err
		}
	}
	// Record it before anything else can fail (PRD #111 M1): the credential HAS been
	// opened at this point, so from the run's perspective it is already the account
	// this claim commits to, whether or not the rest of assembly succeeds.
	if err := s.recordRunCredential(ctx, run, cred, choice); err != nil {
		return nil, err
	}
	anthropic := cred.Token

	// Only the templates allocated to this run's owner ride the claim (PRD #18
	// M7): builtin/global defaults ± the owner's overlay + the owner's own
	// allocated user templates. The reserved-name check (M6) guarantees at most
	// one lead-matching template can exist, so the payload can never carry two.
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgUUID(run.UserID))
	if err != nil {
		return nil, fmt.Errorf("list claim agent templates: %w", err)
	}

	// The run owner's per-user default model overrides the lead template's model
	// on the worker (PRD #17 Decision 6). NULL ⇒ nil ⇒ omitted from the payload,
	// so the worker falls back to the lead template's model. PRD #300 layers a
	// per-schedule freeze on top of this (see the run.Model override just below).
	defaultModel, err := s.q.GetUserDefaultModel(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("default model lookup: %w", err)
	}
	// PRD #300: a schedule can freeze a per-run model onto the run at fire time
	// (runs.model). When present it takes precedence over the owner's per-user Worker
	// default for THIS run's DefaultModel, and is delivered on the SAME default_model
	// claim field the worker already consumes — so the worker is unchanged (Decision 7)
	// and a subagent template's own model: pin, carried separately on each agent, still
	// wins. NULL run.model = inherit = today's behaviour (byte-identical for every
	// non-scheduled run and every schedule without a model).
	if run.Model.Valid {
		defaultModel = run.Model
	}

	// The run owner's per-user default reasoning effort (PRD #617). NULL ⇒ nil ⇒
	// omitted from the payload, so the worker never sets the SDK effort key and the
	// SDK default (`high`) applies. Unlike DefaultModel there is no per-schedule
	// freeze — the owner's per-user value is the only source.
	defaultEffort, err := s.q.GetUserDefaultEffort(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("default effort lookup: %w", err)
	}

	// Skills (PRD #16): the per-run union of every skill allocated to any template
	// for this run's owner (shared ∪ overlay), after precedence + the per-run cap.
	// Re-assembled on every claim, including resume — a skill deleted between claim
	// and resume simply disappears from the resumed session (accepted; the worker
	// logs it). All skill content is user data, never a secret.
	skillRows, err := s.q.ListRunSkillAllocations(ctx, pgUUID(run.UserID))
	if err != nil {
		return nil, fmt.Errorf("list run skill allocations: %w", err)
	}
	skills := assembleRunSkills(skillRows, s.p.SkillsMaxPerRun)

	// Tier-1 tool packages for the worker's provisioning engine (PRD #18 M4): the
	// owner's per-repo profile, re-validated against the current allowlist. A
	// rejected package fails the claim (errToolPackagesRejected → the run is failed
	// in Claim, not delivered). The tier-2 opt-in flag rides from the repos row.
	toolPackages, err := s.resolveTooling(ctx, run)
	if err != nil {
		return nil, err
	}

	// A ci_fix run carries no issue and instead delivers the failed-pipeline
	// snapshot; an issue run carries its issue iid and no pipeline (PRD #6).
	var issueIID *int64
	if run.IssueIid.Valid {
		v := run.IssueIid.Int64
		issueIID = &v
	}
	var pipeline *ClaimPipeline
	if run.Kind == RunKindCIFix {
		pipeline = claimPipelineFromSnapshot(run.FailureSnapshot)
	}

	// PRD #700 / issue #778: for an mr_rework run runs.branch is NULL (the run
	// carries no issue-run branch) and the MR's existing branch lives in
	// pipeline_ref, so source the claim's Branch from pipeline_ref there. The
	// worker still reads it off the already-wired Branch field; no new wire field.
	branch := run.Branch
	if run.Kind == RunKindMRRework {
		branch = run.PipelineRef
	}

	// PRD #122 M1: replay the FROZEN milestone list on every claim. A malformed column
	// degrades to nil-and-log rather than failing the claim, matching the repo_agents
	// decode on the DTO path — the column is data a prior write left, not an invariant
	// of this claim, and stranding a run over it would be worse than serving no list.
	milestones, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		slog.Error("workersvc: decode run milestones", "run_id", run.ID, "error", err)
		milestones = nil
	}

	// PRD #381: replay the structured issue-comments snapshot captured at run
	// creation. A malformed column degrades to nil-and-log rather than failing the
	// claim, matching the milestone decode above — the column is data a prior write
	// left, not an invariant of this claim.
	var issueComments *IssueCommentsSnapshot
	if len(run.IssueComments) > 0 {
		var snap IssueCommentsSnapshot
		if err := json.Unmarshal(run.IssueComments, &snap); err != nil {
			slog.Error("workersvc: decode run issue comments", "run_id", run.ID, "error", err)
		} else {
			issueComments = &snap
		}
	}

	// PRD #700 M2: replay the structured MR review-comments snapshot captured at
	// mr_rework run creation. A malformed column degrades to nil-and-log rather than
	// failing the claim, exactly like the issue-comments decode above.
	var reviewComments *ReviewCommentsSnapshot
	if len(run.ReviewComments) > 0 {
		var snap ReviewCommentsSnapshot
		if err := json.Unmarshal(run.ReviewComments, &snap); err != nil {
			slog.Error("workersvc: decode run review comments", "run_id", run.ID, "error", err)
		} else {
			reviewComments = &snap
		}
	}

	// Run-summary model resolution is user-value-wins (PRD #362 Decision 8), the same
	// shape as the judge model in assembleJudgeClaim but delivered on this ISSUE-run
	// claim: the run owner's per-user summary_model overrides the instance
	// summary_model; NULL/blank inherits the instance value. Guarded by s.settings !=
	// nil — a nil-settings deployment leaves it nil (the summary generator is advisory
	// and skips when no model rides). On a user-row read error we fall back to the
	// instance value best-effort with a log; we never send an empty model.
	var summaryModel *string
	if s.settings != nil {
		if um, uerr := s.q.GetUserSummaryModel(ctx, run.UserID); uerr != nil {
			slog.Warn("issue claim: read user summary model", "user", run.UserID.String(), "error", uerr)
		} else if um.Valid && strings.TrimSpace(um.String) != "" {
			m := um.String
			summaryModel = &m
		}
		if summaryModel == nil {
			if m, merr := s.settings.SummaryModel(ctx); merr == nil && strings.TrimSpace(m) != "" {
				summaryModel = &m
			}
		}
	}

	// PRD #517 M5: deliver the interactive-task park idle backstop ONLY on an interactive
	// task claim (the sole path that parks on awaitFollowUp). Left zero for every other run
	// so omitempty keeps its claim byte-identical to today's wire; the worker falls back to
	// its own TASK_FOLLOWUP_IDLE_MS constant when the field is absent. run.Interactive is
	// immutable from create, so a resumed interactive run re-delivers the same value.
	taskIdleTimeoutSeconds := 0
	if run.Interactive {
		taskIdleTimeoutSeconds = int(s.p.WorkerTaskIdleTimeout.Seconds())
	}

	payload := &ClaimPayload{
		RunID:            run.ID.String(),
		Kind:             run.Kind,
		IssueIID:         issueIID,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		// PRD #381: the structured comments snapshot, decoded above. omitempty keeps a
		// comment-less run's claim byte-identical to today's.
		IssueComments: issueComments,
		// PRD #700 M2: the MR review-comments snapshot, decoded above. omitempty keeps
		// every non-mr_rework run's claim byte-identical to today's wire.
		ReviewComments: reviewComments,
		Status:         run.Status,
		Pipeline:       pipeline,
		Branch:         textPtr(branch),
		SessionID:      textPtr(run.SessionID),
		LastSeq:        run.LastSeq,
		IterationCount: run.IterationCount,
		RequeueCount:   run.RequeueCount,
		PlanMd:         textPtr(run.PlanMd),
		AutoApprove:    run.AutoApprove,
		// PRD #400 M2: task-run MR gate + source ref. open_mr is a plain bool (false
		// for every non-task run); base_branch is pgtype.Text (nil for a run that has
		// none). Both re-read from the row on every claim, like AutoApprove above.
		OpenMr:     run.OpenMr,
		BaseBranch: textPtr(run.BaseBranch),
		// PRD #517 M1: the interactive opt-in rides every claim (a plain bool, false for
		// every non-interactive and non-task run), re-read from the row like OpenMr above so
		// a resumed run re-delivers it unchanged. It tells the worker to keep the run alive
		// (park in awaiting_followup) after signal_done rather than terminating.
		Interactive: run.Interactive,
		// issue #552 M3: re-deliver the durable stop_kind='stopped' fact so a graceful
		// stop survives a worker crash. Derived from the loaded row, like OpenQuestionID
		// below and for the same reason — the in-memory stopRequested flag is lost on a
		// death, but the runs row keeps stop_kind='stopped', so the resumed worker winds
		// the park down instead of waiting out the idle timeout. Never set for a terminal
		// run (a finished run has nothing left to wind down).
		StopPending: run.Interactive && run.StopKind.Valid && run.StopKind.String == "stopped" && !terminalStatuses[run.Status],
		// PRD #400 M4a: when set, this task run is a diff-review of that target task, and
		// the worker (M4b) routes on it. nil for a plain handoff and every non-task run.
		ReviewTargetRunID: uuidPtr(run.ReviewTargetRunID),
		OpenQuestionID:    textPtr(run.OpenQuestionID),
		// PRD #35. Re-read from the row on EVERY claim, like AutoApprove above: a
		// park-resume-park cycle must keep asking the row rather than remembering what
		// the first claim said, so a per-run toggle flipped mid-flight takes effect on
		// the next resume.
		WaitOnLimit: run.WaitOnLimit,
		// plan_approved is derived HERE, not by the worker (Decision 6b). Its two halves
		// are the human one — a consumed approve_plan input, projected by
		// GetRunClaimContext, whose comment carries the gate-bypass invariant this
		// relies on — and autopilot, which never had a gate to pass. A resumed run uses
		// it to skip the Phase-1 planning turn and replay plan_md; without it the resume
		// re-plans, re-parks at awaiting_approval in front of a human who already
		// approved, and can fail with REASON_NO_PLAN when the resumed session declines
		// to re-emit its plan.
		// PRD #209 D8 adds the THIRD disjunct: a seeded run has neither auto_approve
		// (D3 forbids overloading it) nor a consumed approve_plan (M1 asserts none
		// exists), so without this the claim would ship plan_approved:false and the
		// seeded-implement path (D4 row 2) would be unreachable — the feature inert.
		// The compare is a plain string because plan_source is NOT NULL DEFAULT 'agent'
		// (00095), deliberately so this reads `== planSourceSeeded` rather than a
		// pgtype.Text unwrap. Soundness note: this decouples plan_approved from plan_md's
		// provenance, which SetRunAwaitingApproval's plan_source='agent' write re-couples
		// (see GetRunClaimContext's invariant block and runtime.sql D8 comment).
		// PRD #71 M5: the run.AutoApprove disjunct is likewise decoupled from the plan
		// GATE by SetRunAwaitingApproval's symmetric auto_approve=false clear — parking a
		// forceGate ci_fix run for human review clears auto_approve so a restart-requeued
		// resume re-gates rather than shipping plan_approved=true past no human (runtime.sql).
		PlanApproved: run.AutoApprove || rc.HumanPlanApproved || run.PlanSource == planSourceSeeded,
		// PlanSource travels to the worker so it can tell D4 row 2 (seeded, no session ⇒
		// implement) from row 3 (dropped session, not seeded ⇒ re-plan). Server writes
		// it in M1; the worker consumes it in M2. Additive on the wire — an old worker
		// ignores the key.
		PlanSource: run.PlanSource,
		// PRD #209 M4 staleness guard: the commit the seeded plan was written against and
		// whether a divergence should fail the run. Read from the runs row like PlanSource,
		// so both re-deliver unchanged on every resume. PlannedBaseCommit is nil for a run
		// that supplied no commit (the compare is then inert); RequireBaseMatch is a plain
		// bool (NOT NULL DEFAULT false), false for every non-opted-in run.
		PlannedBaseCommit: textPtr(run.PlannedBaseCommit),
		RequireBaseMatch:  run.RequireBaseMatch,
		// Ships with PlanApproved, deliberately adjacent: the two halves of one human
		// verdict, and propagating the approval without the exclusions is what silently
		// gives a user back a subagent they excluded. See ClaimPayload.AgentSelection.
		AgentSelection: persistedSelection(run),
		// PRD #122 M1: the frozen milestone list, decoded above. omitempty keeps a
		// milestone-less run's claim byte-identical to today's.
		Milestones: milestones,
		// PRD #362 Decision 8: the model the inline run-summary generator runs on,
		// resolved user-value-wins above. nil (omitted) when settings are unwired, so
		// an old worker's wire shape is unchanged.
		SummaryModel: summaryModel,
		// PRD #362 M3c (Decision 3): tell the worker whether the intent summary is
		// already set, so a resume/re-claim skips INTENT generation rather than
		// re-spending the owner's token. Derived straight off the run row.
		SummaryIntentPresent: run.SummaryIntent.Valid,
		Repo: ClaimRepo{
			ID:              uuid.UUID(run.RepoID.Bytes).String(),
			URL:             rc.RepoWebUrl,
			CloneURL:        rc.RepoWebUrl + ".git",
			DefaultBranch:   textPtr(rc.DefaultBranch),
			SkillsEnabled:   rc.RepoSkillsEnabled,
			ClaudemdEnabled: rc.RepoClaudemdEnabled,
			ForgeType:       rc.ForgeType,
		},
		Secrets: ClaimSecrets{
			ForgeUsername:       rc.BotUsername,
			ForgePAT:            string(botPAT),
			AnthropicOAuthToken: string(anthropic),
		},
		Agents:        agentsFromTemplates(templates, skills.perTemplate),
		Skills:        skills.union,
		SkillsDropped: skills.dropped,
		Config: ClaimConfig{
			// PRD #122 M2 (Decision 5/5b): serve the EFFECTIVE per-run budget from the
			// persisted columns, falling back to the global default when NULL (a 0/1-
			// milestone run, byte-for-byte today). The worker also reads the scaled wall
			// clock off the state-ack, but the claim carries it too for a fresh resume.
			RunTimeoutSeconds:      coalesceInt(run.BudgetWallSeconds, int(s.p.RunTimeout.Seconds())),
			IdleTimeoutSeconds:     int(s.p.RunIdleTimeout.Seconds()),
			TaskIdleTimeoutSeconds: taskIdleTimeoutSeconds,
			MaxIterations:          coalesceInt(run.BudgetMaxIterations, s.p.RunMaxIterations),
			PlanMaxRevisions:       s.p.PlanMaxRevisions,
			QuestionMax:            s.p.QuestionMax,
			QuestionTimeoutSeconds: s.p.QuestionTimeoutSeconds,
			DefaultModel:           textPtr(defaultModel),
			DefaultEffort:          textPtr(defaultEffort),
			// PRD #305 M3: deliver the flag frozen onto the run at fire time (M1). Read
			// straight off the run row — not re-derived from the schedule. false for every
			// run that did not opt in, so omitempty keeps its claim byte-identical to today.
			OverrideSubagentModel: run.OverrideSubagentModel,
			SkillMaxBytes:         s.p.SkillMaxBytes,
			SkillsMaxPerRun:       s.p.SkillsMaxPerRun,
			ToolPackages:          toolPackages,
			RepoDevboxOptIn:       rc.RepoDevboxOptIn,
			// PRD #123 M1b: ship the Decision 6 denylist base names so the worker applies
			// the same credential-CLI policy to TIER-2 (repo devbox.json opt-in) packages,
			// which are shape-filtered server-side and so never denylist-checked there.
			// Compile-time constant list (no new DB read); always a non-nil slice.
			DeniedToolPackages: toolprofile.DenylistNames(),
			// PRD #71 M2: nil for non-ci_fix runs (column NULL) → omitted by omitempty.
			CIConfigPaths: run.CiConfigPaths,
		},
	}

	// issue #297: a self_improve run carries the in-flight avoid-set so the picker skips
	// a recommendation whose fix another active run is already doing. Best-effort and
	// self_improve-only; every other kind's claim stays byte-identical to today's.
	if run.Kind == RunKindSelfImprove {
		payload.InflightTargets = s.inflightTargets(ctx, run)
		// PRD #686 M10 (D11/D12): the repo's currently-OPEN self-improve MRs' "what was
		// proposed" text, so the picker chooses a non-overlapping improvement. Best-effort
		// and forge-sourced; empty ⇒ omitted so the wire stays byte-identical.
		payload.SelfImproveOpenMRs = s.selfImproveOpenMRs(ctx, run, rc)
		// PRD #686 M3: true only for a repo that opted into uzi dogfooding
		// (repos.fold_improve_uzi_backlog, read from the same GetRunClaimContextRow as
		// RepoDevboxOptIn above); false ⇒ the worker runs the generic directive (m4).
		payload.SelfImproveDogfood = rc.FoldImproveUziBacklog
	}

	return payload, nil
}

// maxInflightTargets caps the self_improve in-flight avoid-set handed to the picker
// (issue #297): newest-first (ListActiveRunsAll is ORDER BY created_at DESC), so the
// most recently started runs win the cap. Advisory context, not a hard block.
const maxInflightTargets = 30

// maxInflightLineLen bounds one assembled in-flight coordinate line (issue #297): the
// titles are untrusted issue/milestone text of unbounded length, so a single line is
// trimmed to keep the avoid-set compact on the wire.
const maxInflightLineLen = 300

// inflightTargets builds the self_improve in-flight avoid-set at claim time (issue
// #297): every non-terminal run on the SAME repo (excluding this self_improve run
// itself), formatted as one compact coordinate line each. Best-effort — a query
// failure yields nil and never fails the claim (mirrors the knownTargets posture in
// assembleJudgeClaim).
//
// ListActiveRunsAll is a GLOBAL, all-repos LIMIT-500 window ordered by recency; the
// same-repo filter runs in Go over that window. On a very busy multi-tenant fleet a
// repo's in-flight runs could in principle be crowded out of the 500 newest rows and
// silently drop from the avoid-set. That is acceptable here: this set is ADVISORY
// context for the picker (D4), not a correctness gate — a missed entry only means the
// picker might overlap, which the human MR review still catches. Reusing the existing
// query is the deliberate trade for no new query and no migration (D5).
func (s *Service) inflightTargets(ctx context.Context, run store.Run) []string {
	rows, err := s.q.ListActiveRunsAll(ctx, s.activeRunsPriorityCutoff())
	if err != nil {
		slog.Warn("self_improve claim: list active runs for in-flight set", "run", run.ID.String(), "error", err)
		return nil
	}
	var out []string
	for _, row := range rows {
		r := row.Run
		if r.ID == run.ID || r.RepoID != run.RepoID {
			continue // exclude self and other repos
		}
		out = append(out, formatInflightLine(r))
		if len(out) >= maxInflightTargets {
			break
		}
	}
	return out
}

// formatInflightLine renders one active run as a single compact coordinate line for the
// self_improve in-flight avoid-set (issue #297). Shape:
//
//	issue #<iid> "<title>" (kind=<kind>, status=<status>) — milestones: <id> "<title>"; ...
//
// An issue-less kind (self_improve/ci_fix has a NULL issue_iid) drops the "#<iid>" and
// leads with "<kind> run" instead. The milestone tail is omitted when MilestonesFrozen is
// empty or fails to decode. The whole line is trimmed to maxInflightLineLen. All text is
// untrusted repo content — the worker renders it nonce-fenced, never as instructions.
func formatInflightLine(r store.Run) string {
	var b strings.Builder
	if r.IssueIid.Valid {
		fmt.Fprintf(&b, "issue #%d", r.IssueIid.Int64)
		if r.IssueTitle != "" {
			fmt.Fprintf(&b, " %q", r.IssueTitle)
		}
	} else {
		fmt.Fprintf(&b, "%s run", r.Kind)
	}
	fmt.Fprintf(&b, " (kind=%s, status=%s)", r.Kind, r.Status)

	// MilestonesFrozen is data a prior write left behind, not an invariant of this read:
	// a decode error just omits the milestone tail (best-effort, matching the claim path).
	if ms, err := DecodeMilestones(r.MilestonesFrozen); err == nil && len(ms) > 0 {
		b.WriteString(" — milestones:")
		for i, m := range ms {
			if i > 0 {
				b.WriteByte(';')
			}
			fmt.Fprintf(&b, " %s %q", m.ID, m.Title)
		}
	}

	line := b.String()
	if len(line) > maxInflightLineLen {
		// Trim back to a rune boundary so an untrusted multibyte title is never sliced
		// mid-rune into invalid UTF-8.
		cut := maxInflightLineLen
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut]
	}
	return line
}

// maxOpenSelfImproveMRCandidates bounds how many recent MR-bearing self_improve runs
// the open-MR picker context checks live against the forge (PRD #686 D11/D12). Cycles are
// days apart and the concurrent-open cap is small, so a tiny window covers every
// plausibly-open MR without an unbounded historical scan. Mirrors schedsvc's
// selfImproveMRCandidateWindow.
const maxOpenSelfImproveMRCandidates = 10

// maxOpenSelfImproveMRs caps the open-MR "what was proposed" lines handed to the picker
// (PRD #686 D11). At most 2 can be open per the concurrent-open cap (schedsvc), but this
// stays generous so a transient over-cap window still renders every open MR.
const maxOpenSelfImproveMRs = 5

// selfImproveOpenMRs builds the self_improve open-MR picker context at claim time (PRD
// #686 D11): the "what was proposed" text of the repo's currently-OPEN self-improve MRs,
// so the picker chooses a non-overlapping improvement. Best-effort throughout — any
// query/forge error yields nil (or skips the offending candidate) and NEVER fails the
// claim, mirroring inflightTargets' posture. Unlike m9's fire-time cap (which must be
// strict), this is advisory context, so a per-candidate GetMergeRequest error skips only
// that candidate and the loop continues.
//
// Open-state is resolved LIVE from the forge per candidate (D12): runs.mr_state is
// unreliable for this multi-MR-per-tracking-issue lane. The proposed text comes from the
// RUN ROW (plan_md if present, else issue_description — plan_md is NULL for autopilot
// self_improve runs today, so issue_description is the effective source), never from the
// MR title/body: GetMergeRequest is used ONLY for the open-state check.
func (s *Service) selfImproveOpenMRs(ctx context.Context, run store.Run, rc store.GetRunClaimContextRow) []string {
	if s.forges == nil {
		return nil
	}
	f, err := s.forges.ForgeForConnection(rc.ForgeType, rc.BaseUrl, rc.TokenCiphertext)
	if err != nil {
		slog.Warn("self_improve claim: build forge for open-MR set", "run", run.ID.String(), "error", err)
		return nil
	}
	rows, err := s.q.RecentSelfImproveMRRunsForRepo(ctx, store.RecentSelfImproveMRRunsForRepoParams{
		RepoID: uuid.UUID(run.RepoID.Bytes),
		Lim:    maxOpenSelfImproveMRCandidates,
	})
	if err != nil {
		slog.Warn("self_improve claim: list recent self-improve MR runs", "run", run.ID.String(), "error", err)
		return nil
	}
	var out []string
	for _, row := range rows {
		if row.ID == run.ID || !row.MrIid.Valid {
			continue // exclude self and any row without an MR iid
		}
		mr, err := f.GetMergeRequest(ctx, rc.ForgeProjectID, row.MrIid.Int64)
		if err != nil {
			// Best-effort: this is advisory context, not the strict fire-time cap, so a
			// per-candidate forge error skips only this candidate and the loop continues.
			slog.Warn("self_improve claim: check open-MR state", "run", run.ID.String(), "mr_iid", row.MrIid.Int64, "error", err)
			continue
		}
		if mr.State != forge.MRStateOpened {
			continue
		}
		proposed := row.IssueDescription
		if row.PlanMd.Valid && strings.TrimSpace(row.PlanMd.String) != "" {
			proposed = row.PlanMd.String
		}
		if line := firstNonEmptyLine(proposed); line != "" {
			out = append(out, line)
			if len(out) >= maxOpenSelfImproveMRs {
				break
			}
		}
	}
	return out
}

// firstNonEmptyLine returns the first non-blank line of s, trimmed and bounded to
// maxInflightLineLen on a rune boundary (the same untrusted-text bound the in-flight set
// uses). Empty when s has no non-blank line.
func firstNonEmptyLine(s string) string {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if len(line) > maxInflightLineLen {
			cut := maxInflightLineLen
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = strings.TrimSpace(line[:cut])
		}
		return line
	}
	return ""
}

// errToolPackagesRejected marks a claim whose grandfathered tool packages fell out
// of the (possibly shrunk) allowlist at claim time. Non-retryable: the run is
// failed with this message (which lists the rejected package names — never secret
// bytes) so the owner fixes the profile or an admin restores the allowlist entry.
var errToolPackagesRejected = errors.New("tool packages no longer allowed")

// resolveTooling resolves the run's TIER-1 tool packages for the claim payload
// (PRD #18 M4). The desired list is the run owner's per-(user,repo)
// repo_tool_profiles, RE-VALIDATED against the current tool_allowlist (it can
// shrink after the profile was saved — Technical §3). A rejected package fails the
// claim (Success Criteria 5), not silently drops. The tier-2 repo_devbox_opt_in
// flag rides separately (set from the repos row in assembleClaim); the worker does
// the tier-2 extraction after clone (PRD #18 M5).
func (s *Service) resolveTooling(ctx context.Context, run store.Run) (toolPackages []string, err error) {
	// assembleClaim only reaches resolveTooling for a run-lane (issue/ci_fix) run,
	// whose repo_id is always non-NULL (runs_kind_shape), so the conversion is safe.
	profile, err := s.q.GetRepoToolProfile(ctx, store.GetRepoToolProfileParams{UserID: run.UserID, RepoID: uuid.UUID(run.RepoID.Bytes)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []string{}, nil // no profile ⇒ no tier-1 provisioning
		}
		return nil, fmt.Errorf("get repo tool profile: %w", err)
	}
	desired := decodePackageList(profile.Packages)
	if len(desired) == 0 {
		return []string{}, nil
	}
	rules, err := s.loadToolRules(ctx)
	if err != nil {
		return nil, err
	}
	allowed, rejected := toolprofile.Resolve(desired, rules)
	if len(rejected) > 0 {
		return nil, fmt.Errorf("%w: %s", errToolPackagesRejected, strings.Join(rejected, ", "))
	}
	// PRD #123 M3 (Decision 4c): an allowlisted package that is not in the baked
	// worker toolchain cannot be provisioned behind the egress block, so fail the
	// claim here rather than let the run hang at 0 iterations. Wrap
	// errToolPackagesRejected so recoverClaimAssembly's existing terminal handling
	// applies (the run is failed with the offending names, never secret bytes).
	var unbaked []string
	for _, p := range allowed {
		if !toolseed.Covered(p) {
			unbaked = append(unbaked, p)
		}
	}
	if len(unbaked) > 0 {
		return nil, fmt.Errorf("%w: not in baked toolchain (image roll required): %s", errToolPackagesRejected, strings.Join(unbaked, ", "))
	}
	if allowed == nil {
		allowed = []string{} // always send an array, never null (wire contract)
	}
	return allowed, nil
}

// loadToolRules projects the DB tool_allowlist into the toolprofile.Rules map the
// pure resolver consumes, via the shared loader (identical to the write-time
// loader in the handler, so save and claim can never diverge).
func (s *Service) loadToolRules(ctx context.Context) (toolprofile.Rules, error) {
	rows, err := s.q.ListToolAllowlist(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tool allowlist: %w", err)
	}
	return toolprofile.RulesFromRows(rows), nil
}

// decodePackageList decodes a repo_tool_profiles.packages JSONB array into a slice.
// A NULL/empty/malformed column yields an empty list (never fails the claim on
// bad data — an out-of-band write can't wedge a run).
func decodePackageList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		slog.Error("workersvc: decode repo tool profile packages", "error", err)
		return nil
	}
	return out
}

// Caps for the two PRD #99 attribution fields on the UNTRUSTED worker insert
// path. `agent_label` is free, model-authored prose stored in a bare `text`
// column, and Decision 1 repeats it on EVERY frame of an invocation — so one
// label is stored N times, where N is that subagent's whole frame count. The
// only other ceiling in the path is httpx.DecodeJSON's 1 MiB LimitReader, which
// is PER BATCH, so a single message could otherwise carry a ~1 MiB label.
//
// This belongs here rather than in the pane: web-side truncation is a render
// concern that does nothing for storage and nothing for `api/cmd/uzi`, a second
// consumer that would print the full string to a terminal.
//
// 80 runes matches maxChatTitleRunes (workersvc/chat.go) — the closest analogue,
// a one-line title derived from untrusted text — and the field is exactly that:
// a lane title. The SDK's `task_description` mirrors the Agent tool's short
// `description` argument (RunEvent.tsx already renders it through `firstLine`,
// i.e. one line by construction), and the stub fixture's real labels are ~11
// runes, so 80 is generous for the intended content while bounding the
// amplification. NOT measured against a live SDK run — no live session is
// available here; revisit if real labels are seen to clip.
//
// Truncate, never reject: a rejected batch is a lost message, and the label is
// cosmetic. Rune-based, not byte-based, per the house idiom — byte-slicing can
// split a multibyte rune into invalid UTF-8, which the INSERT then rejects
// (spelled out at handler/cli_auth_flow.go:148).
const (
	maxAgentLabelRunes    = 80
	maxAgentInstanceRunes = 128 // an SDK `toolu_*` id is ~30; this is headroom, not a fit
	// maxAgentRunes caps the THIRD worker-controlled attribution field, which had
	// no cap at all until PRD #108 M2 — not "truncated but not stripped" like the
	// two above, but wholly unbounded into an unbounded `text` column
	// (00020_workers_runs.sql), on the same untrusted route and with the same
	// per-frame repetition.
	//
	// The number is not new: agenttmpl.MaxNameLen is the existing authority on how
	// long an agent name may be, mirrored by the worker's AGENT_NAME_MAX_LEN, and
	// `agent` is exactly a subagent name. Reusing it means there is one cap to
	// change rather than two to keep in step. It is a BYTE bound there and a rune
	// bound here — legitimate names are [a-z0-9-] so the two coincide, and for a
	// hostile non-ASCII value this admits at most 4x, which still bounds it.
	//
	// Truncate, never reject, for the reason spelled out above: a rejected batch is
	// a lost message and this field is attribution.
	maxAgentRunes = agenttmpl.MaxNameLen
	// maxKindRunes caps `kind`, which f2ddb5ce NUL-stripped but left uncapped — so
	// it was the last worker-controlled text column with no length bound, on the
	// same untrusted route and with the same per-frame repetition that justified
	// capping `agent`. It also rides two log lines per message, so an unbounded kind
	// is an unbounded log WRITE on top of an unbounded column; the cap is applied
	// before those lines read it. 64 is comfortably above the whole SDK-frame
	// vocabulary (`tool_result`, `plan_revising`, … all under 14) while bounding a
	// hostile value; the column is bare `text NOT NULL` (00020_workers_runs.sql), so
	// the number is a policy choice, not a schema fit (PRD #108 A3).
	maxKindRunes = 64

	// maxQuestionIDRunes bounds the worker-minted clarification-question identity
	// (PRD #88 M1). It is an opaque token the server only ever compares for equality
	// — never parses — so the cap exists solely to keep a hostile worker from writing
	// an unbounded value into runs.open_question_id and every answer row that quotes
	// it. A UUID is 36; 128 leaves room for a differently-shaped id without inviting
	// prose. Like maxKindRunes, the column is bare `text`, so this is policy.
	maxQuestionIDRunes = 128

	// maxAnswerBodyRunes bounds a user's answer to a clarification question (PRD #88
	// M1, D-G). Slack's inbound path already caps replies at maxReplyRunes; the web
	// and CLI paths had no bound at all, and #88 is the feature that turns "the user
	// types free text at the agent" from an occasional follow_up into a prompted
	// round-trip, so the cap moves to SubmitInput where every surface inherits it.
	maxAnswerBodyRunes = 4000

	// maxAnswerCount bounds how many answers one submission may carry (PRD #88,
	// auditor LOW-2). It mirrors the question-side cap in agent/src/signals.ts
	// (MAX_QUESTIONS): answers are index-aligned with the questions, so more answers
	// than could ever have been asked is malformed by construction. Without it the
	// only bound was the 1 MiB request body — tens of thousands of short strings,
	// each scrubbed and re-encoded.
	maxAnswerCount = 10
)

// truncateRunes caps s at n runes, cutting on a rune boundary so the result is
// always valid UTF-8. No ellipsis: unlike a chat title this value is also a
// grouping key on the read side, and appending a character would make a
// truncated label collide differently than the raw one.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// IncomingMessage is one seq-numbered message a worker appends.
//
// Compat note: DecodeJSON sets DisallowUnknownFields, so a field a NEW worker
// sends must already be DECLARED here in the release BEFORE that worker ships, or
// the whole batch 400s and the batcher retries it forever. That rule is stated in
// full on the register path (handler/worker_protocol.go, the Capabilities field);
// it is repeated here because a reader of the messages path would not find it
// there. The PRD #99 fields satisfy it: they were declared in M1, which lands
// strictly before M2 starts emitting them.
type IncomingMessage struct {
	Seq   int32  `json:"seq"`
	Kind  string `json:"kind"`
	Agent string `json:"agent"`
	// AgentInstance/AgentLabel are the PRD #99 per-frame subagent invocation id
	// (the SDK's parent_tool_use_id) and its task description. Both are absent
	// when the frame carried no parent_tool_use_id; empty string persists as NULL.
	AgentInstance string          `json:"agent_instance,omitempty"`
	AgentLabel    string          `json:"agent_label,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// AppendMessages persists a worker's batched messages (idempotent on
// (run_id, seq)) and advances the run's last_seq high-water mark. The worker
// must own the run.
//
// It is a thin RECORDER around appendMessages (PRD #108 M4): the persistence work
// is unchanged and lives below, and this layer exists only to feed the per-run
// failure streak the health detector and the auto-stop evaluator read. The split
// is what lets the counter see EVERY failure return of a function that has five of
// them, instead of the one a caller happens to remember to instrument.
//
// The recording rules and the reasons they are rules, not defaults, are on each
// arm of the switch; the invariant they all serve is persistfail.go's ownership
// tripwire.
func (s *Service) AppendMessages(ctx context.Context, wkr store.Worker, runID uuid.UUID, msgs []IncomingMessage) error {
	obs, err := s.appendMessages(ctx, wkr, runID, msgs)
	switch {
	case !obs.resolved:
		// Ownership never resolved (ErrRunNotOwned, or the lookup itself failed), so
		// this run is not this worker's to vouch for. Recording here is what would let
		// worker A build a kill streak against user B's run — see persistfail.go's
		// ownership tripwire. NOT a persistence failure, and not counted as one.
	case obs.status != "running":
		// NOT RUNNING — checked BEFORE the success arm on purpose, and stated as one
		// rule rather than as a terminal special case: A STREAK IS EVIDENCE ABOUT ONE
		// RUNNING ATTEMPT, so leaving `running` ends that attempt's claim on it.
		//
		// This single arm retires a whole class of defect instead of one path of it:
		//
		//   - TERMINAL. worker_id survives the terminal transition and neither
		//     GetRunOwnedByWorker nor this method filters on status, so a late or
		//     hostile POST would resurrect a streak on a dead run one tick after
		//     eviction — and on the SUCCESS path would keep a terminal run in M5's
		//     comparison set indefinitely for one deduplicated append every few minutes.
		//   - REQUEUED. The evaluator evicts too, but it only ever sees CANDIDATES
		//     (streak >= autoStopStreak and the window elapsed), so a SUB-THRESHOLD
		//     streak crossed a requeue untouched — and kept growing, since this method
		//     went on recording against a queued run. Measured: 12 carried across, then
		//     the fresh attempt killed after 8 new failures, with the entire window leg
		//     satisfied by the DEAD attempt's firstAt. A streak must pass through 12 to
		//     reach 20, so which side of the threshold an OOM lands on is close to a
		//     coin flip; half that population landed here.
		//   - Every OTHER path that resets status without a hook, including Register's
		//     RequeueWorkerRuns, which returns no ids and so can never have one.
		//
		// Sweep's two requeue-site evictions and the evaluator's are now belt and
		// braces rather than the mechanism, which is the safer arrangement: this arm
		// needs no candidacy test and no enumeration of the paths.
		//
		// 🔴 AND IT HAS A SECOND EFFECT, WHICH IS DELIBERATE. Sitting above the
		// success arm means recordSuccess fires ONLY for running runs, so this also
		// narrows M5's G4 COMPARISON SET to running runs. Measured, per status:
		//
		//	running            joins lastOK  -> counts as a peer
		//	awaiting_approval  does not      -> does not count
		//	claimed            does not      -> does not count
		//	queued             does not      -> does not count
		//	completed          does not      -> does not count
		//
		// So a run parked at the approval gate that IS successfully persisting
		// messages — alive and doing real work by any ordinary reading — no longer
		// vouches for the write path. That is intended on both counts: it is the
		// fail-safe direction (fewer peers ⇒ fewer kills), and it keeps the earlier
		// hardening's rule intact, that warming the comparison set should cost a live
		// run doing real work rather than a parked or finished one.
		//
		// Written down because the three bullets above are all about STREAKS, and a
		// reader restoring recordSuccess for a parked run would be undoing something
		// deliberate with nothing in the place they would look to say so. Pinned by
		// TestAppendMessagesComparisonSetIsRunningRunsOnly.
		s.persistFail.evict(runID)
	case err == nil:
		s.persistFail.recordSuccess(runID, s.now())
	default:
		s.persistFail.recordFailure(runID, classifyPersistFail(err), obs.lastSeq, s.now())
	}
	return err
}

// appendObservation is what the recorder needs from one append attempt and cannot
// get from the error alone: whether ownership resolved at all, whether the run was
// already terminal, and the run's message high-water mark AS THIS ATTEMPT SAW IT.
//
// lastSeq is max(runs.last_seq, maxStored) — the PRD's "max(seq) has not advanced"
// evidence. It reads runs.last_seq rather than a SELECT max(seq) FROM run_messages
// because UpdateRunLastSeq is `last_seq = GREATEST(last_seq, @seq)` over maxStored,
// which counts deduplicated rows too, so the column is a faithful high-water mark
// that is already on the row this path holds and costs no extra query.
type appendObservation struct {
	// resolved is true once runOwnedByWorker has returned a run — i.e. once the
	// caller is known to own this run. Nothing may be recorded while it is false.
	resolved bool
	// status is the run's status as this attempt read it. Carried whole rather than
	// as a derived `terminal bool` on purpose: the recorder's rule is "is this run
	// RUNNING", and a boolean named for one of the several non-running cases invites
	// the next reader to treat that case as the rule. One field cannot disagree
	// with itself.
	status  string
	lastSeq int32
}

// NoteOversizeBatch records a 413 against the run's persistence-failure streak.
//
// It exists because a 413 is answered in handler.WorkerRunMessages BEFORE
// AppendMessages is ever called, so an oversize batch is otherwise invisible to
// the recorder. That is not academic: a pre-0.10.1 worker's retry batch GROWS (PRD
// #108 M0 defect 4), so the incident's own long tail rotates 500 → 413 and then
// stays 413 forever. Without this hook, both M4's flag and M5's kill go blind in
// exactly that steady state.
//
// It re-checks ownership ITSELF — the one recording hook not already below
// runOwnedByWorker — because an unowned record is a cross-tenant kill primitive.
// Best-effort: a lookup failure records nothing.
//
// COST, stated for the case that is not benign. This arm previously did zero
// database work; it now does one indexed lookup. Rare for the incident's own
// shape (a 413 means a worker already past the 1 MiB cap), but a worker holding a
// valid join token can POST oversized bodies as fast as it likes, so this is one
// GetRunOwnedByWorker per such request — on a path that exists because the
// database is already under stress. The alternative was leaving both the flag and
// the kill blind in the incident's own steady state, which is worse; naming the
// cost is not the same as calling it free.
func (s *Service) NoteOversizeBatch(ctx context.Context, wkr store.Worker, runID uuid.UUID) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return
	}
	// THE SAME RULE AS THE RECORDER'S, and it must stay the same rule. This hook was
	// left on the old terminal-only check when AppendMessages moved to
	// `status != "running"`, so the two recording sites disagreed with each other —
	// one recording on any non-running run, one unless terminal. That is a worse
	// divergence than the `terminal bool` the observation type was changed to avoid,
	// because the boolean at least meant the same thing in both places.
	//
	// It was reachable, and through the case that forced the status narrowing to
	// begin with: /state is a different route and does not wedge, so a run reports
	// its plan and parks at `awaiting_approval` while a pre-0.10.1 batcher keeps
	// re-POSTing its grown batch and takes a 413 each time. Measured: streak 20 built
	// entirely at the gate, `window_seconds=95`, then the human approves and the
	// first sweep after the run returns to `running` kills it — and `oversize` IS a
	// killable class, so nothing downstream stopped it.
	if run.Status != "running" {
		s.persistFail.evict(runID)
		return
	}
	s.persistFail.recordFailure(runID, persistFailOversize, run.LastSeq, s.now())
}

// appendMessages is AppendMessages' persistence half. It returns the observation
// the recorder needs alongside the error; every early return therefore carries the
// state observed so far.
//
// ONLY the InsertRunMessage error is eligible for the ErrUnstorableMessage (→
// 400) classification, and that narrowness is the point rather than an oversight.
// This function returns store errors from three places — the insert,
// UpdateRunLastSeq and foldRunUsage — and 400 on this route tells the worker
// "this batch is permanently poisoned, stop retrying it". Only the insert's error
// is evidence for that claim. Reporting a failure from elsewhere as the batch's
// fault makes the worker drop messages that were never the problem: data loss
// from a misattributed error. The narrow shape has in-repo precedent in
// handler/secrets.go's constraint-name check, for the same reason.
//
// 🔴 THE ASSUMPTION THIS RESTS ON, AND WHEN IT BREAKS:
//
//	EVERY worker-controlled value that reaches the store on this path does so
//	through the sanitized InsertRunMessage.
//
// If you add a store call here that writes a worker-controlled value, or add such
// a value to one of the two existing calls, THAT VALUE IS NO LONGER COVERED —
// silently. It gets a 500, the worker retries it forever, and this PRD's exact
// wedge reappears in a new location with no signal. Revisit this placement in the
// same change, and extend the audit below rather than assuming it still holds.
//
// The audit as it stands, per non-insert call:
//
//   - UpdateRunLastSeq — takes the run id and an int32 seq. No worker text.
//   - BumpRunLineageEpoch — takes only the run id (a uuid) and writes a
//     server-computed `lineage_epoch + 1`. No worker-controlled text or value
//     reaches the store through it, so it cannot produce a WORKER-VALUE-DEPENDENT
//     22P02/22021/54000/22003 code the way an unsanitized text/numeric column
//     could — which is what makes returning its error raw correct. (It could in
//     principle raise a state-dependent 22003 only if the counter ever reached
//     INT_MAX — not worker-value-dependent, needs ~2^31 events, and the raw return
//     still handles it: the worker retries, the seq-deduped break is skipped, and
//     no poison loop forms.) A CLEARED suspect, written out for the same reason as
//     foldRunUsage below: it is correctly returned raw (500), never wrapped,
//     because a failure is a genuine server error the worker should retry, not a
//     poisoned batch.
//   - foldRunUsage → UpsertRunUsage — every column it writes, checked one by one
//     because "I cannot think of a case" is not the same as "there is no case".
//     Read this as a CLEARED suspect, not a live hazard: it is written out because
//     it is the call that looks most dangerous and the check is what makes the
//     placement defensible, not because it can currently produce these codes.
//     `model` IS worker-controlled (a JSON object key out of the payload), and
//     `session_id` is too (the worker reports it; the runs row only relays it), so
//     BOTH are handled on TWO axes, not one. Byte VALIDITY: sanitation writes
//     through the index in a pass that completes before foldRunUsage iterates msgs,
//     so the model name the fold inserts is already NUL-free — pinned by
//     TestWorkerMessagesUsageFoldSeesSanitizedModelNamesLiveDB, which reddens if
//     that ordering inverts. LENGTH: both are members of run_usage's composite PK,
//     whose btree index entry caps at 2704 bytes, so foldRunUsage caps each with
//     truncateRunes before the upsert (maxUsageSessionRunes) — without which an over-long
//     value raises SQLSTATE 54000, which is NOT in unstorableSQLSTATEs and would
//     wedge the run one sink over (pinned by the UsageFoldCapsOversized*LiveDB
//     tests). `session_id`'s earlier acceptance into the runs row is not evidence
//     here: that column is unindexed `text`, and acceptance there says nothing
//     about indexability inside this composite PK — which is why relaying it is not
//     on its own sufficient. `run_id` is a uuid. The token columns are bigint and
//     take an int64, which always fits. `cost_usd` is numeric(12,6) and numericUSD
//     clamps to that domain (its own comment names 22003 as the poison-loop trigger
//     it exists to prevent).
//
// A broader wrap was considered and rejected: with the above holding it catches
// nothing extra, while reintroducing exactly the misattribution this narrowness
// exists to prevent.
func (s *Service) appendMessages(ctx context.Context, wkr store.Worker, runID uuid.UUID, msgs []IncomingMessage) (appendObservation, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return appendObservation{}, err
	}
	obs := appendObservation{resolved: true, status: run.Status, lastSeq: run.LastSeq}
	// Validate the whole batch before persisting any of it: a single invalid
	// message rejects the batch with nothing written, so a [valid, valid, invalid]
	// batch never leaves the first two half-persisted.
	//
	// That all-or-nothing property is TRUE OF VALIDATION AND FALSE OF THE STORE.
	// The insert loop below is not transactional, so a batch whose third message
	// the database refuses leaves the first two committed. This comment used to
	// claim the batch was all-or-nothing outright; that was unreachable in practice
	// before PRD #108 and is routine after it, and it is about to be load-bearing
	// for the worker's bisection, so it is corrected here rather than left to
	// mislead. Idempotency on (run_id, seq) is what keeps the partial apply benign:
	// any regrouping or re-post converges. Making it genuinely transactional needs
	// a Store interface change (the generated queries take no tx) and is deferred
	// to Phase 2, which has its own reason to care — its "max(seq) has not
	// advanced" guard reads the column this path leaves behind.
	for i := range msgs {
		m := &msgs[i]
		if m.Seq <= 0 || m.Kind == "" || len(m.Payload) == 0 || !json.Valid(m.Payload) {
			return obs, ErrInvalidMessage
		}
		// A kind of nothing but NUL escapes passes the emptiness check above — it is
		// non-empty on the wire — and would then strip to "" in the sanitation pass,
		// which `text NOT NULL` accepts happily. Check the POST-STRIP value HERE, in
		// the validation pass, rather than after stripping: an empty kind is exactly
		// what that check exists to reject, and rejecting it here keeps both batch
		// invariants intact — nothing is written, and nothing is logged as laundered.
		// The double stripNUL costs one strings.Count on the fast path.
		if stripped, _ := stripNUL(m.Kind); stripped == "" {
			return obs, ErrInvalidMessage
		}
	}
	// SECOND pass for sanitation, separate from validation on purpose. The
	// count-and-log requirement (PRD #108 Risk 3) exists so a future NUL-emitting
	// tool stays visible, and folding it into the loop above would report only up
	// to the first invalid message — the batches most worth understanding are
	// exactly the ones that would go unreported. Split this way, a batch rejected
	// by validation logs nothing (nothing was laundered, because nothing is
	// stored) and a batch that proceeds logs every message it altered.
	//
	// It writes through the index (not a range copy), so the capped and sanitized
	// values are what the insert, the WS broadcast and the usage fold all see —
	// otherwise the stored row and the live frame would disagree, and the fold
	// would still be reading the unstorable bytes.
	for i := range msgs {
		m := &msgs[i]
		var c stripCounts
		// Strictly AFTER the json.Valid check above: the scanner presumes
		// well-formed JSON, and today's invalid-JSON→400 arm must keep answering
		// first.
		m.Payload, c = sanitizePayloadJSON(m.Payload)
		// FOUR text sinks, not three. `kind` is worker-supplied and lands in a bare
		// `text NOT NULL` column whose vocabulary lives in a COMMENT with no CHECK
		// constraint (00020_workers_runs.sql), and it decodes into a Go string exactly
		// like the other three — so a u0000 escape becomes a real 0x00 and Postgres
		// answers 22021. Ordinary tool output cannot produce it (kind comes from a
		// fixed SDK-frame vocabulary), so this needs a hostile or buggy worker — which
		// is precisely the threat model that produced sanitizeSelfReported on this
		// same route. Stripped BEFORE the log below, so the log line reports a clean
		// kind rather than smuggling the NUL into the operator's terminal.
		var nKind, nAgent, nInstance, nLabel int
		m.Kind, nKind = stripNUL(m.Kind)
		m.Agent, nAgent = stripNUL(m.Agent)
		m.AgentInstance, nInstance = stripNUL(m.AgentInstance)
		m.AgentLabel, nLabel = stripNUL(m.AgentLabel)
		c.textNUL = nKind + nAgent + nInstance + nLabel
		// Cap `kind` HERE, before the warn log below echoes it (and the second log on
		// a permanently-unstorable insert): an unbounded kind is an unbounded log
		// write, not only an unbounded column. Capped after the strip so the cap
		// counts runes the row will actually hold; `agent` is capped further down.
		m.Kind = truncateRunes(m.Kind, maxKindRunes)
		if c.any() {
			slog.Warn("workersvc: sanitized unstorable bytes out of a worker message",
				"run_id", runID.String(), "seq", m.Seq, "kind", m.Kind,
				"payload_nul_dropped", c.payloadNUL,
				"payload_unpaired_surrogates_replaced", c.payloadSurrogate,
				"payload_invalid_utf8_replaced", c.payloadBadUTF8,
				"text_column_nul_dropped", c.textNUL)
		}
		// Strip BEFORE truncating, so each rune cap is counted over the NUL-free
		// string the row will actually hold.
		m.Agent = truncateRunes(m.Agent, maxAgentRunes)
		m.AgentInstance = truncateRunes(m.AgentInstance, maxAgentInstanceRunes)
		m.AgentLabel = truncateRunes(m.AgentLabel, maxAgentLabelRunes)
	}
	// maxStored is the high-water mark of what ACTUALLY reached the table,
	// including rows the insert deduplicated (rows == 0 means this (run_id, seq)
	// was already persisted by an earlier delivery — stored is stored).
	var maxStored int32
	var insertErr error
	inserted := make([]IncomingMessage, 0, len(msgs))
	for _, m := range msgs {
		rows, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID:         runID,
			Seq:           m.Seq,
			Kind:          m.Kind,
			Agent:         pgText(m.Agent),
			AgentInstance: pgText(m.AgentInstance),
			AgentLabel:    pgText(m.AgentLabel),
			Payload:       []byte(m.Payload),
		})
		if err != nil {
			// The ONLY classified error on this path. See the tripwire on
			// AppendMessages before widening this to another store call.
			insertErr = classifyStoreError(err)
			// The one diagnostic line for this event, emitted HERE because this is
			// the only place that holds the seq and kind alongside the SQLSTATE. The
			// code only, never pgErr.Message: for 22P02 and 22021 that text quotes a
			// fragment of the offending value (measured: `invalid byte sequence for
			// encoding "UTF8": 0xff`), which is worker-controlled bytes in a log line.
			var pgErr *pgconn.PgError
			code := ""
			if errors.As(err, &pgErr) {
				code = pgErr.Code
			}
			if errors.Is(insertErr, ErrUnstorableMessage) {
				slog.Warn("workersvc: message permanently unstorable",
					"run_id", runID.String(), "seq", m.Seq, "kind", m.Kind, "sqlstate", code)
			}
			break
		}
		if m.Seq > maxStored {
			maxStored = m.Seq
		}
		// rows == 0 means a duplicate (run_id, seq) — a worker re-delivery. Only
		// broadcast genuinely new messages so a retry never double-emits over WS.
		if rows > 0 {
			inserted = append(inserted, m)
		}
	}
	// Bump the run's lineage epoch once per NEWLY-INSERTED resume_lineage_break
	// status event (PRD #632, dropped-resume signal #334). Scan `inserted`, NOT
	// `msgs`: a re-delivered break is seq-deduped (rows == 0 ⇒ absent from
	// `inserted`), so a retry never double-bumps and the bump stays idempotent
	// under at-least-once delivery. A malformed payload just isn't a break (skip).
	//
	// This runs HERE — right after the insert loop, before the high-water-mark
	// update AND the insertErr guard — on purpose. The message inserts are not
	// transactional (each commits on its own), so once a break's row is committed it
	// is owed its bump: any later `return` before this loop (an unstorable message
	// co-batched after the break, or an UpdateRunLastSeq failure) would lose the bump
	// permanently, because on the worker's retry the committed break is seq-deduped
	// (rows == 0 ⇒ absent from `inserted`) and never re-bumped. Bumping at the
	// earliest point where `inserted` is complete shrinks that loss window to the
	// irreducible one — the bump statement itself failing — which no reordering can
	// close without a shared transaction (advisory-telemetry impact only). The
	// `inserted` gate already makes the bump exactly-once regardless of position, so
	// moving it up cannot double-bump.
	//
	// One consequence of sitting ahead of the high-water-mark update: a bump failure
	// now returns before UpdateRunLastSeq, so last_seq is not advanced on that path.
	// This is self-healing — the worker retries, maxStored is recomputed (its
	// assignment is outside the rows>0 gate), the seq-deduped break is skipped, and
	// UpdateRunLastSeq advances last_seq on the retry — so the only durable casualty
	// is the same irreducible lost bump noted above, not a stuck watermark.
	// The seqs of the breaks NEWLY inserted in this batch (those actually bumped),
	// handed to foldRunUsage so it stamps per-frame epochs off the SAME set the bump
	// used — never off `msgs`, which also carries seq-deduped re-deliveries whose bump
	// already landed in a prior batch (and is therefore already in run.LineageEpoch).
	var insertedBreakSeqs []int32
	for _, m := range inserted {
		if m.Kind != "status" {
			continue
		}
		var ev statusEventPayload
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			continue // malformed payload → not a break
		}
		if ev.Event != "resume_lineage_break" {
			continue
		}
		// Return the error RAW (500), never through classifyStoreError: this call
		// writes only the run_id and a server-computed +1 — no worker-controlled
		// value reaches the store — so a failure is a genuine server error the
		// worker should retry, never a "batch poisoned" 400. See the 🔴 audit block.
		if err := s.q.BumpRunLineageEpoch(ctx, runID); err != nil {
			return obs, err
		}
		insertedBreakSeqs = append(insertedBreakSeqs, m.Seq)
	}
	// The high-water mark AS OBSERVED, whether or not the insert loop broke and
	// whether or not the UpdateRunLastSeq below runs. This is what the streak's
	// no-progress reset compares (PRD #108 M4).
	//
	// The partial apply the comment above the validation loop flags, worked through:
	// on a batch whose Nth message the database refuses, rows 1..N-1 commit and this
	// advances the mark ONCE. It is frozen from the next attempt onward, because the
	// loop breaks at the same message every time so maxStored can never exceed the
	// value it already set. Whether that costs a streak reset depends on whether an
	// entry already existed — this line runs BEFORE the recorder sees the failure, so
	// a run whose first-ever failure is the partial apply is recorded at the advanced
	// mark and never resets at all. Either way it can only DELAY a kill by one
	// failure; it can never cause a false one.
	//
	// Where it genuinely helps: a 0.10.1+ worker bisecting the poison out re-groups
	// the batch, so messages after it DO land, this advances repeatedly, and the
	// streak keeps resetting — the server correctly declines to kill a run whose
	// client is already handling it.
	if maxStored > obs.lastSeq {
		obs.lastSeq = maxStored
	}
	// Advance the high-water mark to what landed, BEFORE propagating any insert
	// error. Leaving it stale is not cosmetic: on resume the worker restarts from
	// runs.last_seq and re-emits those seq numbers carrying DIFFERENT content, the
	// idempotent insert answers rows == 0, the server reads that as a re-delivery,
	// and the new content is silently dropped and never broadcast. That was
	// unreachable before PRD #108 (a failing insert was the anomaly) and is routine
	// after it, which is why it is fixed here rather than left to the transaction
	// Phase 2 will consider.
	if maxStored > run.LastSeq {
		if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: maxStored}); err != nil {
			if insertErr != nil {
				return obs, insertErr // the insert failure is the more informative of the two
			}
			return obs, err
		}
	}
	if insertErr != nil {
		return obs, insertErr
	}
	// Fold every DELIVERED result frame's usage into run_usage (PRD #40 Decision 2)
	// — over `msgs`, NOT `inserted`: a seq-deduped re-delivery (crash retry) must
	// still re-run the fold, which is exactly what makes at-least-once delivery plus
	// the idempotent GREATEST merge converge to correct totals with no crash window.
	// Malformed/absent usage is skipped inside; a DB error propagates so the worker
	// re-delivers and the fold retries. No terminal-status guard — a result frame
	// that lands after a mid-flight cancel still folds (pre-cancel spend is real
	// spend, Decision 4).
	if err := s.foldRunUsage(ctx, run, msgs, insertedBreakSeqs); err != nil {
		return obs, err
	}
	// Fan out after the log + high-water mark are durably advanced, so a browser
	// that reacts by replaying from last_seq sees a consistent state.
	if s.bcast != nil {
		now := s.now()
		for _, m := range inserted {
			s.bcast.PublishMessage(runID, m.Seq, m.Kind, m.Agent, m.AgentInstance, m.AgentLabel, []byte(m.Payload), now)
		}
	}
	return obs, nil
}

// resultUsagePayload is the subset of a terminal result frame's payload the fold
// reads. mapResult (agent/src/sdk-messages.ts) emits result frames as kind
// `status` (success) or `error`, both with `event: "result"` and a `modelUsage`
// per-model breakdown (Decision 1). Only these keys are parsed; everything else
// in the payload is ignored, and the fields are camelCase to match the SDK's
// `ModelUsage` shape as forwarded on the wire.
type resultUsagePayload struct {
	Event      string                      `json:"event"`
	ModelUsage map[string]resultModelUsage `json:"modelUsage"`
}

type resultModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

// statusEventPayload is the minimal shape appendMessages reads off a `status`
// message to detect a resume_lineage_break (dropped-resume signal #334, PRD #632).
// Only `event` is inspected; a malformed payload simply isn't a break.
type statusEventPayload struct {
	Event string `json:"event"`
}

// run_usage's PK is (run_id, session_id, model). run_id is a uuid (16 bytes in
// the index); session_id and model are BOTH unbounded worker-controlled text
// (00062_run_usage.sql). A btree index entry is capped at 2704 bytes on an 8 KiB
// page (BTMaxItemSize); exceeding it raises SQLSTATE 54000, which — unlike the
// 22xxx codes InsertRunMessage can raise (sanitize.go) — is NOT in
// unstorableSQLSTATEs, so it would fall through to a 500 and the worker's batcher
// would retry the batch forever (PRD #108 A1). So both columns are length-capped
// before the upsert, exactly as `agent` is (c88cfea8), NOT merely NUL-stripped.
//
// The arithmetic, worst case: each rune is at most 4 UTF-8 bytes, so 200+200 runes
// is at most 1600 bytes of key data; add the uuid run_id (16), the index tuple
// header (~8) and two varlena headers with alignment (~12) and the entry is ~1636
// bytes — over 1000 bytes clear of the 2704 limit. A real model id (`claude-opus-
// 4-8` and the like, ~35 bytes) and a UUID session id (36) sit far under either
// cap, so only a garbled or hostile worker is ever truncated.
//
// Truncate, never reject: session_id and model are a grouping key, not content,
// and truncateRunes is deterministic, so the same input always folds to the same
// capped key. Two distinct over-long values could then collide onto one key, which
// the GREATEST merge in UpsertRunUsage absorbs — the right trade for hostile input.
const (
	maxUsageSessionRunes = 200
	maxUsageModelRunes   = 200
)

// foldRunUsage upserts run_usage for every delivered result frame in the batch
// (PRD #40 Decision 2). It is called with ALL delivered messages, not just the
// newly-inserted ones, so a seq-deduped re-delivery re-runs the fold — the
// GREATEST merge in UpsertRunUsage makes that idempotent. `insertedBreakSeqs` is
// the seqs of the resume_lineage_break events NEWLY inserted in this batch (the
// set the caller's bump loop incremented); the per-frame epoch is counted off it,
// never off `msgs` (see the per-frame block for why). session_id is sourced
// from the run row (the frame payload carries none); it is ” until the run has
// reported one, which the monotonic merge + latest/MAX-per-model rollup tolerate.
// It also stamps a PER-FRAME lineage epoch (PRD #632): the run's committed epoch at
// batch start — run.LineageEpoch, from runOwnedByWorker's fresh per-call fetch, left
// UNMUTATED here — plus the number of resume_lineage_break events preceding that
// frame in seq order within this batch (see the per-frame block below). The epoch is
// pinned to first insert in UpsertRunUsage, never overwritten by a re-fold.
// Malformed/absent usage is skipped (never fails the append); a DB error
// propagates so the append fails and the worker re-delivers.
func (s *Service) foldRunUsage(ctx context.Context, run store.Run, msgs []IncomingMessage, insertedBreakSeqs []int32) error {
	// Fold work runs — issue AND ci_fix both spend the user's tokens working a card
	// or a pipeline end to end — and exclude ONLY chat. Chat-run spend is explicitly
	// OUT of scope for PRD #40 ("Counting tokens spent outside runs, e.g. the PRD #39
	// chat agent"), yet mapResult is shared with the chat executor so a chat run's
	// result frames now carry usage too; skip the whole fold for kind='chat' rather
	// than let chat consumption leak into run_usage. This is an exclude-list (skip
	// chat), NOT an allowlist of {issue, ci_fix}, so a future WORK-run kind folds by
	// default — matching the success criterion "every run started after this shows
	// tokens" (a new non-work kind would need adding here, the same as chat).
	if run.Kind == RunKindChat {
		return nil
	}
	var sessionID string
	if run.SessionID.Valid {
		sessionID = run.SessionID.String
	}
	// Cap the composite-PK text columns before any upsert (see maxUsageSessionRunes
	// for the 54000/2704-byte reasoning). session_id is capped once here; model is
	// capped per-frame inside the loop, since each frame carries its own.
	sessionID = truncateRunes(sessionID, maxUsageSessionRunes)
	// Per-frame lineage epoch (PRD #632). A result frame belongs to the epoch in
	// force WHEN IT WAS EMITTED — the run's committed epoch at batch start
	// (run.LineageEpoch, fetched fresh in appendMessages and NOT mutated) plus the
	// number of resume_lineage_break events preceding it in seq order within this
	// batch. The case this defends: the old leg's final result frame is co-batched
	// with the break signal (result precedes break in seq order), while the fresh
	// leg's result frames arrive in a LATER batch under a new session_id (the run is
	// re-fetched with the bumped epoch by then). Applying one batch-final epoch to
	// every frame would stamp that pre-break result with the post-break epoch; since
	// UpsertRunUsage pins lineage_epoch on first insert (omitted from DO UPDATE SET),
	// a later re-fold could not repair it, and the totals view would MAX-collapse the
	// old leg into the new one's epoch group. (Two frames in ONE batch always share
	// run.SessionID and so collapse by GREATEST regardless of epoch — session_id is
	// the row-splitter, per ADR-632; the epoch only matters once the legs land under
	// distinct session_ids, which happens across batches.) In the normal case there
	// are no breaks in a frame-carrying batch, so every frame gets baseEpoch unchanged.
	//
	// Count breaks off `insertedBreakSeqs` — the breaks NEWLY inserted in THIS batch,
	// the exact set the bump loop incremented — NOT off `msgs`. `msgs` also carries
	// seq-deduped re-deliveries under at-least-once delivery, and a re-delivered
	// break's bump already landed in a prior batch and is therefore ALREADY in
	// baseEpoch. Recounting it here would add it twice: harmless for a re-folded frame
	// (the upsert pins the epoch, so the recomputed value is discarded), but WRONG for
	// a genuinely NEW result frame co-batched with that re-delivered break (partial
	// prior persistence) — that frame is a first insert, so the double-counted phantom
	// epoch would be pinned and split one lineage leg across two epoch groups.
	baseEpoch := run.LineageEpoch
	for _, m := range msgs {
		// Result frames are only ever kind status (success) or error; skip the
		// rest without paying an unmarshal for every text/tool_use message.
		if m.Kind != "status" && m.Kind != "error" {
			continue
		}
		var p resultUsagePayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			continue // malformed payload → skip, never fail the append
		}
		if p.Event != "result" || len(p.ModelUsage) == 0 {
			continue // not a result frame, or no per-model usage to fold
		}
		// Epoch for THIS frame = base + newly-inserted breaks that precede it in seq order.
		frameEpoch := baseEpoch
		for _, bs := range insertedBreakSeqs {
			if bs < m.Seq {
				frameEpoch++
			}
		}
		for model, mu := range p.ModelUsage {
			if model == "" {
				continue
			}
			model = truncateRunes(model, maxUsageModelRunes)
			if err := s.q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
				RunID:               run.ID,
				SessionID:           sessionID,
				Model:               model,
				LineageEpoch:        frameEpoch,
				InputTokens:         nonNegTokens(mu.InputTokens),
				CacheReadTokens:     nonNegTokens(mu.CacheReadInputTokens),
				CacheCreationTokens: nonNegTokens(mu.CacheCreationInputTokens),
				OutputTokens:        nonNegTokens(mu.OutputTokens),
				CostUsd:             numericUSD(mu.CostUSD),
			}); err != nil {
				return fmt.Errorf("fold run usage (run %s, model %s): %w", run.ID, model, err)
			}
		}
	}
	return nil
}

// maxCostUSD is the numeric(12,6) ceiling ($999,999.999999). A single frame's
// cost is far below this, but the fold MUST clamp to it: a bogus costUSD >= 1e6
// would quantize past the column and raise Postgres 22003, failing the append —
// and the worker's batcher retries a failed batch at head forever (poison loop).
const maxCostUSD = 999999.999999

// numericUSD builds a numeric(12,6) cost from the SDK's float dollar amount by
// quantizing to microdollars (Int = round(usd*1e6), Exp = -6) — deterministic and
// free of the float-string-parse ambiguity of Scan. Out-of-range costs (never
// expected) are clamped into the column's domain rather than poisoning the fold:
// NaN/negative/-Inf → 0, and anything above the ceiling (incl. +Inf) → the ceiling.
func numericUSD(usd float64) pgtype.Numeric {
	switch {
	case math.IsNaN(usd) || usd < 0:
		usd = 0
	case usd > maxCostUSD: // also catches +Inf
		usd = maxCostUSD
	}
	return pgtype.Numeric{Int: big.NewInt(int64(math.Round(usd * 1e6))), Exp: -6, Valid: true}
}

// nonNegTokens clamps a token count to >= 0 at fold time. GREATEST only protects an
// existing row; a fresh (run_id, session_id, model) key inserts whatever arrives, so
// a negative count (buggy/hostile worker) would otherwise land verbatim.
func nonNegTokens(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// StateRequest is the worker's report of a run's new state. Only the fields
// relevant to State are read. The wire key is `status` (matches the runs.status
// column and the M2 worker client); the Go field stays
// `State` to avoid churn in the switch below.
type StateRequest struct {
	State  string  `json:"status"` // running|awaiting_approval|awaiting_input|completed|failed
	PlanMd *string `json:"plan_md"`
	Branch *string `json:"branch"`
	MrIID  *int64  `json:"mr_iid"`
	// MrWebURL is the MR/PR web URL as the forge reported it (PRD #65 D8), reported
	// on completion alongside MrIID. Additive + optional (R8): an OLD worker omits it
	// and textParam(nil) lands NULL, which the web renders via the legacy forgeUrls.ts
	// reconstruction exactly as before the column existed.
	MrWebURL       *string `json:"mr_web_url"`
	FailureReason  *string `json:"failure_reason"`
	IterationCount int32   `json:"iteration_count"`
	// SessionID pins the worker's SDK session for resume. Empty means "no change"
	// (the run keeps whatever session_id it already had); when set it is persisted
	// atomically with this state transition.
	SessionID *string `json:"session_id"`
	// FixVerdict travels the outbound 'not_code' verdict for a ci_fix run (PRD #6):
	// the agent judged the failure not a code problem, so the run completes with the
	// diagnosis and no MR. Only 'not_code' is expected here — verified/fix_failed are
	// stamped later by the pipeline sync; a completed report on an issue run omits it.
	FixVerdict *string `json:"fix_verdict"`
	// PrdDonePath is the repo-relative path a run's lead declared it moved its PRD
	// to (PRD #72 M4), reported on the `completed` transition. It is a DECLARATION
	// by an untrusted worker, not something derived server-side, and it drives a
	// later forge write against the issue description, so it is gated on the run's
	// kind and validated before it is stored — see clampWirePRDDonePath.
	PrdDonePath *string `json:"prd_done_path"`
	// ReportOnly and ReportMd carry issue #279's report-only/evidence completion. A run
	// whose deliverable is a report/command-output/verification result with NO code change
	// completes with report_only=true and its findings in report_md, and the worker opens
	// NO merge request. Both are DECLARATIONS by an untrusted worker on the terminal
	// `completed` report: kind-gated (issue runs only) and, for report_md, control-char
	// stripped + secret-scrubbed + length-bounded server-side before storage — see
	// clampWireReportMd / clampWireReportOnly. Absent on a normal MR completion.
	ReportOnly *bool   `json:"report_only"`
	ReportMd   *string `json:"report_md"`
	// ScopeCapped (PRD #634 M3) is the worker's DECLARATION on the terminal `completed`
	// report that an operator scope directive truncated the run at the loop top — the run
	// finalized the already-committed slice and started no further milestone. The server
	// stamps stop_kind='scope_capped', but only when the run actually carries a
	// scope_ceiling (owned.ScopeCeiling.Valid), so an untrusted worker cannot mint the
	// disposition on a run the operator never narrowed. Additive + optional and OMITTED
	// (never false) on a normal completion, so an old worker's payload and a new worker's
	// normal completion stay byte-identical on the wire. httpx.DecodeJSON rejects unknown
	// fields, so this field MUST exist here or a new worker's report 400s.
	ScopeCapped *bool `json:"scope_capped"`
	// RepoAgents is the roster the worker parsed from the clone's .claude/agents/
	// (PRD #37), reported on the first `running` report after checkout. A POINTER to
	// a slice, because the three states differ: absent (nil) = this report says
	// nothing about the roster; `[]` = detection ran and found none; non-empty = the
	// detected agents. Only `running` carries it; it is re-validated below, never
	// trusted from the worker.
	RepoAgents *[]RepoAgent `json:"repo_agents"`
	// PlanChangedFiles is the `git status --porcelain` line list the worker computed
	// (as the RUNNER uid) at the plan gate. Tri-state pointer with repo_agents
	// semantics: absent (nil) = a pre-#212 worker says nothing (COALESCE preserves);
	// a non-nil array (possibly empty) = this round's actual tree, which REPLACES the
	// column. Never collapse an empty non-nil slice to nil — an empty-clears a prior
	// round's list, which is the point (a revert-between-rounds must not show stale).
	PlanChangedFiles *[]string `json:"plan_changed_files"`
	// Milestones is the run's milestone list (PRD #122 M1), a POINTER to a slice for
	// the same tri-state as RepoAgents: absent (nil) = this report says nothing about
	// milestones; `[]` = an explicitly empty list; non-empty = the entries. The worker
	// sends the CANDIDATE list on the `awaiting_approval` report and the FROZEN list on
	// the autopilot `running` report; it is absent on every other report. Re-validated
	// and kind-gated here (Decision 12/13), never trusted from the worker — a rejected
	// list is DROPPED, not persisted, and never fails the report.
	Milestones *[]Milestone `json:"milestones"`
	// MilestonesCompleted and MilestonesInProgress are the run's live PROGRESS report
	// (PRD #122 M2), each a POINTER to a slice of frozen milestone IDS for the same
	// tri-state as RepoAgents/Milestones: absent (nil) = this call reports nothing about
	// that set; `[]` = an explicitly empty set; non-empty = the ids. MilestonesInProgress
	// rides `running` reports only. MilestonesCompleted rides `running` reports AND — since
	// PRD #265 M1 — the terminal `completed` report, where the lead's signal_done
	// declaration of what it finished is unioned in (the completion path reconciles a run
	// that never emitted a mid-run report). completed is UNIONED server-side (monotone,
	// dedup); in_progress is OVERWRITTEN wholesale (Decision 3) and cleared on every
	// terminal transition. Every id is membership-checked against the run's FROZEN list and
	// kind-gated here (progressParams, Decision 12/13); a rejected set is DROPPED, never
	// persisted, and never fails the report.
	MilestonesCompleted  *[]string `json:"milestones_completed"`
	MilestonesInProgress *[]string `json:"milestones_in_progress"`
	// SeededFromDefault (PRD #628 M4) is the worker's TREE signal that a cross-worker
	// re-claim reseeded from the DEFAULT branch (seededFrom === "default" / priorCommits
	// === 0) — no committed work was recovered, so pass-1's milestones_completed is stale
	// and must be CLEARED (ClearRunMilestonesCompleted) rather than unioned onto. Carried
	// ONLY on the dedicated one-shot run-start `running` report, never on iteration
	// heartbeats (a clear on every heartbeat would wipe live progress). Additive + optional:
	// the worker EMITS it only when true, so an omitted/absent field (nil, or false) is the
	// default and no clear runs — a pre-#628 worker and a resume that recovered its tree
	// (seededFrom "tracking"/"checkpoint") both omit it. Keyed on the TREE signal, NOT the
	// session signal resume_lineage_break, which diverges from it once M2 lands.
	SeededFromDefault *bool `json:"seeded_from_default"`
	// AgentSelection is the default an AUTOPILOT run resolved for itself (Decision 6).
	// Such a run self-approves the gate and never receives a SubmitInput, so the state
	// report is its only channel for recording which roster it used. A human-gated run
	// omits this and persists its selection through the approve_plan input instead.
	AgentSelection *AgentSelection `json:"agent_selection"`
	// OpenQuestionID is the identity of the question the run is parking on (PRD #88
	// M1), carried only by the `awaiting_input` transition. The worker mints it at the
	// first park and re-sends the SAME value when a resumed worker re-parks on the same
	// question — that stability is the whole mechanism behind SetRunRunning's
	// answer guard, which asks "was THIS question answered" rather than "has this run
	// ever been answered". Required for `awaiting_input`; ignored on every other state.
	OpenQuestionID *string `json:"open_question_id"`
	// OpenFollowupID is the park-scoped follow_up watermark reported by the worker on
	// the `awaiting_followup` transition ONLY (issue #559 M1): the highest follow_up id
	// the worker has already APPLIED to a turn at the moment it parks. The server CLAMPS
	// it to the run's max already-consumed follow_up (SetRunAwaitingFollowup's LEAST) —
	// a correct worker's last-delivered id is always ≤ max-consumed, so the clamp only
	// neutralizes a buggy huge value; and absent (an old worker, or the first park before
	// anything was delivered) → the server falls back to that server-derived max-consumed,
	// byte-identical to the pre-#559 behavior. Deriving the watermark purely server-side
	// races a follow_up consumed during this report's DB round-trip and would strand the
	// run, which is why the worker provides it. Additive and OPTIONAL, but the field MUST
	// exist because httpx.DecodeJSON sets DisallowUnknownFields — a new worker that sends
	// it would 400 otherwise. Ignored on every state other than awaiting_followup.
	OpenFollowupID *int64 `json:"open_followup_id"`
	// LimitResetsAt is the epoch at which the exhausted Anthropic usage window
	// reopens, as the worker read it off the SDK frame (PRD #35). The worker
	// normalizes the SDK's unit-less number to MILLISECONDS before sending; the
	// server re-validates rather than trusting that. UNTRUSTED input: it is stored
	// for display and cross-checked against this user's own anthropic_rate_limits
	// gauge, but it is never the promotion gate — retry_not_before is computed
	// server-side and clamped, so a worker cannot park a run for years.
	LimitResetsAt *int64 `json:"limit_resets_at"`
	// RateLimitType is the SDK's rateLimitType for the window that rejected the run.
	// UNTRUSTED free text on arrival: the server allowlists it against the SDK union
	// and coerces anything unrecognised to "unknown" before it reaches the DB, any
	// DTO, the feed or Slack. That enum allowlist is what closes the injection,
	// length and control-char concerns in one move — unlike the register path, the
	// state path has no sanitizeSelfReported.
	RateLimitType *string `json:"rate_limit_type"`
	// FailOrigin is the worker's structured guess at WHY a `failed` report is failing
	// (PRD #69 M7a): the reason CONSTANT the report site mapped to an origin, e.g.
	// "provisioning_failed" or "rate_limited". UNTRUSTED free text on arrival — the
	// server allowlists it through CoerceFailOrigin (unknown → nil) before it reaches
	// the DB, exactly as it does RateLimitType. Ordinary agent failures omit it and the
	// server defaults them to 'agent_failure'. Ignored on every non-`failed` state.
	FailOrigin *string `json:"fail_origin"`
	// PreservedPatch is the agent's branch diff (PRD #377 M1), carried ONLY on a
	// `failed` report whose fail_origin is workflow_scope_missing: the run touched
	// .github/workflows/** which the bot's repo-only PAT cannot push, so instead of
	// discarding the work the worker preserves the diff for a human to land. UNTRUSTED
	// worker text — the agent may have seen repo secrets during the run — so it is
	// control-char-stripped and length-capped server-side (clampWirePreservedPatch)
	// before storage. Absent on every other report; a dedicated column keeps report_md's
	// report-only semantics untouched (PRD D3).
	PreservedPatch *string `json:"preserved_patch"`
	// RequiredCapabilities, RequiredTools and SizeClass are the plan-time INFERRED
	// requirement set the lead emits on the `awaiting_approval` report (PRD #84 M4 4a),
	// each a tri-state pointer for the same absent="say nothing" semantics as Milestones:
	// absent (nil) = this report says nothing; a non-nil value = the inferred set. The
	// worker sends each array only when non-empty. UNTRUSTED on arrival — every name is
	// Filter-ed / FilterTools-ed against the server-owned vocabulary before storage
	// (capability package), and SizeClass is clamped to {"s","m","l"}, so a garbled
	// report cannot smuggle arbitrary strings into the requirement panel. Ignored on
	// every non-`awaiting_approval` report.
	//
	// required_capabilities is UNION-MERGED onto the M2 repo hint (escalation-only:
	// inference can add, never drop); required_tools is SET. size_class is SET the same
	// way (PRD #84 M4 4b): the clamped value REPLACES the run's size_class column, and a
	// nil / off-vocabulary value COALESCEs to a no-op. A later unit surfaces it on the DTO.
	RequiredCapabilities *[]string `json:"required_capabilities"`
	RequiredTools        *[]string `json:"required_tools"`
	SizeClass            *string   `json:"size_class"`
}

// SetState applies a worker's state transition and returns the run's resulting
// row plus whether the transition was applied. All transitions are guarded
// against terminal statuses, so a report that lands on an already-terminal run
// (e.g. a cancel raced in) is a no-op: applied is false and the run's real
// status is returned. The handler maps applied==false to 409 (the worker treats
// "already terminal" as success and learns it was cancelled), per the M2 wire
// contract.
func (s *Service) SetState(ctx context.Context, wkr store.Worker, runID uuid.UUID, req StateRequest) (run store.Run, applied bool, err error) {
	owned, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	sessionID := stripNULParam(req.SessionID)
	var rows int64
	// PRD #634 M4: the disposition to settle the pending scope audit row(s) with, decided in
	// the `completed` case and applied best-effort in the applied-transition block below.
	// Empty means "no settle" (every non-completed transition leaves it empty).
	var settleScopeDisposition string
	switch req.State {
	case "running":
		// PRD #37: the roster and (for autopilot) the resolved selection ride the
		// `running` report. Both are re-validated here — the worker capped them, but
		// the API does not take a worker's word for the shape of what it persists and
		// then renders in an approval panel.
		var runningParams store.SetRunRunningParams
		runningParams, err = s.runningStateParams(ctx, owned, req)
		if err != nil {
			return store.Run{}, false, err
		}
		runningParams.IterationCount = req.IterationCount
		runningParams.SessionID = sessionID
		runningParams.ID = runID
		runningParams.WorkerID = pgUUID(wkr.ID)
		// PRD #122 M2 (Decision 5/5b): the budget-scaling config the SQL freeze reads.
		// Harmless on every heartbeat (the query COALESCEs the derived budget against the
		// existing immutable columns, so only the FIRST report that carries a frozen list
		// writes it — a later report re-supplies the same config and changes nothing).
		runningParams.RunMaxIterations = int32(s.p.RunMaxIterations)
		runningParams.RunTimeoutSeconds = int32(s.p.RunTimeout.Seconds())
		runningParams.MilestoneBudgetCap = milestoneBudgetCap
		runningParams.BudgetWallCeilingSeconds = budgetWallCeilingSeconds
		// PRD #84 M4: an AUTOPILOT run auto-approves its own plan and NEVER reports
		// awaiting_approval, so it rides the plan-time INFERRED requirement set on this
		// self-contained `running` report instead (runner.ts toolchainReportFields, the
		// same fields the awaiting_approval case consumes). Persist it the SAME way — the
		// shared inferredRequirementParams sanitiser feeding the query's absent-safe
		// COALESCE guards (required_capabilities union-merged escalation-only,
		// required_tools/size_class replaced) — so autopilot runs are not silently stripped
		// of their inference (the SWEEP auto-approves, so this is the only path that carries
		// it for them). Absent on every ordinary session-id/iteration heartbeat, so those
		// stay no-ops.
		runningParams.InferredCapabilities, runningParams.InferredTools, runningParams.SizeClass = inferredRequirementParams(req)
		// PRD #628 M4: a cross-worker re-claim that reseeded from the DEFAULT branch recovered
		// no committed work, so pass-1's milestones_completed is stale — it must be cleared,
		// NOT unioned onto. milestones_completed is a monotone union (SetRunRunning below,
		// SetRunCompleted), so a worker cannot walk it back by reporting []; this targeted
		// clear is the only writer that can. It keys on the TREE signal seeded_from_default
		// (worker-computed seededFrom === "default"), never the session signal
		// resume_lineage_break — the two diverge once M2 lands (a re-claim can recover the tree
		// via checkpoint while the session still breaks). Additive-optional: an old/omitting
		// worker sends nil ⇒ no clear. Run BEFORE SetRunRunning so its union refills from empty;
		// the run-start report carries no milestones, so that same-call union is a no-op. The
		// SQL is ownership+status guarded (id + worker_id, non-terminal), so a superseded worker
		// cannot wipe the live owner's progress; a 0-row refusal is benign and not surfaced.
		if req.SeededFromDefault != nil && *req.SeededFromDefault {
			if _, cerr := s.q.ClearRunMilestonesCompleted(ctx, store.ClearRunMilestonesCompletedParams{
				ID:       runID,
				WorkerID: pgUUID(wkr.ID),
			}); cerr != nil {
				return store.Run{}, false, cerr
			}
		}
		rows, err = s.q.SetRunRunning(ctx, runningParams)
	case "awaiting_approval":
		// PRD #122 M1: the CANDIDATE milestone list rides the pre-approval report.
		// milestonesParam validates + kind-gates it (Decision 12/13) and returns NULL
		// when the list is absent, rejected, or from a non-issue run — the query writes
		// that directly, clearing the candidate (Decision 2: replaced each round).
		//
		// PRD #84 M4 4b: the plan-time INFERRED requirement set also rides this report.
		// Each array is a tri-state pointer — absent (nil) means "no change", and the
		// query's COALESCE makes a nil param a no-op (union with '{}' / keep-existing).
		// A present set is Filter-ed / FilterTools-ed against the server-owned vocabulary
		// so the worker cannot smuggle an unknown name into storage. size_class rides the
		// same report (PRD #84 M4 4b): it is clamped to the {s,m,l} vocabulary and passed
		// as an absent-safe pgtype.Text, so an off-vocabulary or absent value is an invalid
		// (SQL NULL) param the query's COALESCE keeps out of the column.
		inferredCaps, inferredTools, sizeClass := inferredRequirementParams(req)
		rows, err = s.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: stripNULParam(req.PlanMd), SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
			MilestonesCandidate:  milestonesParam(owned.Kind, req.Milestones),
			InferredCapabilities: inferredCaps,
			InferredTools:        inferredTools,
			SizeClass:            sizeClass,
			PlanChangedFiles:     planChangedFilesParam(req.PlanChangedFiles),
		})
	case "awaiting_input":
		// PRD #88 M1: park on a clarification question. The question id is REQUIRED
		// and rejected when absent rather than defaulted, because a NULL
		// open_question_id makes SetRunRunning's answer guard unsatisfiable — the run
		// would park and then be unable to resume no matter what the user answered.
		// Failing the report is loud; a silently unresumable run is not.
		var qid string
		if req.OpenQuestionID != nil {
			clean, _ := stripNUL(*req.OpenQuestionID)
			qid = strings.TrimSpace(clean)
		}
		if qid == "" {
			return store.Run{}, false, fmt.Errorf("%w: awaiting_input requires open_question_id", ErrInvalidState)
		}
		if len([]rune(qid)) > maxQuestionIDRunes {
			return store.Run{}, false, fmt.Errorf("%w: open_question_id is too long", ErrInvalidState)
		}
		rows, err = s.q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
			OpenQuestionID: pgText(qid), SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "awaiting_followup":
		// PRD #517 M2/M3 (Decision 3): the interactive-task park. On signal_done an
		// interactive task run parks IN-PROCESS here instead of finalizing to `completed`,
		// so the SAME worker holds its slot, clone and SDK session alive for `uzi run
		// follow-up` to resume with full context. No question requirement (unlike
		// awaiting_input): the park is not gated on a clarification, and the resume rides a
		// `follow_up` steering input that SetRunRunning's Decision-7 wake guard keys on.
		//
		// ONLY an interactive task run may park this way. The status is meaningless for any
		// other kind, and accepting it for one would strand a non-resumable run in a
		// non-terminal status the follow-up path never wakes — so a mismatched report is
		// genuinely invalid input (a stale, buggy, or hostile worker) and is rejected loudly
		// with ErrInvalidState, consistent with awaiting_input's missing-question rejection,
		// rather than silently persisted. A legitimate park always satisfies both guards
		// (the interactive opt-in is immutable from create, PRD #517 M1), so this never
		// rejects a real one.
		if owned.Kind != RunKindTask || !owned.Interactive {
			return store.Run{}, false, fmt.Errorf("%w: awaiting_followup requires an interactive task run", ErrInvalidState)
		}
		rows, err = s.q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
			// int8Param maps nil → pgtype.Int8{} (Valid:false → SQL NULL), so an old
			// worker that omits open_followup_id lands NULL and the query's COALESCE
			// fallback recomputes the server-derived max-consumed watermark. A present
			// value is clamped to ≤ max-consumed by the query's LEAST.
			OpenFollowupID: int8Param(req.OpenFollowupID),
			SessionID:      sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "completed":
		// PRD #265 M1: reconcile the milestone tracker from the lead's signal_done
		// declaration. progressParams subset-validates the declared ids against the run's
		// FROZEN list (Decision 12/13) exactly as the `running` path does — a non-issue
		// run, an empty/absent declaration, or any non-member id yields nil, which the
		// query's CASE leaves the column untouched for (additive-absent: byte-identical to
		// before). The in_progress side is not declared on completion (the SQL clears it
		// unconditionally on every terminal transition, D4), so only the completed side is
		// passed here.
		completedIDs, _ := progressParams(owned.Kind, owned.MilestonesFrozen, req.MilestonesCompleted, nil)
		// report_md is the deliverable of a report-only completion, so it is stored ONLY
		// when report_only was accepted — this keeps the column's invariant (non-NULL only
		// on a report_only run) true even against an untrusted worker that sends report_md
		// alone. clampWireReportOnly is the issue-run gate report_md then inherits.
		reportOnly := clampWireReportOnly(owned, req.ReportOnly)
		// PRD #634 M3: stamp stop_kind='scope_capped' on an operator scope-truncated
		// completion. GUARDED on the run actually carrying a scope_ceiling
		// (owned.ScopeCeiling.Valid), so an untrusted worker cannot mint the disposition on
		// a run the operator never narrowed. NULL (the zero pgtype.Text) leaves any existing
		// stop_kind untouched via the query's COALESCE-narg, so a normal completion is
		// byte-identical to before.
		var scopeStopKind pgtype.Text
		if req.ScopeCapped != nil && *req.ScopeCapped && owned.ScopeCeiling.Valid {
			scopeStopKind = pgText("scope_capped")
		}
		// PRD #634 M4: settle the still-pending scope audit row at completion. 'applied' when
		// this is a genuine scope-capped completion (scopeStopKind set); 'declined' otherwise —
		// a run that completed normally despite a scope directive (e.g. the lead under-reported
		// so the gate never fired; the directive did not change behavior). 'declined' is set
		// ONLY when the run actually carried a scope directive (owned.ScopeCeiling.Valid);
		// without one settleScopeDisposition stays "" and the post-switch settle block is
		// skipped, so a normal completion issues no 0-row UPDATE. Applied best-effort after the
		// transition commits (rows>0), so a no-op onto an already-terminal run does not re-settle.
		if scopeStopKind.Valid {
			settleScopeDisposition = "applied"
		} else if owned.ScopeCeiling.Valid {
			settleScopeDisposition = "declined"
		}
		rows, err = s.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch: stripNULParam(req.Branch), MrIid: int8Param(req.MrIID), MrWebUrl: stripNULParam(req.MrWebURL), SessionID: sessionID,
			FixVerdict:          clampWireFixVerdict(req.FixVerdict),
			PrdDonePath:         clampWirePRDDonePath(owned, req.PrdDonePath),
			ReportOnly:          reportOnly,
			ReportMd:            clampWireReportMd(owned, req.ReportMd, reportOnly),
			MilestonesCompleted: completedIDs,
			StopKind:            scopeStopKind,
			ID:                  runID, WorkerID: pgUUID(wkr.ID),
		})
	case "limit_wait":
		rows, err = s.setLimitWait(ctx, owned, wkr, req, sessionID)
	case "failed":
		// PRD #634 follow-up: a scope-directed run that terminates abnormally (failed/cancelled/
		// stopped/plan-rejected) never applied the scope cap, so settle its pending audit row
		// 'declined' — otherwise `run inputs`/the web card render it "active" for a terminal run.
		// The post-switch settle block fires this on the committed transition (rows>0). Guarded on
		// the run actually carrying a directive so it is a no-op (0-row) on ordinary failures.
		if owned.ScopeCeiling.Valid {
			settleScopeDisposition = "declined"
		}
		// PRD #503 M1 (REC A): a LIVE worker cannot report a `cancelled` terminal status —
		// there is no `cancelled` case in this switch — so a consumed cancel or plan-reject
		// verdict arrives here as `failed`. The run's stop_kind was stamped by
		// CreateStopVerdictInput BEFORE this report, so it is already on the loaded `owned`
		// row; branch on it BEFORE the agent_failure default so an operator's deliberate stop
		// is not mis-classified as an agent failure (which would also get judged, contradicting
		// "cancelled runs are not judged").
		switch {
		case owned.StopKind.Valid && owned.StopKind.String == "cancelled":
			// Route to a `cancelled` transition (status 'cancelled', fail_origin NULL),
			// converging with the server-side CancelRunServerSide path. SetRunFailed is NOT
			// called. rows drives the same applied/not-applied logic below (execrows, like
			// SetRunFailed).
			rows, err = s.q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{
				ID: runID, WorkerID: pgUUID(wkr.ID),
			})
		case owned.StopKind.Valid && owned.StopKind.String == "stopped":
			// PRD #517 M4: a graceful stop stamped stop_kind='stopped' BEFORE this report.
			// Its happy path is the worker finalizing (push + MR iff open_mr) and reporting
			// `completed`; this arm is the EDGE where the worker instead reports `failed` —
			// either the finalize (push/MR) threw, or a cancel-then-stop sequence let the
			// cancel win in steering and the worker threw REASON_CANCELLED. Either way it is a
			// deliberate wind-down, not an agent bug, so it must NOT default to
			// fail_origin='agent_failure' and must NOT be judged. Route to CancelRunByWorker
			// exactly like the 'cancelled' arm: status 'cancelled', fail_origin NULL (CHECK-safe,
			// no new vocabulary value), which Gate 0 of maybeEnqueueJudge excludes from judging.
			rows, err = s.q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{
				ID: runID, WorkerID: pgUUID(wkr.ID),
			})
		case owned.StopKind.Valid && owned.StopKind.String == "plan_rejected":
			// A live plan-reject: stamp fail_origin='plan_rejected' (overriding the untrusted
			// req.FailOrigin, which the worker cannot forge), matching the server-side
			// RejectRunServerSide path rather than defaulting to agent_failure.
			rows, err = s.q.SetRunFailed(ctx, store.SetRunFailedParams{
				FailureReason:  limitAwareFailureReason(req),
				FailOrigin:     pgText("plan_rejected"),
				PreservedPatch: clampWirePreservedPatch(req.PreservedPatch),
				SessionID:      sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
			})
		default:
			// PRD #69 M7a: stamp the TRUSTED failure class. The worker-reported origin is
			// UNTRUSTED, so CoerceFailOrigin allowlists it (unknown → nil) before it reaches
			// the DB. A classless (nil) failure is exactly the judgeable AGENT-FAILURE case —
			// an ordinary agent death reports `failed` with no fail_origin — so it defaults to
			// 'agent_failure' here rather than storing NULL (NULL is reserved for pre-feature /
			// unclassified rows). The rate-limit opt-out path sends fail_origin='rate_limited'.
			failOrigin := "agent_failure"
			if o := CoerceFailOrigin(req.FailOrigin); o != nil {
				failOrigin = *o
			}
			rows, err = s.q.SetRunFailed(ctx, store.SetRunFailedParams{
				// PRD #35 §7.8: when a `failed` report carries the structured limit fields,
				// the SERVER composes the sentence from its own allowlisted enum and replaces
				// whatever text the worker sent. That is the opt-out path (a run with
				// wait_on_limit=false reports failed directly), and letting the worker compose
				// it would put the enum on the untrusted side of the wire — the criterion "a
				// compromised worker cannot smuggle a non-enum rate_limit_type past the
				// server" would then be false on exactly the path a human reads. When the
				// fields are absent this is nil and every other failure path is untouched.
				FailureReason:  limitAwareFailureReason(req),
				FailOrigin:     pgText(failOrigin),
				PreservedPatch: clampWirePreservedPatch(req.PreservedPatch),
				SessionID:      sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
			})
		}
	default:
		return store.Run{}, false, ErrInvalidState
	}
	if err != nil {
		return store.Run{}, false, err
	}
	// issue #329: record the MR the worker opened INDEPENDENT of the terminal status
	// the switch above wrote. If SetRunCompleted applied, ReconcileRunMR is a COALESCE
	// no-op; if the status transition no-oped (e.g. the run was cancelled or already
	// terminal), this still captures the MR so the run never reports "MR: none".
	// Best-effort: a failure here must not fail the worker's terminal report.
	if req.MrIID != nil {
		if _, mrErr := s.q.ReconcileRunMR(ctx, store.ReconcileRunMRParams{
			MrIid: int8Param(req.MrIID), MrWebUrl: stripNULParam(req.MrWebURL), Branch: stripNULParam(req.Branch),
			ID: runID, WorkerID: pgUUID(wkr.ID),
		}); mrErr != nil {
			slog.Warn("reconcile run mr", "run", runID, "error", mrErr)
		}
	}
	// Re-read so the worker sees the authoritative status. Ownership already
	// held above, so 0 rows means the transition was not applied under either branch
	// of SetRunCompleted's now-two-branch guard (terminal, unless a superseded
	// run_timeout) → not applied.
	run, err = s.runOwnedByWorker(ctx, runID, wkr)
	if err == nil && rows > 0 {
		if s.bcast != nil {
			s.bcast.PublishState(runID, run.Status)
		}
		// Only a genuinely-applied transition drives the column automation; a
		// no-op onto an already-terminal run (rows == 0) must not.
		s.notify(runID, run.Status)
		// PRD #634 M4: settle the pending scope audit row's disposition on the committed
		// completion transition. Best-effort: a failure here must NEVER fail the worker's
		// terminal report. Idempotent (WHERE disposition IS NULL) and matches 0 rows on a run
		// with no scope directive, so it is harmless when unset by a non-completed transition
		// (settleScopeDisposition stays empty then and this is skipped).
		if settleScopeDisposition != "" {
			if _, setErr := s.q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
				RunID:       runID,
				Disposition: pgText(settleScopeDisposition),
			}); setErr != nil {
				slog.Warn("settle scope input disposition", "run", runID, "disposition", settleScopeDisposition, "error", setErr)
			}
		}
		// PRD #46 Decision 2: enqueue a judge on the COMMITTED terminal transition
		// (rows>0), not the lossy notify seam. Best-effort — never fails the report.
		s.maybeEnqueueJudge(ctx, run)
		// PRD #400 M4a: auto-create a diff-review run for a just-completed --review task,
		// on the SAME committed transition. Best-effort — never fails the report.
		s.maybeEnqueueTaskReview(ctx, run)
		// PRD #400 M5: auto-create a chained fix run when a --then-fix handoff's review
		// completes with findings. Its gates fire ONLY for a completed review run, and
		// maybeEnqueueTaskReview's review_target_run_id-null gate makes the two mutually
		// exclusive. Best-effort — never fails the report.
		s.maybeEnqueueThenFix(ctx, run)
		// PRD #529 M4: an ephemeral worker exists only to serve its bound run, so a
		// genuinely-applied terminal transition (rows>0) on that run — completed /
		// worker-cancel / failed alike — is its cue to tear down. Fires on the SAME
		// committed transition; guarded on wkr.Ephemeral and busy-checked in-query so a
		// normal run's completion is a no-op. Best-effort — never fails the report.
		s.maybeTeardownEphemeral(ctx, wkr, run)
	}
	return run, rows > 0, err
}

// maybeTeardownEphemeral drops the ephemeral worker bound to a just-terminal run
// (PRD #529 M4). Best-effort: a failure here must never fail the worker's terminal
// report — M5's reaper is the backstop. Guarded on wkr.Ephemeral so a normal run's
// completion does not issue a pointless DELETE.
func (s *Service) maybeTeardownEphemeral(ctx context.Context, wkr store.Worker, run store.Run) {
	if !wkr.Ephemeral || !terminalStatuses[run.Status] {
		return
	}
	if _, err := s.q.DeleteEphemeralWorkerForRun(ctx, run.ID); err != nil {
		slog.Warn("ephemeral teardown on run completion", "run", run.ID, "worker", wkr.ID, "error", err)
	}
}

// inferredRequirementParams sanitises the plan-time INFERRED requirement set a worker
// emits on a state report (PRD #84 M4 4b) into the three absent-safe params BOTH write
// paths take: SetRunAwaitingApproval on a human-gated plan park, SetRunRunning on an
// autopilot's self-contained running report (an autopilot run never reports
// awaiting_approval). Shared so the two consumers can never drift in how they filter a
// report against the server-owned vocabulary. Each return is absent-safe, matching the
// queries' COALESCE guards:
//   - inferredCaps: capability.Filter (unknown names dropped). An absent (nil) field or
//     an all-unknown report yields nil/empty, which UNION-MERGES with '{}' — a no-op —
//     so caps are never wiped; a present set escalates (adds), never replaces.
//   - inferredTools: capability.FilterTools, but only passed through when NON-empty.
//     required_tools is a COALESCE-guarded REPLACE, so a non-nil empty slice ({}) would
//     WIPE a prior tool set; leaving it nil makes the query keep the existing column.
//     The worker only emits required_tools when non-empty, so this loses no behaviour.
//   - sizeClass: clamped to the {s,m,l} vocabulary; an absent or off-vocabulary value is
//     an invalid pgtype.Text (SQL NULL) the query's COALESCE keeps out of the column.
func inferredRequirementParams(req StateRequest) (inferredCaps, inferredTools []string, sizeClass pgtype.Text) {
	if req.RequiredCapabilities != nil {
		inferredCaps = capability.Filter(*req.RequiredCapabilities)
	}
	if req.RequiredTools != nil {
		if filtered := capability.FilterTools(*req.RequiredTools); len(filtered) > 0 {
			inferredTools = filtered
		}
	}
	if req.SizeClass != nil {
		switch *req.SizeClass {
		case "s", "m", "l":
			sizeClass = pgText(*req.SizeClass)
		}
	}
	return inferredCaps, inferredTools, sizeClass
}

// planChangedFilesParam maps the worker's per-round changed-file list onto the
// nullable text[] param. repo_agents semantics (Decision 4): a nil pointer stays nil
// (COALESCE preserves), a non-nil slice — even empty — is returned as a non-nil
// []string so COALESCE REPLACES (empty clears). Each element is control-char-stripped
// and length-clamped (untrusted repo-controlled paths); the whole list is capped with
// a synthetic truncation marker. This is the OPPOSITE of inferredRequirementParams /
// clampWirePreservedPatch, which collapse empty->NULL and would show a stale list on a
// revert-between-rounds.
func planChangedFilesParam(p *[]string) []string {
	if p == nil {
		return nil
	}
	const maxLines = 200
	const maxLineBytes = 512
	src := *p
	out := make([]string, 0, len(src)) // non-nil even when src is empty -> COALESCE replaces
	for i, line := range src {
		if i >= maxLines {
			out = append(out, fmt.Sprintf("… (+%d more)", len(src)-maxLines))
			break
		}
		out = append(out, sanitizePlanChangedLine(line, maxLineBytes))
	}
	return out
}

// sanitizePlanChangedLine strips control/bidi runes from a single git-status
// --porcelain line and byte-clamps it (rune-safe, like termsafe.SanitizeBounded's
// cut), WITHOUT trimming whitespace. termsafe.SanitizeBounded is deliberately NOT
// used: it TrimSpace-es, and the LEADING space of the porcelain two-char XY status
// code is semantically load-bearing (" M path" = modified-in-worktree vs "M  path"
// = modified-in-index — Decision 7), so trimming would silently rewrite the change
// kind the human reads at the gate. termsafe.Unsafe drops \n and \t too (both are Cc
// control runes), which is correct for a single-line element — a porcelain entry is
// one line by construction (the worker split stdout on \n) — and also denies the
// embedded-newline/tab row-forgery a hostile repo-controlled filename could carry,
// at persist time rather than relying on the CLI/web renderers alone.
func sanitizePlanChangedLine(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if termsafe.Unsafe(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// runningStateParams builds the `running` update's PRD #37 columns from a worker's
// report, re-validating both payloads. Absent fields leave the columns untouched
// (the query COALESCEs them against themselves), so the ordinary session-id and
// iteration heartbeats never disturb a roster a previous report established.
//
// The fields are read ONLY from a `running` report. A completed/failed report that
// carried them would be ignored rather than rejected: a terminal report is the one
// call a worker must not be able to fail on a technicality, and no worker sends
// them there.
func (s *Service) runningStateParams(ctx context.Context, run store.Run, req StateRequest) (store.SetRunRunningParams, error) {
	var p store.SetRunRunningParams

	// PRD #122 M1: an AUTOPILOT run reports its FROZEN milestone list on this report.
	// Validated + kind-gated exactly like the candidate path (Decision 12/13); a
	// rejected/absent/non-issue list is NULL, which the query COALESCEs away, so it
	// never overwrites a frozen list and never disturbs the heartbeat. Unlike the
	// RepoAgents/AgentSelection paths below, a bad milestone list is DROPPED rather
	// than failing the report — additive-optional.
	p.MilestonesFrozen = milestonesParam(run.Kind, req.Milestones)

	// PRD #122 M2 (Decision 3/12): the live progress sets. Validated + membership-checked
	// against the run's FROZEN list and kind-gated (progressParams); a bad or non-issue
	// set is DROPPED to NULL, which the query leaves the column untouched for. completed
	// is unioned server-side, in_progress overwritten. Additive-optional, never fails the
	// report.
	p.MilestonesCompleted, p.MilestonesInProgress = progressParams(run.Kind, run.MilestonesFrozen, req.MilestonesCompleted, req.MilestonesInProgress)

	if req.RepoAgents != nil {
		if err := validateRepoAgents(*req.RepoAgents); err != nil {
			return p, err
		}
		encoded, err := encodeJSONArray(*req.RepoAgents)
		if err != nil {
			return p, fmt.Errorf("encode repo agents: %w", err)
		}
		p.RepoAgents = encoded
	}

	if req.AgentSelection == nil {
		return p, nil
	}
	// An autopilot run's self-resolved default. Validate it against the roster it
	// names — for the repo source that is the roster reported in THIS request when
	// present (the worker sends both together), else whatever a previous report
	// persisted.
	sel := *req.AgentSelection
	roster, err := s.rosterFor(ctx, run, sel.Source, req.RepoAgents)
	if err != nil {
		return p, err
	}
	if err := validateSelection(sel, roster); err != nil {
		return p, err
	}
	exclusions, err := encodeJSONArray(sel.Exclusions)
	if err != nil {
		return p, fmt.Errorf("encode agent exclusions: %w", err)
	}
	p.AgentSource = pgText(sel.Source)
	p.AgentExclusions = exclusions
	return p, nil
}

// rosterFor resolves the selectable subagent names of a source for one run:
//   - "repo": the detected roster — `reported` when this very request carries it,
//     else the run's persisted repo_agents column. A repo file named `lead` stays
//     in it (Decision 3).
//   - "own":  the templates the claim delivers to the run's owner, minus the lead
//     orchestrator (which is the main thread, not a subagent).
func (s *Service) rosterFor(ctx context.Context, run store.Run, source string, reported *[]RepoAgent) ([]string, error) {
	if source == AgentSourceRepo {
		if reported != nil {
			names := make([]string, 0, len(*reported))
			for _, a := range *reported {
				names = append(names, a.Name)
			}
			return names, nil
		}
		return repoAgentNames(run.RepoAgents), nil
	}
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgUUID(run.UserID))
	if err != nil {
		return nil, fmt.Errorf("list claim agent templates: %w", err)
	}
	names := make([]string, 0, len(templates))
	for _, t := range templates {
		names = append(names, t.Name)
	}
	return ownSubagentNames(names), nil
}

// OwnAgentRoster resolves the OWN-source subagent roster (name + description) for a
// run's owner: exactly the templates ListClaimAgentTemplates delivers to that
// owner's claim, minus the lead orchestrator (the main thread, never a selectable
// subagent). It is the SAME query rosterFor/validateSelection use for source="own",
// so populating it onto the run-detail DTO lets the plan-gate picker show precisely
// what the validator accepts and the worker runs — a chip can never name a template
// that approve_plan would reject, and the picker's "N of your templates" count is
// exact even when the owner has a disabled or shadowed template (PRD #37 M4-fix).
func (s *Service) OwnAgentRoster(ctx context.Context, userID uuid.UUID) ([]RepoAgent, error) {
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list claim agent templates: %w", err)
	}
	out := make([]RepoAgent, 0, len(templates))
	for _, t := range templates {
		if agenttmpl.IsLeadName(t.Name) {
			continue
		}
		out = append(out, RepoAgent{Name: t.Name, Description: t.Description})
	}
	return out, nil
}

// InputDTO is a consumed steering input handed to the worker. ID is the input's
// primary key (per the M2 wire contract).
type InputDTO struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Body      *string   `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ConsumeInputs returns and marks-consumed every pending steering input for a
// run the worker owns, FIFO. Delivery marks the input consumed (there is no
// separate ack), so a worker crash right after the GET drops that input — an
// accepted MVP trade-off for the steering channel (the user can re-send).
func (s *Service) ConsumeInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID) ([]InputDTO, error) {
	if _, err := s.runOwnedByWorker(ctx, runID, wkr); err != nil {
		return nil, err
	}
	rows, err := s.q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]InputDTO, 0, len(rows))
	consumedFollowUp := false
	for _, row := range rows {
		if row.Kind == "follow_up" {
			consumedFollowUp = true
		}
		out = append(out, InputDTO{ID: row.ID, Kind: row.Kind, Body: textPtr(row.Body), CreatedAt: row.CreatedAt.Time})
	}
	// Delivery ack (PRD #95 Decision 5): a follow-up is now consumed (consumed_at
	// stamped, committed above), so poke the browser to re-read its steer queue and
	// flip Queued → Delivered. Only for follow_up — approve_plan/cancel/reject own
	// their own UI and never render in the queue. Nil-guarded (mirrors AppendMessages):
	// ConsumeInputs never broadcast before, so an unset broadcaster (many tests) must
	// not panic. Best-effort; the REST refetch is the source of truth if this is dropped.
	if consumedFollowUp && s.bcast != nil {
		s.bcast.PublishInput(runID)
	}
	return out, nil
}

// SaveMemory persists one cross-run memory entry for the run's (user, repo), the
// worker's save_memory tool landing here (PRD #90). CRITICAL: the (user_id,
// repo_id) are read off the OWNED run — never from the request — so a worker whose
// join token is not user-scoped cannot write another user's memory. A repo-less run
// (chat/self-improve) has no memory scope → ErrMemoryNoRepo. Caps are enforced
// server-side: oversize title/body/evidence → ErrMemoryTooLarge; the per-run write
// count at the cap → ErrMemoryWriteCap; and after the insert the (user,repo) set is
// trimmed to the newest MemoryMaxPerUserRepo (oldest-eviction). The count-check →
// insert → evict are sequential store calls (mirroring AppendMessages) — a single
// lead is the only writer per run, so no cross-write race is in play. basis/evidence
// are the writer's declared provenance (PRD #266): basis is normalized at write to
// one of the two known trust labels or empty, evidence is trimmed/sanitized/capped,
// both stored NULL when empty — a bad or absent basis is never a write failure.
func (s *Service) SaveMemory(ctx context.Context, wkr store.Worker, runID uuid.UUID, title, body, basis, evidence string) (store.AgentMemory, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return store.AgentMemory{}, err
	}
	if !run.RepoID.Valid {
		return store.AgentMemory{}, ErrMemoryNoRepo
	}
	// Strip control characters from the untrusted title/body BEFORE the size cap so
	// the stored byte count reflects the stored value. A prompt-injected ANSI escape
	// or bare control char would otherwise render raw when the owner runs `uzi memory
	// list` (the CLI table printer writes cell values verbatim). Mirrors the
	// handler's sanitizeSelfReported: a title is single-line (no whitespace kept), a
	// body keeps \n and \t but drops every other C0/C1 control char and ANSI escape.
	title = sanitizeMemoryField(strings.TrimSpace(title), false)
	body = sanitizeMemoryField(body, true)
	if title == "" || strings.TrimSpace(body) == "" {
		return store.AgentMemory{}, ErrMemoryEmpty
	}
	// Writer-declared provenance (PRD #266). basis is a single-line trust label and
	// evidence a single-line free-text pointer — both untrusted, so both are trimmed
	// and sanitized single-line (keepWhitespace=false, like the title): an embedded
	// newline/tab in either could otherwise forge a fake marker line where the lead
	// prompt renders evidence inline, and an injected ANSI escape would render raw
	// when the owner runs `uzi memory list`.
	basis = sanitizeMemoryField(strings.TrimSpace(basis), false)
	evidence = sanitizeMemoryField(strings.TrimSpace(evidence), false)
	// Normalize basis at WRITE, not only on read: persist only the two known trust
	// labels ("observed"/"inferred") and store anything else — empty, garbage, or an
	// oversized string a direct worker POST tried to smuggle past the client — as
	// empty (→ NULL below). This closes the basis-amplification path without a
	// separate byte cap, and never fails the write (PRD #90: memory writes must not
	// fail a run). The read mapper's coercion of an unknown value to "inferred" then
	// has nothing left to defend against.
	switch basis {
	case "observed", "inferred":
	default:
		basis = ""
	}
	// Size cap on the sanitized values (evidence stores NULL when empty, the DTO
	// omits it). Evidence is capped like the title so a direct worker POST cannot
	// bypass the client's own 200-byte cap; an oversize evidence is a non-fatal 400
	// the worker treats as such, same as an oversize title/body.
	if len(title) > MemoryMaxTitleBytes || len(body) > MemoryMaxBodyBytes || len(evidence) > MemoryMaxEvidenceBytes {
		return store.AgentMemory{}, ErrMemoryTooLarge
	}
	// Per-run write cap: the spam bound within one run (Decision M4). Counted on
	// run_id, so it survives even when older entries have been evicted from the
	// (user,repo) set.
	n, err := s.q.CountAgentMemoryForRun(ctx, pgUUID(runID))
	if err != nil {
		return store.AgentMemory{}, err
	}
	if n >= MemoryMaxPerRun {
		return store.AgentMemory{}, ErrMemoryWriteCap
	}
	repoID := uuid.UUID(run.RepoID.Bytes)
	mem, err := s.q.InsertAgentMemory(ctx, store.InsertAgentMemoryParams{
		UserID:   run.UserID,
		RepoID:   repoID,
		RunID:    pgUUID(runID),
		Title:    title,
		Body:     body,
		Basis:    pgText(basis),
		Evidence: pgText(evidence),
	})
	if err != nil {
		return store.AgentMemory{}, err
	}
	// Trim to the newest MemoryMaxPerUserRepo for this (user,repo) — the count cap
	// via oldest-eviction, right after the insert so the set can never be observed
	// over cap by a subsequent read.
	if err := s.q.EvictAgentMemoryOverCap(ctx, store.EvictAgentMemoryOverCapParams{
		UserID:    run.UserID,
		RepoID:    repoID,
		KeepCount: MemoryMaxPerUserRepo,
	}); err != nil {
		return store.AgentMemory{}, err
	}
	return mem, nil
}

// sanitizeMemoryField strips control characters from an untrusted memory field
// before it is stored, so a prompt-injected ANSI escape (\x1b[…) or bare control
// char (e.g. \x07) can never render raw when the owner reads it back with `uzi
// memory list` (the CLI table printer writes cell values verbatim). It mirrors
// sanitizeSelfReported in the handler package, minus the byte truncation (SaveMemory
// applies the size cap AFTER this, on the sanitized value). keepWhitespace preserves
// \n and \t for the multi-line body while still dropping every other C0/C1 control
// char; a single-line title keeps neither. The predicate is termsafe.Unsafe, whose
// control half catches C0 (incl. ESC 0x1b and BEL 0x07), DEL, and C1 — the whole
// ANSI-escape lead-in class.
//
// As of issue #161 it ALSO strips Unicode format characters (Cf) via termsafe.Unsafe,
// as Trojan-Source defense (issue #124 / CVE-2021-42574): a bidi override (U+202E and
// its family) could otherwise visually reorder the stored note so it reads as
// something other than what the run wrote, and the zero-widths let two notes look
// identical. That carries the same accepted cost termsafe.SanitizeBounded documents:
// a ZWJ family emoji (U+200D) degrades into its component glyphs and ZWNJ (U+200C) is
// dropped — knowingly, because letting a bidi override persist at rest is the worse
// trade. The \n/\t + keepWhitespace exception runs BEFORE the Unsafe test, so the
// multi-line body keeps its real newlines and tabs.
func sanitizeMemoryField(s string, keepWhitespace bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r == '\n' || r == '\t') && keepWhitespace {
			b.WriteRune(r)
			continue
		}
		if termsafe.Unsafe(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ListMemoryForRun returns the (user, repo) memory for a run the worker owns,
// newest first (PRD #90 read path). A repo-less run has no memory scope, so it
// returns an empty list rather than an error — the worker composes nothing into the
// lead's prompt for such a run.
func (s *Service) ListMemoryForRun(ctx context.Context, wkr store.Worker, runID uuid.UUID) ([]store.AgentMemory, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return nil, err
	}
	if !run.RepoID.Valid {
		return []store.AgentMemory{}, nil
	}
	return s.q.ListAgentMemoryForUserRepo(ctx, store.ListAgentMemoryForUserRepoParams{
		UserID: run.UserID,
		RepoID: uuid.UUID(run.RepoID.Bytes),
	})
}

// RunOwnership returns the current status of a run this worker owns, or
// ErrRunNotOwned when it is not (reclaimed / never owned). Read-only; the
// interactive park-skip path (#559) uses it to detect a mid-turn reclaim or
// terminal transition early, restoring the ACK the skipped park report gave.
func (s *Service) RunOwnership(ctx context.Context, wkr store.Worker, runID uuid.UUID) (string, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return "", err
	}
	return run.Status, nil
}

func (s *Service) runOwnedByWorker(ctx context.Context, runID uuid.UUID, wkr store.Worker) (store.Run, error) {
	run, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgUUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotOwned
		}
		return store.Run{}, err
	}
	return run, nil
}

// ForgeConn is the connection facts a worker-authenticated forge read needs to build
// a driver (PRD #158 M1): the decrypted-at-the-handler token ciphertext plus the
// numeric project id the driver methods require. It carries NO plaintext secret and
// is never serialised to the worker — the DTOs the handlers build deliberately drop
// the project id, base url and token.
type ForgeConn struct {
	ForgeType       string
	BaseUrl         string
	TokenCiphertext []byte
	ForgeProjectID  int64
	// BotForgeUserID is the connection's stored bot user id, used by the get_issue
	// route to drop uzi's own bot-authored comments (PRD #381 M4, D1). Zero when the
	// legacy connection never recorded one, in which case comments are omitted (D9).
	BotForgeUserID int64
	// MRIID is the run's source merge-request iid (PRD #700 M4), nil when the run
	// carries none. The mr_rework write-back endpoints resolve/reply against THIS iid
	// — never a client-supplied one — so an injected id cannot redirect a write to a
	// different MR.
	MRIID *int64
	// ReviewComments is the run's raw runs.review_comments JSONB (PRD #700 M4), nil/
	// empty when the run has no MR review snapshot. The mr_rework write-back endpoints
	// unmarshal it into a ReviewCommentsSnapshot and reject any reply/resolve id not
	// present in it (the Decision-11 server-side scope check).
	ReviewComments []byte
}

// ForgeConnForRun authorizes a worker's forge read against a run it holds and returns
// the connection facts to read with (PRD #158 M1). The authz mirrors SaveMemory: the
// (repo, connection) are derived from the OWNED run — never from the request — so a
// worker cannot read another tenant's forge. A run the worker does not hold is
// ErrRunNotOwned (→ 404); a repo-less run (chat/self-improve) is ErrForgeNoRepo (→
// 409). The repo_id check runs off the owned run FIRST, before the connection query,
// because that query INNER-JOINs repos and so cannot itself tell "not owned" apart
// from "no repo" — both are no-rows.
func (s *Service) ForgeConnForRun(ctx context.Context, wkr store.Worker, runID uuid.UUID) (ForgeConn, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return ForgeConn{}, err
	}
	if !run.RepoID.Valid {
		return ForgeConn{}, ErrForgeNoRepo
	}
	row, err := s.q.GetRunForgeConnForWorker(ctx, store.GetRunForgeConnForWorkerParams{RunID: runID, WorkerID: pgUUID(wkr.ID)})
	if err != nil {
		// The worker owns the run and it has a repo, so a no-rows here is a race (the
		// claim dropped between the two reads); treat it as not-owned rather than a 500.
		if errors.Is(err, pgx.ErrNoRows) {
			return ForgeConn{}, ErrRunNotOwned
		}
		return ForgeConn{}, err
	}
	// PRD #700 M4: carry the run's source mr_iid and raw review-comments snapshot from
	// the SAME owned run read, so the mr_rework write-back endpoints can enforce the
	// Decision-11 scope check without a second (unscoped) run read.
	var mrIID *int64
	if run.MrIid.Valid {
		v := run.MrIid.Int64
		mrIID = &v
	}
	return ForgeConn{
		ForgeType:       row.ForgeType,
		BaseUrl:         row.BaseUrl,
		TokenCiphertext: row.TokenCiphertext,
		ForgeProjectID:  row.ForgeProjectID,
		BotForgeUserID:  row.BotForgeUserID,
		MRIID:           mrIID,
		ReviewComments:  run.ReviewComments,
	}, nil
}

// PublishResult is the outcome of a checkpoint publish (PRD #122 M8). Published is
// true only when the push landed. Skipped names the benign reason a publish did NOT
// advance the ref ("no_ref" | "not_descendant" | "unsupported" | "workflow_scope"); it
// is empty on a successful publish. Either way Ref is the checkpoint ref the worker
// asked about.
type PublishResult struct {
	Published bool
	Ref       string
	Skipped   string
}

// Publish is the api side of the M8 brokered origin push: the worker ships a delta
// PACK plus the tip OID it claims, and the api — the SOLE holder of forge secrets —
// derives the repo, branch, and PAT ENTIRELY from the run row, fetches origin's
// base objects, applies the pack, and pushes it NON-FORCED to
// refs/uzi-checkpoints/<branch>. A worker can never name the repo/ref/credential:
// authorization is server-derived.
//
// It returns a benign PublishResult skip (nil error) for the outcomes the worker
// treats as "origin moved, nothing to do" — a diverged tip or an unsupported pack —
// and an error only for a genuine 5xx (misconfig, decrypt failure, transport fault)
// the worker ignores as best-effort.
func (s *Service) Publish(ctx context.Context, wkr store.Worker, runID uuid.UUID, tipOid string, pack []byte) (PublishResult, error) {
	// 1. Server-derived authorization: the run must be owned by THIS worker.
	// ErrRunNotOwned bubbles to the handler → 404. This is the only thing the worker
	// named (the run id), and it can only reach its own runs.
	owned, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return PublishResult{}, err
	}

	// 2. Branch is derived SERVER-SIDE from the run's issue iid — never from the
	// worker, and never from owned.Branch. runs.branch is written ONLY at completion
	// (SetRunCompleted), so it is NULL/empty for a `running` run; a checkpoint
	// publish fires MID-RUN, so reading it would make the whole feature inert.
	// Checkpoints only ever fire for issue runs (the agent gates the tool to
	// kind==="issue"), so any other kind, or a run missing its issue iid, is an
	// unsupported publish we skip (best-effort) rather than fault. Deriving the
	// branch from the int-typed iid — exactly the worker's agent/issue-<iid> naming —
	// also means no worker-controlled string ever flows into the refspec.
	if owned.Kind != RunKindIssue || !owned.IssueIid.Valid {
		return PublishResult{Published: false, Ref: "", Skipped: "unsupported"}, nil
	}
	branch := agentIssueBranch(owned.IssueIid.Int64)
	ref := "refs/uzi-checkpoints/" + branch

	// 3. Repo + connection facts (clone URL, base URL, default branch, bot username,
	// sealed PAT) come from the run claim context — the same INNER JOIN the claim
	// path uses.
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The run is owned but has no repo/forge connection (e.g. a repo-less
			// run): there is nothing to push to. Treat as a no-ref skip.
			return PublishResult{Published: false, Ref: ref, Skipped: "no_ref"}, nil
		}
		return PublishResult{}, fmt.Errorf("publish claim context: %w", err)
	}
	cloneURL := rc.RepoWebUrl + ".git"

	// 4. SSRF gate, BEFORE decrypting the PAT. A nil gate — or a nil publisher — is a
	// loud misconfiguration (handler → 500), never fail-open: the broker must never be
	// pointed at an arbitrary host.
	if s.forgeBaseURLAllowed == nil {
		return PublishResult{}, fmt.Errorf("publish: forge base URL allowlist is not configured")
	}
	if s.publishFn == nil {
		return PublishResult{}, fmt.Errorf("publish: publisher is not configured")
	}
	// The allowlist must cover BOTH the declared base_url AND the host go-git actually
	// DIALS (cloneURL = repo_web_url + ".git"): the two columns can diverge, and only
	// the dialed host is the real SSRF target. Normalize the clone URL to scheme+host
	// (the shape the gate matches) and assert it too.
	if !s.forgeBaseURLAllowed(rc.BaseUrl) {
		return PublishResult{}, fmt.Errorf("publish: forge base URL is not allowlisted")
	}
	cloneHost, err := forgeHostFromURL(cloneURL)
	if err != nil || !s.forgeBaseURLAllowed(cloneHost) {
		return PublishResult{}, fmt.Errorf("publish: clone host is not allowlisted")
	}

	// 5. Decrypt the bot PAT under the master box. box.Open errors carry no
	// plaintext; wrap without exposing it (mirrors assembleClaim).
	botPAT, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		return PublishResult{}, fmt.Errorf("publish: bot PAT could not be decrypted")
	}

	// 6. Hand the mechanical push to the go-git broker (stubbable seam). Map its
	// benign sentinels to skips; anything else is a best-effort 5xx.
	// The broker's Result.Ref equals the ref computed above; the service returns its
	// own derived ref (never a value shaped by the worker's input), so the Result is
	// consulted only for the error.
	_, err = s.publishFn(ctx, pushbroker.Options{
		CloneURL:      cloneURL,
		BaseURL:       rc.BaseUrl,
		Branch:        branch,
		DefaultBranch: rc.DefaultBranch.String,
		Username:      rc.BotUsername,
		PAT:           string(botPAT),
		DeclaredTip:   tipOid,
		Pack:          pack,
	})
	switch {
	case err == nil:
		return PublishResult{Published: true, Ref: ref}, nil
	case errors.Is(err, pushbroker.ErrNotDescendant):
		return PublishResult{Published: false, Ref: ref, Skipped: "not_descendant"}, nil
	case errors.Is(err, pushbroker.ErrWorkflowScopeRejected):
		// The branch is behind on .github/workflows/** relative to the default branch,
		// so the bot's repo-only PAT cannot push the checkpoint (PRD #456 M4). This is a
		// benign skip like ErrNotDescendant — nil error, so it never reaches the 5xx /
		// slog.Error default arm and never fails the run. Checkpoints stay best-effort;
		// the finalize base-align (PRD #456 M1) is the real safety net that saves this
		// run's work.
		return PublishResult{Published: false, Ref: ref, Skipped: "workflow_scope"}, nil
	case errors.Is(err, pushbroker.ErrTipMissing),
		errors.Is(err, pushbroker.ErrPackTooLarge),
		errors.Is(err, pushbroker.ErrPackInvalid):
		// A tip the pack never delivered, a pack over the reconstruction budget, or a
		// malformed pack: all best-effort "unsupported" skips — never a 5xx. Neither an
		// over-budget nor a malformed worker pack may OOM or 5xx-storm the shared api.
		return PublishResult{Published: false, Ref: ref, Skipped: "unsupported"}, nil
	default:
		// A genuine 5xx from the go-git broker (transport fault, non-sentinel
		// go-git error). This is the ONE forge-touching path whose error does NOT
		// pass through internal/forge's PAT-scrubbing redactor, and a go-git transport
		// error can embed the remote URL — which carries the bot PAT in its userinfo —
		// before it reaches slog.Error in the handler. Restore the stated invariant:
		// run it through the known-credential scrub. The pushbroker sentinels are all
		// matched above, so this error is only logged/returned, never errors.Is-checked
		// downstream — flattening the %w chain to a scrubbed string is safe here.
		return PublishResult{}, fmt.Errorf("publish: %s", secretscrub.Scrub(err.Error()))
	}
}

// forgeHostFromURL reduces a URL (here the clone URL, repo_web_url + ".git") to the
// scheme+host form the forge allowlist gate matches: https-only, lowercased host,
// no path/query/fragment. It mirrors config.NormalizeForgeBaseURL deliberately —
// kept local so workersvc does not depend on the config package — so the value
// handed to forgeBaseURLAllowed is the same shape the allowlist was built from.
func forgeHostFromURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return "", fmt.Errorf("clone URL %q must be https with a host", raw)
	}
	return "https://" + strings.ToLower(u.Host), nil
}

// -------------------------------------------------------------------------
// Web-facing worker + run management
// -------------------------------------------------------------------------

// CreateWorker issues a worker for the user and returns the plaintext join token
// exactly once (only its hash is stored).
// tokenLabel is the optional mint-time Anthropic token binding (PRD #104 M3):
// empty means "no binding", i.e. the worker spends its owner's default. A label
// that names none of the user's tokens is ErrUnknownSecretLabel — minting a worker
// pointed at a credential that does not exist would produce a worker that only
// fails at its first claim.
func (s *Service) CreateWorker(ctx context.Context, userID uuid.UUID, name, templateDeclared, tokenLabel string) (store.Worker, string, error) {
	secretID, err := s.resolveSecretLabel(ctx, userID, tokenLabel)
	if err != nil {
		return store.Worker{}, "", err
	}
	token, hash, err := jointoken.Generate()
	if err != nil {
		return store.Worker{}, "", err
	}
	// The BIND MODE is derived from the same resolution that produced the id, and
	// written in the same statement (PRD #111 M3). It is not optional: since M3 the
	// mode is what decides whether the id is read at all, so a mint that set the id
	// and let the mode take its column default produced a worker carrying a real
	// binding that resolved the owner's DEFAULT — PRD #104 M3's mint-time binding,
	// silently dead. Deriving it here rather than defaulting it in SQL is the same
	// rule PatchWorker applies and the same one 00088's backfill applied to existing
	// rows: a resolved label is `pinned`, its absence is `default`.
	bindMode := BindModeDefault
	if secretID.Valid {
		bindMode = BindModePinned
	}
	// templateDeclared is the UI-chosen worker template (PRD #18), validated
	// against the registry by the caller; empty → NULL (no choice made).
	wkr, err := s.q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID:            userID,
		Name:              name,
		TokenHash:         hash,
		TemplateDeclared:  pgText(templateDeclared),
		AnthropicSecretID: secretID,
		AnthropicBindMode: bindMode,
	})
	if err != nil {
		return store.Worker{}, "", err
	}
	return wkr, token, nil
}

// resolveSecretLabel maps an optional token label to the secret id to store.
// Empty label ⇒ an invalid pgtype.UUID, i.e. NULL, i.e. "use my default".
func (s *Service) resolveSecretLabel(ctx context.Context, userID uuid.UUID, label string) (pgtype.UUID, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return pgtype.UUID{}, nil
	}
	id, err := s.q.GetUserSecretIDByLabel(ctx, store.GetUserSecretIDByLabelParams{
		UserID: userID,
		Kind:   store.KindAnthropicToken,
		Label:  label,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, ErrUnknownSecretLabel
		}
		return pgtype.UUID{}, err
	}
	return pgUUID(id), nil
}

// SetWorkerAnthropicToken points a worker at one of its owner's Anthropic
// credentials, or clears the binding back to the owner's default (PRD #104 M3).
// secretID nil clears it. Takes effect on the worker's NEXT claim: no restart, no
// re-minted join token, because the credential rides each claim response and has
// never been held by the worker.
//
// Ownership is checked THREE times over, deliberately (D11). Here, so an id that
// is not the caller's produces a 404 rather than a constraint violation. In the
// UPDATE's own WHERE, which is scoped to user_id. And in the composite FK, which
// is the layer that still refuses a cross-user binding when this check is bypassed
// — the one the acceptance test exercises with the handler check stubbed out.
func (s *Service) SetWorkerAnthropicToken(ctx context.Context, userID, workerID uuid.UUID, mode string, secretID *uuid.UUID) (store.Worker, error) {
	if !ValidBindMode(mode) {
		return store.Worker{}, ErrInvalidBindMode
	}
	// A non-pinned mode never carries an id. Enforced HERE rather than trusted from
	// the caller, because the pair is what makes the resolution rule work: a
	// 'default' or 'auto' worker that kept a stale id would be one refactor away
	// from spending it, and workerSecretID's "the id is read only in pinned mode" is
	// a much weaker guarantee if the column is allowed to disagree with the mode.
	if mode != BindModePinned {
		secretID = nil
	}
	var bind pgtype.UUID
	if secretID != nil {
		// Confirm the secret is this user's before writing, so the caller gets a 404
		// naming what was wrong instead of a 500 from the FK.
		if _, err := s.q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
			ID:     *secretID,
			UserID: userID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Worker{}, ErrSecretNotOwned
			}
			return store.Worker{}, err
		}
		bind = pgUUID(*secretID)
	}
	wkr, err := s.q.SetWorkerAnthropicSecret(ctx, store.SetWorkerAnthropicSecretParams{
		ID:                workerID,
		UserID:            userID,
		AnthropicSecretID: bind,
		AnthropicBindMode: mode,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Worker{}, ErrWorkerNotFound
		}
		return store.Worker{}, err
	}
	return wkr, nil
}

// SetUserJudgeToken points the user's JUDGE lane at one of their own Anthropic
// credentials, or clears it back to their default when secretID is nil (PRD #104
// M4). Per-user, not per-worker: which credential reviews your work is a property
// of you, not of whichever worker claims the retrospective.
//
// Ownership is checked here so the caller gets a 404 rather than a constraint
// violation; 00079's composite FK refuses the same binding independently, and is
// the layer that holds if this check is ever bypassed (D11).
func (s *Service) SetUserJudgeToken(ctx context.Context, userID uuid.UUID, secretID *uuid.UUID) (store.User, error) {
	var bind pgtype.UUID
	if secretID != nil {
		if _, err := s.q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
			ID:     *secretID,
			UserID: userID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.User{}, ErrSecretNotOwned
			}
			return store.User{}, err
		}
		bind = pgUUID(*secretID)
	}
	return s.q.SetUserJudgeAnthropicSecret(ctx, store.SetUserJudgeAnthropicSecretParams{
		ID:                     userID,
		JudgeAnthropicSecretID: bind,
	})
}

// ResolveTokenLabel exposes label → secret id for the handler's PATCH body, which
// accepts a label (what a human types) rather than a uuid. Returns
// ErrUnknownSecretLabel for a label the user has no token under.
func (s *Service) ResolveTokenLabel(ctx context.Context, userID uuid.UUID, label string) (uuid.UUID, error) {
	id, err := s.resolveSecretLabel(ctx, userID, label)
	if err != nil {
		return uuid.UUID{}, err
	}
	if !id.Valid {
		return uuid.UUID{}, ErrUnknownSecretLabel
	}
	return uuid.UUID(id.Bytes), nil
}

// ListWorkers returns the user's workers with derived busy status.
func (s *Service) ListWorkers(ctx context.Context, userID uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	return s.q.ListWorkersByUser(ctx, userID)
}

// DeleteWorker revokes a worker (its token stops authenticating). A worker that
// still owns a non-terminal run is refused (ErrWorkerHasActiveRuns): deleting it
// would NULL the run's worker_id and strand it (see ErrWorkerHasActiveRuns). The
// count-then-delete is not one statement, but the worst a lost race does is
// re-queue one run to a now-deleted worker_id, which the next claim's affinity
// fallback and the running/claimed sweeps still recover.
func (s *Service) DeleteWorker(ctx context.Context, userID, workerID uuid.UUID) error {
	active, err := s.q.CountWorkerNonTerminalRuns(ctx, store.CountWorkerNonTerminalRunsParams{
		WorkerID: pgUUID(workerID), UserID: userID,
	})
	if err != nil {
		return err
	}
	if active > 0 {
		return ErrWorkerHasActiveRuns
	}
	n, err := s.q.DeleteWorkerForUser(ctx, store.DeleteWorkerForUserParams{ID: workerID, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// CreateRun queues a manually-started run from a board card. The issue must be a
// cached issue carrying the uzi_label (PRD #764 M1) in a repo the user owns; its
// title is snapshotted from the cache and its description from the request, so the
// run is self-contained even if the issue cache is later evicted. A PRD link is no
// longer required. The one-non-terminal-run-per-issue index rejects a duplicate
// active run.
func (s *Service) CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, seed *SeededPlan) (store.Run, error) {
	// waitOnLimit nil ⇒ inherit the owner's default. It is a *bool rather than a bool
	// because "the caller said false" and "the caller said nothing" are different
	// requests, and collapsing them would make every API client that omits the field
	// override the user's own Settings choice with false.
	//
	// seed nil ⇒ an ordinary run planned from the issue (PRD #209): plan_source stays
	// 'agent' and the run behaves byte-identically to a pre-#209 run (Success Criterion
	// 2). Non-nil ⇒ a seeded-plan run that skips Phase 1 and the gate.
	//
	// This is THE interactive, human-initiated path (the board / issue-view Start
	// button, and `uzi run start` through the same HTTP endpoint).
	// nil model: a non-scheduled human run inherits the owner's per-user Worker default
	// (PRD #300) — the per-schedule override applies only to runs a schedule fires.
	// false overrideSubagentModel (PRD #305): an interactive run is not a schedule fire,
	// so it stays in the default lane where subagent pins win.
	return s.createRun(ctx, userID, repoID, issueIID, description, false, waitOnLimit, nil, false, seed)
}

// CreateScheduledRun queues a NON-auto-approve scheduled issue run (PRD #241: a timer
// or label-sweep schedule firing an issue with the plan gate still requiring a human).
// It is IDENTICAL to CreateRun — same single uzi_label eligibility gate (PRD #764 M1) —
// and, like every create path, no longer requires a PRD link.
func (s *Service) CreateScheduledRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, model *string, overrideSubagentModel bool, seed *SeededPlan) (store.Run, error) {
	return s.createRun(ctx, userID, repoID, issueIID, description, false, waitOnLimit, model, overrideSubagentModel, seed)
}

// CreateAutopilotRun queues a run the poller's autopilot detection started on a
// user's behalf (PRD #19 M4). It is IDENTICAL to CreateRun — same ownership, single
// uzi_label eligibility gate (PRD #764 M1), and one-active-run gate, same state
// machine, same queued→In Progress column notify — except it sets auto_approve, which
// the worker reads (M5) to resolve the plan gate without a human. Sharing one createRun
// body is the whole point: the invariant that an autopilot run and a manual run are
// born through the same path is enforced structurally, not by two implementations that
// could drift.
func (s *Service) CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string) (store.Run, error) {
	// nil waitOnLimit: an autopilot run has no human in the loop to express a per-run
	// choice, so it takes the owner's default (PRD #35 Decision 7 / design brief 7.3).
	// nil model: the label poller's autopilot has no per-run model (PRD #300) — a
	// label-driven run inherits the owner's per-user Worker default. The final nil is the
	// seeded plan: autopilot NEVER seeds — it derives its plan in Phase 1 exactly as
	// before (PRD #209 D3 keeps auto_approve and plan_source orthogonal).
	// false overrideSubagentModel (PRD #305): label-poller autopilot is not a schedule
	// fire and carries no per-run opt-in, so subagent pins win (default lane).
	return s.createRun(ctx, userID, repoID, issueIID, description, true, nil, nil, false, nil)
}

// CreateScheduledAutopilotRun queues an auto-approve run for a schedule while honouring
// the schedule's explicit wait-on-limit intent (PRD #274 Decision 1a). It is IDENTICAL
// to CreateAutopilotRun EXCEPT it threads the caller-supplied waitOnLimit instead of
// forcing nil: unlike the label poller (which has no per-run choice and so falls back to
// the owner default), a scheduled run carries a persisted per-schedule wait_on_limit that
// must take effect. Kept as a SEPARATE method on purpose — the poller's CreateAutopilotRun
// seam (its interface, fake, and call site) stays byte-identical, so widening the
// scheduler seam cannot change label-driven autopilot. seed=nil for the same reason as
// CreateAutopilotRun: autopilot never seeds its plan.
func (s *Service) CreateScheduledAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, model *string, overrideSubagentModel bool) (store.Run, error) {
	return s.createRun(ctx, userID, repoID, issueIID, description, true /*autoApprove*/, waitOnLimit, model, overrideSubagentModel, nil /*seed*/)
}

// SeededPlan carries a create-time externally-authored plan and its optional agent
// selection (PRD #209). A run created with a SeededPlan skips the Phase-1 planning
// turn and the approval gate: the worker implements PlanMD directly. Nil for an
// ordinary run planned from the issue alone.
type SeededPlan struct {
	// PlanMD is the externally-authored plan. Untrusted input (D5): capped and
	// secret-scrubbed before storage, and an empty/whitespace plan is rejected (D8).
	PlanMD string
	// Selection is the run's subagent roster, persisted through the SAME columns the
	// human plan gate writes (agent_source / agent_exclusions). Nil ⇒ the run makes no
	// selection, so the worker's resolveAgentSelection default applies: repo agents
	// when the clone has a roster, else own (Success Criterion 5). Validated for SHAPE
	// only at create time — the clone roster is unknown here (Open Question 1).
	Selection *AgentSelection
	// PlannedCommit is the commit the external planner planned against (PRD #209 M4),
	// forwarded on `uzi run create --planned-commit`. Empty ⇒ no planned commit was
	// supplied, so the worker's staleness compare is inert. Stored verbatim (full or
	// abbreviated SHA); the worker compares it prefix-tolerantly to the clone's base.
	PlannedCommit string
	// RequireBase, when true, makes a base-commit divergence FAIL the run rather than
	// warn into the feed (`uzi run create --require-base`, PRD #209 M4, Open Question 3).
	// Only meaningful with a PlannedCommit to compare against — the handler rejects it
	// otherwise.
	RequireBase bool
}

func (s *Service) createRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, autoApprove bool, waitOnLimit *bool, model *string, overrideSubagentModel bool, seed *SeededPlan) (store.Run, error) {
	// The description cap is enforced HERE, once, so the manual (handler → 422) and
	// autopilot (poller → too-large comment) paths cannot drift (PRD #19 M5). Checked
	// first: it is pure input validation, independent of the repo/issue gates below.
	if len(description) > MaxIssueDescriptionBytes {
		return store.Run{}, ErrDescriptionTooLarge
	}
	// Seeded-plan validation (PRD #209 D5/D8), also pure input validation, so checked
	// alongside the description cap and BEFORE the repo/issue gates. The four run
	// columns default to the not-seeded state — a NULL plan_md, plan_source 'agent', a
	// NULL selection — which is exactly a pre-#209 run and satisfies Success Criterion
	// 2 by construction.
	planMD := pgtype.Text{}
	planSource := planSourceAgent
	agentSource := pgtype.Text{}
	var agentExclusions []byte
	// M4 staleness-guard columns default to the not-seeded state: no planned commit
	// (NULL) and warn-only (false). Only a seeded run created with --planned-commit /
	// --require-base overrides them below.
	plannedBaseCommit := pgtype.Text{}
	requireBaseMatch := false
	if seed != nil {
		// D5: cap the RAW input first (mirrors ErrDescriptionTooLarge — reject, do not
		// truncate), THEN scrub secrets, THEN reject an empty/whitespace result. The cap
		// is on the RAW input deliberately — capping after the scrub could let an oversize
		// body through if the scrub happened to shrink it. The empty check, by contrast,
		// is ORTHOGONAL to the scrub: secretscrub.Scrub replaces each matched secret with
		// the non-empty marker "[redacted]" and deletes nothing, so it can never turn a
		// non-empty plan into an empty one (a plan that is only a leaked secret is stored
		// as "[redacted]", NOT blanked or rejected). So this rejects a genuinely empty or
		// whitespace-only plan; running it on the scrubbed text is equivalent to running
		// it on the raw and just stays correct if the scrub ever gains a deleting rule.
		if len(seed.PlanMD) > MaxSeededPlanBytes {
			return store.Run{}, ErrPlanTooLarge
		}
		scrubbed := secretscrub.Scrub(seed.PlanMD)
		if strings.TrimSpace(scrubbed) == "" {
			return store.Run{}, ErrPlanEmpty
		}
		// issue #280: bright-line safety screen. A seeded plan skips the approval
		// gate, so screen the scrubbed body for prohibited infrastructure-recon
		// targets and REJECT at create — the run is never persisted. Runs after the
		// scrub so a redacted secret can't mask a target, and only on the seeded
		// path (this whole block is `if seed != nil`); ordinary runs are untouched.
		if target, unsafe := planpolicy.Screen(scrubbed); unsafe {
			return store.Run{}, fmt.Errorf("%w: %s", ErrPlanUnsafe, target)
		}
		planMD = pgText(scrubbed)
		planSource = planSourceSeeded
		if seed.Selection != nil {
			sel := *seed.Selection
			// Shape only — no roster exists at create time (Open Question 1). A source
			// that is not 'repo'/'own' would otherwise hit the agent_source CHECK
			// constraint as a 500; a malformed exclusion name would persist unchecked.
			if err := validateSelectionShape(sel); err != nil {
				return store.Run{}, err
			}
			agentSource = pgText(sel.Source)
			enc, err := encodeJSONArray(sel.Exclusions)
			if err != nil {
				return store.Run{}, fmt.Errorf("encode seeded agent exclusions: %w", err)
			}
			agentExclusions = enc
		}
		// M4: the commit the plan was written against. Trimmed, then validated as a
		// plausible git sha (hex, 7-64) and stored TRIMMED. Empty ⇒ NULL, and the worker's
		// staleness compare is inert. The validation is load-bearing, not cosmetic: the
		// worker compare is prefix-tolerant with no floor, so an unvalidated 1-2 char value
		// would match almost any base and silently disarm --require-base (ErrInvalidPlannedCommit).
		// require_base_match only ever fires alongside a planned commit; the handler rejects
		// require_base without planned_commit.
		if pc := strings.TrimSpace(seed.PlannedCommit); pc != "" {
			if !plannedCommitRe.MatchString(pc) {
				return store.Run{}, ErrInvalidPlannedCommit
			}
			plannedBaseCommit = pgText(pc)
		}
		requireBaseMatch = seed.RequireBase
	}
	row, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	// #66 D1 layer 2: the service-layer guardrail, shared across the PAT-bearing
	// inserts (issue lane, CI-fix, self-improve, scheduled prompt). Reached by the UI
	// AND autopilot, so gating here (not the handler) is what covers the unattended path.
	if err := s.guardDefaultBranch(ctx, row); err != nil {
		return store.Run{}, err
	}
	issue, err := s.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: issueIID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrIssueNotFound
		}
		return store.Run{}, err
	}
	// The RUN-ELIGIBILITY gate (PRD #764, widened by PRD #767): an issue is uzi's to
	// run iff it carries the configured uzi_label OR is assigned to the uzi-bot account
	// (row.BotForgeUserID). Assignment is an ADDITIVE second signal, the same single
	// concept expressed two natural ways; it grants eligibility only — unattended
	// execution still needs autopilot or an enabled sweep (PRD #767 D1). A run no longer
	// requires a PRD link or any escape-hatch/waiver label. A linked prds/*.md is still
	// detected and implemented when present, but never required.
	//
	// Derived from the cached labels/assignees rather than a fresh forge read: the same
	// jsonb the board renders the card from, so the button a user sees and the gate the
	// server applies cannot disagree. Promote writes the label forge-first AND updates
	// this cache row in the same request, so the promote-then-run sequence is not racing
	// the poller.
	if !isEligibleIssue(issue.Labels, []string{s.uziLabel(ctx)}) &&
		!isAssignedToBot(issue.AssigneeIds, row.BotForgeUserID) {
		return store.Run{}, ErrNotPRDIssue
	}
	// Cross-kind same-branch exclusion (PRD #6): this issue run will use the
	// worktree agent/issue-<iid>; refuse if an active ci_fix run is already fixing
	// that ref. The reverse check lives in CreateCIFixRun; the two partial unique
	// indexes are disjoint and cannot express this cross-kind rule.
	fixing, err := s.q.CountActiveCIFixForRef(ctx, store.CountActiveCIFixForRefParams{
		RepoID:      repoID,
		PipelineRef: pgtype.Text{String: agentIssueBranch(issueIID), Valid: true},
	})
	if err != nil {
		return store.Run{}, err
	}
	if fixing > 0 {
		return store.Run{}, ErrBranchInUse
	}
	// Manual-path dedup pre-check (PRD #754 M4 Decision 8). The uq_runs_one_active_per_issue
	// index — the ONLY dedup the manual/board/Slack path had (it relies solely on the index
	// catching 23505 → ErrActiveRunExists below) — now EXCLUDES pool_wait so a held run is
	// non-locking. That would let a second manual start slip past the index while a run is
	// held, so the gate is restored HERE for the issue kind: HasActiveRunForIssue still
	// counts pool_wait as active (it is a `status NOT IN terminal` SELECT), and mirrors the
	// poller's own pre-check (same repo_id, issue_iid params). Scoped to the index's
	// kind='issue' domain — createRun is the issue-kind path, so this gate matches it and
	// leaves ci_fix/chat/etc. ungated. It returns the SAME sentinel the index path returns,
	// so a caller cannot tell which layer caught the duplicate.
	active, err := s.q.HasActiveRunForIssue(ctx, store.HasActiveRunForIssueParams{
		RepoID:   repoID,
		IssueIid: pgtype.Int8{Int64: issueIID, Valid: true},
	})
	if err != nil {
		return store.Run{}, err
	}
	if active {
		return store.Run{}, ErrActiveRunExists
	}
	// PRD #381: snapshot the issue's human comments alongside the description. One
	// extra forge round-trip, centralized here so every issue-backed origin (manual,
	// autopilot, scheduled) captures it (D6) without rippling the Create*Run seam.
	// Best-effort: a forge glitch, a nil forge builder (tests), or an unknown bot id
	// (D9) all degrade to a NULL snapshot rather than failing run creation.
	var issueCommentsJSON []byte
	if s.forges != nil {
		if snap := s.fetchIssueCommentsSnapshot(ctx, row, issueIID); snap != nil {
			if b, err := json.Marshal(snap); err != nil {
				slog.Error("workersvc: marshal issue comments snapshot", "issue_iid", issueIID, "error", err)
			} else {
				issueCommentsJSON = b
			}
		}
	}
	run, err := s.q.CreateRun(ctx, store.CreateRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueIid:         pgtype.Int8{Int64: issueIID, Valid: true},
		IssueTitle:       issue.Title,
		IssueDescription: description,
		OriginColumn:     s.originColumn(ctx, repoID, issue),
		AutoApprove:      autoApprove,
		// PRD #35 Decision 7. Stamped at creation from the owner's default (or the
		// caller's explicit choice), never read from users at park time: a run must
		// keep the behaviour it was created with, so flipping the default later cannot
		// retroactively change a run already in flight.
		WaitOnLimit: s.resolveWaitOnLimit(ctx, userID, waitOnLimit),
		// PRD #209 seeded-plan columns, listed explicitly (runtime.sql's 🔴 warning):
		// all zero-valued to the not-seeded state above unless seed != nil. The M4
		// staleness-guard pair (planned_base_commit, require_base_match) is here for the
		// same reason — require_base_match is NOT NULL DEFAULT false, so omitting it would
		// silently opt every seeded run out of the fail-on-divergence behaviour.
		PlanMd:            planMD,
		PlanSource:        planSource,
		AgentSource:       agentSource,
		AgentExclusions:   agentExclusions,
		PlannedBaseCommit: plannedBaseCommit,
		RequireBaseMatch:  requireBaseMatch,
		// PRD #300: the per-schedule model override, frozen onto the run at fire time.
		// nil for every non-scheduled caller (interactive, label-poller autopilot) →
		// NULL → the run inherits the owner's per-user Worker default at claim assembly.
		Model: pgTextPtr(model),
		// PRD #305: the schedule's "apply model also to agents" opt-in, frozen onto the
		// run at fire time. false for every non-scheduled caller (interactive,
		// label-poller autopilot) → the default lane where subagent pins win. M1 stores
		// it only; claim delivery (M3) and worker behaviour (M4) are separate milestones.
		OverrideSubagentModel: overrideSubagentModel,
		// PRD #381: the structured human-comments snapshot, fetched best-effort above.
		// nil (→ NULL) for a non-issue kind, a comment-less issue, an unknown bot id
		// (D9), or when no forge builder is wired (tests).
		IssueComments: issueCommentsJSON,
		// PRD #700 M2: issue runs never carry MR review comments — always NULL here.
		// The mr_rework create path (M3's CreateAutoMRReworkRun) fetches the MR review
		// snapshot via fetchReviewCommentsSnapshot and populates this itself.
		ReviewComments: nil,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActiveRunExists
		}
		return store.Run{}, err
	}
	// queued is the start intent → In Progress. The move marker is already stamped
	// in the CreateRun statement; Notify performs the move.
	s.notify(run.ID, "queued")
	return run, nil
}

// fetchIssueCommentsSnapshot builds a forge driver from the run's repo connection,
// reads the issue's comments, and returns the filtered/capped snapshot (PRD #381).
// Returns nil (→ NULL) on any error or when the D1/D9 filter leaves nothing — a
// comment snapshot is best-effort run CONTEXT, never a reason to fail creation.
func (s *Service) fetchIssueCommentsSnapshot(ctx context.Context, row store.GetRepoForUserRow, issueIID int64) *IssueCommentsSnapshot {
	f, err := s.forges.ForgeForConnection(row.ForgeType, row.BaseUrl, row.TokenCiphertext)
	if err != nil {
		slog.Error("workersvc: build forge for issue comments", "issue_iid", issueIID, "error", err)
		return nil
	}
	comments, err := f.ListIssueComments(ctx, row.ForgeProjectID, issueIID)
	if err != nil {
		slog.Error("workersvc: list issue comments", "issue_iid", issueIID, "error", err) // err is PAT-redacted by the driver
		return nil
	}
	return buildIssueCommentsSnapshot(comments, row.BotForgeUserID)
}

// uziLabel resolves the configured run-eligibility label (PRD #764 M1), falling back
// to the compiled-in default when settings are unwired or a read fails. Same shape as
// the old prdLabel helper it replaces: an unavailable settings read degrades to
// enforcing the gate on "uzi", never to skipping it — the accessor already returns the
// default alongside a cold error, so this stays best-effort by design without ever
// failing open.
func (s *Service) uziLabel(ctx context.Context) string {
	if s.settings != nil {
		if l, _ := s.settings.UziLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultUziLabel
}

// isEligibleIssue reports whether a cached issue's labels jsonb carries ANY of the
// given eligible labels (PRD #764 M1: the single-element uzi_label set). A row whose
// labels cannot be decoded is NOT eligible: the gate has no basis for letting it
// through, and a corrupt or absent value must not read as consent. Matching is exact,
// like the forge-side label filter the sync applies and like every other label
// comparison in this codebase.
func isEligibleIssue(labelsJSON []byte, eligible []string) bool {
	var labels []string
	if err := json.Unmarshal(labelsJSON, &labels); err != nil {
		return false
	}
	for _, e := range eligible {
		if slices.Contains(labels, e) {
			return true
		}
	}
	return false
}

// isAssignedToBot reports whether the cached issue's assignee_ids jsonb (a set of
// numeric forge user ids) contains the connection's bot forge user id. Like
// isEligibleIssue, an undecodable value is NOT a match: the gate has no basis for
// consent. botID <= 0 (an unset/absent bot id) never matches, so a connection
// without a resolved bot never grants assignment-eligibility by accident.
func isAssignedToBot(assigneeIDsJSON []byte, botID int64) bool {
	if botID <= 0 {
		return false
	}
	var ids []int64
	if err := json.Unmarshal(assigneeIDsJSON, &ids); err != nil {
		return false
	}
	return slices.Contains(ids, botID)
}

// originColumn resolves the issue's current column to snapshot onto the run, so a
// failed/cancelled run restores where it started. A successful resolution is
// always a valid text value ("" = the implicit Open column). If the columns
// cannot be listed the origin is left NULL (unknown), NOT "" — the run is not
// blocked, but a later restore must skip rather than confidently strip the card
// to Open on a guess (spec invariant: NULL = unknown = never restore).
func (s *Service) originColumn(ctx context.Context, repoID uuid.UUID, issue store.Issue) pgtype.Text {
	cols, err := s.q.ListBoardColumns(ctx, repoID)
	if err != nil {
		return pgtype.Text{} // NULL = unknown, never guess Open
	}
	position := make(map[string]int, len(cols))
	for _, c := range cols {
		position[c.LabelName] = int(c.Position)
	}
	var labels []string
	if err := json.Unmarshal(issue.Labels, &labels); err != nil {
		labels = []string{}
	}
	col, _, _ := board.ResolveColumn(labels, issue.State, position)
	return pgtype.Text{String: col, Valid: true}
}

// GetRun returns a run owned by the user.
func (s *Service) GetRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	return run, nil
}

// ListRunMessages returns a run's persisted messages after a seq, for replay.
func (s *Service) ListRunMessages(ctx context.Context, userID, runID uuid.UUID, afterSeq int32) ([]store.RunMessage, error) {
	if _, err := s.GetRun(ctx, userID, runID); err != nil {
		return nil, err
	}
	return s.q.ListRunMessagesAfter(ctx, store.ListRunMessagesAfterParams{RunID: runID, AfterSeq: afterSeq})
}

// ListFollowUpInputs returns a run's follow_up steer queue (newest-first, uncapped)
// for the web/CLI queue (PRD #95). GetRun resolves owner-or-404 FIRST, so a non-owner
// — including an admin_ro token on another user's run — gets ErrRunNotFound and the
// handler 404s. This is strict owner-only (matching the follow-up WRITE), NOT the
// owner-or-admin view: follow-ups are never in run_messages, so an owner-or-admin read
// here would leak another user's steer text.
func (s *Service) ListFollowUpInputs(ctx context.Context, userID, runID uuid.UUID) ([]store.RunUserInput, error) {
	if _, err := s.GetRun(ctx, userID, runID); err != nil {
		return nil, err
	}
	return s.q.ListFollowUpInputsForRun(ctx, runID)
}

// GetRunForViewer returns a run visible to the viewer: the owner sees their own
// run; an admin sees any run. A non-owner non-admin gets ErrRunNotFound, exactly
// as an unknown id would — the same authorization REST and WS both enforce.
func (s *Service) GetRunForViewer(ctx context.Context, userID uuid.UUID, isAdmin bool, runID uuid.UUID) (store.Run, error) {
	if isAdmin {
		run, err := s.q.GetRunByID(ctx, runID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Run{}, ErrRunNotFound
			}
			return store.Run{}, err
		}
		return run, nil
	}
	return s.GetRun(ctx, userID, runID)
}

// ListRunMessagesForViewer is ListRunMessages with the owner-or-admin visibility
// rule, backing the run view's replay for both an owner and an admin observer.
func (s *Service) ListRunMessagesForViewer(ctx context.Context, userID uuid.UUID, isAdmin bool, runID uuid.UUID, afterSeq int32) ([]store.RunMessage, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, runID); err != nil {
		return nil, err
	}
	return s.q.ListRunMessagesAfter(ctx, store.ListRunMessagesAfterParams{RunID: runID, AfterSeq: afterSeq})
}

// ListRunMessagesForViewerPage is the bounded twin of ListRunMessagesForViewer
// (issue #160): same owner-or-admin visibility gate, but @limit caps the page so
// the CLI (M3) can page instead of pulling an unbounded response. The caller is
// responsible for clamping limit to a sane maximum before calling.
func (s *Service) ListRunMessagesForViewerPage(ctx context.Context, userID uuid.UUID, isAdmin bool, runID uuid.UUID, afterSeq int32, limit int32) ([]store.RunMessage, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, runID); err != nil {
		return nil, err
	}
	return s.q.ListRunMessagesAfterPage(ctx, store.ListRunMessagesAfterPageParams{RunID: runID, AfterSeq: afterSeq, Lim: limit})
}

// ListRunsForUser returns the user's runs (newest first) with repo path and
// worker name for the Runs index. repoID and issueIID are optional narrowings
// (nil = no filter): repo scope backs the board attention strip, repo+issue backs
// the in-app issue history.
func (s *Service) ListRunsForUser(ctx context.Context, userID uuid.UUID, repoID *uuid.UUID, issueIID *int64) ([]store.ListRunsForUserRow, error) {
	arg := store.ListRunsForUserParams{
		UserID: userID,
		// PRD #320 D8: the D4 fail-open cutoff for the row's priority_class column, built
		// the same way as ClaimRun's BackgroundGraceCutoff so the pill and the claim order
		// are one decision. A demoted run created before it reads `restored`, not stuck.
		BackgroundGraceCutoff: pgTime(s.now().Add(-s.p.WorkerBackgroundGrace)),
	}
	if repoID != nil {
		arg.RepoID = pgUUID(*repoID)
	}
	if issueIID != nil {
		arg.IssueIid = pgtype.Int8{Int64: *issueIID, Valid: true}
	}
	return s.q.ListRunsForUser(ctx, arg)
}

// ListAllWorkers returns every worker with owner email and busy status (admin).
func (s *Service) ListAllWorkers(ctx context.Context) ([]store.ListAllWorkersRow, error) {
	return s.q.ListAllWorkers(ctx)
}

// ListActiveRunsAll returns every non-terminal run across all users (admin).
func (s *Service) ListActiveRunsAll(ctx context.Context) ([]store.ListActiveRunsAllRow, error) {
	return s.q.ListActiveRunsAll(ctx, s.activeRunsPriorityCutoff())
}

// activeRunsPriorityCutoff builds the ListActiveRunsAll @background_grace_cutoff param
// (PRD #320 D8 fail-open flag for the row's priority_class), shared by both callers of
// the query (the admin DTO path and the self_improve in-flight avoid-set).
func (s *Service) activeRunsPriorityCutoff() pgtype.Timestamptz {
	return pgTime(s.now().Add(-s.p.WorkerBackgroundGrace))
}

// RunUsageTotal returns one run's rolled-up token/cost totals (PRD #40 M3). Returns
// pgx.ErrNoRows for a run with no usage — the caller renders that as "no usage".
func (s *Service) RunUsageTotal(ctx context.Context, runID uuid.UUID) (store.GetRunUsageTotalRow, error) {
	return s.q.GetRunUsageTotal(ctx, runID)
}

// SelfUsage returns the user's own lifetime + last-7-days usage totals and their
// usage-bearing run count (PRD #40 M3, GET /api/usage).
func (s *Service) SelfUsage(ctx context.Context, userID uuid.UUID) (store.SelfUsageRow, error) {
	return s.q.SelfUsage(ctx, userID)
}

// AdminUsageTotals returns factory-wide usage totals across all users (PRD #40 M3).
func (s *Service) AdminUsageTotals(ctx context.Context) (store.AdminUsageTotalsRow, error) {
	return s.q.AdminUsageTotals(ctx)
}

// AdminUsagePerUser returns the per-user usage breakdown for the admin factory view
// (PRD #40 M3); the rows sum to the factory total by construction.
func (s *Service) AdminUsagePerUser(ctx context.Context) ([]store.AdminUsagePerUserRow, error) {
	return s.q.AdminUsagePerUser(ctx)
}

// SubmitInputResult reports how a steering input was handled.
type SubmitInputResult struct {
	// ServerSide is true when a cancel/reject was applied directly because no
	// live poller would ever consume it.
	ServerSide bool
	// ID + CreatedAt are the created run_user_inputs row (PRD #95 S2), set only on the
	// follow_up plain-input path so the handler can return them — the web's optimistic
	// queue entry adopts the real id + timestamp instead of stranding a temp one. Zero
	// on the server-side and approve_plan paths (no queue row to surface).
	ID        int64
	CreatedAt time.Time
	// ExcludedGuardRoles are the guard roles (workersvc guardRoles) this approve
	// EXPLICITLY excluded, in exclusion order; the handler emits one owner heads-up
	// notification when it is non-empty (PRD #319 M3). Empty on every non-approve path,
	// and on an approve that dropped no guard role. Populated only AFTER validateSelection
	// accepts the selection, so a rejected exclusion never notifies.
	ExcludedGuardRoles []string
	// ScopeCeiling is the RESOLVED (post-clamp) scope ceiling written by a `scope` directive
	// or by a milestone-run `stop` mapped to a scope write (PRD #634 M2), so m5's CLI can
	// report the clamped value back to the operator. Nil on every non-scope path.
	ScopeCeiling *int
}

// SubmitInput records a steering input (approve/reject/follow-up/cancel) for a
// run the user owns. When the target is a cancel or plan rejection and no live
// poller exists (the run is still queued, or its worker has gone stale), the
// transition is applied server-side so the input is never stranded waiting for a
// GET /inputs poll that will never come. Otherwise the input is enqueued for the
// worker to consume.
//
// sel is the PRD #37 agent selection and is legal ONLY on approve_plan (nil
// everywhere else, including the Slack approve path, which offers no picker — such
// a run keeps whatever default its worker resolves). It is validated against the
// run's actual roster, then persisted to the run row and JSON-encoded into the
// input body in one statement.
//
// approve_plan deliberately has no server-side no-poller branch: a run can only be
// awaiting approval because a live worker put it there, so an approve with no
// poller is a race the worker's own gate timeout resolves. Only cancel/reject_plan
// need the branch below.
//
// The capability approval gate (PRD #84 M4 4c) is ENFORCED here — use
// SubmitInputWithCapabilityOverride for the owner "run without the capability" override.
func (s *Service) SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection) (SubmitInputResult, error) {
	return s.submitInput(ctx, userID, runID, kind, body, sel, false)
}

// SubmitInputWithCapabilityOverride is SubmitInput for the PRD #84 M4 4c owner override
// ("run without the capability", Decision 12). It BYPASSES the capability approval gate and
// clears the run's inferred/hinted required_capabilities — but ATOMICALLY with a successful
// approve: the clear runs ONLY after the approve's own validation (selection roster check)
// and enqueue succeed, so a FAILED approve (e.g. an invalid agent selection) leaves
// required_capabilities INTACT and the retry stays gated. This closes the non-atomic drop
// the old handler-side pre-clear had, where a failed approve permanently dropped the
// requirement. The override is meaningful only for approve_plan; on any other kind the gate
// never runs, so the flag is inert.
func (s *Service) SubmitInputWithCapabilityOverride(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection) (SubmitInputResult, error) {
	return s.submitInput(ctx, userID, runID, kind, body, sel, true)
}

func (s *Service) submitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection, overrideCapabilities bool) (SubmitInputResult, error) {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if terminalStatuses[run.Status] {
		return SubmitInputResult{}, ErrRunTerminal
	}
	// A chat run's turns MUST ride SubmitChatMessage, which counts persisted
	// follow_ups against ChatMaxTurns before enqueuing (chat.go). A follow_up posted
	// to the generic /inputs endpoint would skip that count and burn spend past the
	// cap, so reject it here — at the service boundary, before any row is written, so
	// the guard covers HTTP/CLI/future Slack. Only follow_up is blocked: cancel (which
	// EndChat rides), reject_plan, approve_plan and answer stay legal on a chat run.
	if run.Kind == RunKindChat && kind == "follow_up" {
		return SubmitInputResult{}, ErrChatInputNotAllowed
	}
	if sel != nil && kind != "approve_plan" {
		return SubmitInputResult{}, fmt.Errorf("%w: an agent selection is only valid when approving a plan", ErrInvalidSelection)
	}
	// PRD #84 M4 4c: the AUTHORITATIVE capability approval gate runs here for EVERY
	// approve_plan — both the selection-bearing dispatch and the nil-selection plain-enqueue
	// path — so neither can approve a plan onto a worker that cannot run it. See
	// capabilityGate.
	//
	// The owner OVERRIDE ("run without the capability", Decision 12) instead BYPASSES the
	// gate and clears the run's required_capabilities — but the clear is ATOMIC with a
	// successful approve: it runs ONLY after the approve's own validation (submitApproval's
	// roster check) and enqueue have succeeded, so a FAILED approve (e.g. an invalid
	// selection) leaves the requirement INTACT and the retry stays gated. Doing the clear
	// here, after the enqueue, rather than in the handler BEFORE this call, is the fix for
	// the non-atomic drop.
	if kind == "approve_plan" {
		if !overrideCapabilities {
			if err := s.capabilityGate(ctx, run); err != nil {
				return SubmitInputResult{}, err
			}
		}
		var res SubmitInputResult
		if sel != nil {
			res, err = s.submitApproval(ctx, run, *sel)
		} else {
			res, err = s.enqueueRunInput(ctx, runID, kind, body)
		}
		if err != nil {
			return SubmitInputResult{}, err
		}
		if overrideCapabilities {
			// Only reached once the approve fully succeeded, so a failed approve above never
			// clears the requirement. Owner- and awaiting_approval-scoped in SQL.
			if err := s.OverrideRunRequiredCapabilities(ctx, userID, runID); err != nil {
				return SubmitInputResult{}, err
			}
		}
		return res, nil
	}

	// An `answer` resolves the clarification question the run is CURRENTLY parked on
	// (PRD #88 M1). Unlike every other kind it is rejected outright unless the run is
	// actually asking: SubmitInput otherwise accepts any non-terminal run, so an
	// answer posted before any ask_user would be enqueued, consumed by the steering
	// poll, and auto-resolve the first question the moment it opened — the user would
	// never see the question, and the feed would show it answered by text written
	// before it existed. The run row is already loaded here, so the guard is free.
	//
	// It is belt-and-braces rather than the primary control (the identity check below
	// independently rejects an answer that does not name the open question), but it
	// rejects at the earliest point and with an error the caller can act on.
	if kind == "answer" {
		return s.submitAnswer(ctx, run, body)
	}

	// PRD #634 M2: decode the run's milestone facts once, shared by the `scope` branch and by
	// the milestone-run `stop` remap below. `run` is a GetRun SELECT *, so both jsonb columns
	// are populated; a decode error degrades to an empty slice (len 0), which correctly makes
	// milestoneIssueRun false rather than failing the input. len(frozen) is the ceiling upper
	// bound; len(completed) the current floor (already-done milestones can never be un-done).
	frozen, _ := DecodeMilestones(run.MilestonesFrozen)
	completed, _ := DecodeMilestoneIDs(run.MilestonesCompleted)
	milestoneIssueRun := run.Kind == RunKindIssue && len(frozen) > 0

	// A `scope` directive (PRD #634 M2) bounds how many of the run's frozen milestones it may
	// complete. It writes runs.scope_ceiling (the control the worker honors on its ACK) plus a
	// kind='scope' audit row in one statement, and NEVER a stop_kind — the scope-capped
	// disposition is stamped at finalize by the worker (m3/m4). The desired ceiling is clamped
	// into [len(completed), len(frozen)] and reported back (never rejected for range), so an
	// operator can only ever bound the run between "nothing further" and "the whole list".
	if kind == "scope" {
		if !milestoneIssueRun {
			return SubmitInputResult{}, ErrScopeNotMilestoneRun
		}
		n, err := strconv.Atoi(strings.TrimSpace(body))
		if err != nil {
			return SubmitInputResult{}, ErrInvalidScopeCeiling
		}
		ceiling := clampInt(n, len(completed), len(frozen))
		auditBody := fmt.Sprintf("scope ceiling → complete through milestone %d of %d", ceiling, len(frozen))
		if ceiling != n {
			auditBody += fmt.Sprintf(" (clamped from %d)", n)
		}
		return s.submitScopeCeiling(ctx, run, ceiling, auditBody)
	}

	// A graceful `stop` (PRD #517 M4) is the interactive-run wind-down: unlike cancel/
	// reject_plan it has NO server-side !live transition branch, because only the worker can
	// finalize it (push + open MR iff open_mr) and report `completed` with stop_kind='stopped'.
	// So a stop ALWAYS enqueues via CreateStopVerdictInput, stamping stop_kind='stopped' in the
	// same statement. A live parked/running worker consumes it and finalizes; a dead-worker
	// parked run is requeued by M2's stale-heartbeat sweep (awaiting_followup is in
	// RequeueRunsOfStaleWorkers) and honors the pending stop on resume. The terminal guard at
	// the top of SubmitInput already 409s a stop on a finished run. Never routes through
	// CancelRunServerSide/RejectRunServerSide.
	//
	// stop_reason carries the operator's OPTIONAL message (like a cancel's — a stop reason is
	// helpful, not mandatory); the same message is co-written to run_user_inputs.body, NUL-
	// stripped to avoid Postgres 22021 aborting the CTE.
	if kind == "stop" {
		// PRD #634 M2: on a milestone-structured issue run a `stop` means "finalize what is
		// already complete, start no further milestone" — which is exactly a scope write with
		// ceiling = len(completed). Map it to a scope write BEFORE the interactive-task guard
		// below, so a stop on an issue run is accepted rather than 409'd. It does NOT go through
		// CreateStopVerdictInput (no stop_kind='stopped') — the scope-capped disposition is
		// stamped at finalize by the worker in m3, not here.
		if milestoneIssueRun {
			reason, _ := stripNUL(body)
			reason = strings.TrimSpace(reason)
			auditBody := fmt.Sprintf("stop → finalize %d completed milestone(s), start no further", len(completed))
			if reason != "" {
				auditBody += ": " + reason
			}
			return s.submitScopeCeiling(ctx, run, len(completed), auditBody)
		}
		// Only an interactive task run has a park that reads the stop flag, so a stop is
		// meaningful ONLY there. Reject it on any other run BEFORE stamping, so a
		// non-interactive-task / chat / plan-gated run cannot acquire a spurious permanent
		// stop_kind='stopped' and return a misleading success. `run` came from GetRun (a
		// SELECT *), so Kind and Interactive are populated. The guard is on kind+interactive
		// only — a RUNNING interactive task (not yet parked) is still a legal stop target.
		// The owner-scope (GetRun→404) and terminal (ErrRunTerminal→409) guards above run
		// first and are unchanged.
		if run.Kind != RunKindTask || !run.Interactive {
			return SubmitInputResult{}, ErrStopNotInteractive
		}
		cleanBody, _ := stripNUL(body)
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID: runID, Kind: kind, Body: pgText(cleanBody), StopKind: pgText(stopKindFor(kind)), StopReason: stopReasonParam(body),
		}); err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false}, nil
	}

	if kind == "cancel" || kind == "reject_plan" {
		live, err := s.hasLivePoller(ctx, run)
		if err != nil {
			return SubmitInputResult{}, err
		}
		if !live {
			status := "cancelled"
			if kind == "cancel" {
				// PRD #503 M3: persist the operator's OPTIONAL cancel reason; an empty
				// body stores NULL (a cancel reason is helpful, not mandatory).
				_, err = s.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
					ID: runID, UserID: userID, StopReason: stopReasonParam(body),
				})
			} else {
				status = "failed"
				// PRD #503 M2 — persist the operator's reject reason as failure_reason
				// instead of the hardcoded literal; the CLI now requires it, but keep a
				// fallback for non-CLI callers that may still send an empty body. Sanitize
				// like the worker-reported failure_reason (strip NUL — a NUL in a text column
				// raises 22021 and would abort the reject — then cap length).
				reason, _ := stripNUL(body)
				reason = strings.TrimSpace(reason)
				if reason == "" {
					reason = "plan rejected"
				}
				reason = truncateRunes(reason, maxFailureReasonRunes)
				_, err = s.q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{
					ID: runID, UserID: userID, FailureReason: pgText(reason),
				})
			}
			if err != nil {
				return SubmitInputResult{}, err
			}
			if s.bcast != nil {
				s.bcast.PublishState(runID, status)
			}
			s.notify(runID, status) // cancelled → origin restore; failed (reject) → origin restore
			// PRD #46 Decision 2: a server-side plan REJECT commits the run to 'failed',
			// a judged status — enqueue a judge on it. A server-side CANCEL commits
			// 'cancelled', which the enqueue gate filters out. Best-effort, gated inside.
			s.maybeEnqueueJudgeByID(ctx, runID)
			// PRD #634 follow-up: a server-side cancel/reject on a scope-directed run commits
			// terminal outside SetState, so settle the pending scope audit row here too (it would
			// otherwise stay "active" forever). Best-effort — must never fail the operator's cancel.
			// Guarded on the run carrying a directive so it is a no-op when there is none.
			if run.ScopeCeiling.Valid {
				if _, setErr := s.q.SettleScopeInputDisposition(ctx, store.SettleScopeInputDispositionParams{
					RunID: runID, Disposition: pgText("declined"),
				}); setErr != nil {
					slog.Warn("settle scope input disposition (server-side cancel/reject)", "run", runID, "error", setErr)
				}
			}
			return SubmitInputResult{ServerSide: true}, nil
		}
		// Live poller: the worker will consume this verdict. Enqueue it AND stamp the
		// deliberate-stop signal in one statement (PRD #33 Decision 3) via the
		// dedicated CreateStopVerdictInput CTE, so the signal is never lost
		// independently of the input that requested it. stopKindFor is always non-empty
		// here (kind is cancel/reject_plan). The stamp lands while the run is still
		// non-terminal; the client's terminal-guarded isStoppedRun ignores it until the
		// run reaches failed/cancelled.
		// PRD #503 M3: the shared CTE stamps stop_reason unconditionally, but the reason
		// belongs on a CANCEL only. Pass the operator's optional reason for a cancel;
		// NULL for reject_plan, whose reason lives in failure_reason via the M2 path (the
		// server-side reject branch above) — double-writing would contradict that split.
		var stopReason pgtype.Text // NULL for reject_plan
		if kind == "cancel" {
			stopReason = stopReasonParam(body)
		}
		// Strip NUL from the body co-written to run_user_inputs.body in the SAME INSERT:
		// a NUL in a text column raises Postgres 22021, which would abort the whole CTE and
		// silently drop the cancel/reject verdict (the stop_reason sanitizing above would be
		// moot if this INSERT never lands). NUL is never meaningful in an operator message.
		cleanBody, _ := stripNUL(body)
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID: runID, Kind: kind, Body: pgText(cleanBody), StopKind: pgText(stopKindFor(kind)), StopReason: stopReason,
		}); err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false}, nil
	}

	// A revise_plan (PRD #41) is a plain enqueue like follow_up/approve_plan — no
	// stop signal, no server-side transition — but it is capped: at most
	// PlanMaxRevisions persisted revisions per run. The cap spans ALL revise_plan rows
	// (no consumed_at filter), so a consumed revision still counts toward it.
	//
	// The cap check and the enqueue are ONE statement, and — since #106 — the check is
	// a predicate on runs.revise_count inside the UPDATE that bumps it, not a count of
	// run_user_inputs. That distinction is the fix, not a detail: a lock on `runs` never
	// covered a count of a DIFFERENT table, because READ COMMITTED gives the blocked
	// caller a refreshed view of the LOCKED ROW only, not a refreshed snapshot. Two
	// concurrent submits — e.g. web + Slack on the same single-owner gate racing at
	// N-1 — could both slip past and persist an N+1th row, measured 100/100 with the
	// interleave forced. They now cannot. No row = the cap is already reached. The
	// terminal-run guard above already blocks a revise on a finished run.
	//
	// This branch is the SOLE writer of revise_plan rows, which is what keeps the counter
	// and the rows in step; a second writer added later would defeat the cap silently —
	// rows the counter never sees, with nothing going red.
	//
	// 🔴 THAT INVARIANT IS ONLY PARTLY GUARDED, and the parts are worth naming exactly,
	// because an earlier version of this comment credited a test that cannot see it at
	// all. Measured 2026-07-29:
	//
	//   - a second writer added INSIDE this branch, or replacing the capped call, is
	//     caught by TestSubmitInputRevisePlanEnqueuesPlain (service_test.go) — it asserts
	//     CreateRunInput was not called for a revise_plan;
	//   - a NEW SQL query that inserts a 'revise_plan' literal is caught by
	//     store.TestOnlyOneQueryInsertsRevisePlanRows;
	//   - a writer added ELSEWHERE IN GO, reusing the generic CreateRunInput query, is
	//     caught by NOTHING. Adding one left the whole `go test -count=1 ./...` gate green
	//     (43 packages ok, 0 FAIL). Note 00074's CHECK permits 'revise_plan' through
	//     CreateRunInput, whose kind is a bare parameter, so the only thing between the
	//     two writers is the early return below.
	//
	// If you are adding a revise_plan write anywhere else: bump runs.revise_count in the
	// same statement, or the cap stops meaning anything.
	if kind == "revise_plan" {
		row, err := s.q.CreateRunReviseInputIfUnderCap(ctx, store.CreateRunReviseInputIfUnderCapParams{
			RunID: runID, Body: pgText(body), MaxRevisions: int32(s.p.PlanMaxRevisions),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return SubmitInputResult{}, ErrReviseCapReached
		}
		if err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
	}

	// A plain steering input (follow_up, or a nil-selection approve_plan handled above):
	// enqueue for the worker with no stop signal and no runs-row touch.
	return s.enqueueRunInput(ctx, runID, kind, body)
}

// enqueueRunInput writes a plain worker-bound input row (no stop signal, no runs-row
// touch) and returns the created row (PRD #95 S2) so the handler can surface id +
// created_at for a follow_up's optimistic reconcile. Shared by the follow_up path and the
// nil-selection approve_plan path so both go through one enqueue.
func (s *Service) enqueueRunInput(ctx context.Context, runID uuid.UUID, kind, body string) (SubmitInputResult, error) {
	row, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: kind, Body: pgText(body),
	})
	if err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
}

// submitApproval enqueues an approve_plan carrying an agent selection (PRD #37):
// validate against the run's real roster, then write the run's agent_source /
// agent_exclusions and the worker-bound input body in one statement, so the row can
// never disagree with what the worker was told to use.
//
// The body is the SERVER's canonical encoding of the validated selection, never the
// client's text: the worker parses it back with parseAgentSelection, and a raw
// pass-through would hand an unvalidated string to the process that builds the
// agent map.
func (s *Service) submitApproval(ctx context.Context, run store.Run, sel AgentSelection) (SubmitInputResult, error) {
	roster, err := s.rosterFor(ctx, run, sel.Source, nil)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if err := validateSelection(sel, roster); err != nil {
		return SubmitInputResult{}, err
	}
	// The capability approval gate (capabilityGate) is enforced UPSTREAM in SubmitInput for
	// every approve_plan — both the selection-bearing path that reaches here and the
	// nil-selection plain-enqueue path — so a plan can never be approved onto a worker that
	// cannot run it, whichever path the client used.
	exclusions, err := encodeJSONArray(sel.Exclusions)
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent exclusions: %w", err)
	}
	body, err := json.Marshal(AgentSelection{Source: sel.Source, Exclusions: orEmpty(sel.Exclusions)})
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent selection: %w", err)
	}
	// Issue #260 instrumentation: capture the live milestone freeze state on both sides of
	// the approve-time freeze so a future human-gated dev-cluster run reveals what
	// CreateApprovePlanInput saw. Best-effort: a snapshot read error is logged at Warn and
	// NEVER aborts the approve.
	before, beforeErr := s.q.GetRunMilestoneFreezeSnapshot(ctx, run.ID)
	if beforeErr != nil {
		slog.Warn("workersvc: approve-freeze pre-read failed", "run_id", run.ID, "error", beforeErr)
	}
	if _, err := s.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: run.ID, Body: pgText(string(body)), AgentSource: pgText(sel.Source), AgentExclusions: exclusions,
		// PRD #122 M2 (Decision 5/5b): the budget-scaling config the freeze reads to derive
		// this run's effective budget from its frozen milestone count, atomically with the
		// candidate→frozen copy. IDEMPOTENT via COALESCE — a re-gate resume re-supplies the
		// same config and never changes a budget frozen once.
		RunMaxIterations:         int32(s.p.RunMaxIterations),
		RunTimeoutSeconds:        int32(s.p.RunTimeout.Seconds()),
		MilestoneBudgetCap:       milestoneBudgetCap,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
	}); err != nil {
		return SubmitInputResult{}, err
	}
	after, afterErr := s.q.GetRunMilestoneFreezeSnapshot(ctx, run.ID)
	if afterErr != nil {
		slog.Warn("workersvc: approve-freeze post-read failed", "run_id", run.ID, "error", afterErr)
	}
	// Issue #260: emit ONE structured log capturing what the approve-time freeze saw at the
	// live instant. Approves are human-gated and rare, so this logs unconditionally. The
	// pathological signature is the #260 bug SHAPE specifically — a candidate WAS present at
	// the pre-read yet frozen came out NULL after the freeze — raised to Warn with a stable
	// signature field for alerting. A 0-milestone run correctly freezes NULL from a NULL
	// candidate (see CreateApprovePlanInput's own comment), so it must NOT trip the signature,
	// or every no-milestone approve would drown the real signal; hence the before-candidate
	// guard, not a bare after-frozen-empty test.
	logArgs := []any{
		"run_id", run.ID,
		"before_frozen", string(before.MilestonesFrozen),
		"before_candidate", string(before.MilestonesCandidate),
		"before_updated_at", before.UpdatedAt.Time,
		"after_frozen", string(after.MilestonesFrozen),
		"after_updated_at", after.UpdatedAt.Time,
	}
	if afterErr == nil && beforeErr == nil && len(before.MilestonesCandidate) > 0 && len(after.MilestonesFrozen) == 0 {
		slog.Warn("workersvc: approve-time milestone freeze", append(logArgs, "signature", "approve_froze_null")...)
	} else {
		slog.Info("workersvc: approve-time milestone freeze", logArgs...)
	}
	// Populated AFTER validateSelection accepted the selection: only a valid, accepted
	// guard-role exclusion warrants the owner heads-up (PRD #319 M3).
	return SubmitInputResult{ServerSide: false, ExcludedGuardRoles: excludedGuardRoles(sel)}, nil
}

// capabilityGate is the AUTHORITATIVE, server-side PRD #84 M4 4c approval gate. A run at
// awaiting_approval is owned by exactly one worker (run.WorkerID); if that worker's EFFECTIVE
// capabilities do not satisfy the run's plan-time-inferred (and M2 repo-hinted)
// required_capabilities, the approve is BLOCKED (a *CapabilityUnmetError → 409) so a plan can
// never be approved onto a worker that cannot run it. The effective-caps fold —
// worker.capabilities ∪ {docker if docker_enabled} — is the SAME one fn_worker_can_claim
// (migration 00142) and CountOnlineWorkersSatisfyingCaps apply, so approve and claim never
// disagree. Gated by the capability-aware kill-switch (default ON), identically to the
// claim/health paths: with the flag OFF the fleet claims best-effort, so there is no
// eligibility to enforce and this stays silent. Called from submitInput for every approve_plan
// (both the selection and nil-selection paths) so the gate has no bypass — EXCEPT the owner
// override path (SubmitInputWithCapabilityOverride), which skips this gate deliberately and
// clears required_capabilities AFTER a successful approve (OverrideRunRequiredCapabilities).
func (s *Service) capabilityGate(ctx context.Context, run store.Run) error {
	if len(run.RequiredCapabilities) == 0 || !s.capabilityAwareOn(ctx) {
		return nil
	}
	effective, err := s.effectiveOwningWorkerCaps(ctx, run)
	if err != nil {
		return err
	}
	if unmet := capability.Unmet(run.RequiredCapabilities, effective); len(unmet) > 0 {
		return &CapabilityUnmetError{Unmet: unmet}
	}
	return nil
}

// effectiveOwningWorkerCaps folds the run's OWNING worker's effective capability set the
// SAME way fn_worker_can_claim and CountOnlineWorkersSatisfyingCaps do — via the shared
// capability.EffectiveWorkerCaps (the Go mirror of SQL fn_effective_worker_caps, single
// source since #512 M5) — the worker's stored capabilities plus `docker` when
// docker_enabled — so the PRD #84 M4 4c
// approval gate and the claim gate evaluate the identical set and can never disagree. A run
// with no owning worker (no awaiting_approval run reaches submitApproval without one) folds
// to the empty set, which fails CLOSED: every required capability is then unmet, matching
// the claim path's fail direction. A GetWorkerByID error (including the worker having been
// deleted) propagates as an error rather than silently opening the gate.
func (s *Service) effectiveOwningWorkerCaps(ctx context.Context, run store.Run) ([]string, error) {
	if !run.WorkerID.Valid {
		return nil, nil
	}
	wkr, err := s.q.GetWorkerByID(ctx, uuid.UUID(run.WorkerID.Bytes))
	if err != nil {
		return nil, err
	}
	return capability.EffectiveWorkerCaps(wkr.Capabilities, wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool), nil
}

// OverrideRunRequiredCapabilities backs the PRD #84 M4 4c user override ("run without the
// capability", Decision 12): it clears the run's inferred/hinted required_capabilities. The
// clear is owner- AND awaiting_approval-scoped in SQL, so a non-owner runID or a run outside
// the plan gate is a silent no-op (0 rows). It is called from submitInput's override path
// AFTER the approve has validated and enqueued (the gate is bypassed for that path), so the
// clear runs ONLY on a successful approve — a failed approve leaves the requirement intact
// and the retry stays gated. v1 clears the WHOLE set (repo hint + inferred); a
// hint-vs-inference split is a future refinement (Decision 6/12), and no runtime security
// boundary is bypassed — the §300 guardrail still denies docker USE on a daemon-less worker
// at run time.
func (s *Service) OverrideRunRequiredCapabilities(ctx context.Context, userID, runID uuid.UUID) error {
	_, err := s.q.ClearRunRequiredCapabilities(ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: userID})
	return err
}

// AnswerBody is the wire shape of an `answer` steering input (PRD #88 M1). It is
// JSON rather than the bare prose every other kind carries, because an answer must
// name the QUESTION it answers — `approve_plan` already establishes the JSON-body
// idiom, so this is in keeping rather than novel.
//
// One shape, every surface: the web and CLI construct it directly, and the Slack
// replier (whose inbound is free text) resolves the thread anchor to the open
// question id and constructs the same JSON server-side. There is deliberately not a
// second, text-only contract for Slack.
type AnswerBody struct {
	// QuestionID names the question this answers. Compared for equality against the
	// run's open_question_id; never parsed for meaning.
	QuestionID string `json:"question_id"`
	// Answers is index-aligned with the question payload's `questions` array. Free
	// text is always allowed, including where the question offered options.
	Answers []string `json:"answers"`
}

// submitAnswer records an answer to the clarification question a run is parked on
// (PRD #88 M1). Four things happen here that do not happen for any other kind, and
// each closes a failure the others do not:
//
//  1. The run must actually be parked. See the caller's comment.
//  2. A malformed body is REJECTED, deliberately unlike parseAgentSelection's
//     fallback-to-a-safe-default. There, a default is genuinely safe (the run's own
//     agents). Here the whole point of the payload is to identify what is being
//     answered, so accepting an unidentifiable answer IS the harm — it would resolve
//     whatever question happens to be open.
//  3. The named question must be the one currently open. This is the stale-answer
//     guard, and it is keyed on identity precisely because it must survive a requeue:
//     a worker death re-queues and re-parks the run, so any clock- or arrival-ordinal
//     key would reject an answer the user correctly submitted before the death.
//  4. The text is scrubbed and bounded (D-G). #88 is the feature that makes the agent
//     ASK the user for information, which is exactly the prompt that elicits a
//     credential paste — and the question text itself is attacker-influenceable, so an
//     injected repo file can make the lead ask for a PAT "to continue". Slack's
//     inbound path already scrubbed; web and CLI did not. Doing it here means every
//     surface inherits it.
func (s *Service) submitAnswer(ctx context.Context, run store.Run, body string) (SubmitInputResult, error) {
	if run.Status != "awaiting_input" {
		return SubmitInputResult{}, ErrRunNotAwaitingInput
	}
	// NOTE on calling this "belt-and-braces": it is only redundant with the identity
	// check below BECAUSE the worker clears its open-question id when a park settles
	// (RunRunner.askUser's settle) and SetRunRunning clears the column. If either
	// stopped, a run could sit non-parked while still naming a question, and this
	// status check would become the only thing rejecting an answer to it.
	var ab AnswerBody
	if err := json.Unmarshal([]byte(body), &ab); err != nil {
		return SubmitInputResult{}, fmt.Errorf("%w: body must be JSON {question_id, answers}", ErrInvalidAnswer)
	}
	qid := strings.TrimSpace(ab.QuestionID)
	if qid == "" {
		return SubmitInputResult{}, fmt.Errorf("%w: question_id is required", ErrInvalidAnswer)
	}
	// A run parked with no open question id cannot be resumed by any answer
	// (SetRunRunning's guard is unsatisfiable), so an equality test against "" must
	// never pass. SetState rejects an empty id at the park, making this unreachable —
	// it is here because "unreachable" is a claim about another function.
	if !run.OpenQuestionID.Valid || run.OpenQuestionID.String == "" || qid != run.OpenQuestionID.String {
		return SubmitInputResult{}, ErrStaleAnswer
	}
	if len(ab.Answers) > maxAnswerCount {
		return SubmitInputResult{}, fmt.Errorf("%w: at most %d answers", ErrInvalidAnswer, maxAnswerCount)
	}
	answers := make([]string, 0, len(ab.Answers))
	for _, a := range ab.Answers {
		clean, _ := stripNUL(a)
		answers = append(answers, truncateRunes(secretscrub.Scrub(clean), maxAnswerBodyRunes))
	}
	// Re-encode from the server's own validated values rather than storing the
	// caller's raw text, the same rule submitApproval follows: what the worker reads
	// back is what the server checked, not what the client sent.
	encoded, err := json.Marshal(AnswerBody{QuestionID: qid, Answers: answers})
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode answer: %w", err)
	}
	row, err := s.q.CreateRunAnswerInput(ctx, store.CreateRunAnswerInputParams{
		RunID: run.ID, Body: pgText(string(encoded)), QuestionID: pgText(qid),
	})
	if err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false, ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
}

// orEmpty makes a nil slice marshal as `[]`, never `null` — the worker's
// parseAgentSelection accepts both, but the persisted body is also what a human
// reads in the inputs table.
func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// stopKindFor maps a deliberate-stop steering kind to the stop signal stamped on the
// run (PRD #33): a cancel verdict is 'cancelled', a plan reject is 'plan_rejected', a
// graceful interactive wind-down (PRD #517 M4) is 'stopped'. Only cancel/reject_plan/
// stop reach it (the stop-verdict branches of SubmitInput); the server owns this mapping
// so the signal never depends on the reason string the worker later reports.
// clampInt clamps v into [lo, hi]. Callers guarantee lo <= hi (PRD #634 M2 passes
// len(completed) <= len(frozen)); if not, hi wins (the upper bound is returned).
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// submitScopeCeiling writes runs.scope_ceiling + the kind='scope' audit row in one
// statement (PRD #634 M2) and returns the resolved ceiling so m5's CLI can report the
// clamped value. The ceiling is written as a REAL value even when 0 (complete nothing
// further) — pgInt4 nulls a 0 (which would read as "unbounded"), so the Int4 is built
// directly here. The audit body is NUL-stripped to avoid Postgres 22021 aborting the CTE;
// disposition is left NULL (the worker settles it in m4). No stop_kind is stamped.
func (s *Service) submitScopeCeiling(ctx context.Context, run store.Run, ceiling int, auditBody string) (SubmitInputResult, error) {
	cleanBody, _ := stripNUL(auditBody)
	if _, err := s.q.CreateScopeCeilingInput(ctx, store.CreateScopeCeilingInputParams{
		RunID:        run.ID,
		ScopeCeiling: pgtype.Int4{Int32: int32(ceiling), Valid: true},
		Body:         pgText(cleanBody),
	}); err != nil {
		return SubmitInputResult{}, err
	}
	c := ceiling
	return SubmitInputResult{ServerSide: false, ScopeCeiling: &c}, nil
}

func stopKindFor(kind string) string {
	switch kind {
	case "cancel":
		return "cancelled"
	case "reject_plan":
		return "plan_rejected"
	case "stop":
		return "stopped"
	default:
		return ""
	}
}

// hasLivePoller reports whether a worker is currently polling this run's inputs:
// the run must be assigned to a worker whose heartbeat is fresh. A queued run
// (no worker) or a stale/absent worker means no live poller.
func (s *Service) hasLivePoller(ctx context.Context, run store.Run) (bool, error) {
	// PRD #35: a PARKED run has no poller either, and this check is POSITIVE — which
	// is why it is one of the two sites in the whole PRD that genuinely needed
	// editing, while every negative guard elsewhere covered limit_wait for free.
	// A parked run keeps its worker_id (affinity) and that worker keeps heartbeating
	// for its OTHER runs, so both conditions above are false and the cancel would be
	// enqueued for a poller that is not polling this run and never will again. It
	// would then sit unconsumed until the promotion pass, i.e. potentially for days.
	//
	// PRD #754 M4: pool_wait is HELD for the identical reason and so rides the same arm.
	// A held run keeps its worker_id for affinity and its worker keeps heartbeating for
	// other runs, but it is never polling this held run, so a cancel routed to a poller
	// would sit unconsumed (worse than limit_wait — there is no promotion pass in M4).
	// It must go server-side, so it must read as "no live poller" here too.
	if run.Status == "queued" || run.Status == "limit_wait" || run.Status == "pool_wait" || !run.WorkerID.Valid {
		return false, nil
	}
	wkr, err := s.q.GetWorkerByID(ctx, uuid.UUID(run.WorkerID.Bytes))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !wkr.LastHeartbeatAt.Valid {
		return false, nil
	}
	return s.now().Sub(wkr.LastHeartbeatAt.Time) < s.p.WorkerHeartbeatStale, nil
}

// -------------------------------------------------------------------------
// Sweeper
// -------------------------------------------------------------------------

// SweepResult reports the row counts touched by one Sweep pass (for logging).
type SweepResult struct {
	WorkersOffline     int64
	ClaimedReset       int64
	RunningTimeout     int64
	StaleFailed        int64
	StaleRequeued      int64
	ChatIdleCompleted  int64
	ProposalsRecovered int64
	// HealthChanged is the number of runs whose health flag the detector wrote this
	// pass (PRD #47) — raised, changed, or self-cleared. Observability only.
	HealthChanged int64
	// AutoStopped is the number of runs this pass stopped, or requested a stop for,
	// because their message writes are in a confirmed permanent-failure loop
	// (PRD #108 M5). Normally 0 — the candidate set is usually empty.
	AutoStopped int64
	// LimitPromoted is the number of runs this pass brought back from limit_wait to
	// queued because their retry_not_before elapsed (PRD #35). Normally 0: the
	// partial index this reads covers only parked runs, a set that is empty on a
	// healthy instance.
	LimitPromoted int64
	// PoolResumed is the number of runs this pass reactively resumed from pool_wait to
	// queued because their owner's token pool became non-empty (PRD #754 M5). At most
	// ONE per distinct held-run owner per tick (the anti-stampede stagger), so on a
	// busy resume it climbs one owner at a time across ticks. Normally 0.
	PoolResumed int64
}

// Sweep enforces the liveness rules the workers cannot: stale workers go offline
// and their non-terminal runs are re-queued (or failed past the re-queue cap);
// claimed-but-never-started runs are reclaimed; runs past RUN_TIMEOUT are failed.
// It is called on a ticker and once immediately at boot (the orphan sweep).
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	now := s.now()
	staleCutoff := pgTime(now.Add(-s.p.WorkerHeartbeatStale))
	claimCutoff := pgTime(now.Add(-s.p.ClaimGrace))
	max := int32(s.p.RunMaxRequeues)

	var res SweepResult
	var err error

	if res.WorkersOffline, err = s.q.MarkStaleWorkersOffline(ctx, staleCutoff); err != nil {
		return res, fmt.Errorf("mark stale workers offline: %w", err)
	}

	claimed, err := s.q.SweepClaimedNeverStarted(ctx, claimCutoff)
	if err != nil {
		return res, fmt.Errorf("sweep claimed-never-started: %w", err)
	}
	res.ClaimedReset = int64(len(claimed))
	for _, r := range claimed {
		s.publishSwept(r.ID, r.Status)
		// A fresh attempt starts with no evidence against it (PRD #108 M5). See the
		// requeue loop below for the argument; this reset is the same event.
		s.persistFail.evict(r.ID)
	}

	// PRD #122 M2 (Decision 5b): a PER-RUN cutoff now — the sweep honours each run's
	// persisted budget_wall_seconds, falling back to the global RUN_TIMEOUT for a
	// NULL-budget run, so a scaled run is not failed at the global 2h.
	timedOut, err := s.q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
		FailureReason:        pgText("run exceeded RUN_TIMEOUT"),
		Now:                  pgTime(now),
		GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	})
	if err != nil {
		return res, fmt.Errorf("sweep running-timeout: %w", err)
	}
	res.RunningTimeout = int64(len(timedOut))
	for _, r := range timedOut {
		s.publishSwept(r.ID, r.Status)
		// PRD #46 Decision 2: a swept-to-failed run (timed out) is committed-terminal
		// and worth judging. Best-effort, gated (kind/toggles/token) inside.
		s.maybeEnqueueJudgeByID(ctx, r.ID)
	}

	// Fail-over-cap before re-queue: the two are disjoint on requeue_count, but
	// failing first keeps a run that just hit the cap from being re-queued.
	failed, err := s.q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgText("worker lost; exceeded re-queue budget"),
		MaxRequeues:   max,
		Cutoff:        staleCutoff,
	})
	if err != nil {
		return res, fmt.Errorf("fail stale-worker runs over cap: %w", err)
	}
	res.StaleFailed = int64(len(failed))
	for _, r := range failed {
		s.publishSwept(r.ID, r.Status)
		// PRD #46 Decision 2: a swept-to-failed run (worker lost, over re-queue budget)
		// is committed-terminal and worth judging. Best-effort, gated inside.
		s.maybeEnqueueJudgeByID(ctx, r.ID)
	}

	requeued, err := s.q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: max,
		Cutoff:      staleCutoff,
	})
	if err != nil {
		return res, fmt.Errorf("re-queue stale-worker runs: %w", err)
	}
	res.StaleRequeued = int64(len(requeued))
	for _, r := range requeued {
		s.publishSwept(r.ID, r.Status)
		// 🔴 A REQUEUE GRANTS A FRESH ATTEMPT, SO IT MUST CLEAR THE DEAD ATTEMPT'S
		// EVIDENCE (PRD #108 M5). This query writes status='queued' but KEEPS
		// worker_id for affinity, so without this the run returns to `running` under a
		// new attempt still carrying the old one's 20-failure streak and is
		// auto-stopped before the new worker persists a byte — uzi killing a run one
		// tick after deciding it deserved another try and spending re-queue budget to
		// say so. Likely rather than theoretical for the population M5 exists to
		// protect: a pre-0.10.1 worker's retry batch GROWS, so a worker wedged at 2 Hz
		// is a prime OOM candidate, and OOM is exactly what puts it here.
		//
		// The window is wide, and uzi's own configuration is the calibration:
		// defaultClaimGrace budgets FIVE MINUTES for claimed→started, while the sweeper
		// gives this 15 seconds. The whole of the new attempt's checkout sits inside it
		// — ensureClone branches on isBareRepo, so a fresh container from a NEW image
		// has an empty cache and takes the cold cloneBare path, and that clone runs
		// between the worker's reportState({status:"running"}) and its first flush
		// (runner.ts; batcher.emit only buffers and then waits for a tick). The claim
		// is about that ORDERING, not a stopwatched duration.
		s.persistFail.evict(r.ID)
	}

	// Chat idle backstop (PRD #39 Decision 3): a chat run whose last message is
	// older than ChatIdleTimeout is completed even though its worker is alive (so no
	// stale-worker sweep above fired for it). Disabled when ChatIdleTimeout is 0.
	if s.p.ChatIdleTimeout > 0 {
		idleChats, err := s.q.SweepIdleChatRuns(ctx, pgTime(now.Add(-s.p.ChatIdleTimeout)))
		if err != nil {
			return res, fmt.Errorf("sweep idle chat runs: %w", err)
		}
		res.ChatIdleCompleted = int64(len(idleChats))
		for _, r := range idleChats {
			s.publishSwept(r.ID, r.Status)
		}
	}

	// Recover issue proposals stranded in 'confirming' by a confirm handler killed
	// mid-flight (M3): revert them to pending so the user retries/dismisses. Disabled
	// when ProposalConfirmStuckTimeout is 0. No broadcast — proposals have no live
	// channel; the browser re-reads on its next proposal fetch.
	if s.p.ProposalConfirmStuckTimeout > 0 {
		recovered, err := s.q.SweepStuckConfirmingProposals(ctx, pgTime(now.Add(-s.p.ProposalConfirmStuckTimeout)))
		if err != nil {
			return res, fmt.Errorf("sweep stuck confirming proposals: %w", err)
		}
		res.ProposalsRecovered = int64(len(recovered))
	}

	// Usage-limit promotion (PRD #35 M2): limit_wait → queued once retry_not_before
	// has elapsed. Placed here — after the status transitions, before the
	// prune/detector/auto-stop observability block — because Sweep's shape is
	// "transitions first, enforcement second", and because a run promoted before the
	// detector runs is health-visible in THIS tick rather than the next. Ordering is
	// otherwise free: every other pass is disjoint from limit_wait as a source and
	// from queued as a target.
	//
	// No persistFail.evict here, unlike the stale-worker requeue above. autoStopWedgedRuns
	// already evicts on `run.Status != "running"`, which a parked run satisfied for the
	// whole park, so the streak is long gone by the time this fires.
	promoted, err := s.q.PromoteLimitWaitRuns(ctx, pgTime(now))
	if err != nil {
		return res, fmt.Errorf("promote limit-wait runs: %w", err)
	}
	res.LimitPromoted = int64(len(promoted))
	for _, r := range promoted {
		// Same fan-out as every other sweep transition: the broadcaster tells live
		// browsers, and notify moves the board card to In Progress for "queued" —
		// identical to a requeue, which is exactly what a resume looks like from the
		// board's point of view.
		s.publishSwept(r.ID, r.Status)
	}

	// Reactive pool resume (PRD #754 M5): a pool_wait hold is released the moment its
	// owner's Anthropic token pool becomes non-empty again. Scoped to pool_wait ONLY —
	// never folded into PromoteLimitWaitRuns, whose clock-based predicate is a different
	// hold. Placed here for the same reason the limit promote is: transitions first, a
	// run resumed before the detector runs is health-visible in THIS tick.
	if res.PoolResumed, err = s.resumePoolWaitRuns(ctx); err != nil {
		return res, fmt.Errorf("resume pool-wait runs: %w", err)
	}

	// Bound the in-process persistence-failure tracker (PRD #108 M4). This is the
	// memory bound for the one case no other eviction path reaches: a run whose
	// worker vanished without the run ever reaching terminal. Pruned BEFORE the
	// detector so a flag is never raised off an entry this tick was going to expire.
	s.persistFail.prune(now)

	// Run-health detector (PRD #47): flag/clear slow, stalled, looping, stuck-queued,
	// and approval-idle runs from telemetry already in Postgres. Best-effort and
	// non-terminal — it never kills a run and never fails the sweep (it logs and
	// returns a count); a nil settings (tests) disables it entirely.
	res.HealthChanged = s.detectRunHealth(ctx, now)

	// Auto-stop confirmed per-run persistence loops (PRD #108 M5). Deliberately NOT
	// inside detectRunHealth — Decision 8 (it must not ride health_enabled), and
	// because ListActiveRunsForHealth excludes chat runs, which wedge identically.
	// Runs AFTER the detector so the flag always lands first ("health first, kill
	// second"); its own thresholds sit above the flag's, so that ordering is
	// belt-and-braces rather than the mechanism.
	res.AutoStopped = s.autoStopWedgedRuns(ctx, now)
	return res, nil
}

// resumePoolWaitRuns is the reactive half of the pool_wait lifecycle (PRD #754 M5): it
// promotes held runs back to 'queued' once their owner's Anthropic token pool is
// non-empty again. Returns the number promoted this tick and fans each out through
// publishSwept, exactly like the limit promote above.
//
// 🔴 AT MOST ONE HELD RUN PER OWNER PER TICK. The live case had three runs held on one
// user's single token; promoting all of them the instant a token pools would thundering-
// herd that one credential — every resumed run would re-claim, and all but one would find
// the pool empty again and re-hold, a churn the hold exists to avoid. So the pass promotes
// only the OLDEST held run for each user with a now-non-empty pool, and lets the next tick
// (~15s later) take the next one once the first has actually claimed a token. ListPoolWaitRuns
// returns oldest-first, so the FIRST run seen for a user is the one to promote.
//
// The candidate query is issued at most once per distinct held-run owner per tick (users
// are deduped as the list is walked), bounding its cost regardless of how many runs a user
// holds. A per-user candidate-query error is logged and skipped — one user's read fault must
// not fail the whole sweep — mirroring how the sweep treats its other best-effort sub-steps.
// A ListPoolWaitRuns error, by contrast, is returned to fail the pass, matching the limit
// promote's own read.
func (s *Service) resumePoolWaitRuns(ctx context.Context) (int64, error) {
	held, err := s.q.ListPoolWaitRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pool-wait runs: %w", err)
	}
	// seen records users already handled this tick: the first (oldest) held run for a user
	// is the one considered, and no user's candidate pool is read twice.
	seen := make(map[uuid.UUID]bool, len(held))
	var resumed int64
	for _, r := range held {
		if seen[r.UserID] {
			continue
		}
		seen[r.UserID] = true

		rows, err := s.q.ListAutoSelectCandidates(ctx, r.UserID)
		if err != nil {
			// Best-effort: skip this user, keep sweeping the rest. The run stays held and
			// the next tick retries it.
			slog.Error("sweeper: pool-resume candidate read failed", "user", r.UserID, "error", err)
			continue
		}
		// The pool is "non-empty / resumable" iff at least one candidate is AutoEligible.
		// This is Select's PoolNonEmpty (membership counted with NO exclude), whereas the
		// re-claim decides with autoselect.Floor(cands, claimExclude(run)) — Floor.ok, counted
		// AFTER the run's dead-credential exclude. They can only diverge when a run's SOLE
		// AutoEligible token is its own still-excluded dead credential, i.e. claimExclude
		// returns non-Nil. But claimExclude excludes only while retry_not_before is in the
		// FUTURE, and a pool_wait run can never carry a future stamp: SetRunPoolWait does not
		// set retry_not_before, and the claim it came from was itself claimable, so the run's
		// stamp was already NULL (never parked) or in the past (promoted out of limit_wait at
		// retry_not_before <= now). So claimExclude relaxes to Nil at every real resume, making
		// PoolNonEmpty here exactly Floor.ok at re-claim — this trigger never resumes a run that
		// would immediately re-hold.
		poolNonEmpty := false
		for _, row := range rows {
			if autoselectrow.FromCandidateRow(row).AutoEligible {
				poolNonEmpty = true
				break
			}
		}
		if !poolNonEmpty {
			continue
		}
		promoted, err := s.q.PromotePoolWaitRun(ctx, store.PromotePoolWaitRunParams{ID: r.ID, UserID: r.UserID})
		if err != nil {
			// Same best-effort stance: a single promote fault does not sink the sweep.
			slog.Error("sweeper: pool-resume promote failed", "run", r.ID, "user", r.UserID, "error", err)
			continue
		}
		if promoted == 0 {
			// The run moved out of pool_wait between the list and the promote (e.g. a
			// concurrent cancel). Nothing to resume; do not broadcast a transition that
			// did not happen.
			continue
		}
		resumed++
		// Same fan-out as the limit promote: broadcast the queued transition so live
		// browsers and the board's In-Progress column follow the resume.
		s.publishSwept(r.ID, "queued")
	}
	return resumed, nil
}

// publishSwept fans a sweeper-driven run transition out to the same seams a
// worker-reported transition uses: the live WS hub (PublishState) and, once the
// Slack notifier is wired behind the fan-out, the per-owner DM. Before PRD #25 M3
// these bulk transitions returned counts only and never reached the Broadcaster,
// so timeout/worker-loss failures — exactly the "failed" events a user most wants
// pushed — were silently missed. Best-effort and non-blocking (the Broadcaster
// contract), so a slow consumer never delays the sweep.
func (s *Service) publishSwept(runID uuid.UUID, status string) {
	if s.bcast != nil {
		s.bcast.PublishState(runID, status)
	}
	s.notify(runID, status)
}

// -------------------------------------------------------------------------
// pgtype helpers
// -------------------------------------------------------------------------

func pgText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// stopReasonParam maps an operator's OPTIONAL cancel reason (PRD #503 M3) onto the nullable
// runs.stop_reason column, sanitizing like sanitizeFailureReason (its failure-class
// sibling): strip NUL — a NUL in a text column raises Postgres 22021 and would abort the
// cancel — then trim and cap the length (the same 2048-rune bound as failure_reason). An
// empty / whitespace-only / NUL-only body stores NULL, never an empty string. Shared by
// both cancel paths (server-side + live) and, since PRD #517 M4, by the graceful `stop`
// path, which carries the operator's OPTIONAL stop reason the same way a cancel does.
func stopReasonParam(body string) pgtype.Text {
	clean, _ := stripNUL(body)
	clean = truncateRunes(strings.TrimSpace(clean), maxFailureReasonRunes)
	return pgText(clean)
}

func textParam(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgText(*s)
}

// maxFailureReasonRunes bounds the worker-reported failure reason before it lands
// in runs.failure_reason. The worker already slices to 512 (reportState), but the
// API does not take its word for the length any more than for the content — this is
// generous headroom over that slice while still bounding a hostile or buggy worker.
const maxFailureReasonRunes = 2048

// sanitizeFailureReason maps the worker's failure reason onto the nullable
// runs.failure_reason column, stripping NUL and capping the length first.
//
// This is the /messages sanitation (M2) applied to its sibling route (PRD #108 A4).
// A NUL in a `text` column raises 22021 exactly as it does in `jsonb`, and this one
// is worse-placed: it rides `failed` — the run's TERMINAL report — so a 22021 there
// 500s, reportState's bounded retries exhaust, and the terminal state never lands,
// leaving the run to the server-side sweeper. The breaker's own permanent-failure
// report travels this exact field, so a poisoned run reporting its own poison could
// fail to record that it failed.
func sanitizeFailureReason(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	clean = truncateRunes(clean, maxFailureReasonRunes)
	return pgText(clean)
}

// PreservedPatchMaxBytes bounds runs.preserved_patch — the agent's branch diff carried
// on a workflow_scope_missing `failed` report (PRD #377 M1). Generous headroom: the
// motivating #188 workflow file was ~112 lines, but a run can touch several workflow
// files, and this is a one-off column read on a single failed card, not an index key.
// The agent already secret-scrubs and size-caps before sending; this is the server-side
// bound that does not take the worker's word for the length.
const PreservedPatchMaxBytes = 1 << 20 // 1 MiB

// clampWirePreservedPatch maps the worker's preserved_patch onto the nullable
// runs.preserved_patch column. nil/empty → NULL. Otherwise the untrusted worker text is
// run through termsafe.SanitizeBounded, which strips NUL and every other control/bidi
// rune (Trojan-Source defense) while SPARING \n and \t so the diff's line structure
// survives, then applies the byte bound. A NUL would raise 22021 on this terminal
// `failed` write exactly as it does for failure_reason (see stripNULParam); the byte cap
// bounds a hostile or buggy worker. A value that sanitizes to empty maps to NULL.
func clampWirePreservedPatch(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean := termsafe.SanitizeBounded(*s, PreservedPatchMaxBytes)
	return pgText(clean)
}

// stripNULParam is textParam with NUL removed, for the OTHER worker-controlled text
// fields on the /state path that reach a plain `text` column: session_id, plan_md,
// branch, mr_web_url (PRD #108 A4b). A NUL in any of them raises 22021 exactly as it
// does in jsonb, 500s the transition, and — on awaiting_approval/completed/failed —
// the run's new state never lands. M2 sanitized /messages; failure_reason got A4;
// these are the rest of the class on /state.
//
// Deliberately NO length cap, unlike sanitizeFailureReason and run_usage's
// composite-PK keys: plan_md is model prose that is legitimately long, and none of
// these columns is an index key (00020_workers_runs.sql; no index references
// session_id or branch), so a cap would be lossy data loss for no storability gain.
// A NUL-only value strips to "", which pgText maps to NULL — for session_id that is
// its documented "no change" sentinel, which is the right outcome for garbage input.
func stripNULParam(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	clean, _ := stripNUL(*s)
	return pgText(clean)
}

func int8Param(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// pgIntPtr maps an optional int (a nil pointer = "not reported") onto a nullable
// int4 column: nil → NULL, else the value. Used for the worker's advertised
// max_concurrent_runs (PRD #42).
func pgIntPtr(v *int) pgtype.Int4 {
	if v == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*v), Valid: true}
}

// pgFloat4Ptr maps an optional float (nil = "not reported") onto a nullable real
// column: nil → NULL, else the value. Used for the worker's stats_cpu_pct (PRD #49),
// which the worker omits on its first tick.
func pgFloat4Ptr(v *float64) pgtype.Float4 {
	if v == nil {
		return pgtype.Float4{}
	}
	return pgtype.Float4{Float32: float32(*v), Valid: true}
}

// pgUUID wraps a uuid you KNOW IS PRESENT as a valid pgtype.UUID. It sets Valid=true
// unconditionally — including for uuid.Nil, which it turns into the REAL all-zero uuid,
// not SQL NULL.
//
// That is correct for its contract and must not be "fixed" to auto-NULL uuid.Nil: at the
// ~45 call sites that legitimately assume presence, auto-NULLing would convert a loud FK
// violation into a silent NULL write.
//
// The trap is passing uuid.Nil as an "absent" sentinel to a sqlc.narg parameter. The
// query's `IS NULL` escape hatch then never fires and the filter matches nothing, so the
// endpoint SILENTLY RETURNS NOTHING rather than erroring (PRD #98 M1 hit exactly this; it
// took a live-DB test to surface, since a fake store cannot show it). For a genuinely
// optional id use the house idiom instead: *uuid.UUID with an explicit nil guard
// (ListRunsForUser, above), or leave the zero pgtype.UUID (Valid:false → NULL) and call
// this only on the present branch. workersvc.nullableUUID does the latter for the judge
// backlog's ?run= anchor.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// coalesceInt returns the persisted int4 when present and def otherwise (PRD #122
// M2): a NULL budget column means "use the global default", so a 0/1-milestone run
// serves the same caps as a pre-feature run.
func coalesceInt(v pgtype.Int4, def int) int {
	if !v.Valid {
		return def
	}
	return int(v.Int32)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// uniqueViolationOn reports whether err is a Postgres 23505 raised on the named constraint.
func uniqueViolationOn(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
