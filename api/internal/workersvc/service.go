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
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselectrow"
	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/pushbroker"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretopen"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretscrub"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/toolprofile"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
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
	ErrNoPRDLink     = errors.New("issue has no PRD link")
	// ErrNotPRDIssue rejects a run on an issue that does not carry the configured
	// PRD label (PRD #102 Decision 14) → 422.
	//
	// Before M6 every cached issue carried it by construction — the sync filter was
	// the gate — so the PRD-LINK check above was sufficient on its own. The additive
	// non-PRD fetch ends that: a stranger's issue that happens to mention a
	// prds/*.md path would otherwise satisfy HasPrdLink and become runnable by
	// accident, from the board or unattended from autopilot.
	ErrNotPRDIssue         = errors.New("issue does not carry the PRD label")
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
	ErrWorkerNotFound   = errors.New("worker not found")
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
)

// Agent-memory caps (PRD #90, OQ-C). Server-enforced (not client-trusted) and the
// single Go source of truth the SDK tool schema mirrors: at most
// MemoryMaxTitleBytes/MemoryMaxBodyBytes per entry, MemoryMaxPerRun writes per run
// (spam bound), and MemoryMaxPerUserRepo entries per (user,repo) with the oldest
// evicted on insert.
const (
	MemoryMaxTitleBytes  = 200
	MemoryMaxBodyBytes   = 2048
	MemoryMaxPerRun      = 5
	MemoryMaxPerUserRepo = 20
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
	CountWorkerNonTerminalRuns(ctx context.Context, arg store.CountWorkerNonTerminalRunsParams) (int64, error)
	MarkStaleWorkersOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)

	// Runs.
	CreateRun(ctx context.Context, arg store.CreateRunParams) (store.Run, error)
	// CI-fix runs (PRD #6).
	CreateCIFixRun(ctx context.Context, arg store.CreateCIFixRunParams) (store.Run, error)
	// Self-improvement runs (PRD #46 Decision 10).
	CreateSelfImproveRun(ctx context.Context, arg store.CreateSelfImproveRunParams) (store.Run, error)
	// Scheduled prompt runs (PRD #241).
	CreatePromptRun(ctx context.Context, arg store.CreatePromptRunParams) (store.Run, error)
	CountActiveRunsWithBranch(ctx context.Context, arg store.CountActiveRunsWithBranchParams) (int64, error)
	CountActiveCIFixForRef(ctx context.Context, arg store.CountActiveCIFixForRefParams) (int64, error)
	GetRunByIDForUser(ctx context.Context, arg store.GetRunByIDForUserParams) (store.Run, error)
	GetRunByID(ctx context.Context, id uuid.UUID) (store.Run, error)
	ListRunsForUser(ctx context.Context, arg store.ListRunsForUserParams) ([]store.ListRunsForUserRow, error)
	ListActiveRunsAll(ctx context.Context) ([]store.ListActiveRunsAllRow, error)
	ListAllWorkers(ctx context.Context) ([]store.ListAllWorkersRow, error)
	GetRunOwnedByWorker(ctx context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error)
	ClaimRun(ctx context.Context, arg store.ClaimRunParams) (store.Run, error)
	GetRunClaimContext(ctx context.Context, runID uuid.UUID) (store.GetRunClaimContextRow, error)
	// Run judge (PRD #46 M3): terminal-funnel enqueue, judge-run-scoped trace/review
	// authz, the command-not-found scan input, and the review upsert.
	GetUserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	CreateJudgeRun(ctx context.Context, arg store.CreateJudgeRunParams) (store.Run, error)
	GetActiveJudgeRunForWorkerTarget(ctx context.Context, arg store.GetActiveJudgeRunForWorkerTargetParams) (store.Run, error)
	ListToolTraceForRun(ctx context.Context, arg store.ListToolTraceForRunParams) ([]store.ListToolTraceForRunRow, error)
	// ListKnownImproveUziTargetsForUser is the owner's existing improve_uzi target menu
	// carried on a judge claim (issue #232): frequency-ranked, canonical-deduped, capped,
	// so the judge reuses an exact coordinate instead of inventing new phrasing.
	ListKnownImproveUziTargetsForUser(ctx context.Context, arg store.ListKnownImproveUziTargetsForUserParams) ([]string, error)
	ListRunInputsForRun(ctx context.Context, arg store.ListRunInputsForRunParams) ([]store.RunUserInput, error)
	UpsertRunReviewWithRecommendations(ctx context.Context, arg store.UpsertRunReviewWithRecommendationsParams) (uuid.UUID, error)
	// Judge review read side (PRD #46 M4): the run-page verdict + recommendations panel.
	GetRunReviewForTarget(ctx context.Context, targetRunID uuid.UUID) (store.RunReview, error)
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
	DeleteRecommendationDisposition(ctx context.Context, arg store.DeleteRecommendationDispositionParams) (int64, error)
	ListDispositionsForReview(ctx context.Context, reviewID uuid.UUID) ([]store.RecommendationDisposition, error)
	ListJudgeTriageRowsForUser(ctx context.Context, userID uuid.UUID) ([]store.ListJudgeTriageRowsForUserRow, error)
	// Judge filter-chip counts (PRD #244): the per-category GROUP count over the whole
	// backlog, uncapped, so a chip is exact even when the backlog list truncates.
	CountJudgeGroupsByCategoryForUser(ctx context.Context, userID uuid.UUID) ([]store.CountJudgeGroupsByCategoryForUserRow, error)
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
	ListOwnedRecommendationsForCoords(ctx context.Context, arg store.ListOwnedRecommendationsForCoordsParams) ([]store.ListOwnedRecommendationsForCoordsRow, error)
	// The fan-out write itself: ONE multi-row upsert over the RESOLVED coordinates, so a
	// bulk call is a single round-trip that cannot half-apply (PRD #98 M2, audit NB-A).
	UpsertDispositionsForResolvedCoords(ctx context.Context, arg store.UpsertDispositionsForResolvedCoordsParams) (int64, error)
	SetRunRunning(ctx context.Context, arg store.SetRunRunningParams) (int64, error)
	SetRunAwaitingApproval(ctx context.Context, arg store.SetRunAwaitingApprovalParams) (int64, error)
	// SetRunAwaitingInput parks a run on a clarification question (PRD #88 M1) and
	// stamps the question's identity. It clears health on entry, which is what makes
	// leaving `awaiting_input` out of ListActiveRunsForHealth safe — see the query.
	SetRunAwaitingInput(ctx context.Context, arg store.SetRunAwaitingInputParams) (int64, error)
	SetRunCompleted(ctx context.Context, arg store.SetRunCompletedParams) (int64, error)
	SetRunFailed(ctx context.Context, arg store.SetRunFailedParams) (int64, error)
	// SetRunLimitWait parks a run until the owner's Anthropic usage window reopens
	// (PRD #35); PromoteLimitWaitRuns is the sweeper pass that brings it back. The
	// park's source guard is POSITIVE (status = 'running'), unlike every sibling
	// above, so a re-delivered or out-of-order report is a 0-row no-op rather than a
	// second park.
	SetRunLimitWait(ctx context.Context, arg store.SetRunLimitWaitParams) (int64, error)
	PromoteLimitWaitRuns(ctx context.Context, now pgtype.Timestamptz) ([]store.PromoteLimitWaitRunsRow, error)
	MarkRunFailedByID(ctx context.Context, arg store.MarkRunFailedByIDParams) (int64, error)
	CancelRunServerSide(ctx context.Context, arg store.CancelRunServerSideParams) (int64, error)
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
	// RunHasVerdictSinceGateOpened backs issue #182: an awaiting_approval run whose
	// owner already answered THIS gate reports waiting_worker rather than
	// approval_idle. A per-run lookup like ListRunToolWindow above, and for the same
	// reason — it runs only for runs already past the approval threshold.
	RunHasVerdictSinceGateOpened(ctx context.Context, arg store.RunHasVerdictSinceGateOpenedParams) (bool, error)

	// Messages + inputs.
	InsertRunMessage(ctx context.Context, arg store.InsertRunMessageParams) (int64, error)
	ListRunMessagesAfter(ctx context.Context, arg store.ListRunMessagesAfterParams) ([]store.RunMessage, error)
	// UpsertRunUsage folds a delivered result frame's per-model usage into
	// run_usage (PRD #40 M2), GREATEST-merged so re-delivery never regresses.
	UpsertRunUsage(ctx context.Context, arg store.UpsertRunUsageParams) error
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
	// CreateApprovePlanInput enqueues an approve_plan AND records the run's agent
	// selection atomically (PRD #37).
	CreateApprovePlanInput(ctx context.Context, arg store.CreateApprovePlanInputParams) (store.RunUserInput, error)
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
	// Worker → token binding (PRD #104 M3): label resolution for the mint-time and
	// CLI-facing forms, and the id-keyed rebind itself.
	GetUserSecretIDByLabel(ctx context.Context, arg store.GetUserSecretIDByLabelParams) (uuid.UUID, error)
	SetWorkerAnthropicSecret(ctx context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error)
	// Judge-lane → token binding (PRD #104 M4): read at judge-claim time, written by
	// PUT /api/me/judge.
	GetUserJudgeAnthropicSecret(ctx context.Context, id uuid.UUID) (pgtype.UUID, error)
	SetUserJudgeAnthropicSecret(ctx context.Context, arg store.SetUserJudgeAnthropicSecretParams) (store.User, error)
	GetUserDefaultModel(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
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
	RunTimeout       time.Duration
	RunIdleTimeout   time.Duration
	RunMaxIterations int
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
	JudgeModel(ctx context.Context) (string, error)
	// PRDLabel is the label an issue must carry to be runnable (PRD #102 Decision
	// 14). It is read here rather than passed in by each caller because the gate is
	// shared: the board handler and the poller's autopilot must be answering the same
	// question, and an operator renaming prd_label must move both at once.
	//
	// It rides this interface rather than a new one despite the judge scope of its
	// two siblings, because a deployment that wires settings at all wires *Cache,
	// which serves all three. A nil reader falls back to the compiled-in default and
	// the gate still runs — "settings unavailable" must not mean "unguarded".
	PRDLabel(ctx context.Context) (string, error)
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
}

// SetSettings wires the instance settings reader (PRD #46). Call once at startup,
// before serving. A nil reader (tests, or a deployment that never enabled the judge)
// disables the terminal-funnel enqueue and leaves the judge model unset in claims.
func (s *Service) SetSettings(sr SettingsReader) { s.settings = sr }

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
func (s *Service) Register(ctx context.Context, wkr store.Worker, version, template string, maxConcurrentRuns *int) (store.Worker, error) {
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
	row, err := s.q.RegisterWorker(ctx, store.RegisterWorkerParams{
		Version:           pgText(version),
		TemplateReported:  pgText(template),
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

	// Docker-worker repo allowlist (PRD #89 M-allow): a docker-enabled worker may
	// claim ONLY runs whose repo is on the trusted allowlist. Repo-less JUDGE runs are
	// exempt (the SQL narrows the exemption to kind='judge', so a future repo-less kind
	// fail-closes until deliberately exempted) — safe NOT because "repo-less =
	// content-free" (a judge reasons over an untrusted trace) but because the repo-less
	// executor (agent/src/judge-runner.ts, and the chat lane's chat-executor.ts)
	// carries no daemon-reaching tool, so DOCKER_HOST is inert for it; an agent/
	// regression test pins that. This is the accepted-risk likelihood control for the non-rootless
	// DinD tier: the trigger is repo content, so the gate binds at claim, not at
	// provisioning. Non-docker workers skip it entirely (isDocker=false → the SQL
	// predicate short-circuits), so their behavior is unchanged. Fail-closed: a docker
	// worker with no allowlist reader wired, or an empty allowlist, claims no
	// repo-bearing run. Read STRICTLY — a settings read error leaves the run unclaimed
	// (never claim a repo run when the allowlist can't be confirmed).
	isDocker := wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool
	allowlist := []uuid.UUID{}
	if isDocker && s.dockerAllowlist != nil {
		al, aerr := s.dockerAllowlist.DockerRepoAllowlist(ctx)
		if aerr != nil {
			return nil, aerr
		}
		allowlist = al
	}

	run, err := s.q.ClaimRun(ctx, store.ClaimRunParams{
		WorkerID:            pgUUID(wkr.ID),
		UserID:              wkr.UserID,
		AffinityCutoff:      pgTime(s.now().Add(-s.p.WorkerAffinityGrace)),
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allowlist,
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
//   - a missing/undecryptable credential (or a rejected tool package) is terminal —
//     fail the run with the safe (no-secret-bytes) reason and fire the failed notify;
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
	case errors.Is(err, errCredentialUnavailable) || errors.Is(err, errToolPackagesRejected):
		if _, ferr := s.q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{
			ID:            run.ID,
			FailureReason: pgText(err.Error()),
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

// errVaultLocked marks a claim that cannot open the owner's DEK-sealed Anthropic
// token because their vault locked between the claim gate and the open. It must
// NOT be conflated with errCredentialUnavailable: that path fails the run
// (terminal), whereas a locked vault is transient and the run must go back to
// queued to be retried after the next unlock (PRD #32 success criteria 3 & 5).
var errVaultLocked = errors.New("vault locked during claim")

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

// autoPicked reports whether the SELECTOR named this credential, as opposed to a
// binding or a fallback. It is the precise gate on D14's retry: only a credential
// auto chose gets a second attempt on the owner default, because only auto has an
// alternative the user did not ask for. A pinned worker or a judge binding that will
// not open must still fail terminally — the user named that credential, and silently
// billing a different one is the R4 failure this PRD is otherwise built to avoid.
func (c secretChoice) autoPicked() bool {
	return c.secretID != nil &&
		(c.reason == string(autoselect.ReasonAuto) || c.reason == string(autoselect.ReasonBestOfPool))
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
		return s.autoChoice(ctx, wkr, run.UserID)
	}
	return staticChoice(workerSecretID(wkr), selectReasonPinned), nil
}

// autoChoice runs the selector for an `auto` worker (PRD #111 M4).
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
// A fallback (pool_empty / pool_stale) resolves workerSecretID(wkr), which for an
// auto worker is nil ⇒ the owner's default (D9). It is written as the CALL rather
// than as a literal nil so the rule stated in workerSecretID stays the single source
// of what "this worker's non-auto binding" means — the current answer happens to be
// nil for every auto worker, and the rule is what survives the next mode.
func (s *Service) autoChoice(ctx context.Context, wkr store.Worker, userID uuid.UUID) (secretChoice, error) {
	rows, err := s.q.ListAutoSelectCandidates(ctx, userID)
	if err != nil {
		return secretChoice{}, fmt.Errorf("auto-select candidates: %w", err)
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, row := range rows {
		cands = append(cands, autoselectrow.FromCandidateRow(row))
	}
	out := autoselect.Select(cands, s.p.Autoselect, s.now())
	if !out.Picked {
		// D7: auto never fails a run. An empty or unmeasurable pool resolves the
		// worker's non-auto behaviour and records WHY, so "auto was on and I still got
		// the default" is a fact the run view states rather than one a user infers.
		return secretChoice{secretID: workerSecretID(wkr), reason: string(out.Reason)}, nil
	}
	id := out.SecretID
	// The gauge is a SMALLINT 0..100 and headroom is derived from it by subtraction,
	// so the value is in range by construction and the narrowing cannot truncate.
	// runs.anthropic_headroom_pct carries a CHECK BETWEEN 0 AND 100 as the backstop.
	h := int16(out.Headroom)
	return secretChoice{secretID: &id, reason: string(out.Reason), headroom: &h}, nil
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
		// 🔴 D14. Without this arm, D7's "auto never fails a run" is simply untrue.
		// recoverClaimAssembly maps errCredentialUnavailable to MarkRunFailedByID — a
		// TERMINAL failure — so a token that passes the gauge gate and then will not
		// decrypt (a rotated UZI_SECRET_KEY, a corrupt row, a token deleted between the
		// ranking query and the open) kills a run the owner default would have
		// completed. That is the optimizer newly failing runs that static binding
		// finished, which is the one outcome D7 exists to forbid.
		//
		// Scoped as tightly as it can be, on three axes:
		//   - only a credential the SELECTOR named (choice.autoPicked) — a pinned or
		//     judge binding that will not open still fails, because the user named it;
		//   - only errCredentialUnavailable. NOT errVaultLocked: that path already
		//     requeues the run, which is transient and correct, and retrying it would
		//     convert a wait into a spend on the wrong account;
		//   - exactly ONCE, and the code is safer than the first version of this comment.
		//     That version argued from the ID: the retry target is workerSecretID(wkr),
		//     nil for an auto worker, so it cannot satisfy autoPicked. True, but it rests
		//     on the `auto ⇒ id IS NULL` invariant, which a future third writer of
		//     workers.anthropic_secret_id could break without touching this line. The
		//     STRONGER reason, and the one that holds regardless: the retry sets
		//     reason=open_failed, so autoPicked fails on its REASON conjunct whatever the
		//     id turns out to be. The structure forbids a second round; no counter, and
		//     no dependency on an invariant enforced three files away.
		if !choice.autoPicked() || !errors.Is(err, errCredentialUnavailable) {
			return nil, err
		}
		choice = secretChoice{secretID: workerSecretID(wkr), reason: string(autoselect.ReasonOpenFailed)}
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
	// so the worker falls back to the lead template's model.
	defaultModel, err := s.q.GetUserDefaultModel(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("default model lookup: %w", err)
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

	// PRD #122 M1: replay the FROZEN milestone list on every claim. A malformed column
	// degrades to nil-and-log rather than failing the claim, matching the repo_agents
	// decode on the DTO path — the column is data a prior write left, not an invariant
	// of this claim, and stranding a run over it would be worse than serving no list.
	milestones, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		slog.Error("workersvc: decode run milestones", "run_id", run.ID, "error", err)
		milestones = nil
	}

	return &ClaimPayload{
		RunID:            run.ID.String(),
		Kind:             run.Kind,
		IssueIID:         issueIID,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		Status:           run.Status,
		Pipeline:         pipeline,
		Branch:           textPtr(run.Branch),
		SessionID:        textPtr(run.SessionID),
		LastSeq:          run.LastSeq,
		IterationCount:   run.IterationCount,
		RequeueCount:     run.RequeueCount,
		PlanMd:           textPtr(run.PlanMd),
		AutoApprove:      run.AutoApprove,
		OpenQuestionID:   textPtr(run.OpenQuestionID),
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
		Repo: ClaimRepo{
			ID:            uuid.UUID(run.RepoID.Bytes).String(),
			URL:           rc.RepoWebUrl,
			CloneURL:      rc.RepoWebUrl + ".git",
			DefaultBranch: textPtr(rc.DefaultBranch),
			SkillsEnabled: rc.RepoSkillsEnabled,
			ForgeType:     rc.ForgeType,
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
			MaxIterations:          coalesceInt(run.BudgetMaxIterations, s.p.RunMaxIterations),
			PlanMaxRevisions:       s.p.PlanMaxRevisions,
			QuestionMax:            s.p.QuestionMax,
			QuestionTimeoutSeconds: s.p.QuestionTimeoutSeconds,
			DefaultModel:           textPtr(defaultModel),
			SkillMaxBytes:          s.p.SkillMaxBytes,
			SkillsMaxPerRun:        s.p.SkillsMaxPerRun,
			ToolPackages:           toolPackages,
			RepoDevboxOptIn:        rc.RepoDevboxOptIn,
		},
	}, nil
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
	if err := s.foldRunUsage(ctx, run, msgs); err != nil {
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
// GREATEST merge in UpsertRunUsage makes that idempotent. session_id is sourced
// from the run row (the frame payload carries none); it is ” until the run has
// reported one, which the monotonic merge + latest/MAX-per-model rollup tolerate.
// Malformed/absent usage is skipped (never fails the append); a DB error
// propagates so the append fails and the worker re-delivers.
func (s *Service) foldRunUsage(ctx context.Context, run store.Run, msgs []IncomingMessage) error {
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
		for model, mu := range p.ModelUsage {
			if model == "" {
				continue
			}
			model = truncateRunes(model, maxUsageModelRunes)
			if err := s.q.UpsertRunUsage(ctx, store.UpsertRunUsageParams{
				RunID:               run.ID,
				SessionID:           sessionID,
				Model:               model,
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
// column, the M2 worker client, and multica's protocol); the Go field stays
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
	// RepoAgents is the roster the worker parsed from the clone's .claude/agents/
	// (PRD #37), reported on the first `running` report after checkout. A POINTER to
	// a slice, because the three states differ: absent (nil) = this report says
	// nothing about the roster; `[]` = detection ran and found none; non-empty = the
	// detected agents. Only `running` carries it; it is re-validated below, never
	// trusted from the worker.
	RepoAgents *[]RepoAgent `json:"repo_agents"`
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
	// that set; `[]` = an explicitly empty set; non-empty = the ids. They ride `running`
	// reports only. completed is UNIONED server-side (monotone, dedup); in_progress is
	// OVERWRITTEN wholesale (Decision 3). Every id is membership-checked against the run's
	// FROZEN list and kind-gated here (progressParams, Decision 12/13); a rejected set is
	// DROPPED, never persisted, and never fails the report.
	MilestonesCompleted  *[]string `json:"milestones_completed"`
	MilestonesInProgress *[]string `json:"milestones_in_progress"`
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
		rows, err = s.q.SetRunRunning(ctx, runningParams)
	case "awaiting_approval":
		// PRD #122 M1: the CANDIDATE milestone list rides the pre-approval report.
		// milestonesParam validates + kind-gates it (Decision 12/13) and returns NULL
		// when the list is absent, rejected, or from a non-issue run — the query writes
		// that directly, clearing the candidate (Decision 2: replaced each round).
		rows, err = s.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: stripNULParam(req.PlanMd), SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
			MilestonesCandidate: milestonesParam(owned.Kind, req.Milestones),
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
	case "completed":
		rows, err = s.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch: stripNULParam(req.Branch), MrIid: int8Param(req.MrIID), MrWebUrl: stripNULParam(req.MrWebURL), SessionID: sessionID,
			FixVerdict:  clampWireFixVerdict(req.FixVerdict),
			PrdDonePath: clampWirePRDDonePath(owned, req.PrdDonePath),
			ID:          runID, WorkerID: pgUUID(wkr.ID),
		})
	case "limit_wait":
		rows, err = s.setLimitWait(ctx, owned, wkr, req, sessionID)
	case "failed":
		rows, err = s.q.SetRunFailed(ctx, store.SetRunFailedParams{
			// PRD #35 §7.8: when a `failed` report carries the structured limit fields,
			// the SERVER composes the sentence from its own allowlisted enum and replaces
			// whatever text the worker sent. That is the opt-out path (a run with
			// wait_on_limit=false reports failed directly), and letting the worker compose
			// it would put the enum on the untrusted side of the wire — the criterion "a
			// compromised worker cannot smuggle a non-enum rate_limit_type past the
			// server" would then be false on exactly the path a human reads. When the
			// fields are absent this is nil and every other failure path is untouched.
			FailureReason: limitAwareFailureReason(req),
			SessionID:     sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	default:
		return store.Run{}, false, ErrInvalidState
	}
	if err != nil {
		return store.Run{}, false, err
	}
	// Re-read so the worker sees the authoritative status. Ownership already
	// held above, so 0 rows means the run was terminal (the only other WHERE
	// guard) → not applied.
	run, err = s.runOwnedByWorker(ctx, runID, wkr)
	if err == nil && rows > 0 {
		if s.bcast != nil {
			s.bcast.PublishState(runID, run.Status)
		}
		// Only a genuinely-applied transition drives the column automation; a
		// no-op onto an already-terminal run (rows == 0) must not.
		s.notify(runID, run.Status)
		// PRD #46 Decision 2: enqueue a judge on the COMMITTED terminal transition
		// (rows>0), not the lossy notify seam. Best-effort — never fails the report.
		s.maybeEnqueueJudge(ctx, run)
	}
	return run, rows > 0, err
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
// server-side: oversize title/body → ErrMemoryTooLarge; the per-run write count at
// the cap → ErrMemoryWriteCap; and after the insert the (user,repo) set is trimmed
// to the newest MemoryMaxPerUserRepo (oldest-eviction). The count-check → insert →
// evict are sequential store calls (mirroring AppendMessages) — a single lead is
// the only writer per run, so no cross-write race is in play. basis/evidence are the
// writer's declared provenance (PRD #266): stored verbatim (NULL when empty) and
// normalized on read — a bad or absent basis is never a write failure.
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
	// evidence a free-text pointer — both untrusted, so sanitize them the same way
	// (a title-shaped basis, a body-shaped evidence) to keep an injected ANSI escape
	// from rendering when the owner runs `uzi memory list`. A bad/empty basis NEVER
	// fails the write (PRD #90: memory writes must not fail a run): it is stored
	// verbatim (NULL when empty) and the API read mapper normalizes an unknown value
	// to "inferred". Evidence is stored NULL when empty (the DTO omits it).
	basis = sanitizeMemoryField(strings.TrimSpace(basis), false)
	evidence = sanitizeMemoryField(evidence, true)
	if len(title) > MemoryMaxTitleBytes || len(body) > MemoryMaxBodyBytes {
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
// char; a single-line title keeps neither. unicode.IsControl catches C0 (incl. ESC
// 0x1b and BEL 0x07), DEL, and C1 — the whole ANSI-escape lead-in class.
func sanitizeMemoryField(s string, keepWhitespace bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r == '\n' || r == '\t') && keepWhitespace {
			b.WriteRune(r)
			continue
		}
		if unicode.IsControl(r) {
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

// PublishResult is the outcome of a checkpoint publish (PRD #122 M8). Published is
// true only when the push landed. Skipped names the benign reason a publish did NOT
// advance the ref ("no_ref" | "not_descendant" | "unsupported"); it is empty on a
// successful publish. Either way Ref is the checkpoint ref the worker asked about.
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
// cached PRD issue (with a PRD link, unless allowWithoutPRD) in a repo the user
// owns; its title is snapshotted from the cache and its description from the
// request, so the run is self-contained even if the issue cache is later
// evicted. allowWithoutPRD is the caller-computed PRDLESS bypass (PRD #22
// Decision 3): the handler sets it from the fresh forge snapshot's labels + the
// prdless settings, and it exempts this run from the HasPrdLink gate. The
// one-non-terminal-run-per-issue index rejects a duplicate active run.
func (s *Service) CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool, waitOnLimit *bool, seed *SeededPlan) (store.Run, error) {
	// waitOnLimit nil ⇒ inherit the owner's default. It is a *bool rather than a bool
	// because "the caller said false" and "the caller said nothing" are different
	// requests, and collapsing them would make every API client that omits the field
	// override the user's own Settings choice with false.
	//
	// seed nil ⇒ an ordinary run planned from the issue (PRD #209): plan_source stays
	// 'agent' and the run behaves byte-identically to a pre-#209 run (Success Criterion
	// 2). Non-nil ⇒ a seeded-plan run that skips Phase 1 and the gate.
	return s.createRun(ctx, userID, repoID, issueIID, description, false, allowWithoutPRD, waitOnLimit, seed)
}

// CreateAutopilotRun queues a run the poller's autopilot detection started on a
// user's behalf (PRD #19 M4). It is IDENTICAL to CreateRun — same ownership,
// cached-PRD-issue, PRD-link, and one-active-run gates, same state machine, same
// queued→In Progress column notify — except it sets auto_approve, which the worker
// reads (M5) to resolve the plan gate without a human. allowWithoutPRD threads the
// PRDLESS bypass the same way the manual path does (PRD #22 Decision 3): the poller
// computes it from its fresh GetIssue snapshot, so an autopilot issue carrying the
// prdless label runs with no PRD link (the PRD+autopilot+PRDLESS composition, all
// three explicit opt-ins). Sharing one createRun body is the whole point: the
// invariant that an autopilot run and a manual run are born through the same path is
// enforced structurally, not by two implementations that could drift.
func (s *Service) CreateAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error) {
	// nil: an autopilot run has no human in the loop to express a per-run choice, so
	// it takes the owner's default (PRD #35 Decision 7 / design brief 7.3). The final
	// nil is the seeded plan: autopilot NEVER seeds — it derives its plan in Phase 1
	// exactly as before (PRD #209 D3 keeps auto_approve and plan_source orthogonal).
	return s.createRun(ctx, userID, repoID, issueIID, description, true, allowWithoutPRD, nil, nil)
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

func (s *Service) createRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, autoApprove, allowWithoutPRD bool, waitOnLimit *bool, seed *SeededPlan) (store.Run, error) {
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
	if _, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}
	issue, err := s.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: issueIID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrIssueNotFound
		}
		return store.Run{}, err
	}
	// The PRD-LABEL gate (PRD #102 Decision 14). Checked BEFORE the PRD-link gate
	// because it is the coarser question — "is this issue uzi's work at all" comes
	// before "is this issue ready to run" — and because its rejection must not be
	// reported as a missing PRD link, which would send a user off to add one.
	//
	// PRDLESS does NOT bypass this. It is the escape hatch for a PRD issue with no
	// prds/*.md file yet (PRD #22 Decision 3); it was never a claim about issues
	// that are not uzi's, and letting it through here would restore exactly the
	// accident this gate exists to stop.
	//
	// Derived from the cached labels rather than a fresh forge read (Decision 12):
	// the same jsonb the board renders the card from, so the button a user sees and
	// the gate the server applies cannot disagree. Promote (Decision 15) writes the
	// label forge-first AND updates this cache row in the same request, so the
	// promote-then-run sequence is not racing the poller.
	if !isPRDIssue(issue.Labels, s.prdLabel(ctx)) {
		return store.Run{}, ErrNotPRDIssue
	}
	// The PRD-link gate (PRD invariant) with the PRDLESS exception (PRD #22):
	// allowWithoutPRD is the caller's bypass decision, computed from the fresh
	// forge snapshot's labels and the prdless settings. This is the single
	// enforcement point; both the manual and autopilot callers pass the bool in.
	if !issue.HasPrdLink && !allowWithoutPRD {
		return store.Run{}, ErrNoPRDLink
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

// prdLabel resolves the configured PRD label for the run-eligibility gate,
// falling back to the compiled-in default when settings are unwired or a read
// fails. Same shape as forgesvc.prdLabel, and deliberately so: the label the sync
// filters on and the label a run is gated by must be the same string, or an
// operator's rename would make the board and the gate disagree about which cards
// are runnable.
//
// Note the fallback direction. An unavailable settings read degrades to enforcing
// the gate on "PRD", never to skipping it — the accessor already returns the
// default alongside a cold error, so this stays best-effort by design without
// ever failing open.
func (s *Service) prdLabel(ctx context.Context) string {
	if s.settings != nil {
		if l, _ := s.settings.PRDLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultPRDLabel
}

// isPRDIssue reports whether a cached issue's labels jsonb carries label. A row
// whose labels cannot be decoded is NOT a PRD issue: the gate has no basis for
// letting it through, and a corrupt or absent value must not read as consent.
//
// Matching is exact, like the forge-side label filter the sync applies and like
// every other label comparison in this codebase.
func isPRDIssue(labelsJSON []byte, label string) bool {
	var labels []string
	if err := json.Unmarshal(labelsJSON, &labels); err != nil {
		return false
	}
	return slices.Contains(labels, label)
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

// ListRunsForUser returns the user's runs (newest first) with repo path and
// worker name for the Runs index. repoID and issueIID are optional narrowings
// (nil = no filter): repo scope backs the board attention strip, repo+issue backs
// the in-app issue history.
func (s *Service) ListRunsForUser(ctx context.Context, userID uuid.UUID, repoID *uuid.UUID, issueIID *int64) ([]store.ListRunsForUserRow, error) {
	arg := store.ListRunsForUserParams{UserID: userID}
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
	return s.q.ListActiveRunsAll(ctx)
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
func (s *Service) SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string, sel *AgentSelection) (SubmitInputResult, error) {
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
	if kind == "approve_plan" && sel != nil {
		return s.submitApproval(ctx, run, *sel)
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

	if kind == "cancel" || kind == "reject_plan" {
		live, err := s.hasLivePoller(ctx, run)
		if err != nil {
			return SubmitInputResult{}, err
		}
		if !live {
			status := "cancelled"
			if kind == "cancel" {
				_, err = s.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: runID, UserID: userID})
			} else {
				status = "failed"
				_, err = s.q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{
					ID: runID, UserID: userID, FailureReason: pgText("plan rejected"),
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
			return SubmitInputResult{ServerSide: true}, nil
		}
		// Live poller: the worker will consume this verdict. Enqueue it AND stamp the
		// deliberate-stop signal in one statement (PRD #33 Decision 3) via the
		// dedicated CreateStopVerdictInput CTE, so the signal is never lost
		// independently of the input that requested it. stopKindFor is always non-empty
		// here (kind is cancel/reject_plan). The stamp lands while the run is still
		// non-terminal; the client's terminal-guarded isStoppedRun ignores it until the
		// run reaches failed/cancelled.
		if _, err := s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID: runID, Kind: kind, Body: pgText(body), StopKind: pgText(stopKindFor(kind)),
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

	// A plain steering input (approve_plan / follow_up): enqueue for the worker with no
	// stop signal and no runs-row touch. Return the created row (PRD #95 S2) so the
	// handler can surface id + created_at for a follow_up's optimistic reconcile.
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
	exclusions, err := encodeJSONArray(sel.Exclusions)
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent exclusions: %w", err)
	}
	body, err := json.Marshal(AgentSelection{Source: sel.Source, Exclusions: orEmpty(sel.Exclusions)})
	if err != nil {
		return SubmitInputResult{}, fmt.Errorf("encode agent selection: %w", err)
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
	return SubmitInputResult{ServerSide: false}, nil
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
// run (PRD #33): a cancel verdict is 'cancelled', a plan reject is 'plan_rejected'.
// Only cancel/reject_plan reach it (the stop-verdict branch of SubmitInput); the
// server owns this mapping so the signal never depends on the reason string the
// worker later reports.
func stopKindFor(kind string) string {
	switch kind {
	case "cancel":
		return "cancelled"
	case "reject_plan":
		return "plan_rejected"
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
	if run.Status == "queued" || run.Status == "limit_wait" || !run.WorkerID.Valid {
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
