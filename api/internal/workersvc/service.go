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
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/planpolicy"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
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
	// ErrOpenMRExists refuses a fresh issue run when the issue already has a completed
	// run owning an OPEN merge request (issue #856). Distinct from ErrActiveRunExists:
	// the offending prior run is TERMINAL, so the active-run gate cannot see it. The
	// create path wraps this with the issue and MR numbers; --force (workersvc create
	// param) bypasses ONLY this guard, never the active-run gate.
	ErrOpenMRExists = errors.New("issue already has an open MR")
	ErrRunTerminal  = errors.New("run has already finished")
	// ErrStopNotInteractive rejects a `stop` on a run that is not an interactive task
	// (PRD #517 M4) → 409. A graceful stop is honored ONLY by the interactive-task park:
	// no other run kind reads the stop flag, so a stop on a plan-gated / chat /
	// non-interactive task run would stamp a permanent, spurious stop_kind='stopped' and
	// return a misleading "stop sent" success while nothing actually winds the run down.
	// Gated on kind+interactive alone, NOT on status: a stop on a RUNNING interactive task
	// (not yet parked) is still valid. It is a run-state conflict, hence 409 (the CLI maps
	// 409 → ExitConflict).
	ErrStopNotInteractive = errors.New("run stop applies only to interactive task runs")
	// ErrPauseNotRunning rejects a `pause` (PRD #1190 M1) on a run that is not currently
	// `running` → 409. Only a running run holds the live worker a park report comes from, and
	// every other non-terminal status is already a park with its own stopped clock (Decision
	// 6), so the message names the current status ("run is awaiting_approval; the clock is
	// already stopped"). Disambiguated from ErrPauseNotSupported by re-reading the run after a
	// 0-row CreatePauseInput: this is the status half.
	ErrPauseNotRunning = errors.New("run is not running")
	// ErrPauseNotSupported rejects a `pause` (PRD #1190 M1, Decision 7) on a run whose kind or
	// interactivity is outside the allowlist (issue / non-interactive task / prompt /
	// self_improve) → 409. The other kinds either already park between turns or finish within
	// minutes, so the message names why per kind ("chat runs already park between turns", …).
	// The kind half of the two 0-row CreatePauseInput causes.
	ErrPauseNotSupported = errors.New("pause is not supported for this run")
	// ErrNoPausePending rejects a `pause_cancel` (PRD #1190 M1) when no pause is pending on a
	// still-running run → 409. A 0-row CancelPauseInput is the only cause.
	ErrNoPausePending = errors.New("no pause is pending")
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
	// ErrInvalidPauseMode rejects a `pause` (PRD #1190 M1) whose body is neither 'milestone'
	// nor 'now' → 400. A dedicated input-validation sentinel (like ErrInvalidScopeCeiling)
	// so CreateRunInput can map it to 400 with the reason verbatim, rather than letting an
	// invalid mode fall through to the 500 default arm.
	ErrInvalidPauseMode = errors.New("invalid pause mode")
	// The four `extend` sentinels (PRD #1189 M1). ErrInvalidExtension rejects a body that is
	// not a whole number of seconds >= 60 → 400. ErrExtendDisabled is the admin off-switch
	// (run_extension_cap_seconds = 0) → 409. ErrExtensionCapExceeded is the per-run cap gate,
	// whose message names the remaining allowance → 409. ErrExtendNotTimed rejects a run of a
	// kind that never times out (chat/judge/interactive) → 409. The last two are disambiguated
	// from a 0-row CreateExtendInput by extendRefusalReason over the already-fetched run.
	ErrInvalidExtension     = errors.New("invalid extension")
	ErrExtendDisabled       = errors.New("extending runs is turned off")
	ErrExtensionCapExceeded = errors.New("extension cap exceeded")
	ErrExtendNotTimed       = errors.New("this run has no wall-clock timeout to extend")
	// ErrWallParkStopUnavailable (PRD #1497 M1, D9) rejects a `stop` on a budget_exhausted wall park
	// that has nothing to finalize (zero completed milestones), is not a milestone issue run, or has
	// already used its one-time finalize allowance. It names the choices that remain: extend or cancel.
	ErrWallParkStopUnavailable = errors.New("this parked run has nothing to finalize; extend it to keep working, or cancel it")
	// ErrReviseCapReached rejects a revise_plan once the run has hit
	// PLAN_MAX_REVISIONS persisted revisions (PRD #41). Counted over ALL
	// revise_plan rows for the run (a consumed revise still counts), so the cap is
	// the lifetime number of revisions requested, not the pending backlog. → 409.
	ErrReviseCapReached = errors.New("plan revision limit reached")
	ErrInvalidState     = errors.New("invalid run state")
	// The two forge-park precedence sentinels (PRD #1392 M1). Both are returned by SetState's
	// forge-park transaction alongside the run row (nothing else returns them), and the handler
	// maps each to a 409 whose body carries {run, reason}: ErrForgeParkStaleClaim → reason
	// "stale_claim" (the report's claim_generation did not match the locked run's — nothing was
	// mutated, #1247's rule, checked FIRST); ErrForgeParkCustodyUnsettled → reason
	// "custody_unsettled" (a current, unreleased generation whose exact-hold cardinality or
	// release step could not settle — the transaction rolled back). The idempotent
	// already-parked case is NOT an error: it returns the parked run with applied=false, so the
	// handler's ordinary 409 {run} path (status recovery_wait, no reason) covers it.
	ErrForgeParkStaleClaim       = errors.New("forge park: claim generation mismatch")
	ErrForgeParkCustodyUnsettled = errors.New("forge park: custody could not be settled")
	// ErrStaleClaim rejects a worker-driven MUTATING report whose claim_generation is stale
	// (PRD #1247 M5, D3): a held-state credential switch RELEASED the claim (claim_released_at
	// set) or a reclaim SUPERSEDED it (claim_generation advanced). The generation fence in
	// SetState returns it atomically under a FOR UPDATE lock so no ClaimRun/release can
	// interleave. The handler surfaces it as a `stale_claim` disposition on the state ack (a
	// 409 carrying the run plus disposition:"stale_claim"), which the new worker reads to STOP
	// the flight without further reports; an ordinary applied=false 409 keeps its meaning.
	ErrStaleClaim = errors.New("run claim is stale (released or superseded)")
	// ErrMissingClaimGeneration rejects a worker-driven MUTATING report from a CAPABILITY worker
	// (one advertising capability.CredentialSwitchV1) that OMITS claim_generation (PRD #1247
	// M5a-1 rework, auditor fail-open finding). The fence engages only when a report stamps a
	// generation, so without this a capability worker could bypass it entirely by dropping the
	// field. Making "legacy" a per-WORKER-capability property (not a per-request field-presence
	// choice) closes that hole: a capability worker MUST stamp the generation, while a legacy
	// worker (no capability) is still honoured unfenced when it omits it. The handler maps this to
	// the SAME stale/refuse ack as ErrStaleClaim (a 409 with disposition:"stale_claim").
	ErrMissingClaimGeneration = errors.New("capability worker omitted claim_generation on a mutating report")
	// ErrMessagesPending rejects a TERMINAL transition whose messages_through_seq is not yet
	// backed by a CONTIGUOUS [1..through] in run_messages (PRD #1391 Run B M3c, D3): the
	// high-water last_seq has not reached `through`, or a hole exists below it (count < through).
	// The fence returns it BEFORE the mutating SetRunCompleted/SetRunFailed/completeRunWithPermit
	// call, so nothing is written; the worker fills the missing seqs and re-reports. The handler
	// maps it to a typed 409 with a {run, reason:"messages_pending"} body — the SAME {run, reason}
	// shape as the forge-park refusals, NOT the disposition shape. It is a RECOVERABLE refusal:
	// the gap is small enough to fill (≤ WorkerGapFillMax), unlike ErrGapUnrecoverable.
	ErrMessagesPending = errors.New("terminal transition refused: run messages are not yet contiguous through the reported seq")
	// ErrGapUnrecoverable rejects a TERMINAL transition whose messages_through_seq leaves MORE than
	// WorkerGapFillMax seqs missing from the stored [1..through] (PRD #1391 Run B M3c): the hole is
	// too large to ever fill, so — unlike ErrMessagesPending — the worker must NOT keep re-parking
	// on it. Checked in the SAME fence, BEFORE the mutation, and mapped by the handler to a typed
	// 409 with a {reason:"gap_unrecoverable"} body (NOT a 400, and distinct from messages_pending).
	ErrGapUnrecoverable = errors.New("terminal transition refused: too many messages are missing to recover")
	// ErrOutcomePendingConfirmationRequired rejects a cancel of a run whose executor journaled a
	// terminal (esp. blocked) outcome on its worker (PRD #1391 Run B M3d, D13) UNLESS the owner
	// explicitly discards it. Such a run has no live poller for itself — its executor is gone —
	// but an unconditional cancel would silently discard a completed/failed outcome held on the
	// worker, so the server refuses without `discard_pending_outcome: true`. The handler maps it
	// to a typed 409 with a {error, reason:"outcome_pending_confirmation_required"} body the
	// web/CLI detect; with the discard bit set the cancel proceeds to the atomic no-live-poller
	// branch (CancelRunServerSideWithPendingOutcome). Nothing is ever discarded on a timer.
	ErrOutcomePendingConfirmationRequired = errors.New("cancel refused: this run has a pending outcome held on its worker; retry with discard_pending_outcome to discard it")
	// ErrOutcomePendingCancelRaced rejects a confirmed discard whose guarded cancel matched
	// zero rows while the run is still active. The pending lease expired, rotated or cleared
	// after confirmation; reporting success would leave the run active while telling the owner
	// it was cancelled. The handler maps this retryable state conflict to a typed 409.
	ErrOutcomePendingCancelRaced = errors.New("pending outcome changed while cancelling; retry")
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
	// ErrCompletionNotBlocked rejects a completion-decision (PRD #1226 M5, D7) on a run that
	// is NOT completion-blocked → 409. A continue-decision is valid only in the two states a
	// completion interlock reaches: the live `awaiting_input` completion-question window (an
	// interlocked run with a recorded completion attempt) OR the `paused` hold
	// (hold_reason='completion_blocked'). Any other status/reason is neither, so the message
	// names why rather than acting on a run that never asked for a decision.
	ErrCompletionNotBlocked = errors.New("run is not waiting on a completion decision")
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
	// ErrWorkerHasCustody rejects deletion of a worker that still holds an OPEN custody
	// hold (PRD #1296 M4, D3): deleting the worker cascades hosted_worker_tokens, and a
	// preauthorized recovery upload may still need that token to authenticate, so the last
	// authenticated retry path must survive until custody is released or explicitly
	// discarded. Carried by a *WorkerHasCustodyError so the caller can name the count of
	// affected custody holds (M4b's CLI confirmation enumerates the captures); errors.Is
	// against this sentinel matches via Unwrap.
	ErrWorkerHasCustody = errors.New("worker holds unpublished work awaiting recovery")
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

// OpenMRExistsError carries the issue and MR numbers of an open-MR refusal (issue
// #856) so a caller (the web 409 body) can name the MR structurally rather than
// parse the message. Its Error() keeps the --force hint for the CLI/API `error`
// field; Is() ties it to ErrOpenMRExists so errors.Is, the scheduler skip mapping
// and the poller swallow all keep working.
type OpenMRExistsError struct {
	IssueIID int64
	MRIID    int64
}

func (e *OpenMRExistsError) Error() string {
	return fmt.Sprintf("issue #%d already has open MR !%d — merge or close it, or leave review comments on the MR to iterate, before starting a new run (pass --force to re-run anyway)", e.IssueIID, e.MRIID)
}

func (e *OpenMRExistsError) Is(target error) bool { return target == ErrOpenMRExists }

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

// WorkerHasCustodyError carries the COUNT of open custody holds that block a worker
// delete (PRD #1296 M4, D3), so the handler/CLI (M4b) can name how many captures a
// forced discard would destroy. It unwraps to ErrWorkerHasCustody, so the handler both
// matches the sentinel (→ 409) and surfaces the specific count via err.Error(). Same
// shape as CapabilityUnmetError.
type WorkerHasCustodyError struct{ Holds int64 }

func (e *WorkerHasCustodyError) Error() string {
	return fmt.Sprintf("%s (%d custody hold(s) still open)", ErrWorkerHasCustody.Error(), e.Holds)
}
func (e *WorkerHasCustodyError) Unwrap() error { return ErrWorkerHasCustody }

// PauseRefusedError carries the SPECIFIC user-facing reason a pause was refused (PRD #1190
// M1) and unwraps to ErrPauseNotRunning or ErrPauseNotSupported, so the handler both matches
// the sentinel (→ 409) and surfaces the run-specific sentence via err.Error(). The message is
// built server-side from the already-loaded run row (its status names the not-running case;
// its kind names the not-supported case), the same shape as CapabilityUnmetError.
type PauseRefusedError struct {
	Msg      string
	Sentinel error
}

func (e *PauseRefusedError) Error() string { return e.Msg }
func (e *PauseRefusedError) Unwrap() error { return e.Sentinel }

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
	// On-demand mr_rework (PRD #1202): StartMRReworkForRun reads the loop-guard ledger
	// (what is new since the last cycle) and advances the consumed high-water WITHOUT
	// spending an automatic cycle; UserHasAnthropicToken is the door-check that the owner
	// can pay for the run the endpoint would mint.
	GetMRReworkLedger(ctx context.Context, arg store.GetMRReworkLedgerParams) (store.MrReworkLedger, error)
	// CreateManualMRReworkRunAndAdvance folds the on-demand run INSERT and the non-counting
	// high-water advance into ONE atomic statement (PRD #1202, review-finding hardening):
	// Postgres commits BOTH or NEITHER, so a create can never leave an unadvanced ledger that
	// lets the automatic watcher re-fire on the same comments.
	CreateManualMRReworkRunAndAdvance(ctx context.Context, arg store.CreateManualMRReworkRunAndAdvanceParams) (store.Run, error)
	UserHasAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
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
	// ListRunUsageTotalsForRuns reads the rollup totals for a page of runs (issue #1620):
	// the Runs list no longer LEFT-joins run_usage_totals, whose generic plan re-folded the
	// view once per run row.
	ListRunUsageTotalsForRuns(ctx context.Context, runIds []uuid.UUID) ([]store.RunUsageTotal, error)
	ListActiveRunsAll(ctx context.Context, backgroundGraceCutoff pgtype.Timestamptz) ([]store.ListActiveRunsAllRow, error)
	ListAllWorkers(ctx context.Context) ([]store.ListAllWorkersRow, error)
	GetRunOwnedByWorker(ctx context.Context, arg store.GetRunOwnedByWorkerParams) (store.Run, error)
	// CountRunMessagesThrough counts the stored message seqs in [1..through] for a run, the
	// terminal fence's contiguity probe (PRD #1391 Run B M3c, D3).
	CountRunMessagesThrough(ctx context.Context, arg store.CountRunMessagesThroughParams) (int32, error)
	// RunMessageGaps returns the missing seq ranges in [1..through] as bounded {first,last} pairs,
	// keyset-paginated by seq after a cursor (PRD #1391 Run B M3c).
	RunMessageGaps(ctx context.Context, arg store.RunMessageGapsParams) ([]store.RunMessageGapsRow, error)
	GetRunOrphanIdentity(ctx context.Context, arg store.GetRunOrphanIdentityParams) (store.GetRunOrphanIdentityRow, error)
	// GetRunForgeConnForWorker returns the forge connection facts for a run the
	// worker holds (PRD #158 M1): forge_project_id + the connection. Worker-scoped
	// by construction (its predicate carries r.worker_id), so a run the worker does
	// not hold — or a repo-less run — returns pgx.ErrNoRows.
	GetRunForgeConnForWorker(ctx context.Context, arg store.GetRunForgeConnForWorkerParams) (store.GetRunForgeConnForWorkerRow, error)
	ClaimRun(ctx context.Context, arg store.ClaimRunParams) (store.Run, error)
	GetRunClaimContext(ctx context.Context, runID uuid.UUID) (store.GetRunClaimContextRow, error)

	// Durable run recovery (PRD #1296 M1): the custody/archive store contract M2
	// (upload/owner API), M3 (worker capture/retry) and M5 (owner UX) consume. Frozen
	// here so those parallel milestones never edit the interface. The custody hold itself
	// is opened atomically inside ClaimRun's CTE (D2); everything below operates on the
	// already-open hold and its immutable captures.
	CountUnresolvedCustodyHoldsForOwner(ctx context.Context, userID uuid.UUID) (int64, error)
	BindCaptureManifest(ctx context.Context, arg store.BindCaptureManifestParams) (store.RecoveryCapture, error)
	InsertCaptureChunk(ctx context.Context, arg store.InsertCaptureChunkParams) error
	MarkCaptureReady(ctx context.Context, arg store.MarkCaptureReadyParams) (store.RecoveryCapture, error)
	MarkCaptureState(ctx context.Context, arg store.MarkCaptureStateParams) (store.RecoveryCapture, error)
	GetCaptureForOwner(ctx context.Context, arg store.GetCaptureForOwnerParams) (store.RecoveryCapture, error)
	ListCapturesForRunOwner(ctx context.Context, arg store.ListCapturesForRunOwnerParams) ([]store.RecoveryCapture, error)
	ListCaptureChunks(ctx context.Context, captureID uuid.UUID) ([]store.RecoveryCaptureChunk, error)
	GetRecoverySummaryForRun(ctx context.Context, arg store.GetRecoverySummaryForRunParams) (store.GetRecoverySummaryForRunRow, error)
	// Custody RELEASE, always per-hold and generation-exact (never per-run, which would strand
	// a sibling older-generation orphan hold): ReleaseCustodyHold releases the ONE selected hold
	// by id (the reconciler's release, agreeing with ListReleasableCustodyHolds, PRD #1296 M4);
	// ReleaseCustodyHoldExact releases the open hold on a run at an EXACT generation held live by
	// the worker — the terminal-completion release in SetState, which passes run.claim_generation
	// so completing a newer generation never releases an older same-worker orphan (PRD #1349 M4).
	ReleaseCustodyHold(ctx context.Context, arg store.ReleaseCustodyHoldParams) (int64, error)
	ReleaseCustodyHoldExact(ctx context.Context, arg store.ReleaseCustodyHoldExactParams) (int64, error)
	// Issue #1582 M1: the predecessor-settle pair. GetCustodyHoldForSettle reads the exact hold
	// (scoped to its run); ReleasePredecessorCustodyHoldByAncestry is the single guarded
	// statement that releases that one older-generation hold with 'ancestry' evidence after the
	// api's own forge proof, re-asserting every run/hold guard so a change mid-proof moves 0 rows.
	GetCustodyHoldForSettle(ctx context.Context, arg store.GetCustodyHoldForSettleParams) (store.RecoveryCustodyHold, error)
	ReleasePredecessorCustodyHoldByAncestry(ctx context.Context, arg store.ReleasePredecessorCustodyHoldByAncestryParams) (int64, error)
	// Issue #1582 M1 rework: the server-held facts the settle candidates must match — the
	// source_sha of every capture under the hold created before the successor generation
	// claimed, and the head of the completion permit an interlocked run's completion consumed.
	ListCaptureSourceShasForHold(ctx context.Context, arg store.ListCaptureSourceShasForHoldParams) ([]string, error)
	GetSettleCompletionPermitHead(ctx context.Context, arg store.GetSettleCompletionPermitHeadParams) (string, error)
	DiscardCaptureForOwner(ctx context.Context, arg store.DiscardCaptureForOwnerParams) (int64, error)
	// ExpireReadyCaptures is the periodic ready-artifact retention sweep (PRD #1296 D4): it
	// flips every 'available' capture past its expires_at to 'expired' AND deletes that
	// capture's encrypted chunk rows in one atomic statement (byte reclamation), keeping the
	// metadata row in state 'expired'. It NEVER touches custody — an expired capture whose hold
	// is still open stays retained per D3.
	ExpireReadyCaptures(ctx context.Context, now pgtype.Timestamptz) (int64, error)
	// ExpireStalledUploads is the periodic upload-retry-window sweep (PRD #1296 D3/D4): it
	// flips a capture stuck in a non-terminal upload state (preparing/uploading) past the
	// UZI_RECOVERY_UPLOAD_RETRY_WINDOW to needs_action WITHOUT releasing its custody hold, so
	// the source is retained until capture succeeds or the owner explicitly discards.
	ExpireStalledUploads(ctx context.Context, retryWindow pgtype.Interval) (int64, error)
	// M4 (D3) cleanup-safety reads: ListReleasableCustodyHolds is the custody-release
	// reconciler's candidate set (OPEN holds whose release is now warranted but unapplied);
	// CountOpenCustodyHoldsForWorker backs the DeleteWorker custody guard.
	ListReleasableCustodyHolds(ctx context.Context) ([]store.ListReleasableCustodyHoldsRow, error)
	CountOpenCustodyHoldsForWorker(ctx context.Context, arg store.CountOpenCustodyHoldsForWorkerParams) (int64, error)
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
	// Admin "All users" aggregate reads (PRD #1184 M1): the cross-user twins of the two
	// owner-scoped judge reads above, with NO user predicate at all. Separate queries (never a
	// relaxation of the owner ones), and their projections hide attribution at the SQL layer —
	// user_id/run_id are opaque count inputs the grouper drops, and run_title/rec_id/review_id/
	// filed iid+url are never selected.
	ListJudgeRecommendationRowsAll(ctx context.Context, arg store.ListJudgeRecommendationRowsAllParams) ([]store.ListJudgeRecommendationRowsAllRow, error)
	ListJudgeTriageRowsAll(ctx context.Context) ([]store.ListJudgeTriageRowsAllRow, error)
	// Admin "All users" issue-filing resolve (PRD #1184 M3): the SINGLE newest OPEN occurrence
	// of a (category, target) coordinate across ALL users, with NO user predicate. The admin
	// issue draft renders from it and the admin filer claims against its review_id; unlike the
	// two aggregate reads it DOES project identifiers, because the draft card names the producing
	// run + user by design (Decision 8). ErrNoRows means no open occurrence (a 404 at the handler).
	NewestOpenOccurrenceForCoord(ctx context.Context, arg store.NewestOpenOccurrenceForCoordParams) (store.NewestOpenOccurrenceForCoordRow, error)
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
	// PRD #1247 M5 (D13): the seq of the run's latest submitted-plan frame, fetched only on the
	// awaiting_approval resume so the worker correlates a buffered approve_plan to the right plan
	// revision. Off the hot path for every other claim.
	LatestPlanSeqForRun(ctx context.Context, runID uuid.UUID) (int64, error)
	// /runs + board + run-view current_activity (PRD #1064 M2): the newest tool_use frame
	// per run for a page, folded into the "now" line in Go by runactivity.FromFrame.
	LatestToolUseForRuns(ctx context.Context, runIds []uuid.UUID) ([]store.LatestToolUseForRunsRow, error)
	// PRD #1353 run-view milestones_live: the three runtime reads milestonelanes.Derive folds
	// into per-in-progress-milestone live lanes — the newest tool_use frame per live subagent
	// instance (LiveLaneFramesForRun), the lead-lane Agent dispatch frames that carry each
	// instance's [<id>] tag + label (LeadAgentDispatchFramesForRun), and the completed dispatch
	// ids that expire finished lanes (LeadDispatchCompletionIDsForRun, id-only — never the
	// tool_result output content, which Derive does not read).
	LiveLaneFramesForRun(ctx context.Context, runID uuid.UUID) ([]store.LiveLaneFramesForRunRow, error)
	LeadAgentDispatchFramesForRun(ctx context.Context, runID uuid.UUID) ([]store.LeadAgentDispatchFramesForRunRow, error)
	LeadDispatchCompletionIDsForRun(ctx context.Context, runID uuid.UUID) ([]string, error)
	ListOwnedRecommendationsForCoords(ctx context.Context, arg store.ListOwnedRecommendationsForCoordsParams) ([]store.ListOwnedRecommendationsForCoordsRow, error)
	// The fan-out write itself: ONE multi-row upsert over the RESOLVED coordinates, so a
	// bulk call is a single round-trip that cannot half-apply (PRD #98 M2, audit NB-A).
	UpsertDispositionsForResolvedCoords(ctx context.Context, arg store.UpsertDispositionsForResolvedCoordsParams) (int64, error)
	// Admin cross-user Mark done + Undo (PRD #1184 M2): the CROSS-USER twins of the two owner
	// coordinate queries above, with NO user predicate at all. ListRecommendationsForCoordsAll
	// resolves every owner's member of a coordinate; UpsertAdminDispositionsForResolvedCoords
	// writes status='done'/set_via='admin' with ON CONFLICT DO NOTHING (a human verdict is never
	// overwritten); DeleteAdminDispositionsForCoords removes only set_via='admin' rows on the
	// coordinate across all users. Reachable only through the cookie-only admin write handlers.
	ListRecommendationsForCoordsAll(ctx context.Context, arg store.ListRecommendationsForCoordsAllParams) ([]store.ListRecommendationsForCoordsAllRow, error)
	UpsertAdminDispositionsForResolvedCoords(ctx context.Context, arg store.UpsertAdminDispositionsForResolvedCoordsParams) (int64, error)
	DeleteAdminDispositionsForCoords(ctx context.Context, arg store.DeleteAdminDispositionsForCoordsParams) (int64, error)
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
	// The per-status Findings tally (PRD #1183 M3, GET /api/findings/stats) and the batched
	// per-page evidence read (evidence_preview + occurrences), so FindingsBacklog never N+1s.
	CountFindingsByStatusForUser(ctx context.Context, arg store.CountFindingsByStatusForUserParams) (store.CountFindingsByStatusForUserRow, error)
	ListFindingEvidenceForDispositions(ctx context.Context, arg store.ListFindingEvidenceForDispositionsParams) ([]store.ListFindingEvidenceForDispositionsRow, error)
	InsertFinding(ctx context.Context, arg store.InsertFindingParams) (store.IncidentalFinding, error)
	UpsertOpenDisposition(ctx context.Context, arg store.UpsertOpenDispositionParams) (store.FindingDisposition, error)
	ReopenDispositionOnHashMismatch(ctx context.Context, arg store.ReopenDispositionOnHashMismatchParams) (int64, error)
	UpdateDispositionLastTitle(ctx context.Context, arg store.UpdateDispositionLastTitleParams) (int64, error)
	SetRunRunning(ctx context.Context, arg store.SetRunRunningParams) (int64, error)
	// SetRunAutopilotPlan durably persists an AUTOPILOT run's approved plan_md on its
	// self-contained `running` report (RC1, issue #1197). The autopilot gate never enters
	// awaiting_approval, so SetRunAwaitingApproval — the other plan_md writer — never runs
	// for it, and a resume would otherwise re-plan on a NULL plan_md. Guarded + idempotent:
	// the affected-row count PROVES storage — rows>0 ⟺ @plan_md is durably stored (fresh or
	// an identical body matched idempotently); rows==0 ⟺ refusal (terminal/parked row,
	// human-gated auto_approve=false, seeded plan_source, a different body already stored,
	// or not-owned) with no mutation. Runs BEFORE SetRunRunning in the running arm so a
	// successful running ack proves the plan was stored.
	SetRunAutopilotPlan(ctx context.Context, arg store.SetRunAutopilotPlanParams) (int64, error)
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
	// SetRunCheckpointTip records runs.checkpoint_tip on every successful checkpoint
	// publish (PRD #1042 M2); best-effort, never fails the publish.
	SetRunCheckpointTip(ctx context.Context, arg store.SetRunCheckpointTipParams) (int64, error)
	ReconcileRunMR(ctx context.Context, arg store.ReconcileRunMRParams) (int64, error)
	// SetRunLimitWait parks a run until the owner's Anthropic usage window reopens
	// (PRD #35); PromoteLimitWaitRuns is the sweeper pass that brings it back. The
	// park's source guard is POSITIVE (status = 'running'), unlike every sibling
	// above, so a re-delivered or out-of-order report is a 0-row no-op rather than a
	// second park.
	SetRunLimitWait(ctx context.Context, arg store.SetRunLimitWaitParams) (int64, error)
	PromoteLimitWaitRuns(ctx context.Context, now pgtype.Timestamptz) ([]store.PromoteLimitWaitRunsRow, error)
	// PromoteLimitWaitRunNow is the SINGLE-ROW early promote for `uzi run set-token` (PRD
	// #1247 M4, D4): limit_wait -> queued for ONE owner+run WITHOUT the retry_not_before
	// guard. Its mutation set is EXACTLY PromoteLimitWaitRuns' (a column-parity test pins
	// them together) and it preserves the limit fields + retry_not_before so claimExclude
	// keeps excluding the still-dead token. A 0-row result is a raced no-op (409).
	PromoteLimitWaitRunNow(ctx context.Context, arg store.PromoteLimitWaitRunNowParams) (int64, error)
	// The duration-time auto-failover re-evaluation pass (PRD #1247 M3, D8):
	// ListLimitWaitReeval is the still-parked worklist (retry_not_before still in the
	// future, a dead credential recorded) that reEvaluateParkedLimitWaitRuns walks, and
	// LowerLimitWaitRetryNow lowers ONE due run's retry_not_before to now() so the same
	// tick's PromoteLimitWaitRuns performs the actual resume. Placed BEFORE the promote
	// call in Sweep so a lowered stamp is eligible for that tick's promotion pass.
	ListLimitWaitReeval(ctx context.Context, now pgtype.Timestamptz) ([]store.ListLimitWaitReevalRow, error)
	LowerLimitWaitRetryNow(ctx context.Context, arg store.LowerLimitWaitRetryNowParams) (int64, error)
	// SetRunRecoveryWait parks a run in the transient-recovery hold (issue #1197);
	// PromoteRecoveryWaitRuns is the sweeper pass that auto-promotes it once its capped
	// backoff elapses. Like the limit-wait park its source guard is POSITIVE
	// (status = 'running'), so a re-delivered or out-of-order report is a 0-row no-op; but
	// it has NO per-run cap, so promotion always fires and the run recovers repeatedly.
	SetRunRecoveryWait(ctx context.Context, arg store.SetRunRecoveryWaitParams) (int64, error)
	PromoteRecoveryWaitRuns(ctx context.Context, now pgtype.Timestamptz) ([]store.PromoteRecoveryWaitRunsRow, error)
	// PromoteRecoveryWaitRunNow is the SINGLE-ROW early promote for `uzi run set-token`
	// (PRD #1247 M4, D4): recovery_wait -> queued for ONE owner+run WITHOUT the
	// recovery_retry_not_before guard, mirroring PromoteRecoveryWaitRuns' mutation set. A
	// 0-row result is a raced no-op (409).
	PromoteRecoveryWaitRunNow(ctx context.Context, arg store.PromoteRecoveryWaitRunNowParams) (int64, error)
	// PRD #1190 M1 pause/resume. SetRunPaused parks a running run on the owner's request
	// (positive source guard, like SetRunLimitWait); ResumePausedRun promotes it back
	// (paused → queued) with gate-park accounting; ClearPauseRequest clears a pending
	// request without parking (the worker's pause_failed report); CreatePauseInput /
	// CancelPauseInput are the owner's request / withdrawal CTEs.
	SetRunPaused(ctx context.Context, arg store.SetRunPausedParams) (int64, error)
	// SetRunCompletionHold parks an OWNED, INTERLOCKED run on the completion interlock's
	// dedicated hold (PRD #1226 M4, D6): running/awaiting_input -> paused with
	// hold_reason='completion_blocked'. A SIBLING of SetRunPaused, not a widening — its guard
	// admits only an interlocked run with a recorded completion attempt (completion_attempts > 0)
	// and leaves the pending-pause columns untouched, so the owner-pause path is unaffected. 0
	// rows (guard fail) is the non-paused ack the worker's park order must retain the run on.
	SetRunCompletionHold(ctx context.Context, arg store.SetRunCompletionHoldParams) (store.Run, error)
	ResumePausedRun(ctx context.Context, arg store.ResumePausedRunParams) (store.ResumePausedRunRow, error)
	// ReleaseCredentialSwitch is the held-state credential-switch RELEASE transition (PRD
	// #1247 M5, D3/D4/D14): a worker's {status:"credential_switch", claim_generation} report
	// requeues the held run in ONE fenced statement — generation- and release-gated, banking
	// the held gap like ResumePausedRun and keeping the switch stamp. releaseCredentialSwitch
	// distinguishes a stale redelivery (0 rows) from an idempotent already-released/already-
	// reclaimed one by a re-read.
	ReleaseCredentialSwitch(ctx context.Context, arg store.ReleaseCredentialSwitchParams) (int64, error)
	// StampHeldCredentialSwitch atomically writes the override columns AND the switch stamp
	// (credential_switch_requested_at + _generation) for `uzi run set-token` on a run a worker
	// holds (PRD #1247 M5, D4, BLOCKING-1 rework). One fenced UPDATE, owner-scoped and fenced on
	// the exact live claim (worker_id + claim_generation + claim_released_at IS NULL + held
	// status); it does NOT change status. Requires exactly one affected row — a 0-row result is a
	// raced release/reclaim the caller maps to ErrCredentialSwitchRaced, with nothing written.
	StampHeldCredentialSwitch(ctx context.Context, arg store.StampHeldCredentialSwitchParams) (int64, error)
	// ClearCredentialSwitchByWorker clears a pending switch stamp WITHOUT changing status for the
	// worker's `credential_switch_failed` report — the bounded capture-failure give-up (PRD #1247
	// M5, D3/D14): the run keeps running on its current token. Fenced to the CURRENT claim
	// (worker_id + exact generation + unreleased + a stamp actually pending), so a
	// stale/superseded/foreign or already-cleared report affects 0 rows. failCredentialSwitch reads
	// a 0-row result as a benign no-op (applied=false).
	ClearCredentialSwitchByWorker(ctx context.Context, arg store.ClearCredentialSwitchByWorkerParams) (int64, error)
	// ClearCompletionBudgetExhausted clears the served budget_exhausted steer
	// (completion_budget_exhausted_at) on the owner's CONTINUE decision (PRD #1226 M5, D3): a
	// new owner decision is the third of D3's clears (alongside the worker acting on it /
	// parking, SetRunCompletionHold). ContinueCompletionDecision calls it on both branches —
	// a no-op on the already-cleared paused branch, and the clear that stops a resumed
	// live-window worker being re-steered into the hold off a since-consumed ACK.
	ClearCompletionBudgetExhausted(ctx context.Context, id uuid.UUID) (int64, error)
	ClearPauseRequest(ctx context.Context, arg store.ClearPauseRequestParams) (int64, error)
	CreatePauseInput(ctx context.Context, arg store.CreatePauseInputParams) (store.RunUserInput, error)
	CancelPauseInput(ctx context.Context, id uuid.UUID) (store.RunUserInput, error)
	// The reactive-resume pass (PRD #754 M5): ListPoolWaitRuns is the pool_wait worklist
	// (oldest first), and PromotePoolWaitRun promotes ONE held run (pool_wait → queued),
	// owner-scoped so the sweeper passes each run's own user_id and resume-now cannot
	// promote a foreign run.
	ListPoolWaitRuns(ctx context.Context) ([]store.ListPoolWaitRunsRow, error)
	PromotePoolWaitRun(ctx context.Context, arg store.PromotePoolWaitRunParams) (int64, error)
	MarkRunFailedByID(ctx context.Context, arg store.MarkRunFailedByIDParams) (int64, error)
	// GetActiveMRReworkRunForMR resolves the single non-terminal mr_rework run for a
	// (repo, MR) — the mid-flight abort (issue #853) uses it to cancel a live rework
	// when its MR leaves the opened state. :one is safe because the WHERE matches the
	// uq_runs_one_active_mr_rework partial unique index (migration 00167).
	GetActiveMRReworkRunForMR(ctx context.Context, arg store.GetActiveMRReworkRunForMRParams) (store.Run, error)
	CancelRunServerSide(ctx context.Context, arg store.CancelRunServerSideParams) (int64, error)
	// RunHasPendingOutcomeLease reports whether a run's executor journaled a terminal outcome
	// still leased on its owning worker (PRD #1391 Run B M3d, D13) — the POSITIVE form of the D11
	// claim-exclusion predicate. Reused by hasLivePoller (such a run has no live poller for
	// itself) and by the owner cancel's confirmation gate.
	RunHasPendingOutcomeLease(ctx context.Context, id uuid.UUID) (bool, error)
	// CancelRunServerSideWithPendingOutcome is the atomic owner-scoped cancel of a pending-outcome
	// run (PRD #1391 Run B M3d, D13): one row-locking UPDATE whose WHERE re-checks the
	// pending-outcome predicate, so a replayed SetState and this cancel resolve on the run's row
	// lock rather than a stale Go-side read. Returns rows affected.
	CancelRunServerSideWithPendingOutcome(ctx context.Context, arg store.CancelRunServerSideWithPendingOutcomeParams) (int64, error)
	// CancelRunByWorker is the LIVE-worker cancel transition (PRD #503 M1). SetState's
	// failed arm routes here (instead of SetRunFailed) when the loaded run carries
	// stop_kind='cancelled', so an operator cancel ends 'cancelled'/NULL fail_origin —
	// converging with CancelRunServerSide — rather than being mis-classified as
	// agent_failure. Worker-scoped because SetState holds a worker, not a user.
	CancelRunByWorker(ctx context.Context, arg store.CancelRunByWorkerParams) (int64, error)
	// SupersedeRunByWorker (issue #1117) is the live-worker terminal transition for an
	// mr_rework run whose finalize push lost to a concurrent same-branch writer: status
	// 'cancelled', stop_kind='branch_moved', fail_origin NULL, a static stop_reason. Like
	// CancelRunByWorker it is worker-scoped and a 0-row no-op onto an already-terminal run.
	SupersedeRunByWorker(ctx context.Context, arg store.SupersedeRunByWorkerParams) (int64, error)
	RejectRunServerSide(ctx context.Context, arg store.RejectRunServerSideParams) (int64, error)
	// FailRunAutoStop is the server-side half of PRD #108 M5's auto-stop. Unlike
	// its two neighbours it takes no user_id: it is driven by the sweeper, not by a
	// request from a user whose ownership must be proven.
	FailRunAutoStop(ctx context.Context, arg store.FailRunAutoStopParams) (int64, error)
	UpdateRunLastSeq(ctx context.Context, arg store.UpdateRunLastSeqParams) (int64, error)

	// Sweeper + register-time orphan recovery.
	SweepClaimedNeverStarted(ctx context.Context, cutoff pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error)
	// SweepTaskNeverDispatched terminalizes a kind='task' (handoff) run left queued with
	// dispatched_at NULL past the dispatch grace window (issue #1367): the CLI never landed
	// its push/dispatch, so ClaimRun never offers it and no other sweep touches it. Server-
	// derived fail_origin='task_undispatched'; status='queued' makes it the single winner
	// against a racing DispatchTaskRun (now also guarded on status='queued').
	SweepTaskNeverDispatched(ctx context.Context, arg store.SweepTaskNeverDispatchedParams) ([]store.SweepTaskNeverDispatchedRow, error)
	// RequeueClaimedRunToQueued resets one just-claimed run to queued when its
	// owner's vault locked between the claim gate and the token open (PRD #32 M3).
	RequeueClaimedRunToQueued(ctx context.Context, id uuid.UUID) (int64, error)
	// HasActiveRunForIssue reports whether the issue already has a non-terminal run.
	// CreateRun uses it as the manual-path dedup pre-check that replaces the
	// uq_runs_one_active_per_issue index for pool_wait runs (PRD #754 M4 Decision 8):
	// the index no longer counts a held run as active, so the structural backstop it
	// gave the manual/board/Slack path is restored here (this SELECT still counts
	// pool_wait as active).
	HasActiveRunForIssue(ctx context.Context, arg store.HasActiveRunForIssueParams) (bool, error)
	// GetOpenMRRunForIssue returns the MR iid of a completed issue run that still owns an
	// OPEN merge request for the issue (issue #856), or pgx.ErrNoRows when none does. It
	// backs createRun's create-time open-MR refusal (the open-MR dedup that --force bypasses).
	GetOpenMRRunForIssue(ctx context.Context, arg store.GetOpenMRRunForIssueParams) (pgtype.Int8, error)
	// PRD #1497 M1: the wall-clock sweep PARKS instead of failing. RequestWallParks files a system
	// 'wall' pause request on a past-deadline run whose worker is alive and speaks wall_park_v1 (the
	// worker drops the turn and captures); ParkRunsAtWall parks the row server-side when the worker is
	// dead, incapable, or unresponsive. Both return (id, user_id, status) rows for publishSwept; the
	// row counts feed SweepResult.WallParkRequested / .WallParked. They REPLACE SweepRunningTimeout.
	RequestWallParks(ctx context.Context, arg store.RequestWallParksParams) ([]store.RequestWallParksRow, error)
	ParkRunsAtWall(ctx context.Context, arg store.ParkRunsAtWallParams) ([]store.ParkRunsAtWallRow, error)
	// SetRunWallPark (PRD #1497 M1) is the worker-authored, fenced wall-park transition for the
	// `wall_park` report: running -> paused with hold_reason='budget_exhausted', gated on the
	// three-term deadline (the sole authority) and completion_attempts = 0. RecordWallParkCapturedHead
	// records a late head after the server already parked the row.
	SetRunWallPark(ctx context.Context, arg store.SetRunWallParkParams) (store.Run, error)
	RecordWallParkCapturedHead(ctx context.Context, arg store.RecordWallParkCapturedHeadParams) (int64, error)
	// CountOnlineWorkersClaimableForRun (PRD #1497 M1, D19) counts the owner's workers that could
	// actually claim THIS run now (ClaimRun's full per-worker conjunction, incl. the released
	// incarnation exclusion), for the new queued-health rung.
	CountOnlineWorkersClaimableForRun(ctx context.Context, arg store.CountOnlineWorkersClaimableForRunParams) (int64, error)
	// ExtendAndResumeWallPark / StopWallPark (PRD #1497 M1, D7/D9) are the owner's Extend and Stop on
	// a budget_exhausted wall park, each in one statement: bank the overrun, resume, and (Stop) cap
	// the scope + grant the 1800s finalize allowance. ExtendAndResumeWallPark returns the new total
	// extension; StopWallPark returns the run id (0 rows / ErrNoRows on refusal).
	ExtendAndResumeWallPark(ctx context.Context, arg store.ExtendAndResumeWallParkParams) (int32, error)
	StopWallPark(ctx context.Context, arg store.StopWallParkParams) (uuid.UUID, error)
	// StampCompletionBudgetExhausted (PRD #1226 M4, D3) arms the server-side served
	// `budget_exhausted` steer on EXACTLY the post-attempt live-worker interlocked rows
	// the wall-park sweep's carve-out spared. PRD #1497 M1: its deadline now mirrors the
	// sweep's three-term total. Same now/global_timeout_seconds/worker_stale_cutoff args;
	// returns the execrows count of rows freshly stamped this tick.
	StampCompletionBudgetExhausted(ctx context.Context, arg store.StampCompletionBudgetExhaustedParams) (int64, error)
	FailRunsOfStaleWorkersOverCap(ctx context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) ([]store.FailRunsOfStaleWorkersOverCapRow, error)
	RequeueRunsOfStaleWorkers(ctx context.Context, arg store.RequeueRunsOfStaleWorkersParams) ([]store.RequeueRunsOfStaleWorkersRow, error)
	FailWorkerRunsOverCap(ctx context.Context, arg store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error)
	// RequeueWorkerRuns returns the re-queued run ids (PRD #1390 M2a) so Register can publish
	// each transition post-commit, mirroring the sweeper's RequeueRunsOfStaleWorkers twin.
	RequeueWorkerRuns(ctx context.Context, arg store.RequeueWorkerRunsParams) ([]uuid.UUID, error)

	// Run-health detector (PRD #47): the per-tick active-run scan, the per-running-run
	// tool window (loop + in-flight), the single health writer, and the queued-run
	// worker-online count.
	ListActiveRunsForHealth(ctx context.Context, codexCuratedModels []string) ([]store.ListActiveRunsForHealthRow, error)
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
	// CountOnlineWorkersSatisfyingProtocol backs PRD #1226 M1's queued-reason rung: an
	// INTERLOCKED queued run whose owner has NO online worker self-reporting the
	// 'completion_interlock_v1' protocol capability gets reasonNoCompletionCapableWorker — the
	// run's non-bypassable claim clause can never be satisfied. A per-run lookup like
	// CountOnlineWorkersSatisfyingCaps above, and off the hot path for the same reason — it runs
	// only for an interlocked queued run already past its health threshold.
	CountOnlineWorkersSatisfyingProtocol(ctx context.Context, userID uuid.UUID) (int64, error)
	// CountOnlineWorkersSatisfyingCodexHarness backs PRD #1332 M5A's (D3) queued-reason rung: a
	// CODEX-INDICATING queued run whose owner has NO online worker advertising the 'codex_harness_v1'
	// protocol capability gets reasonNoCodexCapableWorker — the run's non-bypassable Codex claim
	// clause can never be satisfied. A per-run lookup like CountOnlineWorkersSatisfyingProtocol
	// above, and off the hot path for the same reason — it runs only for a Codex-indicating queued
	// run already past its health threshold.
	CountOnlineWorkersSatisfyingCodexHarness(ctx context.Context, userID uuid.UUID) (int64, error)
	// CountOnlineWorkersSatisfyingCustomCodex backs PRD #1551 M4's (D6) queued-reason rung: a
	// CUSTOM-Codex-root queued run whose owner has NO online worker advertising BOTH
	// 'codex_harness_v1' AND 'codex_custom_model_v1' gets reasonNoCustomCodexCapableWorker — the
	// run's non-bypassable custom-model claim clause can never be satisfied. A per-run lookup like
	// CountOnlineWorkersSatisfyingCodexHarness above, and off the hot path for the same reason.
	CountOnlineWorkersSatisfyingCustomCodex(ctx context.Context, userID uuid.UUID) (int64, error)
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
	// InsertRunMessage returns whether the row was inserted (vs a benign duplicate) AND whether
	// the generation fence held (generation_live) — so a fenced-out stale-generation batch is
	// distinguishable from a duplicate (BLOCKING-4 rework).
	InsertRunMessage(ctx context.Context, arg store.InsertRunMessageParams) (store.InsertRunMessageRow, error)
	ListRunMessagesAfter(ctx context.Context, arg store.ListRunMessagesAfterParams) ([]store.RunMessage, error)
	ListRunMessagesAfterPage(ctx context.Context, arg store.ListRunMessagesAfterPageParams) ([]store.RunMessage, error)
	ListRunMessagesBeforePage(ctx context.Context, arg store.ListRunMessagesBeforePageParams) ([]store.RunMessage, error)
	// UpsertRunUsage folds a delivered result frame's per-model usage into
	// run_usage (PRD #40 M2), GREATEST-merged so re-delivery never regresses.
	UpsertRunUsage(ctx context.Context, arg store.UpsertRunUsageParams) error
	// CountRunInitFramesBefore returns a result frame's leg index (PRD #1079): the
	// count of persisted `init` status frames of the run with a lower seq. The fold
	// reads it once per result frame to key run_usage per SDK query() leg.
	CountRunInitFramesBefore(ctx context.Context, arg store.CountRunInitFramesBeforeParams) (int64, error)
	// CountRunLineageRestartsBefore returns a result frame's lineage (session) index
	// (ADR-1562): the count of persisted fresh_session:true `init` frames of the run with
	// a lower seq, excluding the run's first init. The fold reads it once per result frame
	// so the run_usage_totals view can restart the session_cumulative high-water fold at a
	// fresh session instead of collapsing it into the previous one.
	CountRunLineageRestartsBefore(ctx context.Context, arg store.CountRunLineageRestartsBeforeParams) (int64, error)
	// Usage read rollups (PRD #40 M3), all over the run_usage_totals view.
	GetRunUsageTotal(ctx context.Context, runID uuid.UUID) (store.GetRunUsageTotalRow, error)
	SelfUsage(ctx context.Context, userID uuid.UUID) (store.SelfUsageRow, error)
	AdminUsageTotals(ctx context.Context) (store.AdminUsageTotalsRow, error)
	AdminUsagePerUser(ctx context.Context) ([]store.AdminUsagePerUserRow, error)
	// Failed-run rate outcome aggregates (PRD #1293 M1), read straight from `runs`. The
	// needs_landing sub-cut of `failed` (issue #1418) takes the human-landable fail_origin
	// set as a bind array; the Service wrappers inject AllHumanLandableFailOrigins() so the
	// set never leaves Go.
	SelfRunOutcomes(ctx context.Context, arg store.SelfRunOutcomesParams) (store.SelfRunOutcomesRow, error)
	AdminRunOutcomes(ctx context.Context, landableOrigins []string) (store.AdminRunOutcomesRow, error)
	AdminRunOutcomesPerUser(ctx context.Context, landableOrigins []string) ([]store.AdminRunOutcomesPerUserRow, error)
	SelfRunOutcomeOrigins(ctx context.Context, userID uuid.UUID) ([]store.SelfRunOutcomeOriginsRow, error)
	AdminRunOutcomeOrigins(ctx context.Context) ([]store.AdminRunOutcomeOriginsRow, error)
	AdminRunOutcomeOriginsPerUser(ctx context.Context) ([]store.AdminRunOutcomeOriginsPerUserRow, error)
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
	// CreateExtendInput ADDS @secs to runs.budget_extension_seconds AND writes the
	// kind='extend' audit row in one statement (PRD #1189 M1), returning the new total
	// extension. The audit row is excluded from ConsumeRunInputs; the control travels as
	// runs.budget_extension_seconds on the ACK/claim. A refused request (terminal / untimed
	// kind / over the cap) matches 0 rows and yields pgx.ErrNoRows, which the service maps
	// to the extend 409 classes via extendRefusalReason over the already-fetched run.
	CreateExtendInput(ctx context.Context, arg store.CreateExtendInputParams) (int32, error)
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
	// PRD #929 M2: the server-side proposal-filing path (scheduled issues-mode prompt
	// runs). GetRunSchedule resolves the firing schedule's target/slug/output_mode;
	// StampFiledProposal settles the audit row (pending → confirmed + iid) with no
	// status guard, since the api both inserts the row and files the issue.
	GetRunSchedule(ctx context.Context, id uuid.UUID) (store.RunSchedule, error)
	StampFiledProposal(ctx context.Context, arg store.StampFiledProposalParams) (int64, error)
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
	// GetUserSecretMetaByIDOfKind is the kind-SCOPED by-id meta lookup (PRD #1247 M1):
	// the per-run credential override resolves a specific credential and must confirm it
	// is an anthropic_token before opening it, without weakening the worker/judge lanes'
	// non-kind-scoped GetUserSecretMetaByID (D9).
	GetUserSecretMetaByIDOfKind(ctx context.Context, arg store.GetUserSecretMetaByIDOfKindParams) (store.GetUserSecretMetaByIDOfKindRow, error)
	SetRunAnthropicSecret(ctx context.Context, arg store.SetRunAnthropicSecretParams) (int64, error)
	// RecordRunCredentialEpoch appends the per-claim attribution-journal row (PRD #1247
	// M1, D7/D14), keyed by the run's existing claim_generation; ListRunCredentialEpochs
	// reads a run's applied-switch history (owner-scoped) for the GetRun DTO via the
	// RunCredentialEpochs service method.
	RecordRunCredentialEpoch(ctx context.Context, arg store.RecordRunCredentialEpochParams) error
	ListRunCredentialEpochs(ctx context.Context, arg store.ListRunCredentialEpochsParams) ([]store.RunCredentialEpoch, error)
	// SetRunCredentialOverride writes the per-run override columns for `uzi run set-token`
	// (PRD #1247 M4, D4): the FIRST write in every writable-state branch of the verb,
	// before the state-specific transition. Both params are nullable (inherit clears
	// both). Owner-scoped; idempotent.
	SetRunCredentialOverride(ctx context.Context, arg store.SetRunCredentialOverrideParams) (int64, error)
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
	// UserHasAutoEligibleAnthropicToken reports whether the owner has ≥1 auto_eligible
	// anthropic_token (a non-empty auto-select pool). CreateWorker reads it to derive a
	// new worker's bind mode the #804 way (PRD #1140 M1): no label + non-empty pool → auto.
	UserHasAutoEligibleAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
	SetWorkerAnthropicSecret(ctx context.Context, arg store.SetWorkerAnthropicSecretParams) (store.Worker, error)
	// Judge-lane bind mode + pointer (PRD #104 M4, three-valued per PRD #1140 M2): read
	// at judge-claim time, written by PUT /api/me/judge in one statement (D6).
	GetUserJudgeAnthropicBinding(ctx context.Context, id uuid.UUID) (store.GetUserJudgeAnthropicBindingRow, error)
	SetUserJudgeAnthropicBinding(ctx context.Context, arg store.SetUserJudgeAnthropicBindingParams) (store.User, error)
	// Per-harness worker-model lanes (PRD #1551 M4, D4): read at issue- and chat-run
	// claim assembly, keyed on the run owner. The lane matching the already-frozen run
	// harness replaces the legacy shared default_model; each lane NULL ⇒ inherit.
	GetUserHarnessModelDefaults(ctx context.Context, id uuid.UUID) (store.GetUserHarnessModelDefaultsRow, error)
	// Per-user default reasoning effort (PRD #617): read at issue- and chat-run
	// claim assembly, keyed on the run owner. NULL ⇒ inherit, resolved to the uzi
	// default `xhigh` at claim assembly (issue #1157).
	GetUserDefaultEffort(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	// Per-user AI-attribution opt-out (issue #916): read LIVE at standard run-claim
	// assembly, keyed on the run owner, so flipping the toggle takes effect on the
	// next claim with no worker restart. NOT NULL column (default true) ⇒ a definite
	// bool; false tells the worker to suppress the Co-Authored-By: Claude trailer.
	GetUserAttributionEnabled(ctx context.Context, id uuid.UUID) (bool, error)
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

	// Completion interlock — attempts + permits (PRD #1226 M2, D4/D5). RecordCompletionAttempt
	// and UpsertCompletionPermit are called on the non-transactional permit/attempt endpoints
	// through s.q; the completion-transaction queries (GetRunOwnedByWorkerForUpdate,
	// GetUnconsumedCompletionPermit, GetConsumedCompletionPermit, ConsumeCompletionPermit) are
	// invoked through a tx-bound *store.Queries inside completeRunWithPermit, and appear here so
	// the interface stays the complete list of queries this package uses (a new query changes
	// the interface).
	RecordCompletionAttempt(ctx context.Context, arg store.RecordCompletionAttemptParams) (int32, error)
	UpsertCompletionPermit(ctx context.Context, arg store.UpsertCompletionPermitParams) (store.RunCompletionPermit, error)
	GetRunOwnedByWorkerForUpdate(ctx context.Context, arg store.GetRunOwnedByWorkerForUpdateParams) (store.Run, error)
	GetUnconsumedCompletionPermit(ctx context.Context, arg store.GetUnconsumedCompletionPermitParams) (store.RunCompletionPermit, error)
	GetConsumedCompletionPermit(ctx context.Context, arg store.GetConsumedCompletionPermitParams) (store.RunCompletionPermit, error)
	ConsumeCompletionPermit(ctx context.Context, arg store.ConsumeCompletionPermitParams) (int64, error)
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
	// CompletionHoldWindowSeconds (PRD #1226 M5, D6) is the live owner-continue window a
	// completion-blocked run waits before parking. Configured server-side (like the question
	// bounds above) and shipped in the claim so the worker's completion-question timer uses it
	// instead of QuestionTimeoutSeconds for the completion hold.
	CompletionHoldWindowSeconds int
	RunMaxRequeues              int
	WorkerHeartbeatStale        time.Duration
	// WorkerHeartbeatInterval (WORKER_HEARTBEAT_INTERVAL) is the worker's heartbeat cadence.
	// PRD #1390 M2b (D4): the missing-run fence is WorkerHeartbeatStale + WorkerHeartbeatInterval
	// (the stale window plus one heartbeat interval), so a `running` run absent from a snapshot is
	// requeued only once it has been silent past the whole window with margin. Mirrored from config;
	// the zero value narrows the fence to just the stale window (degraded, never wrong).
	WorkerHeartbeatInterval time.Duration
	// DiskPressureThreshold (PRD #837 M4, UZI_DISK_PRESSURE_THRESHOLD) is the used/total
	// fraction in (0,1] at/above which a self-reported disk volume counts as "over
	// threshold" for one heartbeat. Heartbeat feeds it to diskOverThreshold, which drives
	// the debounced stats_disk_pressure_streak the poll derives disk_pressure from. Purely
	// a display/lifecycle tuning knob — it never gates claim or scheduling.
	DiskPressureThreshold float64
	WorkerAffinityGrace   time.Duration
	// WorkerAffinityCeiling (PRD #628 D3a): the run-lane affinity ceiling. ClaimRun pins
	// a promoted run to its prior worker while its row exists and it is heartbeating or
	// draining (PRD #1030, verified against ClaimRun on 2026-09-08; the earlier comment
	// incorrectly excluded draining workers). The ceiling bounds the live-but-wedged case. It is
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
	// DispatchGrace is the undispatched-task-run reaper window (issue #1367): how long a
	// kind='task' (handoff) run may sit status='queued' with dispatched_at NULL before the
	// undispatched-handoff sweep (SweepTaskNeverDispatched) terminalizes it. It is a
	// SEPARATE, calibrated window distinct from the 5m worker ClaimGrace because a local
	// push+dispatch has a different latency profile than a worker claim→start. Like
	// ClaimGrace it is fixed in code (defaulted in New), not yet a PRD env var, and is
	// promotable to one later.
	DispatchGrace time.Duration
	// SweeperBootGrace (PRD #1390 M1, D1, SWEEPER_BOOT_GRACE) is the boot-grace window
	// that suppresses the three stale-worker passes (MarkStaleWorkersOffline,
	// FailRunsOfStaleWorkersOverCap, RequeueRunsOfStaleWorkers) until it has elapsed
	// since the LISTENER became ready (Service.SetReadyAt), so an api that was unreachable
	// does not declare every worker dead the instant it returns. Every other sweep pass runs
	// as today. 0 (the zero value) means "immediate" — today's behaviour — so a Params literal
	// that omits it is a safe no-op. Mirrored from config; the default lives in config.go.
	SweeperBootGrace time.Duration
	// TerminalPendingLease (PRD #1390 M2a, D11, TERMINAL_PENDING_LEASE) is the lifetime of a
	// terminal-pending lease and of the pending_overflow closure, mirrored from config.
	// ReplaceWorkerActiveRuns stamps `terminal_pending_until = now() + TerminalPendingLease` on
	// every terminal_pending entry (and `pending_overflow_until` likewise). The zero value is a
	// safe no-op (a lease of 0 is already expired, so nothing is protected — degraded, never
	// wrong); the positive default lives in config.go.
	TerminalPendingLease time.Duration
	// ActiveSnapshotMaxEntries (PRD #1390 M2a, ACTIVE_SNAPSHOT_MAX_ENTRIES) is the absolute
	// server ceiling on a snapshot's entry count, mirrored from config. A snapshot above it is
	// rejected as invalid. The zero value would reject every non-empty snapshot; the positive
	// default lives in config.go.
	ActiveSnapshotMaxEntries int
	// WorkerOutboxMaxPending (PRD #1390 M2a / #1391, WORKER_OUTBOX_MAX_PENDING) caps the
	// terminal_pending=true entries in one snapshot, mirrored from config. A worker beyond it
	// sets pending_overflow. The default lives in config.go.
	WorkerOutboxMaxPending int
	// WorkerGapFillMax (PRD #1391 Run B M3c, WORKER_GAP_FILL_MAX) is the terminal fence's
	// gap-recovery ceiling, mirrored from config: a terminal report whose messages_through_seq
	// leaves more than this many seqs missing from stored run_messages is refused
	// ErrGapUnrecoverable rather than the recoverable ErrMessagesPending. The zero value means
	// "any hole is unrecoverable"; the positive default lives in config.go.
	WorkerGapFillMax int
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

	// Transient-recovery park (issue #1197), mirrored from config. RunRecoveryParkBase is
	// the FIRST recovery park's wait (the exponential backoff's base); each subsequent
	// park doubles it, clamped at RunRecoveryMaxPark. UNLIKE the usage-limit knobs above,
	// NEITHER can fail a run: there is no per-run park cap, so a 'recovery_wait' run always
	// becomes promotable again after its capped backoff (until it recovers or the owner
	// cancels). recovery_wait_count shapes the curve only. The defaults live in config.go.
	RunRecoveryParkBase time.Duration
	RunRecoveryMaxPark  time.Duration

	// RunForgeUnreachableMaxParks (PRD #1392 M1, D2), mirrored from config. The FORGE-park
	// lifetime cap: past it, SetState's forge-park transaction fails the run with
	// fail_origin='forge_unreachable' instead of parking. UNLIKE the recovery-park cadence
	// knobs above, this CAN fail a run. The ZERO VALUE is "unlimited" (never fail on the
	// count) — the safe off direction and NOT a hole (a Params literal that omits it never
	// caps the forge park), matching RUN_LIMIT_MAX_WAITS==0's "never park" shape. The positive
	// default lives in config.go where the env is read.
	RunForgeUnreachableMaxParks int

	// RecoveryUploadRetryWindow (PRD #1296 D3/D4, UZI_RECOVERY_UPLOAD_RETRY_WINDOW) is the
	// durable-archive upload-retry window: a capture reserved but still non-terminal
	// (preparing/uploading) longer than this has stalled, and the sweep flips it to
	// needs_action WITHOUT releasing its custody hold (the source is retained). Mirrored from
	// config. Like ChatIdleTimeout/ProposalConfirmStuckTimeout, a NON-POSITIVE value DISABLES
	// the sweep (the zero value is the safe off direction, so a Params literal that omits it
	// simply never runs the pass); the positive default lives in config.go where the env is read.
	RecoveryUploadRetryWindow time.Duration

	// RecoveryReadyRetention (PRD #1296 D4, UZI_RECOVERY_READY_RETENTION) is the ready-artifact
	// TTL. The window itself is baked into each capture's expires_at at durable capture
	// (MarkCaptureReady, from config); this field is the ENFORCEMENT toggle for the periodic
	// expiry sweep (ExpireReadyCaptures), which flips 'available' captures past their expires_at
	// to 'expired' AND reclaims their bytes. Like RecoveryUploadRetryWindow above, a NON-POSITIVE
	// value DISABLES the sweep (the zero value is the safe off direction, so a Params literal that
	// omits it never runs the pass); the positive default lives in config.go where the env is read.
	RecoveryReadyRetention time.Duration
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

// CompletionInterlockReader is the narrow settings view createRun reads for the
// completion-interlock rollout switch (PRD #1226 M1, D1). *settings.Cache satisfies it.
// Kept its own interface (interface segregation, like CapabilityScheduleReader) so a
// test exercises only what it uses. Optional (nil-safe): a nil reader — or a read error
// — DEFAULTS the flag OFF (the DELIBERATE opposite of CapabilityScheduleReader's
// default-on), so a new run is interlocked ONLY on an affirmative "true". This gate must
// not accidentally engage a still-rolling-out feature, so both the unconfigured and the
// unreadable case fail safe to legacy (unstamped) runs.
type CompletionInterlockReader interface {
	CompletionInterlockRollout(ctx context.Context) (bool, error)
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
	// codexRefresh is the injectable oauth-exchange seam the coordinated Codex
	// refresher uses (PRD #1147 M2, B6). *codexauth.Client satisfies
	// CodexRefreshClient in production; tests supply a call-counting fake. Optional
	// (nil): a nil client makes CoordinatedCodexRefresh refuse before touching the
	// provider, so a deployment (or test) without it never rotates. Production wires it
	// through SetCodexRefresh for the public Codex path activated by PRD #1429.
	codexRefresh CodexRefreshClient
	// codexRejector records a provider rejection of refresh material (issue #1594). Nil in
	// production: codexRejectionTransition then runs store.QuarantineRejectedCodexRefresh
	// over txBeginner. Tests set it to a recording fake.
	codexRejector codexRefreshRejector
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
	// completionInterlock reads the completion-interlock rollout switch createRun consults
	// to decide whether to stamp completion_contract_version=1 on a new issue run (PRD
	// #1226 M1, D1). Optional (nil-safe); set via SetCompletionInterlockSettings with the
	// same settings cache the HTTP handlers hold. Nil ⇒ the flag DEFAULTS OFF, so tests and
	// deployments without a settings cache create legacy (unstamped) runs exactly as before
	// — the fail-safe direction, opposite the capability-aware default-on.
	completionInterlock CompletionInterlockReader
	// persistFail counts consecutive AppendMessages failures per run (PRD #108 M4),
	// the signal a persistence wedge cannot suppress because the wedge IS the event
	// being counted. Always non-nil (New constructs it).
	//
	// It is NOT sweeper-only: it is written by every HTTP handler goroutine serving
	// /messages and read by the sweeper, so it carries its own mutex — see
	// persistfail.go.
	persistFail *persistFailTracker
	// outbox tracks each worker's last reported outbox depth (PRD #1391 M5), the
	// signal the health detector turns into a truthful "queued on the worker" reason
	// (instead of the misleading `stalled`) and the list/register/heartbeat DTOs
	// overlay onto WorkerDTO. Always non-nil (New constructs it). Like persistFail it
	// is IN-PROCESS, mutex-guarded, capped and TTL-pruned, and restart-losing by
	// design — see outbox_tracker.go.
	outbox *outboxTracker
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
	// deleteCheckpointFn is the go-git checkpoint-ref DELETER (PRD #1030 M4), the
	// cleanup counterpart of publishFn. Defaults to pushbroker.Delete (set in New); a
	// setter lets tests stub it so the terminal-transition cleanup is exercised (and
	// error-injected) without a real forge. Same seam discipline as publishFn:
	// pushbroker stays the ONE place go-git lives.
	deleteCheckpointFn func(ctx context.Context, o pushbroker.DeleteOptions) error
	// background dispatches a best-effort forge side-effect off the request/report
	// goroutine so a slow/down forge can never delay or wedge the caller (PRD #1030
	// M4's checkpoint delete). Defaults to `go fn()` (set in New); tests override it
	// with a synchronous runner so the async side-effect is observed DETERMINISTICALLY,
	// matching forgesvc's ProjectSyncService.background idiom.
	background func(func())
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
	// txBeginner opens the pgx transaction the permit-gated completion runs in (PRD #1226
	// M2, D4): completeRunWithPermit locks the run, consumes the permit and writes
	// `completed` atomically through it. *pgxpool.Pool satisfies it; set via SetTxBeginner.
	// Nil is FAIL-CLOSED — an interlocked completion errors rather than completing
	// non-atomically, so a deployment that never wired it can never complete an interlocked
	// run without the permit transaction. A LEGACY completion never touches it.
	txBeginner TxBeginner
	// codexFreezeFn is the createRunAtomic Codex-binding freeze, a SEAM defaulting (nil) to
	// s.freezeCodexBinding (PRD #1429 M1). It exists so a test can inject a failing freeze and
	// prove the create transaction rolls the run INSERT back (D1: a freeze failure leaves NO
	// runs row). Nil in production and every non-injecting test — createRunAtomic falls back
	// to the real freeze — so the seam is inert unless a test sets it, matching the
	// publishFn/deleteCheckpointFn/background seam idiom above.
	codexFreezeFn func(ctx context.Context, q codexFreezeStore, userID, runID, secretID uuid.UUID, authMode string) error
	// claimHooks are the run-lane claim race seams (PRD #1590 M1): a LiveDB test injects a
	// concurrent account or claim mutation between ClaimRun and assembly, after the Codex mint,
	// or between assembly and the final exact-claim transaction. Nil in production, so every
	// hook is inert unless a test sets it (the codexFreezeFn idiom above).
	claimHooks *claimTestHooks
	// codexPark is the park_codex_account_unavailable pass's in-memory keyset cursor (PRD
	// #1590 M2, D2): the last queued Codex subscription run id the pass examined. Zero value is
	// the start of the id space. Restart-losing by design: losing it only restarts the scan.
	codexPark codexAccountPageCursor
	// codexPromote is the promote_codex_account_available pass's keyset cursor (PRD #1590 M3,
	// D3) over the held codex_account_unavailable set, with the same restart-losing semantics.
	codexPromote codexAccountPageCursor
	// codexPromoteHooks are the promotion pass's LiveDB race seams (nil in production).
	codexPromoteHooks *codexPromoteTestHooks
	// readyAt is the moment the worker-facing listener(s) became ready (PRD #1390 M1, D1),
	// stored as Unix nanoseconds (0 = not yet ready). main.go writes it via SetReadyAt after
	// binding every enabled listener; the sweeper goroutine reads it each tick to anchor the
	// boot grace. An atomic.Int64 rather than a time.Time so the cross-goroutine read/write is
	// data-race-free under -race.
	readyAt atomic.Int64
	// bootGraceFirstRunLogged flips true the first time a post-grace sweep runs the gated
	// stale-worker passes, so that transition is logged exactly once (PRD #1390 M1).
	bootGraceFirstRunLogged atomic.Bool
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

// SetCodexRefresh wires the production Codex oauth-exchange client into the coordinated
// refresher (PRD #1171 M1, activated by PRD #1429), mirroring SetVault. Call once at startup, before
// serving. In production main.go injects a fixed-endpoint, deadline-bounded
// *codexauth.Client; tests inject a call-counting fake (or a real Client over a fake
// httpDoer). A nil client (the default, and every test that never wires it) leaves
// CoordinatedCodexRefresh refusing before it touches the provider
// (ErrCodexRefreshNoClient) — so a deployment without it never rotates, fail-closed.
func (s *Service) SetCodexRefresh(c CodexRefreshClient) { s.codexRefresh = c }

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

// SetCompletionInterlockSettings wires the completion-interlock rollout-switch reader
// createRun consults (PRD #1226 M1, D1). Call once at startup, before serving, with the
// same settings cache the HTTP handlers hold. Nil (the default in tests) defaults the flag
// OFF — new runs are created legacy/unstamped — so the omission is a safe no-op rather than
// a silent enable.
func (s *Service) SetCompletionInterlockSettings(r CompletionInterlockReader) {
	s.completionInterlock = r
}

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

// SetDeleteCheckpointFn overrides the go-git checkpoint-ref deleter (PRD #1030 M4).
// Production leaves the pushbroker.Delete default New installs; tests stub it to
// exercise the terminal-transition cleanup — and inject a delete error to prove the
// terminal state is still recorded regardless of the delete outcome.
func (s *Service) SetDeleteCheckpointFn(fn func(ctx context.Context, o pushbroker.DeleteOptions) error) {
	s.deleteCheckpointFn = fn
}

// SetBackground overrides the best-effort side-effect dispatcher (PRD #1030 M4).
// Production leaves the `go fn()` default New installs; tests set a synchronous
// runner so the async checkpoint delete is observed deterministically.
func (s *Service) SetBackground(fn func(func())) { s.background = fn }

// SetReadyAt records the moment the worker-facing listener(s) became ready (PRD #1390 M1,
// D1). main.go calls it exactly once, AFTER every enabled listener has bound, so the boot
// grace is anchored on when workers could actually reconnect — not on process start, which
// ages while the api is still unreachable. Race-safe: stores Unix nanoseconds atomically for
// the sweeper goroutine to read. A zero t clears it back to "not ready".
func (s *Service) SetReadyAt(t time.Time) {
	if t.IsZero() {
		s.readyAt.Store(0)
		return
	}
	s.readyAt.Store(t.UnixNano())
}

// readyAtTime reads the listener-ready timestamp set by SetReadyAt, returning the zero
// time.Time when the listener is not yet ready (the sweeper treats that as grace-active).
func (s *Service) readyAtTime() time.Time {
	ns := s.readyAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// bootGraceActive decides whether the three stale-worker passes are suppressed this tick
// (PRD #1390 M1, D1). A pure function of the config knob, the listener-ready timestamp and
// the current time, so it is table-testable without a DB:
//   - bootGrace <= 0 ⇒ never active (SWEEPER_BOOT_GRACE=0 reproduces today's behaviour).
//   - readyAt zero (the listener has not bound yet) ⇒ active (a worker cannot have reconnected).
//   - now before readyAt+bootGrace ⇒ active; at or after ⇒ the grace has elapsed.
func bootGraceActive(bootGrace time.Duration, readyAt, now time.Time) bool {
	if bootGrace <= 0 {
		return false
	}
	return readyAt.IsZero() || now.Before(readyAt.Add(bootGrace))
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

// defaultDispatchGrace is the undispatched-task-run reaper window (issue #1367): a
// kind='task' (handoff) run may sit queued+undispatched this long before the sweep
// terminalizes it. Deliberately larger than defaultClaimGrace — a local push+dispatch
// has a different latency profile than a worker claim→start.
const defaultDispatchGrace = 15 * time.Minute

// New constructs a Service. box may be nil only in tests that never call Claim.
func New(q Store, box *secretbox.Box, p Params) *Service {
	if p.ClaimGrace <= 0 {
		p.ClaimGrace = defaultClaimGrace
	}
	if p.DispatchGrace <= 0 {
		p.DispatchGrace = defaultDispatchGrace
	}
	return &Service{
		q: q, box: box, p: p, now: time.Now, persistFail: newPersistFailTracker(), outbox: newOutboxTracker(),
		publishFn:          pushbroker.Publish,
		deleteCheckpointFn: pushbroker.Delete,
		background:         func(fn func()) { go fn() },
	}
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
//
// PRD #1390 M2a runs the whole of Register in ONE transaction (canonical worker-row lock
// order): mint + persist a fresh register nonce and reset the snapshot epoch, persist any
// register-carried snapshot (ownership-validated, preserving leased rows) BEFORE the orphan
// pass so the D11 lease predicate reads a settled snapshot, then run the orphan fail/requeue
// pass, commit, and only THEN publish each transition and judge the failures. It returns the
// nonce so the handler can echo it in the register response. When no transaction beginner is
// wired (unit tests with a fake store), it degrades to the tx-less path: no worker-row lock and
// no snapshot persist — a fake store never carries a snapshot.
func (s *Service) Register(ctx context.Context, wkr store.Worker, version, template string, maxConcurrentRuns *int, capabilities []string, protocolCapabilities []string, snapshot *ActiveSnapshot) (store.Worker, string, error) {
	nonce, err := mintSnapshotNonce()
	if err != nil {
		return store.Worker{}, "", err
	}
	max := int32(s.p.RunMaxRequeues) //nolint:gosec // G115: RunMaxRequeues is a small bounded config int (env RUN_MAX_REQUEUES), never near int32 range
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
	// protocolCaps is the STORED protocol set (PRD #1226 M1, D2): the worker's
	// self-reported protocol capabilities, passed through the server-owned
	// capability.FilterProtocol for vocabulary validation, dedupe and stable order — so
	// a garbled/hostile report cannot smuggle an arbitrary protocol string in. It is
	// kept SEPARATE from storedCaps and written to workers.protocol_capabilities, the
	// column the ClaimRun hard clause reads directly. Overwritten on every register (the
	// fresh-start signal), so a downgraded image that stops reporting the protocol loses
	// it here — which correctly makes it unable to claim an interlocked run.
	protocolCaps := capability.FilterProtocol(protocolCapabilities)
	regParams := store.RegisterWorkerParams{
		Version:               pgconv.TextOrNull(version),
		TemplateReported:      pgconv.TextOrNull(template),
		Capabilities:          storedCaps,
		ProtocolCapabilities:  protocolCaps,
		MaxConcurrentRuns:     pgconv.Int4Ptr(maxConcurrentRuns),
		SnapshotRegisterNonce: pgconv.TextOrNull(nonce),
		ID:                    wkr.ID,
	}
	failParams := store.FailWorkerRunsOverCapParams{
		FailureReason: pgconv.TextOrNull("worker restarted; run orphaned and out of re-queue budget"),
		WorkerID:      pgconv.UUID(wkr.ID),
		MaxRequeues:   max,
	}
	requeueParams := store.RequeueWorkerRunsParams{
		WorkerID:    pgconv.UUID(wkr.ID),
		MaxRequeues: max,
	}

	// Tx-less degraded path: no pool wired (fake-store unit tests). No worker-row lock and no
	// snapshot persist; still rotates the nonce and resets the epoch via RegisterWorker.
	if s.txBeginner == nil {
		row, err := s.q.RegisterWorker(ctx, regParams)
		if err != nil {
			return store.Worker{}, "", err
		}
		orphanFailed, err := s.q.FailWorkerRunsOverCap(ctx, failParams)
		if err != nil {
			return store.Worker{}, "", err
		}
		requeued, err := s.q.RequeueWorkerRuns(ctx, requeueParams)
		if err != nil {
			return store.Worker{}, "", err
		}
		s.publishRegisterSweeps(ctx, orphanFailed, requeued)
		return store.Worker(row), nonce, nil
	}

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Worker{}, "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := store.New(tx)
	// (a) Lock the worker row FOR UPDATE — the canonical order that serialises Register against
	// a concurrent heartbeat and the stale-worker passes (both of which lock the worker row).
	if _, err := qtx.GetWorkerForUpdate(ctx, wkr.ID); err != nil {
		return store.Worker{}, "", err
	}
	// (b) Rotate the nonce + reset the epoch (folded into RegisterWorker's update).
	row, err := qtx.RegisterWorker(ctx, regParams)
	if err != nil {
		return store.Worker{}, "", err
	}
	// (c) Persist a register-carried snapshot BEFORE the orphan pass, so its leased rows protect
	// #1391's pending outcomes from the D11-predicated fail/requeue that follows. #1390's worker
	// never sends one; the path exists for #1391. An invalid register snapshot is ignored, never
	// fatal (register must not wedge on soft input).
	if snapshot != nil {
		if _, err := s.ReplaceWorkerActiveRuns(ctx, qtx, store.Worker(row), snapshot, snapshotModeRegister); err != nil {
			return store.Worker{}, "", err
		}
	}
	// (d) Orphan pass — fail-over-cap then requeue, both D11-lease/overflow-predicated (M1).
	orphanFailed, err := qtx.FailWorkerRunsOverCap(ctx, failParams)
	if err != nil {
		return store.Worker{}, "", err
	}
	requeued, err := qtx.RequeueWorkerRuns(ctx, requeueParams)
	if err != nil {
		return store.Worker{}, "", err
	}
	// (e) Commit, then publish + judge post-commit (never inside the tx).
	if err := tx.Commit(ctx); err != nil {
		return store.Worker{}, "", err
	}
	committed = true
	s.publishRegisterSweeps(ctx, orphanFailed, requeued)
	return store.Worker(row), nonce, nil
}

// publishRegisterSweeps fans a register-time orphan pass out post-commit (PRD #1390 M2a):
// publishSwept for both the failed and the requeued transitions (closing the gap where a
// register-time requeue/fail reached no live channel — before, only the judge saw the fails)
// and maybeEnqueueJudgeByID for the committed-terminal fails (PRD #46 Decision 2, the same
// worker-lost runs the sweeper funnels). Both are best-effort and never fail the register.
func (s *Service) publishRegisterSweeps(ctx context.Context, failed, requeued []uuid.UUID) {
	for _, id := range failed {
		s.publishSwept(id, "failed")
		s.maybeEnqueueJudgeByID(ctx, id)
	}
	for _, id := range requeued {
		s.publishSwept(id, "queued")
	}
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
	// Disk usage per volume (PRD #837 M1), each nil when the worker's statfs failed or
	// the mount was absent. Used and total bytes for the /nix and data volumes; a nil
	// pointer writes NULL, exactly like the mem fields. Display-only, same as the rest.
	DiskNixBytes       *int64
	DiskNixTotalBytes  *int64
	DiskDataBytes      *int64
	DiskDataTotalBytes *int64
}

// Heartbeat refreshes liveness, overwrites the worker's latest resource sample (PRD
// #49), records the worker's per-run outbox depth (PRD #1391 M5), stores the worker's
// active-run snapshot when one is carried (PRD #1390 M2a), and returns the updated worker.
// A nil stats writes NULLs for every stats_ column, so a worker that stops reporting
// self-clears its gauge; an empty/nil outbox CLEARS the worker's tracked depth (the "clears
// on the next empty report" contract).
//
// When a snapshot is carried it runs inside ONE transaction in the canonical order (PRD #1390
// M2b): HeartbeatWorker (locks the worker row), then LockOwnedRunsByIDs (pre-locks the runs the
// snapshot lists so a sibling's concurrent FOR UPDATE SKIP LOCKED claim skips them), then
// ReplaceWorkerActiveRuns in heartbeat mode. ONLY when the snapshot was applied does it then
// reconcile: ReadoptRunsFromSnapshot (restore a queued run the worker still lists), then
// FailRunsMissingFromSnapshot then RequeueRunsMissingFromSnapshot (fail before requeue) for a
// running run this worker owns but no longer lists, past the missing fence. Each reconciled
// transition is published post-commit (never inside the tx), mirroring publishRegisterSweeps.
// An invalid/stale/wrong-nonce snapshot is IGNORED (applied=false) — a warning, no error, rows
// and leases left as they were, and the reconciliation is skipped — so an invalid snapshot never
// turns a heartbeat into a 400 and liveness is still refreshed. When no snapshot is carried (or
// no tx beginner is wired), it takes the plain single-statement path, exactly as before.
func (s *Service) Heartbeat(ctx context.Context, wkr store.Worker, stats *WorkerStats, outbox []OutboxEntry, snapshot *ActiveSnapshot) (store.Worker, error) {
	// Record the outbox depth BEFORE the liveness write: it is in-memory and cannot
	// fail, and doing it unconditionally means an empty report clears the set even on
	// a tick that carries no stats.
	s.outbox.record(wkr.ID, outbox, s.now())
	arg := store.HeartbeatWorkerParams{ID: wkr.ID}
	if stats != nil {
		arg.StatsCpuPct = pgconv.Float4Ptr(stats.CPUPct)
		arg.StatsMemBytes = pgtype.Int8{Int64: stats.MemBytes, Valid: true}
		arg.StatsMemLimitBytes = pgconv.Int8Ptr(stats.MemLimit)
		arg.StatsSource = pgconv.TextOrNull(stats.Source)
		arg.StatsDiskNixBytes = pgconv.Int8Ptr(stats.DiskNixBytes)
		arg.StatsDiskNixTotalBytes = pgconv.Int8Ptr(stats.DiskNixTotalBytes)
		arg.StatsDiskDataBytes = pgconv.Int8Ptr(stats.DiskDataBytes)
		arg.StatsDiskDataTotalBytes = pgconv.Int8Ptr(stats.DiskDataTotalBytes)
		// Disk-pressure debounce input (PRD #837 M4): whether THIS sample crossed the
		// threshold. HeartbeatWorker increments the streak when true and resets it to 0
		// when false; a nil stats leaves this false, which correctly resets the streak
		// (the tick carried no evidence of pressure).
		arg.DiskOverThreshold = diskOverThreshold(stats, s.p.DiskPressureThreshold)
	}
	// No snapshot (an old worker, or the field absent), or no pool wired (tests): the plain
	// single-statement liveness write, unchanged.
	if snapshot == nil || s.txBeginner == nil {
		return s.q.HeartbeatWorker(ctx, arg)
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Worker{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := store.New(tx)
	updated, err := qtx.HeartbeatWorker(ctx, arg)
	if err != nil {
		return store.Worker{}, err
	}
	// (b) Pre-lock the runs the snapshot lists, in canonical id order, BEFORE the replace and the
	// reconciliation writes — so a sibling's concurrent claim (FOR UPDATE SKIP LOCKED) skips them
	// until this tx commits. Unparseable ids are skipped (they cannot name a real run); an id for a
	// run this worker does not own is a no-op (the WHERE excludes it), never locked.
	if ids := snapshotRunIDs(snapshot); len(ids) > 0 {
		if _, err := qtx.LockOwnedRunsByIDs(ctx, store.LockOwnedRunsByIDsParams{
			RunIds:   ids,
			WorkerID: pgconv.UUID(updated.ID),
		}); err != nil {
			return store.Worker{}, err
		}
	}
	// (c) heartbeat mode: an invalid snapshot returns (false, nil) — ignored, not fatal — so the
	// heartbeat's liveness refresh still commits. Only a real DB error aborts the tx. CAPTURE
	// applied: the reconciliation runs ONLY when the snapshot was actually applied (an invalid,
	// stale-epoch or wrong-nonce snapshot must not drive readopt/requeue off rows it did not write).
	applied, err := s.ReplaceWorkerActiveRuns(ctx, qtx, updated, snapshot, snapshotModeHeartbeat)
	if err != nil {
		return store.Worker{}, err
	}
	var (
		readopted       []store.ReadoptRunsFromSnapshotRow
		missingFailed   []store.FailRunsMissingFromSnapshotRow
		missingRequeued []store.RequeueRunsMissingFromSnapshotRow
	)
	if applied {
		// (d) Reconcile, fail before requeue (over-cap fail-first ordering). The missing fence is the
		// stale window plus one heartbeat interval (D4). max_requeues is RUN_MAX_REQUEUES.
		missingCutoff := pgconv.Time(s.now().Add(-(s.p.WorkerHeartbeatStale + s.p.WorkerHeartbeatInterval)))
		maxRequeues := int32(s.p.RunMaxRequeues) //nolint:gosec // G115: RunMaxRequeues is a small bounded config int (env RUN_MAX_REQUEUES), never near int32 range
		readopted, err = qtx.ReadoptRunsFromSnapshot(ctx, updated.ID)
		if err != nil {
			return store.Worker{}, err
		}
		missingFailed, err = qtx.FailRunsMissingFromSnapshot(ctx, store.FailRunsMissingFromSnapshotParams{
			FailureReason: pgconv.TextOrNull("worker lost the execution; exceeded re-queue budget"),
			WorkerID:      pgconv.UUID(updated.ID),
			MissingCutoff: missingCutoff,
			MaxRequeues:   maxRequeues,
		})
		if err != nil {
			return store.Worker{}, err
		}
		missingRequeued, err = qtx.RequeueRunsMissingFromSnapshot(ctx, store.RequeueRunsMissingFromSnapshotParams{
			WorkerID:      pgconv.UUID(updated.ID),
			MissingCutoff: missingCutoff,
			MaxRequeues:   maxRequeues,
		})
		if err != nil {
			return store.Worker{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Worker{}, err
	}
	committed = true
	// (e) Publish + judge POST-COMMIT (never inside the tx), mirroring publishRegisterSweeps: a
	// readopt and a missing-requeue publish their live status; a missing-fail publishes 'failed'
	// AND funnels into the judge (PRD #46 Decision 2, the same worker-lost runs the sweeper funnels).
	for _, r := range readopted {
		s.publishSwept(r.ID, r.Status)
	}
	for _, r := range missingFailed {
		s.publishSwept(r.ID, r.Status)
		s.maybeEnqueueJudgeByID(ctx, r.ID)
	}
	for _, r := range missingRequeued {
		s.publishSwept(r.ID, r.Status)
	}
	return updated, nil
}

// snapshotRunIDs parses the run ids an ActiveSnapshot lists into uuids for the canonical pre-lock
// (PRD #1390 M2b). Unparseable ids are skipped (ReplaceWorkerActiveRuns rejects the whole snapshot
// on a bad uuid anyway; the pre-lock only needs the parseable ones for its FOR UPDATE). Returns nil
// for a nil/empty snapshot so the caller can skip the lock statement.
func snapshotRunIDs(snap *ActiveSnapshot) []uuid.UUID {
	if snap == nil || len(snap.Active) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(snap.Active))
	for _, e := range snap.Active {
		if id, err := uuid.Parse(e.RunID); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// diskOverThreshold: any reported volume at/above threshold. >= pins the comparator
// (0.90 fires, 0.899 does not). nil pair or non-positive total => not over (no div-by-zero).
func diskOverThreshold(stats *WorkerStats, threshold float64) bool {
	over := func(used, total *int64) bool {
		if used == nil || total == nil || *total <= 0 {
			return false
		}
		return float64(*used)/float64(*total) >= threshold
	}
	return over(stats.DiskNixBytes, stats.DiskNixTotalBytes) || over(stats.DiskDataBytes, stats.DiskDataTotalBytes)
}

// OutboxAggregate returns a worker's summed outbox depth (PRD #1391 M5) for the
// WorkerDTO overlay: pending messages, pending terminals, stale-retired frames, and
// the oldest blocked reason (nil when nothing is blocked). All zero and nil when the
// worker has no tracked depth, so a worker with no outbox shows nulls.
func (s *Service) OutboxAggregate(workerID uuid.UUID) (pendingMessages, pendingTerminal, staleRetired int, blocked *string) {
	return s.outbox.workerAggregate(workerID)
}

// OutboxRunDepth returns the outbox entry reported for a run AND the id of the worker
// that reported it, across whichever worker reported it last (PRD #1391 M5). The health
// detector uses it to turn a non-zero pending depth into the truthful "queued on the
// worker" reason instead of `stalled` — but ONLY when the reporting worker is the run's
// current owner, so it passes the reporting worker id back for the caller to owner-gate.
func (s *Service) OutboxRunDepth(runID uuid.UUID) (OutboxEntry, uuid.UUID, bool) {
	return s.outbox.runDepth(runID)
}

// blockedOutcomeReasons is the CLOSED set of blocked-terminal reasons the api will
// surface on a run (PRD #1391 M3, D13). The worker's heartbeat carries a free-text
// blocked_reason, but the worker is untrusted (fact 8: a worker cannot forge health,
// and the same posture applies to what it can put on the owner's run page), so only a
// recognised member of this set is ever passed through — every other value is dropped.
// The three members are the run-B permanent-refusal reasons: a completion-permit
// mismatch, a message hole too large to fill (gap_unrecoverable), and an exhausted
// terminal-journal reserve.
var blockedOutcomeReasons = map[string]struct{}{
	"completion_permit_mismatch": {},
	"gap_unrecoverable":          {},
	"reserve_exhausted":          {},
}

// RunBlockedOutcome reports the reason a run's OWNING worker is holding a finished
// terminal outcome it could not land (PRD #1391 M3, D13), for the GetRun DTO overlay.
// It reads the outbox tracker's per-run entry and returns its BlockedReason ONLY when
// it is a recognised member of blockedOutcomeReasons — arbitrary worker text is
// dropped (ok == false), so a hostile worker cannot inject a reason string onto the
// owner's run page.
//
// It also returns the reporting worker id so the caller OWNER-GATES the overlay,
// exactly as the health detector owner-gates OutboxRunDepth: runIndex is
// "last reporter wins" with no ownership check (see outbox_tracker.go), so a
// cross-tenant worker that knows a run's UUID could otherwise report a blocked reason
// for it, and a reclaim-during-outage leaves runIndex pointing at the prior worker
// until the TTL. The caller trusts the depth only when this id EQUALS the run's
// current owning worker.
func (s *Service) RunBlockedOutcome(runID uuid.UUID) (reason string, workerID uuid.UUID, ok bool) {
	entry, reporter, found := s.outbox.runDepth(runID)
	if !found {
		return "", uuid.UUID{}, false
	}
	if _, recognised := blockedOutcomeReasons[entry.BlockedReason]; !recognised {
		return "", uuid.UUID{}, false
	}
	return entry.BlockedReason, reporter, true
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
//
// snapshot is the worker's active-run snapshot riding the claim request (PRD #1390 M3, D3/D8),
// nil for an old bodyless worker. When it is carried AND a tx beginner is wired, the claim runs in
// ONE transaction in the canonical lock order — lock the worker row, pre-lock the request
// snapshot's own runs, replace the snapshot, then (claimant guard) refuse the claim if the worker
// is under an unexpired pending_overflow closure, else select and claim — so a run any fresh
// snapshot lists (its own request's included) is never double-claimed and no sibling can steal a
// pre-locked run until this tx commits. An invalid/stale/wrong-nonce claim snapshot fails the claim
// CLOSED (ErrActiveSnapshotInvalid → the handler's 400). Without a snapshot (or without a tx
// beginner: fake-store unit tests) it keeps the single auto-commit ClaimRun, which still applies
// the persisted-snapshot exclusions and the overflow closure. Assembly always runs OUTSIDE the tx.
func (s *Service) Claim(ctx context.Context, wkr store.Worker, snapshot *ActiveSnapshot) (*ClaimPayload, error) {
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

	// Drain gate (PRD #422 M3, narrowed by PRD #1030 M2): a cordoned worker claims
	// nothing NEW, so the controller can roll it once idle — but it MUST still be able
	// to re-claim its OWN promoted run so a run parked (limit_wait→queued, worker_id
	// retained for affinity) while its owner was cordoned for a roll resumes in place
	// on the same worker/PVC instead of being stolen cold by a live peer (run #1009).
	// So we no longer short-circuit here; instead ClaimRun is told the claimant is
	// draining (ClaimantDraining below), and its `NOT @claimant_draining OR
	// r.worker_id = @worker_id` clause scopes a draining claimant to its own runs.
	// draining_since is cleared on the worker's next register (after its roll), which
	// re-enables new claims. (This replaces the old `if wkr.DrainingSince.Valid {
	// return nil, nil }` early return; nothing downstream of Claim assumed a draining
	// worker never reaches ClaimRun — the vault gate above still fires, and the claim
	// assembly / recovery paths below are indifferent to the owner's drain state.)

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

	// PRD #1390 M3: the request snapshot's index-aligned (run_id, generation) pairs, feeding
	// ClaimRun's request-array exclusion so the claimant never re-claims a run its OWN request
	// snapshot lists at the current generation, before the first heartbeat persists those rows
	// (fact 7). Empty (nil-safe) when no snapshot rides the claim — an empty exclusion set.
	reqIDs, reqGens := requestActivePairs(snapshot)

	// ClaimRun params are built IDENTICALLY for both the no-snapshot auto-commit path and the
	// transactional path (only the request arrays differ, and they are empty in the no-snapshot
	// path). @snapshot_fresh_cutoff is the stale window plus one heartbeat interval (D3): a
	// worker_active_runs row reported within it fences the run out of every claim.
	params := store.ClaimRunParams{
		WorkerID:            pgconv.UUID(wkr.ID),
		UserID:              wkr.UserID,
		AffinityCutoff:      pgconv.Time(s.now().Add(-s.p.WorkerAffinityCeiling)),
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allowlist,
		WorkerCaps:          wkr.Capabilities,
		CapabilityAware:     capabilityAware,
		// PRD #1226 M1 (D2): the claiming worker's self-reported protocol capabilities,
		// read by the non-bypassable completion-protocol clause in ClaimRun. An interlocked
		// run is claimable only when this set contains 'completion_interlock_v1'; a legacy
		// run is unaffected. Deliberately NOT folded into WorkerCaps — it is a separate
		// column and a separate, override-proof clause.
		WorkerProtocolCaps: wkr.ProtocolCapabilities,
		// PRD #529 Decision 4: an ephemeral worker may claim only its bound run.
		IsEphemeral:           wkr.Ephemeral,
		EphemeralRunID:        wkr.EphemeralRunID,
		SpreadCutoff:          pgconv.Time(s.now().Add(-s.p.WorkerSpreadGrace)),
		BackgroundGraceCutoff: pgconv.Time(s.now().Add(-s.p.WorkerBackgroundGrace)),
		HeartbeatCutoff:       pgconv.Time(s.now().Add(-s.p.WorkerHeartbeatStale)),
		// PRD #1030 M2: a draining claimant is scoped to its own promoted run (see the
		// drain gate above and the `NOT @claimant_draining OR r.worker_id = @worker_id`
		// clause in ClaimRun). A non-draining worker passes false — a no-op.
		ClaimantDraining: wkr.DrainingSince.Valid,
		// PRD #1296 M1 (D2/D3/D4): the durable-recovery claim/custody contract.
		//   - CustodyHoldLimit gates admission: a claim is blocked once the owner holds
		//     >= this many unresolved (open) custody holds (owner-scoped, never global).
		//   - RecoveryCapable derives from the worker's advertised recovery_archive_v1
		//     protocol capability (D9 additive versioned contract): the custody hold is
		//     opened in the claim CTE only for a capable worker on a code-publishing
		//     profile, so an old worker on a supporting API is honestly unsupported rather
		//     than falsely promised recovery.
		//   - WorkerIdentity is the immutable provenance value recorded on the hold for a
		//     later AAD-authenticated post-terminal recovery retry (never nulled).
		CustodyHoldLimit: custodyHoldLimit,
		RecoveryCapable:  slices.Contains(wkr.ProtocolCapabilities, capability.RecoveryArchiveV1),
		WorkerIdentity:   workerIdentity(wkr),
		// PRD #1390 M3: the three snapshot-dedupe params. @snapshot_fresh_cutoff bounds the
		// persisted-snapshot freshness test; the request arrays are the claimant's own listed runs
		// (empty in the no-snapshot path). All three are always passed so an old worker still gets
		// the persisted-snapshot exclusions + the overflow closure.
		SnapshotFreshCutoff: pgconv.Time(s.now().Add(-(s.p.WorkerHeartbeatStale + s.p.WorkerHeartbeatInterval))),
		RequestActiveIds:    reqIDs,
		RequestActiveGens:   reqGens,
		// PRD #1551 M4 (D6): the curated Codex model vocabulary, feeding ClaimRun's non-bypassable
		// custom-Codex-model clause. Its effective-root CASE treats a run.model that IS a curated id
		// as the shipped root (curated ⇒ no new requirement) and otherwise the owner's Codex lane; a
		// custom (non-curated) effective root then requires codex_custom_model_v1. Sorted server-side.
		CodexCuratedModels: codexCuratedModelsSlice(),
	}

	// No request snapshot (an old worker) or no tx beginner wired (fake-store unit tests): keep
	// TODAY'S path — a single auto-commit ClaimRun, then assembleClaim. The predicate params above
	// still apply, so an old worker gets the persisted-snapshot exclusions and the overflow closure;
	// only the claimant guard (the flagged-worker-itself refusal) and the request pre-lock are
	// snapshot-only, and an old worker is never flagged and lists nothing. Either path ends in
	// finishRunClaim's exact-claim transaction (PRD #1590 M1), which refuses with no payload
	// when no tx beginner is wired, so a fake-store claim never delivers or mutates unfenced.
	if snapshot == nil || s.txBeginner == nil {
		run, err := s.q.ClaimRun(ctx, params)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil // idle
			}
			return nil, err
		}
		return s.assembleAndFinishRunClaim(ctx, wkr, run, params.RecoveryCapable)
	}

	// Snapshot carried + tx beginner: ONE transaction in the canonical lock order (D8), so a
	// sibling's concurrent FOR UPDATE SKIP LOCKED claim cannot see this claimant's pre-locked runs
	// or replaced snapshot until it commits.
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := store.New(tx)
	// (a) Lock the worker row FIRST — the canonical order shared with Register, Heartbeat and the
	// stale-worker passes.
	locked, err := qtx.GetWorkerForUpdate(ctx, wkr.ID)
	if err != nil {
		return nil, err
	}
	// (b) Pre-lock the request snapshot's own runs in deterministic id order (skip when empty), so a
	// sibling's concurrent FOR UPDATE SKIP LOCKED claim skips them until this tx commits its
	// replacement. An id for a run this worker does not own is a no-op (the WHERE excludes it).
	if ids := snapshotRunIDs(snapshot); len(ids) > 0 {
		if _, err := qtx.LockOwnedRunsByIDs(ctx, store.LockOwnedRunsByIDsParams{
			RunIds:   ids,
			WorkerID: pgconv.UUID(wkr.ID),
		}); err != nil {
			return nil, err
		}
	}
	// (c) Replace the snapshot in CLAIM mode: an invalid/stale-epoch/wrong-nonce snapshot returns
	// ErrActiveSnapshotInvalid — ROLLBACK (via the defer) and return it so the handler fails the
	// claim CLOSED (400, no claim, no side effect). A real DB error propagates the same way.
	if _, err := s.ReplaceWorkerActiveRuns(ctx, qtx, locked, snapshot, snapshotModeClaim); err != nil {
		return nil, err
	}
	// (d) Claimant guard (snapshot-replace BEFORE overflow, blocker 2). The replace just set/cleared
	// pending_overflow_until from the snapshot's pending_overflow flag; re-read the row to see the
	// CURRENT value. If the worker is under an unexpired overflow closure it is refused every claim,
	// unassigned runs included — but the replace is valid info that must persist, so COMMIT and
	// report idle. An UNFLAGGED valid snapshot cleared the closure here, so the claim proceeds in
	// this SAME tx below.
	reread, err := qtx.GetWorkerByID(ctx, wkr.ID)
	if err != nil {
		return nil, err
	}
	if reread.PendingOverflowUntil.Valid && reread.PendingOverflowUntil.Time.After(s.now()) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil // idle: the worker is overflowed, no claim
	}
	// (e) Claim inside the tx. On no candidate, COMMIT (the snapshot replace must persist) and report
	// idle; on success, COMMIT and then assemble OUTSIDE the tx (assembly opens credentials + builds
	// snapshots and must not hold the DB tx), exactly as the no-snapshot path does.
	run, err := qtx.ClaimRun(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if cerr := tx.Commit(ctx); cerr != nil {
				return nil, cerr
			}
			committed = true
			return nil, nil // idle
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	return s.assembleAndFinishRunClaim(ctx, wkr, run, params.RecoveryCapable)
}

// requestActivePairs extracts the index-aligned (run_id, claim_generation) pairs a claim's request
// snapshot lists (PRD #1390 M3), for ClaimRun's request-array exclusion. Only entries whose run_id
// parses as a uuid are kept, and the two slices stay index-aligned (a dropped entry drops from
// both). A nil/empty snapshot yields empty (non-nil) slices, which ClaimRun's WITH ORDINALITY zip
// reads as an empty exclusion set.
func requestActivePairs(snap *ActiveSnapshot) ([]uuid.UUID, []int64) {
	ids := []uuid.UUID{}
	gens := []int64{}
	if snap == nil {
		return ids, gens
	}
	for _, e := range snap.Active {
		id, err := uuid.Parse(e.RunID)
		if err != nil {
			continue
		}
		ids = append(ids, id)
		gens = append(gens, e.ClaimGeneration)
	}
	return ids, gens
}

// recoverClaimAssembly turns a failed CHAT-lane claim assembly into the right
// terminal/transient outcome, always reporting idle (nil payload) to the caller so a
// broken claim never wedges the worker's poll loop. The run lane no longer comes here:
// since PRD #1590 M1 every run-lane outcome settles in finishRunClaim's exact-claim
// transaction (claim_recovery.go), whose fenced writers mirror these arms:
//   - a locked vault is transient — reset the just-claimed run to queued (never
//     fail it), keeping worker_id for affinity and NOT bumping requeue_count, so a
//     persistently locked vault can't trip the requeue cap (mirrors
//     SweepClaimedNeverStarted);
//   - a missing/undecryptable credential (or a rejected tool package), and a #66
//     guardrail block at claim (D1 layer 3), are terminal — fail the run with the
//     safe (no-secret-bytes) reason and fire the failed notify;
//   - a vanished run (its forge connection cascade-deleted the repo → run) is dropped.
//
// A chat spends the owner's default token and has no custom Codex root, so it produces
// neither errAutoPoolEmpty nor errCustomModelCapabilityMissing and has no arm for them:
// either would fall through to the default and propagate.
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
			FailureReason: pgconv.TextOrNull(err.Error()),
			FailOrigin:    pgconv.TextOrNull(failOrigin),
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

// errCustomModelCapabilityMissing marks a Codex claim whose effective worker-root model is a
// CUSTOM (non-curated) id that the claiming worker cannot run — it does not advertise
// codex_custom_model_v1 (PRD #1551 M4, D6). finishRunClaim treats it as a TRANSIENT
// requeue (like errVaultLocked, NOT the terminal errCredentialUnavailable): the run returns to
// queued for a capable worker rather than failing or silently running Astra. Its message carries
// no secret bytes.
var errCustomModelCapabilityMissing = errors.New("worker lacks codex_custom_model_v1 for a custom Codex root model")

// errGuardrailBlockedClaim marks a claim the #66 default-branch guardrail refused
// AT CLAIM (D1 layer 3, the security net): the bot can reach the repo's default
// branch, or that could not be verified (fail-closed). finishRunClaim treats
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
// HOLDABLE like errVaultLocked, and finishRunClaim HOLDS the run in the
// non-locking pool_wait status (PRD #754 M4, via RequeueClaimAssemblyExact's pool_wait
// arm) rather than failing it — M5 resumes it reactively or manually. Its message
// carries no secret bytes.
var errAutoPoolEmpty = errors.New("auto pool is empty")

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

// maxInflightTargets caps the self_improve in-flight avoid-set handed to the picker
// (issue #297): newest-first (ListActiveRunsAll is ORDER BY created_at DESC), so the
// most recently started runs win the cap. Advisory context, not a hard block.
const maxInflightTargets = 30

// maxInflightLineLen bounds one assembled in-flight coordinate line (issue #297): the
// titles are untrusted issue/milestone text of unbounded length, so a single line is
// trimmed to keep the avoid-set compact on the wire.
const maxInflightLineLen = 300

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

// errToolPackagesRejected marks a claim whose grandfathered tool packages fell out
// of the (possibly shrunk) allowlist at claim time. Non-retryable: the run is
// failed with this message (which lists the rejected package names — never secret
// bytes) so the owner fixes the profile or an admin restores the allowlist entry.
var errToolPackagesRejected = errors.New("tool packages no longer allowed")

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
	// ClaimGeneration is the runs.claim_generation the frame was produced under (PRD #1247 M9,
	// D7) — the epoch the usage fold attributes this frame's leg to. It is SERVER-POPULATED, not a
	// wire field (`json:"-"`): the batch generation rides its own top-level `claim_generation` on
	// the request, and appendMessages stamps the per-call effectiveClaimGen onto every frame it
	// folds; the refold reads each frame's PERSISTED run_messages.claim_generation. nil = legacy /
	// unknown (a pre-feature frame, or a legacy worker's unfenced batch), folded as NULL provenance.
	ClaimGeneration *int64 `json:"-"`
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
	// UsageBasis is the worker's per-frame reading marker (ADR-1562). The only value the
	// fold honours is the exact string "session_cumulative", stamped on a Claude result
	// frame whose modelUsage is the running total of the session the leg continued (SDK >=
	// 0.3.277). Any other value, or absence, is per_leg — the ADR-1079 semantics of every
	// pre-#1562 frame, every Codex frame and the stub executor. The marker is NEVER honoured
	// on a Codex run (see foldUsageFrames), and it is a server-internal marker the worker
	// stamps, not a public field.
	UsageBasis string `json:"usage_basis"`
}

type resultModelUsage struct {
	InputTokens              int64 `json:"inputTokens"`
	OutputTokens             int64 `json:"outputTokens"`
	CacheReadInputTokens     int64 `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64 `json:"cacheCreationInputTokens"`
	// CostUSD is the agent's per-model provider dollar cost, decoded as RAW JSON (PRD
	// #1332 M5A / m4) rather than float64: a non-numeric token (e.g. "lots", {}) on ONE
	// sibling model must not reject the whole frame and lose every other model's usage.
	// resolveCostUSD collapses it to an (amount, present) pair — absent/empty or a
	// non-numeric/non-finite token is present=false, a finite JSON number is (value, true).
	CostUSD json.RawMessage `json:"costUSD"`
	// CostStatus is the agent's CLOSED per-model cost marker (PRD #1332 D5), decoded as
	// RAW JSON (m4) — again so a non-string token on one sibling never rejects the frame.
	// resolveCostStatusMarker collapses the raw token to the absent / invalid / valid
	// trichotomy:
	//   - ABSENT  — the key was omitted (nil/empty RawMessage: a pre-C4b or Claude frame), OR a
	//     JSON `null` literal — a `null` is treated as ABSENT for BOTH costStatus and costUSD → "";
	//   - INVALID — present but NOT a JSON string (false, {}, an array, a number): → the
	//     costMarkerInvalid sentinel, which deriveUsageCost routes to 'unreported' for Codex;
	//   - VALID   — a JSON string: the marker verbatim ("subscription" | "metered" | anything
	//     else), validated by deriveUsageCost's closed switch, HONORED only for Codex.
	// It is HONORED only for Codex runs and only through that closed switch — a Claude row
	// ignores it, and no worker-supplied string ever reaches run_usage verbatim (the switch
	// emits only its three fixed literals).
	CostStatus json.RawMessage `json:"costStatus"`
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

// maxCostUSD is the numeric(12,6) ceiling ($999,999.999999). A single frame's
// cost is far below this, but the fold MUST clamp to it: a bogus costUSD >= 1e6
// would quantize past the column and raise Postgres 22003, failing the append —
// and the worker's batcher retries a failed batch at head forever (poison loop).
const maxCostUSD = 999999.999999

// StateRequest is the worker's report of a run's new state. Only the fields
// relevant to State are read. The wire key is `status` (matches the runs.status
// column and the M2 worker client); the Go field stays
// `State` to avoid churn in the switch below.
type StateRequest struct {
	State string `json:"status"` // running|awaiting_approval|awaiting_input|awaiting_followup|limit_wait|recovery_wait|paused|pause_failed|credential_switch|completed|failed
	// ClaimGeneration is the runs.claim_generation the worker believes it holds (PRD #1247
	// M5, D3). A CAPABILITY worker (credential_switch_v1) stamps it on EVERY mutating report;
	// SetState then fences the transition atomically on `claim_generation = @gen AND
	// claim_released_at IS NULL`, so a report from a flight whose claim was RELEASED (a
	// held-state switch requeued the run) or SUPERSEDED (a reclaim bumped the generation) is
	// rejected with a typed stale_claim disposition instead of mutating the run. It is ALSO
	// REQUIRED on a `credential_switch` release report (the release targets an exact
	// generation). Nullable and OPTIONAL: a LEGACY worker (no capability) omits it, and the
	// report is honoured UNFENCED exactly as before — back-compat by construction. The field
	// MUST exist here because httpx.DecodeJSON sets DisallowUnknownFields — a capability
	// worker that sends it would 400 otherwise. The SAME field is also the forge-park fence
	// (PRD #1392 M1): the forge_unreachable park transaction requires it to equal the locked
	// run's claim_generation and answers 409 stale_claim on a mismatch (#1247's precedence).
	ClaimGeneration *int64 `json:"claim_generation"`
	// MessagesThroughSeq is the terminal fence (PRD #1391 Run B M3c, D3): the highest message
	// seq the worker asserts it has durably delivered to the api at the moment it reports a
	// TERMINAL transition (completed/failed). When present, SetState refuses the terminal write
	// until run_messages holds a CONTIGUOUS [1..through] — runs.last_seq must have reached
	// `through` AND count(run_messages in [1..through]) must equal `through` (last_seq alone
	// cannot see a hole below it) — answering ErrMessagesPending (409, reason messages_pending)
	// otherwise so the worker fills the gap and re-reports. A missing count more than
	// WorkerGapFillMax below `through` is instead ErrGapUnrecoverable (409, reason
	// gap_unrecoverable): the hole is too large to ever fill, so the worker must stop re-parking.
	// Nullable and OPTIONAL — a legacy worker omits it and the terminal transition is UNFENCED,
	// byte-identical to before. The field MUST exist here because httpx.DecodeJSON sets
	// DisallowUnknownFields — a fence-capable worker that sends it would 400 otherwise. A negative
	// value is invalid (ErrInvalidState). Ignored on every non-terminal report.
	MessagesThroughSeq *int64  `json:"messages_through_seq"`
	PlanMd             *string `json:"plan_md"`
	Branch             *string `json:"branch"`
	MrIID              *int64  `json:"mr_iid"`
	// Head is the EXACT source-branch tip H a `completed` report is being made against
	// (PRD #1226 M2, D5). It is REQUIRED for an INTERLOCKED run's completion: SetState routes
	// such a completion through completeRunWithPermit, which consumes the permit issued for
	// (run, contract_revision, Head) and writes `completed` in one transaction. A completed
	// report whose Head does not match an unconsumed permit leaves the run non-terminal
	// (applied=false). Absent on a LEGACY run's completion (that path is byte-for-byte
	// unchanged) and on every non-completed report. httpx.DecodeJSON rejects unknown fields,
	// so this field MUST exist here or a new worker's report 400s.
	Head *string `json:"head"`
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
	// Proposal (PRD #929 M2) carries a scheduled issues-mode prompt run's structured
	// idea — a title + body — that the SERVER files as a forge issue on run completion
	// (Design C: the agent gains no forge-write tool; the api does the writing). It is a
	// DECLARATION by an untrusted worker on the terminal `completed` report, kind-gated
	// (prompt/scheduled runs only) and clamped server-side (control-char stripped,
	// secret-scrubbed, length-bounded — see clampWireProposal) before it is filed.
	// Absent (nil) on every normal completion and on a no-proposal no-op run, so an old
	// worker's payload and a new worker's plain completion stay byte-identical on the
	// wire. httpx.DecodeJSON rejects unknown fields, so this field MUST exist here or a
	// new worker's report 400s.
	Proposal *ProposalPayload `json:"proposal"`
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
	// BranchMoved (issue #1117) is the worker's DECLARATION, on an mr_rework `failed` report,
	// that the finalize push was rejected non-fast-forward because a concurrent same-branch
	// writer advanced the MR branch. UNTRUSTED like ScopeCapped: the server honors it ONLY when
	// the run's own kind is mr_rework (owned.Kind == runkind.MRRework), routing to a
	// 'cancelled'/'branch_moved' disposition instead of defaulting to 'agent_failure'. Absent/
	// false on every other report ⇒ byte-identical to before.
	BranchMoved *bool `json:"branch_moved"`
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
	// MilestonesAgents is the lead's OPTIONAL per-in-progress-milestone agent attribution
	// (PRD #1224). RAW, UNVALIDATED wire input decoded here; milestoneAgentsParam
	// per-entry-validates it (Decision 4/5) and couples the write to the validated in_progress
	// set (Decision 6). Absent (nil) ⇒ nothing declared. Rides `running` reports only.
	MilestonesAgents *[]apitypes.MilestoneAgent `json:"milestones_agents"`
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
	// CompletionQuestion (PRD #1226 M5) is the worker's DECLARATION, on an `awaiting_input`
	// report, that the question it is parking on is a COMPLETION-interlock question (the
	// worker authored it while awaiting the owner's continue decision) rather than an
	// ordinary PRD #88 ask_user clarification. It drives the dedicated completion-question
	// marker (completion_question_at): true → SetState stamps the marker to now(), so
	// completionQuestionOpen / completionPhaseRule can tell the D6 completion-question window
	// from an ordinary clarification and an owner completion-continue can never resolve the
	// wrong question. Absent/false on an ordinary ask_user report (byte-identical to the
	// pre-marker behavior) and ignored on every non-`awaiting_input` state. The agent unit
	// sends it in a later change; httpx.DecodeJSON rejects unknown fields, so this field MUST
	// exist here or a new worker's report 400s.
	CompletionQuestion bool `json:"completion_question"`
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
	// RecoveryCause is the worker's TYPED cause for a 'recovery_wait' park (PRD #1392 M1).
	// UNTRUSTED free text on arrival: SetState validates it against the server enum
	// {forge_unreachable, empty_turn, provider_outage} BEFORE any state SQL and rejects an
	// unknown non-nil value as ErrInvalidState (400), so a garbled cause can never reach the
	// constrained column. Only recovery_cause == "forge_unreachable" triggers the dedicated
	// custody-settling park transaction; the other causes take the ordinary untyped park (which
	// writes cause NULL, D9). Absent (nil) on every non-recovery_wait report and on a legacy
	// worker's empty-turn park. httpx.DecodeJSON rejects unknown fields, so this field MUST
	// exist here or a new worker's report 400s.
	RecoveryCause *string `json:"recovery_cause"`
}

// ProposalPayload is the structured idea a scheduled issues-mode prompt run emits on
// its terminal `completed` report (PRD #929 M2). The server files it as a forge issue
// via clampWireProposal → maybeFileProposal. Both fields are untrusted worker output
// and are control-char-stripped, secret-scrubbed and length-bounded before filing.
type ProposalPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// terminalMessageFence is the terminal fence (PRD #1391 Run B M3c, D3). When a terminal
// (completed/failed) report carries messages_through_seq, it refuses the transition — BEFORE any
// mutating write — unless run_messages holds a CONTIGUOUS [1..through]:
//
//   - a NEGATIVE through is invalid (ErrInvalidState → 400);
//   - MORE than WorkerGapFillMax seqs missing from [1..through] is ErrGapUnrecoverable — the hole
//     is too large to ever fill, so the worker must stop re-parking on it (409 gap_unrecoverable);
//   - any REMAINING shortfall — last_seq below through, or a hole below it (stored count < through)
//     — is ErrMessagesPending, a recoverable refusal (409 messages_pending).
//
// A nil messages_through_seq leaves the transition UNFENCED (a legacy worker), byte-identical to
// before. It runs the count on the SAME q the caller mutates through — the tx-bound qtx when the
// generation FOR UPDATE fence is engaged, else the pool — so the contiguity read and the terminal
// write share one snapshot. gap_unrecoverable is checked BEFORE messages_pending so a hole too
// large to fill is never mislabelled as merely pending.
func (s *Service) terminalMessageFence(ctx context.Context, q Store, runID uuid.UUID, owned store.Run, req StateRequest) error {
	if req.MessagesThroughSeq == nil {
		return nil
	}
	through := *req.MessagesThroughSeq
	if through < 0 {
		return fmt.Errorf("%w: messages_through_seq must be non-negative", ErrInvalidState)
	}
	// A message seq is an int32 column, so a run can never store `through` CONTIGUOUS messages once
	// through exceeds int32 — the hole is unrecoverable by definition. Guarding here also makes the
	// int32(through) cast at the count query provably in range (gosec G115).
	if through > math.MaxInt32 {
		return ErrGapUnrecoverable
	}
	// Cheap pre-check: last_seq is the high-water mark, so stored count <= last_seq. If the mark
	// ALONE is more than a fill window short of `through`, the hole is unrecoverable no matter what
	// is stored below it — answer without the count query.
	if through-int64(owned.LastSeq) > int64(s.p.WorkerGapFillMax) {
		return ErrGapUnrecoverable
	}
	// Contiguity count over the run_messages (run_id, seq) unique index. A hole BELOW last_seq is
	// invisible to the high-water mark, so this — not runs.last_seq — is what proves [1..through]
	// is gapless.
	stored, cerr := q.CountRunMessagesThrough(ctx, store.CountRunMessagesThroughParams{RunID: runID, Through: int32(through)})
	if cerr != nil {
		return cerr
	}
	if through-int64(stored) > int64(s.p.WorkerGapFillMax) {
		return ErrGapUnrecoverable
	}
	if int64(stored) < through {
		return ErrMessagesPending
	}
	return nil
}

// SetState applies a worker's state transition and returns the run's resulting
// row plus whether the transition was applied. All transitions are guarded
// against terminal statuses, so a report that lands on an already-terminal run
// (e.g. a cancel raced in) is a no-op: applied is false and the run's real
// status is returned. The handler maps applied==false to 409 (the worker treats
// "already terminal" as success and learns it was cancelled), per the M2 wire
// contract.
func (s *Service) SetState(ctx context.Context, wkr store.Worker, runID uuid.UUID, req StateRequest) (run store.Run, applied bool, err error) {
	// PRD #1392 M1: validate the TYPED recovery cause against the server enum BEFORE any state
	// SQL, so an unknown non-nil value is a loud 400 (ErrInvalidState) rather than a
	// constraint violation at the park write. Absent (nil) is fine (an ordinary report or a
	// legacy untyped park). Only forge_unreachable triggers the dedicated park transaction
	// below; empty_turn/provider_outage are accepted here (reserved, D9) but take the ordinary
	// untyped park, which writes cause NULL.
	if req.RecoveryCause != nil && serverRecoveryWaitCauses[*req.RecoveryCause] {
		return store.Run{}, false, fmt.Errorf("%w: recovery_cause %q is server-only", ErrInvalidState, *req.RecoveryCause)
	}
	if req.RecoveryCause != nil && !recoveryWaitCauses[*req.RecoveryCause] {
		return store.Run{}, false, fmt.Errorf("%w: unknown recovery_cause %q", ErrInvalidState, *req.RecoveryCause)
	}
	owned, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return store.Run{}, false, err
	}
	sessionID := stripNULParam(req.SessionID)
	// PRD #1190 M1: pause_failed is a worker REPORT, not a status transition — the worker could
	// not publish the checkpoint, so the run STAYS running (Decision 8) and only the pending
	// pause is withdrawn. Handle it here, BEFORE the switch, so it bypasses the terminal-
	// transition fan-out below (a running run enqueues no judge, files no proposal, tears down
	// nothing). It clears the request keyed on the worker, re-reads the (unchanged) run,
	// broadcasts its running status so the web drops the pause chip, and returns. The worker's
	// own feed message carries the human-readable reason to the run's activity feed.
	//
	// pause_failed intentionally posts no Slack line — the notifier observes only status
	// transitions and pause_failed is not one; the in-app feed message carries the reason. A
	// Slack DM would need a reason-carrying handler→slacksvc seam (deferred).
	if req.State == "pause_failed" {
		// PRD #1247 M5: FAIL CLOSED for a CAPABILITY worker. pause_failed is handled BEFORE the
		// generic fail-closed check below, so its own check lives here: a worker advertising
		// credential_switch_v1 that OMITS claim_generation is refused (ErrMissingClaimGeneration →
		// the same 409 the handler maps it to) rather than clearing the pause unfenced. A LEGACY
		// worker (no capability, nil generation) skips the fence, byte-identical to before.
		if req.ClaimGeneration == nil && slices.Contains(wkr.ProtocolCapabilities, capability.CredentialSwitchV1) {
			return owned, false, ErrMissingClaimGeneration
		}
		// The nullable claim_generation fences the clear: a stale flight (its claim released by a
		// held-state switch, or superseded by a reclaim) matches 0 rows, so it clears nothing and
		// cannot withdraw the NEW flight's pending pause. The run stays running either way (the
		// rowcount is intentionally not read — the fence is the protection, not a re-read).
		if _, cerr := s.q.ClearPauseRequest(ctx, store.ClearPauseRequestParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: pgconv.Int8Ptr(req.ClaimGeneration)}); cerr != nil {
			return store.Run{}, false, cerr
		}
		run, err = s.runOwnedByWorker(ctx, runID, wkr)
		if err != nil {
			return store.Run{}, false, err
		}
		if s.bcast != nil {
			s.bcast.PublishState(runID, run.Status)
		}
		return run, true, nil
	}
	// PRD #1247 M5 (D3/D4/D14): the held-state credential-switch RELEASE report. It is NOT a
	// generic mutation — a redelivered same-generation release after the release already
	// applied (a lost ack) must converge to idempotent success, not a stale rejection — so it
	// is handled by its own fenced+idempotent helper BEFORE the generation fence below (whose
	// generic stale check would wrongly reject an already-released redelivery).
	if req.State == "credential_switch" {
		return s.releaseCredentialSwitch(ctx, owned, wkr, req)
	}
	// PRD #1247 M5 (D3 step 3 / D14): the bounded capture-failure GIVE-UP. After a bounded number
	// of failed verified-restore-point captures the worker abandons the switch and reports
	// {status:"credential_switch_failed", claim_generation}; the server CLEARS the pending switch
	// stamp WITHOUT changing status — the run keeps running on its current token. Like pause_failed
	// it withdraws a pending request (a SWITCH, not a PAUSE) rather than transitioning, so it is
	// handled by its own fenced+idempotent helper BEFORE the generation fence below (whose generic
	// stale check would wrongly reject an already-cleared redelivery).
	if req.State == "credential_switch_failed" {
		return s.failCredentialSwitch(ctx, owned, wkr, req)
	}
	// PRD #1247 M5a-1 rework (auditor fail-open finding): FAIL CLOSED for a CAPABILITY worker. The
	// fence below engages only when the report STAMPS a generation, so a worker advertising
	// credential_switch_v1 could otherwise bypass it entirely by OMITTING claim_generation on a
	// mutating report. Make "legacy" a per-WORKER-capability property, not a per-request
	// field-presence choice: a worker that advertises the capability MUST stamp the generation on
	// every fenced mutating transition, so an omission is refused (ErrMissingClaimGeneration → the
	// same 409/stale_claim ack as ErrStaleClaim). A LEGACY worker (no capability) is still honoured
	// unfenced when it omits the field — back-compat by construction. INTERLOCKED completion is
	// excluded here (stateUsesGenerationFence is false for it) and enforces the identical check
	// inside completeRunWithPermit's permit transaction, where the rest of that arm's fence lives.
	if req.ClaimGeneration == nil &&
		stateUsesGenerationFence(req.State, owned) &&
		slices.Contains(wkr.ProtocolCapabilities, capability.CredentialSwitchV1) {
		return owned, false, ErrMissingClaimGeneration
	}
	// PRD #1247 M5 (D3): the released-generation fence. A CAPABILITY worker stamps
	// req.ClaimGeneration on every mutating report; we then run the state transition under a
	// FOR UPDATE lock on the run row and REJECT the report (ErrStaleClaim) when the run's
	// generation has moved past the reported one (a reclaim) or its claim has been RELEASED (a
	// held-state switch requeued it). Holding the row lock from the check THROUGH the mutation
	// is what makes it atomic and serialized against ClaimRun / ReleaseCredentialSwitch — a
	// Go-side check on a stale read then an unfenced UPDATE would be a TOCTOU (D3). A LEGACY
	// report (nil generation, no capability) skips the tx entirely and runs UNFENCED exactly as
	// before — back-compat by construction. The single-statement arms run their mutation
	// through the tx-bound `q`; the multi-query park helpers (limit_wait/recovery_wait) and the
	// interlocked-completion permit transaction do NOT nest under this lock — see
	// stateUsesGenerationFence for why each is covered by its own guard.
	q := Store(s.q)
	var fenceTx pgx.Tx
	defer func() {
		if fenceTx != nil {
			_ = fenceTx.Rollback(ctx)
		}
	}()
	// PRD #1497 M1 (D16): close the released window for a generation-less (legacy) report too. A
	// nil-generation report on a fenced state whose claim is RELEASED (a held-state switch, or a
	// server-side wall park set claim_released_at) is rejected as stale — the released window closes
	// for it. This is a best-effort check on the UNLOCKED `owned` snapshot: a legacy worker (no
	// generation) never reclaims, so the released flag is stable, and this path deliberately does NOT
	// take the FOR UPDATE tx (a generation-stamping worker takes the fence below, which re-checks
	// claim_released_at under the lock). A LIVE nil-generation report stays honoured, unchanged.
	if req.ClaimGeneration == nil && stateUsesForUpdateFence(req.State, owned) && owned.ClaimReleasedAt.Valid {
		return owned, false, ErrStaleClaim
	}
	if req.ClaimGeneration != nil && stateUsesForUpdateFence(req.State, owned) {
		if s.txBeginner == nil {
			return store.Run{}, false, fmt.Errorf("state fence unavailable: no tx beginner wired for run %s", runID)
		}
		tx, berr := s.txBeginner.Begin(ctx)
		if berr != nil {
			return store.Run{}, false, berr
		}
		fenceTx = tx
		qtx := store.New(tx)
		locked, lerr := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
		if lerr != nil {
			if errors.Is(lerr, pgx.ErrNoRows) {
				// The run left this worker (a reclaim by a DIFFERENT worker between the
				// unlocked ownership read and this FOR UPDATE) → not this worker's to mutate.
				return store.Run{}, false, ErrRunNotOwned
			}
			return store.Run{}, false, lerr
		}
		if locked.ClaimGeneration != *req.ClaimGeneration || locked.ClaimReleasedAt.Valid {
			// Stale: a reclaim bumped the generation past the reported one, or a switch/wall-park
			// released this claim. Return the LOCKED row so the handler can render it beside the
			// stale_claim disposition. The deferred Rollback releases the lock.
			return locked, false, ErrStaleClaim
		}
		// Locked and current: the arms below mutate through the tx, and the lock is held
		// until the post-mutation commit, so no ClaimRun/release can interleave. Use the
		// freshly-locked snapshot for the arm's own reads of the run row.
		owned = locked
		q = qtx
	}
	var rows int64
	// PRD #634 M4: the disposition to settle the pending scope audit row(s) with, decided in
	// the `completed` case and applied best-effort in the applied-transition block below.
	// Empty means "no settle" (every non-completed transition leaves it empty).
	var settleScopeDisposition string
	switch req.State {
	case "running":
		// RC1 (issue #1197): an AUTOPILOT run auto-approves its own plan and NEVER reports
		// awaiting_approval, so its approved plan text rides THIS self-contained `running`
		// report instead. Persist it via the guarded, idempotent SetRunAutopilotPlan whose
		// affected-row count PROVES storage, BEFORE SetRunRunning acknowledges the report —
		// so a successful running ack means the plan is durably stored and a resume enters
		// implementation instead of re-planning. Present body only (req.PlanMd is *string;
		// nil ⇒ an ordinary heartbeat, unchanged). A present-but-blank body is REJECTED (not
		// silently dropped to "omitted"), mirroring the awaiting_input qid rejection: a
		// running report that carries a plan_md key means the worker intends to store a plan.
		if req.PlanMd != nil {
			// Reuse the awaiting_approval NUL-strip conversion (stripNULParam), and
			// separately trim to reject a blank/whitespace body.
			planBody := stripNULParam(req.PlanMd)
			clean, _ := stripNUL(*req.PlanMd)
			if strings.TrimSpace(clean) == "" {
				return store.Run{}, false, fmt.Errorf("%w: running report carries a blank plan_md", ErrInvalidState)
			}
			var planRows int64
			planRows, err = q.SetRunAutopilotPlan(ctx, store.SetRunAutopilotPlanParams{
				PlanMd:   planBody,
				ID:       runID,
				WorkerID: pgconv.UUID(wkr.ID),
			})
			if err != nil {
				return store.Run{}, false, err
			}
			if planRows == 0 {
				// A concurrent cancel/park is a state refusal, not an invalid plan.
				// Re-read after the guarded write so the worker receives the authoritative
				// state through the ordinary applied=false / 409 contract. The initial
				// ownership snapshot predates the race (issue #1197, verified 2026-09-08).
				current, readErr := s.runOwnedByWorker(ctx, runID, wkr)
				if readErr != nil {
					return store.Run{}, false, readErr
				}
				if current.Status != "claimed" && current.Status != "running" {
					return current, false, nil
				}
				// While still writable, a human-gated/seeded/different-body refusal
				// remains an invalid request. Never acknowledge the plan as stored or
				// continue to SetRunRunning after either kind of refusal.
				return store.Run{}, false, fmt.Errorf("%w: autopilot plan_md was not durably stored (run not in a writable autopilot-approved state)", ErrInvalidState)
			}
		}
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
		runningParams.WorkerID = pgconv.UUID(wkr.ID)
		// PRD #122 M2 (Decision 5/5b): the budget-scaling config the SQL freeze reads.
		// Harmless on every heartbeat (the query COALESCEs the derived budget against the
		// existing immutable columns, so only the FIRST report that carries a frozen list
		// writes it — a later report re-supplies the same config and changes nothing).
		runningParams.RunMaxIterations = int32(s.p.RunMaxIterations) //nolint:gosec // G115: RunMaxIterations is a small bounded config int (env RUN_MAX_ITERATIONS), never near int32 range
		runningParams.RunTimeoutSeconds = int32(s.p.RunTimeout.Seconds())
		runningParams.MilestoneBudgetCap = milestoneBudgetCap
		runningParams.SizeBudgetFactorL = sizeBudgetFactorL
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
			if _, cerr := q.ClearRunMilestonesCompleted(ctx, store.ClearRunMilestonesCompletedParams{
				ID:       runID,
				WorkerID: pgconv.UUID(wkr.ID),
			}); cerr != nil {
				return store.Run{}, false, cerr
			}
		}
		rows, err = q.SetRunRunning(ctx, runningParams)
	case "awaiting_approval":
		// PRD #122 M1: the CANDIDATE milestone list rides the pre-approval report.
		// milestonesParam validates + kind-gates it (Decision 12/13) and returns NULL
		// when the list is absent, rejected, or from a non-issue run — the query writes
		// that directly, clearing the candidate (Decision 2: replaced each round). An
		// interlocked run goes through planMilestonesParam instead (issue #1626, below).
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
		rows, err = q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: stripNULParam(req.PlanMd), SessionID: sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
			// Issue #1626: an interlocked run's FIRST plan-bearing report with no milestones is the
			// explicit `[]` (planMilestonesParam), so the approve freeze builds a criteria:[]
			// contract. `owned` predates this report's plan_md write, which is what lets
			// planMilestonesParam tell a first report (stored plan_md NULL) from a later one. A
			// milestone-less re-presentation (a gate reclaim) or revise round keeps a stored
			// candidate unchanged, and after a REJECTED list (stored candidate NULL beside a
			// stored plan_md) it stays NULL: nothing freezes and the run holds at finalize.
			MilestonesCandidate:  planMilestonesParam(owned, req.PlanMd, req.Milestones),
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
		// PRD #1226 M5: stamp the completion-question marker to now() ONLY when the worker
		// flags this park as a COMPLETION question; an ordinary ask_user clarification passes
		// nil → SQL NULL, leaving the column NULL (byte-identical to the pre-marker behavior).
		var completionQuestionAt *time.Time
		if req.CompletionQuestion {
			now := time.Now().UTC()
			completionQuestionAt = &now
		}
		rows, err = q.SetRunAwaitingInput(ctx, store.SetRunAwaitingInputParams{
			OpenQuestionID:       pgconv.TextOrNull(qid),
			CompletionQuestionAt: pgconv.TimePtr(completionQuestionAt),
			SessionID:            sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
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
		if owned.Kind != runkind.Task || !owned.Interactive {
			return store.Run{}, false, fmt.Errorf("%w: awaiting_followup requires an interactive task run", ErrInvalidState)
		}
		rows, err = q.SetRunAwaitingFollowup(ctx, store.SetRunAwaitingFollowupParams{
			// int8Param maps nil → pgtype.Int8{} (Valid:false → SQL NULL), so an old
			// worker that omits open_followup_id lands NULL and the query's COALESCE
			// fallback recomputes the server-derived max-consumed watermark. A present
			// value is clamped to ≤ max-consumed by the query's LEAST.
			OpenFollowupID: pgconv.Int8Ptr(req.OpenFollowupID),
			SessionID:      sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
		})
	case "completed":
		// PRD #1391 Run B M3c (D3): the terminal fence, BEFORE any mutating write (the
		// SetRunCompleted / completeRunWithPermit calls below). When the report carries
		// messages_through_seq it refuses a completion whose run_messages are not yet contiguous
		// through that seq (ErrMessagesPending) or whose hole is too large to fill
		// (ErrGapUnrecoverable). A legacy worker omits it and this is a no-op. Runs on q, which is
		// the tx-bound qtx when the generation FOR UPDATE fence is engaged (a non-interlocked
		// capability completion), else the pool.
		if ferr := s.terminalMessageFence(ctx, q, runID, owned, req); ferr != nil {
			return owned, false, ferr
		}
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
		//
		// PRD #1227 M2 (D2): the disjoint else-branch stamps stop_kind='scope_reduced' on a
		// completion whose frozen contract carries an owner-deferred set (scope.out non-empty).
		// This is SERVER-DERIVED from the contract the owner's `partial` decision revised — the
		// worker sends no wire signal for it, so it cannot mint a disposition the owner never
		// authorized. The two are effectively disjoint: scope_capped keys on the operator's
		// scope_ceiling directive, scope_reduced on the contract's deferred set; scope_capped
		// wins if both somehow apply, and scope_capped's meaning is NOT widened.
		var scopeStopKind pgtype.Text
		if req.ScopeCapped != nil && *req.ScopeCapped && owned.ScopeCeiling.Valid {
			scopeStopKind = pgconv.TextOrNull("scope_capped")
		} else if contractHasDeferrals(owned.CompletionContract) {
			scopeStopKind = pgconv.TextOrNull("scope_reduced")
		}
		// PRD #634 M4: settle the still-pending scope audit row at completion. 'applied' when
		// this is a genuine scope_capped completion (see the #1227 guard below); 'declined' otherwise —
		// a run that completed normally despite a scope directive (e.g. the lead under-reported
		// so the gate never fired; the directive did not change behavior). 'declined' is set
		// ONLY when the run actually carried a scope directive (owned.ScopeCeiling.Valid);
		// without one settleScopeDisposition stays "" and the post-switch settle block is
		// skipped, so a normal completion issues no 0-row UPDATE. Applied best-effort after the
		// transition commits (rows>0), so a no-op onto an already-terminal run does not re-settle.
		//
		// PRD #1227 M2: guard 'applied' on the disposition being exactly "scope_capped" — a
		// scope_reduced completion must NOT settle the #634 scope-ceiling audit row 'applied'
		// (the scope_ceiling directive did not drive this finalize). A scope_reduced completion
		// then falls to the else-if and settles 'declined' ONLY if the run ALSO carried a
		// scope_ceiling directive; a run with deferrals but no scope_ceiling settles nothing.
		if scopeStopKind.Valid && scopeStopKind.String == "scope_capped" {
			settleScopeDisposition = "applied"
		} else if owned.ScopeCeiling.Valid {
			settleScopeDisposition = "declined"
		}
		completedParams := store.SetRunCompletedParams{
			Branch: stripNULParam(req.Branch), MrIid: pgconv.Int8Ptr(req.MrIID), MrWebUrl: stripNULParam(req.MrWebURL), SessionID: sessionID,
			FixVerdict:          clampWireFixVerdict(req.FixVerdict),
			PrdDonePath:         clampWirePRDDonePath(owned, req.PrdDonePath),
			ReportOnly:          reportOnly,
			ReportMd:            clampWireReportMd(owned, req.ReportMd, reportOnly),
			MilestonesCompleted: completedIDs,
			StopKind:            scopeStopKind,
			ID:                  runID, WorkerID: pgconv.UUID(wkr.ID),
		}
		// PRD #1226 M2 (D4): an INTERLOCKED run's terminal completion is PERMIT-GATED and runs
		// through a service transaction (completeRunWithPermit) that consumes the permit issued
		// for the exact (run, contract_revision, head) and writes `completed` atomically, with a
		// seam for later generation activation. A gated completion with no matching unconsumed
		// permit updates nothing and stays non-terminal (rows==0 -> applied=false -> 409). A
		// retry after response loss (run already completed, this worker's permit consumed) is
		// idempotent success, returned early WITHOUT re-running the terminal automation below
		// (it fired on the original completion). A LEGACY run (completion_contract_version IS
		// NULL) NEVER permit-gates and keeps the existing direct SetRunCompleted path
		// byte-for-byte.
		if owned.CompletionContractVersion.Valid {
			var idempotent bool
			rows, idempotent, err = s.completeRunWithPermit(ctx, wkr, owned, req, completedParams)
			// PRD #1247 M5 (D3): the interlocked completion runs its OWN permit transaction, so
			// it cannot nest under an outer FOR UPDATE fence (self-deadlock) — it is
			// generation-fenced INSIDE that lock instead and returns ErrStaleClaim when a
			// released/reclaimed old flight tries to consume a stale permit. It ALSO enforces the
			// M5a-1 fail-closed check (a capability worker that omitted claim_generation →
			// ErrMissingClaimGeneration), since interlocked-completed does not pass through the
			// top-of-SetState check above. Surface either with the pre-tx run row so the handler
			// renders the stale_claim disposition.
			if errors.Is(err, ErrStaleClaim) || errors.Is(err, ErrMissingClaimGeneration) {
				return owned, false, err
			}
			if err == nil && idempotent {
				run, rerr := s.runOwnedByWorker(ctx, runID, wkr)
				return run, true, rerr
			}
		} else {
			rows, err = q.SetRunCompleted(ctx, completedParams)
		}
	case "limit_wait":
		rows, err = s.setLimitWait(ctx, owned, wkr, req, sessionID)
	case "recovery_wait":
		// PRD #1392 M1: the FORGE pre-clone park is a DEDICATED transaction — validate the claim
		// generation, lock the run, settle the exact-generation custody hold, and park (or
		// cancel, or fail past the cap) atomically. It COMMITS its own transaction and returns
		// the committed run + a rows count, then falls through to the shared post-switch fan-out
		// (re-read, broadcast, judge — which the pre-clone origin skips, teardown), so the whole
		// terminal automation is reused rather than duplicated. Its two typed precedence errors
		// (stale_claim / custody_unsettled) return EARLY carrying the run for the handler's
		// {run, reason} 409 body. A nil txBeginner cannot run the atomic settle, so it MUST NOT
		// fall through to the blind untyped park (that would park while leaking the generation's
		// hold); it takes today's safe FAILED path instead (D3: never release an unproven hold).
		if req.RecoveryCause != nil && *req.RecoveryCause == "forge_unreachable" {
			if s.txBeginner == nil {
				rows, err = s.failForgeUnsettleable(ctx, wkr, runID, req, sessionID)
				break
			}
			var frun store.Run
			frun, rows, err = s.parkForgeUnreachable(ctx, wkr, owned, req, sessionID)
			if err != nil {
				// stale_claim / custody_unsettled: carry the run so the handler can render the
				// 409 {run, reason} body; nothing was committed.
				return frun, false, err
			}
			break
		}
		// Transient-recovery park (issue #1197): a positively-empty SDK turn survived the
		// worker's bounded in-process retries, so the run parks on a server-owned capped
		// backoff and the sweeper auto-promotes it. This is the reusable transient-recovery
		// park primitive (any transient cause reports it and reuses the park->promote
		// lifecycle); it is NOT a usage limit — see setRecoveryWait / recoverywait.go.
		rows, err = s.setRecoveryWait(ctx, owned, wkr, req, sessionID)
	case "paused":
		// PRD #1190 M1: the owner-requested park. SetRunPaused has the SAME positive-source-guard
		// ack contract as limit_wait — the worker keys off the RETURNED status being literally
		// "paused", never off applied — so a refused park (0 rows) surfaces as 409 and the worker
		// cleans up. It clears the pending-pause columns (the request is now consumed).
		rows, err = q.SetRunPaused(ctx, store.SetRunPausedParams{
			SessionID: sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
		})
	case "failed":
		// PRD #1391 Run B M3c (D3): the terminal fence, BEFORE any mutating write (the SetRunFailed /
		// CancelRunByWorker / SupersedeRunByWorker calls below). Same contract as the completed arm:
		// a `failed` report carrying messages_through_seq is refused until run_messages are contiguous
		// through that seq (ErrMessagesPending), or told the hole is unrecoverable
		// (ErrGapUnrecoverable). A legacy worker omits it and this is a no-op.
		if ferr := s.terminalMessageFence(ctx, q, runID, owned, req); ferr != nil {
			return owned, false, ferr
		}
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
			rows, err = q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{
				ID: runID, WorkerID: pgconv.UUID(wkr.ID),
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
			rows, err = q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{
				ID: runID, WorkerID: pgconv.UUID(wkr.ID),
			})
		case owned.StopKind.Valid && owned.StopKind.String == "plan_rejected":
			// A live plan-reject: stamp fail_origin='plan_rejected' (overriding the untrusted
			// req.FailOrigin, which the worker cannot forge), matching the server-side
			// RejectRunServerSide path rather than defaulting to agent_failure.
			rows, err = q.SetRunFailed(ctx, store.SetRunFailedParams{
				FailureReason:  limitAwareFailureReason(req),
				FailOrigin:     pgconv.TextOrNull("plan_rejected"),
				PreservedPatch: clampWirePreservedPatch(req.PreservedPatch),
				SessionID:      sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
				// PRD #1247 M5a-1 rework (m6): explicit nil. A fenced (non-chat, generation-bearing)
				// capability report was already generation-checked under the outer FOR UPDATE fence
				// upstream, so the per-query fence is redundant here; a legacy or chat report carries no
				// generation (chat is deliberately fence-exempt), so nil is correct there too. Behavior
				// preserved.
				ClaimGeneration: pgtype.Int8{},
			})
		case req.BranchMoved != nil && *req.BranchMoved && owned.Kind == runkind.MRRework:
			// Issue #1117: an mr_rework finalize push rejected non-fast-forward because a concurrent
			// same-branch writer advanced the MR branch is a benign, expected race (the "double-fix
			// collision" the uzi-watcher warns about), NOT an agent failure. Route to a distinct
			// 'cancelled'/stop_kind='branch_moved' disposition (fail_origin NULL, not judged — Gate 0)
			// rather than the agent_failure default. GUARDED on the run's own kind (a server-known fact,
			// like ScopeCapped's scope_ceiling guard) so an untrusted worker cannot mint the benign
			// disposition on any other kind. Placed after the operator-stop arms so a concurrent operator
			// cancel/stop (which pre-stamped owned.StopKind) still wins.
			//
			// PRD #1247 fix round: use the TX-bound q (not s.q). This arm runs under the outer
			// FOR UPDATE fence (q rebound to qtx), which holds a row lock on runID; issuing the
			// supersede on the POOL (s.q) would wait on the transaction's own uncommitted lock until
			// the context deadline, hanging every capability-worker mr_rework branch_moved report.
			// Every sibling arm uses q for exactly this reason.
			rows, err = q.SupersedeRunByWorker(ctx, store.SupersedeRunByWorkerParams{
				ID: runID, WorkerID: pgconv.UUID(wkr.ID),
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
			rows, err = q.SetRunFailed(ctx, store.SetRunFailedParams{
				// PRD #35 §7.8: when a `failed` report carries the structured limit fields,
				// the SERVER composes the sentence from its own allowlisted enum and replaces
				// whatever text the worker sent. That is the opt-out path (a run with
				// wait_on_limit=false reports failed directly), and letting the worker compose
				// it would put the enum on the untrusted side of the wire — the criterion "a
				// compromised worker cannot smuggle a non-enum rate_limit_type past the
				// server" would then be false on exactly the path a human reads. When the
				// fields are absent this is nil and every other failure path is untouched.
				FailureReason:  limitAwareFailureReason(req),
				FailOrigin:     pgconv.TextOrNull(failOrigin),
				PreservedPatch: clampWirePreservedPatch(req.PreservedPatch),
				SessionID:      sessionID, ID: runID, WorkerID: pgconv.UUID(wkr.ID),
				// PRD #1247 M5a-1 rework (m6): explicit nil. A fenced (non-chat, generation-bearing)
				// capability report was already generation-checked under the outer FOR UPDATE fence
				// upstream, so the per-query fence is redundant here; a legacy or chat report skips the
				// lock and carries no generation (chat is deliberately fence-exempt), so nil is correct
				// there too. Behavior preserved.
				ClaimGeneration: pgtype.Int8{},
			})
		}
	default:
		return store.Run{}, false, ErrInvalidState
	}
	if err != nil {
		return store.Run{}, false, err
	}
	// PRD #1247 M5 (D3): the fenced transition applied — COMMIT it, releasing the FOR UPDATE
	// lock, BEFORE the post-transition automation below. That automation runs on s.q (a
	// separate pool connection) and re-reads the run, so it must not run while this tx holds
	// the row lock (the re-read would see the pre-commit row, and any run-row write would
	// self-deadlock against the lock). A commit failure fails the report; the deferred Rollback
	// is a no-op once fenceTx is cleared. A legacy (nil-generation) report left fenceTx nil, so this
	// is skipped.
	if fenceTx != nil {
		if cerr := fenceTx.Commit(ctx); cerr != nil {
			return store.Run{}, false, cerr
		}
		fenceTx = nil
	}
	// issue #329: record the MR the worker opened INDEPENDENT of the terminal status
	// the switch above wrote. If SetRunCompleted applied, ReconcileRunMR is a COALESCE
	// no-op; if the status transition no-oped (e.g. the run was cancelled or already
	// terminal), this still captures the MR so the run never reports "MR: none".
	// Best-effort: a failure here must not fail the worker's terminal report.
	if req.MrIID != nil {
		if _, mrErr := s.q.ReconcileRunMR(ctx, store.ReconcileRunMRParams{
			MrIid: pgconv.Int8Ptr(req.MrIID), MrWebUrl: stripNULParam(req.MrWebURL), Branch: stripNULParam(req.Branch),
			ID: runID, WorkerID: pgconv.UUID(wkr.ID),
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
				Disposition: pgconv.TextOrNull(settleScopeDisposition),
			}); setErr != nil {
				slog.Warn("settle scope input disposition", "run", runID, "disposition", settleScopeDisposition, "error", setErr)
			}
		}
		// PRD #46 Decision 2: enqueue a judge on the COMMITTED terminal transition
		// (rows>0), not the lossy notify seam. Best-effort — never fails the report.
		s.maybeEnqueueJudge(ctx, run)
		// PRD #929 M2: file a scheduled issues-mode prompt run's proposal as a forge
		// issue on this same committed completion. The worker's proposal is untrusted, so
		// it is clamped (kind-gated + control-char-stripped + secret-scrubbed + bounded)
		// at the call site with the re-read run before filing. Best-effort — like the
		// judge it must NEVER fail the worker's terminal report (maybeFileProposal
		// log-and-returns on every failure).
		s.maybeFileProposal(ctx, run, clampWireProposal(run, req.Proposal))
		// PRD #400 M4a: auto-create a diff-review run for a just-completed --review task,
		// on the SAME committed transition. Best-effort — never fails the report.
		s.maybeEnqueueTaskReview(ctx, run)
		// PRD #400 M5: auto-create a chained fix run when a --then-fix handoff's review
		// completes with findings. Its gates fire ONLY for a completed review run, and
		// maybeEnqueueTaskReview's review_target_run_id-null gate makes the two mutually
		// exclusive. Best-effort — never fails the report.
		s.maybeEnqueueThenFix(ctx, run)
		// PRD #1349 M4 (D1/D2/D3): on a terminal SUCCESS (status='completed'), release the
		// COMPLETING GENERATION'S custody hold idempotently — a completed code-publishing run
		// published its head, so its unpublished-work custody is moot. GENERATION-EXACT via
		// ReleaseCustodyHoldExact using the server's own run row (run.ClaimGeneration) and this
		// worker's id, NOT a generation-blind run+worker bulk release: one worker can hold TWO
		// open holds on a run — an uncaptured generation-1 hold plus a generation-2 hold from an
		// affinity re-claim after a transient loss — and a blind run+worker release would null
		// BOTH orphans' live FKs and let the worker be reaped, dropping generation 1's only copy.
		// Releasing exactly the completing generation preserves any older-generation sibling
		// orphan (same worker OR a different worker), which is retained until capture/discard
		// (D3). Placed BEFORE the ephemeral teardown so the release nulls this hold's ON DELETE
		// RESTRICT live FKs and the teardown DELETE's custody-skip predicate can reap the worker
		// in the same report. BEST-EFFORT: a release failure must NOT fail or block the
		// completion report (D3) — the M4 custody-release reconciler is the backstop that settles
		// a recorded successful publication later, after which reap proceeds. Deliberately NOT
		// called on failed/cancelled: those retain custody for capture or explicit discard.
		if run.Status == "completed" {
			if _, relErr := s.q.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
				RunID:      runID,
				Generation: run.ClaimGeneration,
				WorkerID:   wkr.ID,
				// PRD #1392 M1 (D3): a completed run published its head, so its release is
				// warranted by publication.
				ReleaseEvidence: pgconv.TextOrNull("publication"),
			}); relErr != nil {
				slog.Warn("release custody on completion", "run", runID, "worker", wkr.ID, "generation", run.ClaimGeneration, "error", relErr)
			}
		}
		// PRD #529 M4: an ephemeral worker exists only to serve its bound run, so a
		// genuinely-applied terminal transition (rows>0) on that run — completed /
		// worker-cancel / failed alike — is its cue to tear down. Fires on the SAME
		// committed transition; guarded on wkr.Ephemeral and busy-checked in-query so a
		// normal run's completion is a no-op. Best-effort — never fails the report.
		s.maybeTeardownEphemeral(ctx, wkr, run)
		// PRD #1030 M4: on a COMMITTED terminal transition (completed, or a
		// failed→cancelled/stopped/plan-reject/agent-failure route through the switch
		// above), delete the run's now-stale checkpoint ref so it cannot later block a
		// new run on the same branch with a not_descendant skip. Best-effort and
		// dispatched OFF this goroutine — it must never delay or fail the worker's
		// terminal report, and it runs AFTER the terminal state is durably recorded. The
		// terminal guard keeps it off `running`/`awaiting_*` transitions; the helper
		// kind-gates it to checkpoint-eligible issue runs. `failed` is terminal and NOT
		// requeued, so deleting here cannot race a requeue-resume (PRD #1030 M4).
		if terminalStatuses[run.Status] {
			s.deleteCheckpointBestEffort(runID, run.Kind, run.IssueIid)
		}
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
			sizeClass = pgconv.TextOrNull(*req.SizeClass)
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
	//
	// Issue #1626: for an INTERLOCKED run, the FIRST plan-bearing report (plan_md present) with
	// no milestones freezes the explicit `[]` (planMilestonesParam), so the contract freeze below
	// and in SetRunRunning lands criteria:[]. The claim-time report carries no plan_md and
	// stays NULL, keeping the milestone-source guard intact. `run` is the SetState snapshot taken
	// BEFORE SetRunAutopilotPlan stored this report's plan_md, so the first plan report sees a
	// NULL stored plan_md and infers `[]`; a later plan report after a REJECTED list (frozen still
	// NULL, plan_md already stored) stays NULL, so a rejection is never turned into a vacuous
	// empty contract.
	p.MilestonesFrozen = planMilestonesParam(run, req.PlanMd, req.Milestones)

	// PRD #122 M2 (Decision 3/12): the live progress sets. Validated + membership-checked
	// against the run's FROZEN list and kind-gated (progressParams); a bad or non-issue
	// set is DROPPED to NULL, which the query leaves the column untouched for. completed
	// is unioned server-side, in_progress overwritten. Additive-optional, never fails the
	// report.
	p.MilestonesCompleted, p.MilestonesInProgress = progressParams(run.Kind, run.MilestonesFrozen, req.MilestonesCompleted, req.MilestonesInProgress)

	// PRD #1224 (Decision 6): couple the validated attribution to the in_progress write.
	// Keyed on the validated in_progress JSON (nil ⇒ in_progress not updated ⇒ leave the
	// column untouched); when in_progress IS updated, milestoneAgentsParam returns the
	// validated subset ('[]' when none survive), which the SQL COALESCEs.
	p.MilestonesAgents = milestoneAgentsParam(run.Kind, p.MilestonesInProgress, req.MilestonesAgents)

	// PRD #1226 M1 (D1): for an INTERLOCKED autopilot run that has not yet frozen its
	// contract, build the structural completion contract to freeze on this `running` report
	// from the SAME 3-way frozen source the query resolves —
	// COALESCE(milestones_frozen, this report's frozen list, milestones_candidate). Guarded
	// on version-set AND contract-not-yet-frozen so an ordinary heartbeat after the freeze
	// rebuilds nothing; the query independently guards the assignment (freezes once,
	// idempotently), so a heartbeat that DID rebuild would still be a no-op. Best-effort: a
	// corrupt milestone column logs and passes NULL rather than failing the running report.
	if run.CompletionContractVersion.Valid && len(run.CompletionContract) == 0 {
		src := run.MilestonesFrozen
		if len(src) == 0 {
			src = p.MilestonesFrozen
		}
		if len(src) == 0 {
			src = run.MilestonesCandidate
		}
		contract, err := buildCompletionContract(src)
		if err != nil {
			slog.Warn("workersvc: build completion contract on running report failed", "run_id", run.ID, "error", err)
		} else {
			p.CompletionContract = contract
		}
	}

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
	p.AgentSource = pgconv.TextOrNull(sel.Source)
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
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgconv.UUID(run.UserID))
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
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgconv.UUID(userID))
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

// ConsumeInputsResult is what ConsumeInputs returns: the drained steering inputs and, when a
// held-state switch is pending for the run's current claim, the worker-facing switch signal (PRD
// #1247 M5, D3/D4). On the consume-nothing path (a pending switch) Inputs is empty and
// CredentialSwitch is set; on the normal path CredentialSwitch is nil.
type ConsumeInputsResult struct {
	Inputs           []InputDTO
	CredentialSwitch *CredentialSwitchSignal
}

// ConsumeInputs returns and marks-consumed every pending steering input for a
// run the worker owns, FIFO. Delivery marks the input consumed (there is no
// separate ack), so a worker crash right after the GET drops that input — an
// accepted MVP trade-off for the steering channel (the user can re-send).
//
// CONSUME-NOTHING RULE (PRD #1247 M5, step 2): the buffered inputs of a run whose claim is being
// switched are earmarked for the RECLAIM, and this drains NOTHING until that reclaim happens. Two
// windows are fenced, both server-side (never on worker discipline):
//
//   - a switch is PENDING for the current claim (PendingCredentialSwitchSignal != nil): return the
//     signal and drain nothing, so a racing answer/follow-up stays unconsumed for the reclaim
//     rather than being drained (and dropped) during the release the worker is about to perform.
//   - the claim has been RELEASED and not yet reclaimed (claim_released_at set): the old worker
//     still passes the worker_id-only ownership check in this window, and the signal has already
//     cleared, so without this fence a stray poll from the departing worker would drain the very
//     rows the reclaim must see. Drain nothing here too. The drain resumes only after ClaimRun
//     reclaims the run (which clears claim_released_at and bumps the generation).
func (s *Service) ConsumeInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID) (ConsumeInputsResult, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return ConsumeInputsResult{}, err
	}
	if sig := PendingCredentialSwitchSignal(run); sig != nil {
		// Pending switch: signal + no drain, so a racing answer/follow-up survives for the reclaim.
		return ConsumeInputsResult{CredentialSwitch: sig}, nil
	}
	if run.ClaimReleasedAt.Valid {
		// Released-but-not-reclaimed: claim_released_at is set by ReleaseCredentialSwitch (a credential
		// switch released this claim) OR by ParkRunsAtWall (PRD #1497: the server parked the run at its
		// wall limit and fenced the old flight), and cleared ONLY by ClaimRun, so a valid value here
		// means exactly "the current flight's claim was released and the reclaim has not happened yet."
		// Either way the buffered inputs belong to that reclaim; fence the drain (no signal — the switch
		// for this claim, if any, is already released).
		return ConsumeInputsResult{}, nil
	}
	rows, err := s.q.ConsumeRunInputs(ctx, runID)
	if err != nil {
		return ConsumeInputsResult{}, err
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
	return ConsumeInputsResult{Inputs: out}, nil
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
	n, err := s.q.CountAgentMemoryForRun(ctx, pgconv.UUID(runID))
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
		RunID:    pgconv.UUID(runID),
		Title:    title,
		Body:     body,
		Basis:    pgconv.TextOrNull(basis),
		Evidence: pgconv.TextOrNull(evidence),
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

// RunOwnership returns the current status of a run this worker owns plus, when the run is
// parked in recovery_wait, its recovery_retry_not_before stamp — or ErrRunNotOwned when the
// worker does not own it (reclaimed / never owned). Read-only; the interactive park-skip path
// (#559) uses the status to detect a mid-turn reclaim or terminal transition early, and the
// forge pre-clone park (PRD #1392 M1, D10) uses the retry stamp to reconcile an unknown park
// outcome after a transport failure — so a reconciled park can still quote its retry time. The
// stamp is nil for a run that is not recovery-parked.
func (s *Service) RunOwnership(ctx context.Context, wkr store.Worker, runID uuid.UUID) (status string, recoveryRetryNotBefore *time.Time, claimGeneration int64, err error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return "", nil, 0, err
	}
	var retry *time.Time
	if run.RecoveryRetryNotBefore.Valid {
		t := run.RecoveryRetryNotBefore.Time
		retry = &t
	}
	// PRD #1391 Run B M4: the run's current claim_generation rides the ownership probe
	// (additive), so the run-lane claim router can proceed ONLY on a claimed/running row AT
	// the claim's generation and end the attempt on a DIFFERENT generation. It already lives
	// on the store.Run this read returns — no new query.
	return run.Status, retry, run.ClaimGeneration, nil
}

func (s *Service) runOwnedByWorker(ctx context.Context, runID uuid.UUID, wkr store.Worker) (store.Run, error) {
	run, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotOwned
		}
		return store.Run{}, err
	}
	return run, nil
}

// clampInt32 saturates an int64 into the int32 range (0..math.MaxInt32) rather than wrapping — the
// explicit bounds also make the narrowing provable to gosec G115 (PRD #1391 Run B M3c). Callers
// already validate their inputs to a far smaller cap; this is defense-in-depth for a direct caller.
func clampInt32(v int64) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < 0 {
		return 0
	}
	return int32(v)
}

// MessageGap is one missing seq range [First..Last] (inclusive) in a run's message log (PRD #1391
// Run B M3c). Both bounds are >= 1 and First <= Last.
type MessageGap struct {
	First int64 `json:"first"`
	Last  int64 `json:"last"`
}

// MessageGapsPage is one keyset page of a run's message gaps (PRD #1391 Run B M3c): the gaps in
// seq order plus, when more pages may follow, the cursor to resume after. NextCursor is nil at the
// end of the scan.
type MessageGapsPage struct {
	Gaps       []MessageGap
	NextCursor *int64
}

// RunMessageGaps returns the MISSING seq ranges in [1..through] for a run the worker holds AT THE
// EXACT CURRENT, UNRELEASED `claimGeneration` (PRD #1391 Run B M3c) — keyset-paginated by seq after
// `cursor`, at most `limit` ranges. Authorization and gap derivation are one SQL statement/snapshot,
// so a same-worker reclaim cannot move the generation between a preliminary ownership read and the
// gap read. Zero rows means a foreign, released or stale-generation claim (ErrRunNotOwned → 404);
// an authorized run with no gaps returns one nullable sentinel row, filtered below. NextCursor is set
// only when the page filled to `limit` and did not reach the trailing gap (whose Last == through).
func (s *Service) RunMessageGaps(ctx context.Context, wkr store.Worker, runID uuid.UUID, claimGeneration, through, cursor, limit int64) (MessageGapsPage, error) {
	// The handler validates through/cursor to <= a cap (1<<30) and limit to a small ceiling, all
	// well within int32; clamp defensively so a direct caller cannot overflow the casts and the
	// conversions are provably in range (gosec G115). A negative here is impossible (the caller
	// rejects it) but is floored to 0 for the same provability.
	rows, err := s.q.RunMessageGaps(ctx, store.RunMessageGapsParams{
		RunID:           runID,
		WorkerID:        pgconv.UUID(wkr.ID),
		ClaimGeneration: claimGeneration,
		Through:         clampInt32(through),
		Cursor:          clampInt32(cursor),
		Lim:             clampInt32(limit),
	})
	if err != nil {
		return MessageGapsPage{}, err
	}
	if len(rows) == 0 {
		return MessageGapsPage{}, ErrRunNotOwned
	}
	page := MessageGapsPage{Gaps: make([]MessageGap, 0, len(rows))}
	for _, r := range rows {
		if !r.GapFirst.Valid {
			continue // authorized run with no gaps: the query's all-NULL sentinel row
		}
		page.Gaps = append(page.Gaps, MessageGap{First: int64(r.GapFirst.Int32), Last: int64(r.GapLast.Int32)})
	}
	// Keyset continuation: a full page MIGHT have more behind it, unless the last range is the
	// trailing gap (Last == through), which is uniquely the final one — the query closes it with a
	// sentinel and nothing can follow. A short page means the LIMIT was not hit, so the scan is
	// exhausted. In both terminal cases NextCursor stays nil.
	if int64(len(page.Gaps)) == limit && len(page.Gaps) > 0 {
		last := rows[len(rows)-1]
		if last.GapLast.Valid && int64(last.GapLast.Int32) < through {
			next := int64(last.NextCursor.Int32)
			page.NextCursor = &next
		}
	}
	return page, nil
}

// RunOrphanIdentity is the authoritative owner-run identity a worker needs to derive an
// orphan clone's canonical path (issue #1319). Pointers carry SQL NULL as JSON null.
type RunOrphanIdentity struct {
	Status      string
	RepoID      uuid.UUID
	Kind        string
	IssueIID    *int64
	Branch      *string
	PipelineRef *string
	PipelineID  *int64
}

// RunOrphanClassification returns the authoritative identity of a terminal-orphan OWNER
// run, scoped to the worker's owner (user) AND the claimant run's repo — NOT worker_id, so
// an owner that moved workers is still found (issue #1319, Gap 2). The claimant run (which
// the worker DOES hold) is the authz anchor and the source of the current repo. Read-only.
// Distinct from RunOwnership, whose worker-scope protects same-run recovery.
func (s *Service) RunOrphanClassification(ctx context.Context, wkr store.Worker, claimantRunID, ownerRunID uuid.UUID) (RunOrphanIdentity, error) {
	claimant, err := s.runOwnedByWorker(ctx, claimantRunID, wkr) // ErrRunNotOwned if not held
	if err != nil {
		return RunOrphanIdentity{}, err
	}
	if !claimant.RepoID.Valid {
		return RunOrphanIdentity{}, ErrRunNotOwned // a repo-less claimant cannot reclaim
	}
	row, err := s.q.GetRunOrphanIdentity(ctx, store.GetRunOrphanIdentityParams{
		ID:     ownerRunID,
		UserID: wkr.UserID, // owner-scope; wkr.UserID == claimant.user_id by ClaimRun
		RepoID: claimant.RepoID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RunOrphanIdentity{}, ErrRunNotOwned
		}
		return RunOrphanIdentity{}, err
	}
	return RunOrphanIdentity{
		Status:      row.Status,
		RepoID:      uuid.UUID(row.RepoID.Bytes),
		Kind:        row.Kind,
		IssueIID:    int64Ptr(row.IssueIid),
		Branch:      textPtr(row.Branch),
		PipelineRef: textPtr(row.PipelineRef),
		PipelineID:  int64Ptr(row.PipelineID),
	}, nil
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
	row, err := s.q.GetRunForgeConnForWorker(ctx, store.GetRunForgeConnForWorkerParams{RunID: runID, WorkerID: pgconv.UUID(wkr.ID)})
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

	// 2. Branch is derived SERVER-SIDE from validated run-row fields (kind, the run's
	// uuid, the issue iid) — never from the worker, and never from owned.Branch.
	// runs.branch is written ONLY at completion (SetRunCompleted), so it is NULL/empty
	// for a `running` run; a checkpoint publish fires MID-RUN, so reading it would make
	// the whole feature inert. Checkpoints fire for the checkpoint-eligible set — an
	// issue run (agent/issue-<iid>) and a self_improve run (uzi/self-improve/<runID>,
	// PRD #1062 M3) — so any other kind, or an issue run missing its iid, is an
	// unsupported publish we skip (best-effort) rather than fault. checkpointBranch
	// dispatches on kind FIRST (a self_improve run also carries a valid issue_iid, its
	// stable tracking issue), and derives the branch only from run-row fields, so no
	// worker-controlled string ever flows into the refspec.
	branch, ok := checkpointBranch(owned.Kind, runID, owned.IssueIid)
	if !ok {
		return PublishResult{Published: false, Ref: "", Skipped: "unsupported"}, nil
	}
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
		// The CAS-accepted advance is the ONLY arm that persists the tip: it runs on
		// EVERY successful publish (mid-run/park/shutdown all route through here), so
		// runs.checkpoint_tip tracks the just-published tip and the terminal CAS-delete's
		// stale-tip fallback stays diagnosable. Persist is best-effort — a failure must
		// NOT fail the publish, since the ref is already advanced on the forge.
		if _, perr := s.q.SetRunCheckpointTip(ctx, store.SetRunCheckpointTipParams{
			CheckpointTip: pgtype.Text{String: tipOid, Valid: true},
			ID:            runID,
		}); perr != nil {
			slog.Warn("checkpoint: persist tip", "run", runID, "tip", tipOid, "error", perr)
		}
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

// deleteCheckpointBestEffort removes a run's stale checkpoint ref
// (refs/uzi-checkpoints/<branch>) from the forge on a TERMINAL transition (PRD #1030
// M4). Once a run is terminal its checkpoint ref is stale scratch state; leaving it
// behind later blocks a NEW run on the same branch with a not_descendant skip, so
// every terminal path calls this.
//
// It is BEST-EFFORT and must NEVER block or fail the caller's terminal transition:
// the terminal DB write has already committed by the time this runs, and the whole
// forge round-trip is dispatched on s.background (a detached goroutine in production;
// tests run it inline) under pushbroker.Delete's own short wall-clock timeout. Any
// failure is logged and swallowed — a surviving stale ref only re-blocks a later run
// with a benign skip, never corrupts anything, and the PVC refs/uzi-runner/* remains
// the primary recovery path.
//
// It is gated to the SAME checkpoint-eligible set Publish supports (an issue run with a
// valid issue iid, or a self_improve run — PRD #1062 M3): only these ever published a
// checkpoint ref, so any other kind is a no-op with no forge call. The branch, repo
// connection and PAT are derived SERVER-SIDE exactly as Publish derives them (via
// checkpointBranch from run-row fields, GetRunClaimContext, the SSRF gate, box.Open),
// never from the worker.
func (s *Service) deleteCheckpointBestEffort(runID uuid.UUID, kind string, issueIid pgtype.Int8) {
	// Kind-gate identically to Publish (dispatch on kind first): a run that never had a
	// checkpoint ref has nothing to delete. Do this BEFORE dispatching so an ineligible
	// kind makes no goroutine and no forge call.
	branch, ok := checkpointBranch(kind, runID, issueIid)
	if !ok {
		return
	}
	// A deployment that never wired the delete seam or the SSRF gate, or a service
	// built without a secretbox (some tests), cannot broker the delete — skip rather
	// than dispatch a goroutine that would only fail. (publishFn/box/forgeBaseURLAllowed
	// nil-checks mirror Publish's own guards.)
	if s.deleteCheckpointFn == nil || s.forgeBaseURLAllowed == nil || s.box == nil || s.background == nil {
		return
	}
	s.background(func() {
		// s.background is a DETACHED goroutine in production, and pushbroker.Delete
		// drives go-git (ListContext/PushContext), which has nil-deref panic paths on
		// malformed forge responses. An unrecovered panic in ANY goroutine crashes the
		// whole api process, so recover FIRST and swallow it — this cleanup is
		// best-effort, and the failure it prevents (a later not_descendant skip) is
		// benign. Mirrors forgesvc.ProjectSyncService.launchSeed's recover idiom.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("checkpoint cleanup: delete panicked", "run", runID, "branch", branch, "panic", secretscrub.Scrub(fmt.Sprint(r)))
			}
		}()
		// Detached from the request/report ctx (which is already returning to the
		// worker): bind to a fresh context.Background, and let pushbroker.Delete apply
		// its own bounded timeout on top. A slow/down forge cannot reach the caller.
		ctx := context.Background()

		rc, err := s.q.GetRunClaimContext(ctx, runID)
		if err != nil {
			// No repo/forge connection (a repo-less run) or the row vanished: there is
			// nothing to delete. Benign.
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Warn("checkpoint cleanup: claim context", "run", runID, "error", err)
			}
			return
		}
		// Skip entirely when this run NEVER published a checkpoint (checkpoint_tip
		// NULL): it owns no ref, so there is nothing to delete — and an unconditional
		// delete could clobber a SIBLING run's fresh checkpoint on the same branch. This
		// is load-bearing, not an optimisation. When set, rc.CheckpointTip.String is the
		// CAS Old the delete must match (pushbroker.DeleteOptions.ExpectedOldTip), so
		// origin's ref is removed only while it still points at exactly the tip THIS run
		// published.
		if !rc.CheckpointTip.Valid {
			slog.Debug("checkpoint cleanup: skip (never published)", "run", runID, "branch", branch)
			return
		}

		cloneURL := rc.RepoWebUrl + ".git"

		// Same SSRF gate as Publish, BEFORE decrypting the PAT: never point go-git at
		// an un-allowlisted host. A misconfigured gate is a skip here (best-effort),
		// not the loud 500 Publish raises — this path must never fail a terminal report.
		if !s.forgeBaseURLAllowed(rc.BaseUrl) {
			slog.Warn("checkpoint cleanup: base URL not allowlisted", "run", runID)
			return
		}
		cloneHost, err := forgeHostFromURL(cloneURL)
		if err != nil || !s.forgeBaseURLAllowed(cloneHost) {
			slog.Warn("checkpoint cleanup: clone host not allowlisted", "run", runID)
			return
		}

		botPAT, err := s.box.Open(rc.TokenCiphertext)
		if err != nil {
			slog.Warn("checkpoint cleanup: bot PAT could not be decrypted", "run", runID)
			return
		}

		if derr := s.deleteCheckpointFn(ctx, pushbroker.DeleteOptions{
			CloneURL:       cloneURL,
			Branch:         branch,
			Username:       rc.BotUsername,
			PAT:            string(botPAT),
			ExpectedOldTip: rc.CheckpointTip.String,
		}); derr != nil {
			// Scrub any credential-bearing go-git error (its remote URL can carry the
			// PAT in userinfo) before logging — the same invariant Publish's default arm
			// keeps. Best-effort: the error is logged and swallowed, never surfaced.
			slog.Warn("checkpoint cleanup: delete ref", "run", runID, "branch", branch, "error", secretscrub.Scrub(derr.Error()))
		}
	})
}

// -------------------------------------------------------------------------
// Web-facing worker + run management
// -------------------------------------------------------------------------

// CreateWorker issues a worker for the user and returns the plaintext join token
// exactly once (only its hash is stored).
// tokenLabel is the optional mint-time Anthropic token binding (PRD #104 M3):
// empty means no pinned token: creation chooses auto when the pool is non-empty,
// otherwise default. A label that names none of the user's tokens returns
// ErrUnknownSecretLabel: minting a worker pointed at a credential that does not
// exist would produce a worker that only fails at its first claim.
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
	//
	// The pool read (PRD #1140 M1) is the SECOND input to that same derivation, making
	// every worker create path agree with the #804 ephemeral path (ephemeral.go:263):
	// a resolved label is `pinned` regardless of the pool; its absence is `auto` when
	// the owner has ≥1 auto_eligible anthropic_token (a non-empty auto-select pool),
	// else `default`. So a pooled-up owner's new worker spends the pool by default
	// instead of the owner's single default token. This choice happens at creation,
	// not at every claim: removing the pooled tokens later leaves an auto worker in
	// auto mode, and finishRunClaim holds its run in pool_wait on errAutoPoolEmpty.
	// Corrected 2026-09-08 against that claim-time branch; the former "never parks"
	// comment incorrectly extended the creation-time pool check to later claims.
	bindMode := BindModeDefault
	switch {
	case secretID.Valid:
		bindMode = BindModePinned
	default:
		hasPool, err := s.q.UserHasAutoEligibleAnthropicToken(ctx, userID)
		if err != nil {
			return store.Worker{}, "", err
		}
		if hasPool {
			bindMode = BindModeAuto
		}
	}
	// templateDeclared is the UI-chosen worker template (PRD #18), validated
	// against the registry by the caller; empty → NULL (no choice made).
	wkr, err := s.q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID:            userID,
		Name:              name,
		TokenHash:         hash,
		TemplateDeclared:  pgconv.TextOrNull(templateDeclared),
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
	return pgconv.UUID(id), nil
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
		bind = pgconv.UUID(*secretID)
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

// SetUserJudgeBinding sets the user's JUDGE-lane bind MODE and pointer in one write
// (PRD #104 M4, three-valued per PRD #1140 M2, D6), mirroring SetWorkerAnthropicToken's
// rules: 'pinned' requires an owned secret; 'default' and 'auto' carry no id and write
// NULL. Per-user, not per-worker: which credential reviews your work is a property of
// you, not of whichever worker claims the retrospective. A 'default' user spends their
// default token; an 'auto' user has the judge lane run the same pool ranker the run
// lane uses, falling to the default only when the pool is genuinely empty (D4).
//
// Ownership is checked here so the caller gets a 404 rather than a constraint
// violation; 00079's composite FK refuses the same binding independently, and is
// the layer that holds if this check is ever bypassed (D11).
func (s *Service) SetUserJudgeBinding(ctx context.Context, userID uuid.UUID, mode string, secretID *uuid.UUID) (store.User, error) {
	if !ValidBindMode(mode) {
		return store.User{}, ErrInvalidBindMode
	}
	// A non-pinned mode never carries an id (the same rule SetWorkerAnthropicToken
	// enforces): 'default' and 'auto' resolve without a pointer, so a stale id left
	// beside them cannot leak into a claim.
	if mode != BindModePinned {
		secretID = nil
	}
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
		bind = pgconv.UUID(*secretID)
	}
	return s.q.SetUserJudgeAnthropicBinding(ctx, store.SetUserJudgeAnthropicBindingParams{
		ID:                     userID,
		JudgeAnthropicBindMode: mode,
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
//
// PRD #1296 M4 (D3): it is ALSO refused when the worker still holds an OPEN custody
// hold (ErrWorkerHasCustody), because deleting the worker cascades hosted_worker_tokens
// and a preauthorized recovery upload may still need that token to authenticate — the
// last authenticated retry path must survive until custody is released or explicitly
// discarded. The error carries the count so M4b's CLI confirmation can name the
// affected captures. This guard is placed BEFORE the delete so the token row is never
// cascaded away while an upload could still need it. (The M1 live_worker_id
// ON DELETE RESTRICT FK is the fail-closed backstop; this guard turns it into a clean,
// enumerated 409 instead of a raw constraint 500.)
func (s *Service) DeleteWorker(ctx context.Context, userID, workerID uuid.UUID) error {
	active, err := s.q.CountWorkerNonTerminalRuns(ctx, store.CountWorkerNonTerminalRunsParams{
		WorkerID: pgconv.UUID(workerID), UserID: userID,
	})
	if err != nil {
		return err
	}
	if active > 0 {
		return ErrWorkerHasActiveRuns
	}
	// OWNER-SCOPED (userID): the count filters on user_id so a FOREIGN owner's held worker
	// returns 0 here and falls through to the owner-scoped DeleteWorkerForUser → 404, instead
	// of leaking the worker's existence + hold count as a 409. Every other sibling check in
	// this method is owner-scoped for the same reason.
	holds, err := s.q.CountOpenCustodyHoldsForWorker(ctx, store.CountOpenCustodyHoldsForWorkerParams{
		WorkerID: workerID,
		UserID:   userID,
	})
	if err != nil {
		return err
	}
	if holds > 0 {
		return &WorkerHasCustodyError{Holds: holds}
	}
	n, err := s.q.DeleteWorkerForUser(ctx, store.DeleteWorkerForUserParams{ID: workerID, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	// The worker is gone; drop its last-known outbox depth (PRD #1391 M5) so a
	// removed worker's stale depth does not linger on the fleet view. In-memory and
	// best-effort — the TTL prune is the backstop for any path that skips this.
	s.outbox.evict(workerID)
	return nil
}

// CreateRun queues a manually-started run from a board card. The issue must be a
// cached issue carrying the uzi_label (PRD #764 M1) in a repo the user owns; its
// title is snapshotted from the cache and its description from the request, so the
// run is self-contained even if the issue cache is later evicted. A PRD link is no
// longer required. The one-non-terminal-run-per-issue index rejects a duplicate
// active run.
// PRD #1429 M2: explicit is the manual request's optional harness (D2), nil ⇒ implicit D11;
// rawOverride is the raw #1247 credential override validated against the resolved harness inside
// the create transaction (D5). The old pre-transaction createRunEffectiveHarness guesser is gone.
func (s *Service) CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, mrReworkEnabled *bool, force bool, seed *SeededPlan, explicit *Harness, rawOverride *RawCredentialOverride) (store.Run, error) {
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
	// force (issue #856): threaded straight through so the interactive/CLI caller can
	// bypass the open-MR dedup with --force; it never affects the active-run gate.
	// credOverride nil ⇒ inherit the worker binding (PRD #1247 M1); the create-time
	// user choice is wired in M2. Threaded through the signature now so M2 does not
	// reopen this seam (the freeze).
	return s.createRun(ctx, userID, repoID, issueIID, "manual", description, false, waitOnLimit, mrReworkEnabled, nil, false, force, seed, nil /*credOverride: manual uses rawOverride*/, explicit, rawOverride)
}

// CreateScheduledRun queues a NON-auto-approve scheduled issue run (PRD #241: a timer
// or label-sweep schedule firing an issue with the plan gate still requiring a human).
// It is IDENTICAL to CreateRun — same single uzi_label eligibility gate (PRD #764 M1) —
// and, like every create path, no longer requires a PRD link.
func (s *Service) CreateScheduledRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, mrReworkEnabled *bool, model *string, overrideSubagentModel bool, seed *SeededPlan, credOverride *CredentialOverride, explicit *Harness) (store.Run, error) {
	// false force (issue #856): a scheduled run never bypasses the open-MR dedup.
	// credOverride: the schedule's stored (pre-validated) override, written as-is. explicit: the
	// schedule's pinned run_schedules.harness (nil ⇒ implicit D11 at fire time), PRD #1429 M2.
	return s.createRun(ctx, userID, repoID, issueIID, "schedule", description, false, waitOnLimit, mrReworkEnabled, model, overrideSubagentModel, false, seed, credOverride, explicit, nil /*rawOverride*/)
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
	// nil mrReworkEnabled (PRD #841 M1): label-poller autopilot has no human/schedule to
	// express a per-run override, so the run's column stays NULL → inherit the owner
	// default live, exactly today's behaviour.
	// false force (issue #856): label-poller autopilot never bypasses the open-MR dedup.
	// nil credOverride (PRD #1247 M1): label-poller autopilot has no per-run credential
	// choice, so it inherits the worker binding exactly as before.
	// nil explicit + nil rawOverride (PRD #1429 M2): label-poller autopilot has no per-run harness
	// choice, so it uses implicit D11 resolution at create.
	return s.createRun(ctx, userID, repoID, issueIID, "autopilot", description, true, nil, nil, nil, false, false, nil, nil /*credOverride*/, nil /*explicit*/, nil /*rawOverride*/)
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
func (s *Service) CreateScheduledAutopilotRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, waitOnLimit *bool, mrReworkEnabled *bool, model *string, overrideSubagentModel bool, credOverride *CredentialOverride, explicit *Harness) (store.Run, error) {
	// false force (issue #856): a scheduled autopilot run never bypasses the open-MR dedup.
	// credOverride: the schedule's stored override; explicit: the schedule's pinned harness
	// (nil ⇒ implicit D11 at fire time), PRD #1429 M2.
	return s.createRun(ctx, userID, repoID, issueIID, "autopilot", description, true /*autoApprove*/, waitOnLimit, mrReworkEnabled, model, overrideSubagentModel, false /*force*/, nil /*seed*/, credOverride, explicit, nil /*rawOverride*/)
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

// explicit is the caller's outright D11 harness selection (PRD #1429 M2): the manual request's
// harness / chat start_run's harness, or a schedule's pinned run_schedules.harness; nil ⇒ implicit
// D11 resolution. credOverride is a PRE-RESOLVED credential override (the schedule producers'
// stored choice), written to the columns as-is; rawOverride is an UNVALIDATED request override
// (manual/chat-start) validated against the D11-resolved harness INSIDE the create transaction
// (D5). At most one of credOverride/rawOverride is non-nil.
func (s *Service) createRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, triggerSource string, description string, autoApprove bool, waitOnLimit *bool, mrReworkEnabled *bool, model *string, overrideSubagentModel bool, force bool, seed *SeededPlan, credOverride *CredentialOverride, explicit *Harness, rawOverride *RawCredentialOverride) (store.Run, error) {
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
		planMD = pgconv.TextOrNull(scrubbed)
		planSource = planSourceSeeded
		if seed.Selection != nil {
			sel := *seed.Selection
			// Shape only — no roster exists at create time (Open Question 1). A source
			// that is not 'repo'/'own' would otherwise hit the agent_source CHECK
			// constraint as a 500; a malformed exclusion name would persist unchecked.
			if err := validateSelectionShape(sel); err != nil {
				return store.Run{}, err
			}
			agentSource = pgconv.TextOrNull(sel.Source)
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
			plannedBaseCommit = pgconv.TextOrNull(pc)
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
	// Open-MR dedup (issue #856): a COMPLETED run owning an OPEN MR is terminal, so the
	// active-run gate above cannot see it — yet a fresh start would re-plan and re-run the
	// whole review wave onto that already-open MR (silent wasted spend). Refuse unless the
	// caller forced it. Scoped to the issue kind by construction (createRun is the issue
	// path). --force bypasses ONLY this guard, never the active-run gate above. The guard
	// keys on the authoritative mr_iid (set atomically at completion by SetRunCompleted, so
	// it is present the instant the run is completed) and releases only once the watcher has
	// recorded a TERMINAL MR state ('merged'/'closed'); a NULL mr_state (watcher not yet
	// ticked) still blocks. That closes the watcher-lag false-negative window a mr_state-only
	// predicate had between MR-open-at-finalize and the first watch tick.
	if !force {
		mrIID, err := s.q.GetOpenMRRunForIssue(ctx, store.GetOpenMRRunForIssueParams{
			RepoID:   repoID,
			IssueIid: pgtype.Int8{Int64: issueIID, Valid: true},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, err
		}
		if err == nil && mrIID.Valid {
			return store.Run{}, &OpenMRExistsError{IssueIID: issueIID, MRIID: mrIID.Int64}
		}
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
	// PRD #1226 M1 (D1): stamp the run as INTERLOCKED before its first claim when the
	// rollout switch is on. completionInterlockOn is FAIL-SAFE OFF (nil reader or any read
	// error → false), the opposite of the capability-aware fail-open, so this gate never
	// accidentally engages a still-rolling-out feature. NULL (the not-interlocked legacy
	// state) unless on. createRun only ever creates issue-kind rows, so this is inherently
	// issue-scoped; the contract CONTENT is frozen later at approval / the first running
	// report, not here.
	//
	// Issue #1626: a SEEDED-plan run (PRD #209, seed != nil) is never stamped and stays legacy.
	// Its plan arrives at create time, the worker takes the plan-approved skip, and it never
	// sends a plan-bearing report (no awaiting_approval, no autopilot running report carrying
	// plan_md); a SeededPlan carries no milestone list either. Nothing would ever freeze its
	// contract, so an interlocked seeded run would hold at finalize on every completion.
	var completionContractVersion pgtype.Int4
	if seed == nil && s.completionInterlockOn(ctx) {
		completionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
	}
	run, err := s.createRunResolved(ctx, userID, explicit, func(q Store, resolved resolvedHarness) (store.Run, error) {
		// PRD #1429 M2 (D5), #1247 override resolved INSIDE the create transaction against the
		// D11-resolved harness (not a pre-tx guess): a rawOverride (manual/chat-start request) is
		// validated here — an effective-Codex harness + Anthropic override fails
		// ErrCredentialOverrideHarnessUnsupported and rolls the whole create back (no run row, no
		// forge write after the invalid fact is known); a pre-resolved credOverride (a schedule's
		// stored choice) is written as-is. nil override ⇒ inherit, byte-identical to a pre-#1247 run.
		effOverride := credOverride
		if rawOverride != nil {
			ov, verr := s.ResolveCredentialOverride(ctx, userID, runkind.Issue, string(resolved.Harness), rawOverride.Mode, rawOverride.SecretID)
			if verr != nil {
				return store.Run{}, verr
			}
			effOverride = ov
		}
		return q.CreateRun(ctx, store.CreateRunParams{
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
			// PRD #841 M1 Decision D1: mr_rework is LIVE-INHERIT, the deliberate opposite of
			// wait_on_limit's snapshot. The pointer is stamped THROUGH with no resolver — nil
			// ⇒ NULL ⇒ the run inherits the owner default live at read time (the candidate
			// query COALESCEs run over owner), and an explicit true/false is a per-run override.
			// Every M1 caller passes nil except a future request/schedule override (M2/M3), so
			// behaviour is byte-identical to today.
			MrReworkEnabled: pgconv.BoolPtr(mrReworkEnabled),
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
			Model: pgconv.TextPtr(model),
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
			// issue #857 M2: the provenance stamp threaded from each public entrypoint
			// ("manual"/"schedule"/"autopilot"), so a run records why it fired.
			TriggerSource: triggerSource,
			// PRD #1226 M1 (D1): NULL (legacy) unless the rollout switch is on, in which case
			// this stamps the run interlocked (version 1) before its first claim. Listed
			// explicitly per runtime.sql's 🔴 silently-omittable-narg warning.
			CompletionContractVersion: completionContractVersion,
			// PRD #1429 M2 (D1): the AUDITOR INVARIANT — runs.harness is stamped from the D11 result
			// createRunAtomic resolved and froze inside THIS transaction, never a hardcoded literal, so
			// the frozen row's harness equals the harness the freeze decision used.
			Harness: string(resolved.Harness),
			// PRD #1247 / #1429 M2: the per-run credential override columns, from the effective override
			// resolved above (rawOverride validated against resolved.Harness, or a schedule's stored
			// credOverride written as-is). nil ⇒ NULL/NULL ⇒ inherit the worker binding.
			CredentialOverrideMode:     pgOverrideMode(effOverride),
			CredentialOverrideSecretID: pgOverrideSecretID(effOverride),
		})
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
	logRunCreated(run)
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
	return pgconv.Text(col) // always valid: "" = the implicit Open column, never NULL
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

// ListRunMessagesForViewerBefore returns the newest <= limit messages with
// seq < beforeSeq, in ASCENDING seq order (the store query returns DESC; the
// reverse happens here, not in SQL — keeps the row store.RunMessage). Same
// owner-or-admin gate as the sibling. Caller clamps limit.
func (s *Service) ListRunMessagesForViewerBefore(ctx context.Context, userID uuid.UUID, isAdmin bool, runID uuid.UUID, beforeSeq int32, limit int32) ([]store.RunMessage, error) {
	if _, err := s.GetRunForViewer(ctx, userID, isAdmin, runID); err != nil {
		return nil, err
	}
	rows, err := s.q.ListRunMessagesBeforePage(ctx, store.ListRunMessagesBeforePageParams{RunID: runID, BeforeSeq: beforeSeq, Lim: limit})
	if err != nil {
		return nil, err
	}
	// reverse DESC -> ASC in place
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
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
		BackgroundGraceCutoff: pgconv.Time(s.now().Add(-s.p.WorkerBackgroundGrace)),
	}
	if repoID != nil {
		arg.RepoID = pgconv.UUID(*repoID)
	}
	if issueIID != nil {
		arg.IssueIid = pgtype.Int8{Int64: *issueIID, Valid: true}
	}
	return s.q.ListRunsForUser(ctx, arg)
}

// RunUsageTotalsForRuns returns the rollup usage totals for the given runs, keyed by run id
// (issue #1620). It backs the Runs list's usage column, which used to ride a LEFT JOIN of
// run_usage_totals inside ListRunsForUser. A run with no usage is ABSENT from the map (the
// caller renders absent, never a fake 0). An empty input issues no query.
func (s *Service) RunUsageTotalsForRuns(ctx context.Context, runIDs []uuid.UUID) (map[uuid.UUID]store.RunUsageTotal, error) {
	if len(runIDs) == 0 {
		return map[uuid.UUID]store.RunUsageTotal{}, nil
	}
	rows, err := s.q.ListRunUsageTotalsForRuns(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]store.RunUsageTotal, len(rows))
	for _, row := range rows {
		out[row.RunID] = row
	}
	return out, nil
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
	return pgconv.Time(s.now().Add(-s.p.WorkerBackgroundGrace))
}

// RunUsageTotal returns one run's rolled-up token/cost totals (PRD #40 M3). Returns
// pgx.ErrNoRows for a run with no usage — the caller renders that as "no usage".
func (s *Service) RunUsageTotal(ctx context.Context, runID uuid.UUID) (store.GetRunUsageTotalRow, error) {
	return s.q.GetRunUsageTotal(ctx, runID)
}

// RunCredentialEpochs returns one run's applied-switch attribution journal, oldest
// generation first (PRD #1247 M1, D7), owner-scoped through the query's run join. Empty
// for a run that has never been claimed. Consumed by the handler's GetRun for the
// credential_epochs DTO field.
func (s *Service) RunCredentialEpochs(ctx context.Context, runID, userID uuid.UUID) ([]store.RunCredentialEpoch, error) {
	return s.q.ListRunCredentialEpochs(ctx, store.ListRunCredentialEpochsParams{RunID: runID, UserID: userID})
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

// SelfRunOutcomes returns the user's own run outcome counts for both windows
// (PRD #1293 M1), the failed-run rate data for GET /api/usage. The needs_landing sub-cut
// (issue #1418) is keyed on AllHumanLandableFailOrigins(), injected here so the human-landable
// set stays inside Go and the handler never has to know it.
func (s *Service) SelfRunOutcomes(ctx context.Context, userID uuid.UUID) (store.SelfRunOutcomesRow, error) {
	return s.q.SelfRunOutcomes(ctx, store.SelfRunOutcomesParams{
		LandableOrigins: AllHumanLandableFailOrigins(),
		UserID:          userID,
	})
}

// AdminRunOutcomes returns factory-wide run outcome counts for both windows (PRD #1293 M1);
// needs_landing (issue #1418) keyed on the Go-owned human-landable set.
func (s *Service) AdminRunOutcomes(ctx context.Context) (store.AdminRunOutcomesRow, error) {
	return s.q.AdminRunOutcomes(ctx, AllHumanLandableFailOrigins())
}

// AdminRunOutcomesPerUser returns the per-user lifetime outcome counts for the admin
// factory breakdown (PRD #1293 M1, D5); joins users so an outcome-only user has an email.
// needs_landing (issue #1418) keyed on the Go-owned human-landable set.
func (s *Service) AdminRunOutcomesPerUser(ctx context.Context) ([]store.AdminRunOutcomesPerUserRow, error) {
	return s.q.AdminRunOutcomesPerUser(ctx, AllHumanLandableFailOrigins())
}

// SelfRunOutcomeOrigins returns the user's per-origin failure causes for both windows
// (PRD #1293 M1), folded into RunOutcomesDTO.fail_origins by the handler.
func (s *Service) SelfRunOutcomeOrigins(ctx context.Context, userID uuid.UUID) ([]store.SelfRunOutcomeOriginsRow, error) {
	return s.q.SelfRunOutcomeOrigins(ctx, userID)
}

// AdminRunOutcomeOrigins returns the factory-wide per-origin failure causes for both
// windows (PRD #1293 M1).
func (s *Service) AdminRunOutcomeOrigins(ctx context.Context) ([]store.AdminRunOutcomeOriginsRow, error) {
	return s.q.AdminRunOutcomeOrigins(ctx)
}

// AdminRunOutcomeOriginsPerUser returns the per-user lifetime per-origin failure causes
// (PRD #1293 M1), keyed by user id so the handler attaches each user's causes.
func (s *Service) AdminRunOutcomeOriginsPerUser(ctx context.Context) ([]store.AdminRunOutcomeOriginsPerUserRow, error) {
	return s.q.AdminRunOutcomeOriginsPerUser(ctx)
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
	// ExtensionSeconds is the new TOTAL budget_extension_seconds after a successful `extend`
	// (PRD #1189 M1) — the value CreateExtendInput's RETURNING gives back — and DeadlineAt is
	// the run's projected wall-clock deadline once the extension applies, both so the CLI can
	// print the outcome without a read-back. Nil on every non-extend path.
	ExtensionSeconds *int
	DeadlineAt       *time.Time
	// Resumed is true when an owner action on a wall park both applied AND resumed the run in one
	// step (PRD #1497 M1): ExtendAndResumeWallPark and StopWallPark. It lets the CLI/handler say the
	// run resumed, not merely that time was added. False on every other path (including a plain
	// extend of a running run, which does not resume).
	Resumed bool
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
	//
	// Issue #1197: recovery_wait is a worker-held park of the SAME shape as limit_wait —
	// the run keeps its worker_id for affinity and that worker keeps heartbeating for its
	// other runs, but it is NOT polling this parked run (the worker parked it and awaits the
	// server-owned promotion), so a cancel routed to a poller would sit unconsumed. It must
	// go server-side too, so it reads as "no live poller" here alongside the other parks.
	if run.Status == "queued" || run.Status == "limit_wait" || run.Status == "pool_wait" || run.Status == "recovery_wait" || !run.WorkerID.Valid {
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
	if s.now().Sub(wkr.LastHeartbeatAt.Time) >= s.p.WorkerHeartbeatStale {
		// The worker's heartbeat has gone stale for ALL its runs — no live poller, unchanged.
		return false, nil
	}
	// PRD #1391 Run B M3d (D13): the worker's heartbeat is fresh, but a run whose executor
	// finished and journaled a terminal outcome sits with a terminal_pending lease on its owning
	// worker (or that worker is under an unexpired pending_overflow) even while the worker keeps
	// heartbeating for its OTHER runs. The poller for THIS run is gone — the executor no longer
	// exists — so a cancel must go server-side (the atomic no-live-poller branch), NOT be enqueued
	// for an executor that will never consume it. The predicate is the POSITIVE form of the D11
	// claim-exclusion join; chat is exempt (D6/D10: it never journals a terminal), so it is skipped
	// here rather than round-tripped for a query that always answers false.
	if run.Kind != runkind.Chat {
		pending, perr := s.q.RunHasPendingOutcomeLease(ctx, run.ID)
		if perr != nil {
			return false, perr
		}
		if pending {
			return false, nil
		}
	}
	return true, nil
}

// -------------------------------------------------------------------------
// Sweeper
// -------------------------------------------------------------------------

// SweepResult reports the row counts touched by one Sweep pass (for logging).
type SweepResult struct {
	WorkersOffline int64
	ClaimedReset   int64
	// TaskUndispatchedFailed is the number of kind='task' (handoff) runs this pass
	// terminalized because they sat queued with dispatched_at NULL past DispatchGrace —
	// the CLI never landed their push/dispatch (issue #1367). Normally 0: a handoff is
	// dispatched within seconds of creation, so a non-zero count means a client crashed or
	// its push failed between create and DispatchTaskRun.
	TaskUndispatchedFailed int64
	// WallParkRequested is the number of past-deadline runs this pass filed a system 'wall' pause
	// request on because their worker is alive and speaks wall_park_v1 (PRD #1497 M1): the worker
	// drops its turn and takes the capture-first park. Set from the RequestWallParks row count.
	// REPLACES the old RunningTimeout (the wall no longer fails a run).
	WallParkRequested int64
	// WallParked is the number of runs this pass parked server-side because their worker is dead,
	// incapable, or unresponsive past the grace (PRD #1497 M1, D5). Set from the ParkRunsAtWall row
	// count. These transition to paused (hold_reason='budget_exhausted') and are fanned out via
	// publishSwept; they are NOT enqueued to the judge (the run is not finished).
	WallParked         int64
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
	// CodexRefreshRecovered is the number of ACCOUNTS this pass recovered while reaping accounts
	// wedged by an interrupted subscription refresh (issue #1532): an in_progress account with an
	// expired lease, or an orphaned rotating intent, is quarantined and its intents driven to a
	// terminal state so a re-link can recover it. Normally 0 (the candidate scan matches nothing
	// on a healthy instance); a non-zero count means that many accounts were un-wedged. Counts
	// accounts, not intents, so a reap that resolves zero intents (the rare crash window where an
	// account is left in_progress with an already-terminal intent) is still observable — see
	// SweepUnresolvedCodexRefresh.
	CodexRefreshRecovered int64
	// CodexAccountParked is the number of queued Codex subscription runs this pass moved to
	// recovery_wait with cause codex_account_unavailable (PRD #1590 M2, D2): runs ClaimRun's
	// account gate excludes because their account is quarantined or a re-login on their alias
	// is in flight. Normally 0. Each tick examines at most one bounded page of queued Codex
	// subscription runs, so a gated run deep in a large queue is parked within a few ticks.
	CodexAccountParked int64
	// CodexAccountPromoted is the number of codex_account_unavailable runs this pass returned
	// from recovery_wait to queued (PRD #1590 M3, D3) because the unchanged release predicate
	// passed on their alias and account, read under lock. Normally 0. Bounded by one page of
	// held runs per tick.
	CodexAccountPromoted int64
	// CodexAccountReadmitted is how many of CodexAccountPromoted were first re-admitted after a
	// verified same-identity re-login on their alias (PRD #1590 M4, D5: runs.codex_material_revision
	// advanced to the alias's, with a feed status line). Normally 0.
	CodexAccountReadmitted int64
	// CodexAccountFailed is the number of codex_account_unavailable runs this pass failed
	// credential_unavailable (PRD #1590 M4, D5): the alias was deleted while held, or a linked
	// alias now names a different identity, credential revision, auth mode or kind. Normally 0.
	CodexAccountFailed int64
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
	// LimitReevaluated is the number of still-parked limit_wait runs this pass LOWERED to
	// retry_not_before = now() because their `auto` next claim now has a pooled
	// alternative that is spendable sooner than their park (PRD #1247 M3, D8 — Decision
	// 6e extended from park-time to park-duration). At most ONE per distinct owner per
	// tick (the same anti-stampede stagger PoolResumed applies, since this pass bypasses
	// the park-time jitter). A lowered run is transitioned to queued by the following
	// PromoteLimitWaitRuns pass — the same tick once that pass's clock has reached the
	// lowered stamp, else the next — so a non-zero LimitReevaluated is normally mirrored
	// by LimitPromoted within a tick. Normally 0.
	LimitReevaluated int64
	// RecoveryPromoted is the number of runs this pass auto-promoted from recovery_wait to
	// queued because their recovery_retry_not_before elapsed (issue #1197). Normally 0: the
	// partial index this reads covers only parked runs, a set that is empty on a healthy
	// instance. Counted like LimitPromoted (len of the returned slice).
	RecoveryPromoted int64
	// CompletionBudgetExhausted is the number of runs this pass armed with the server-side
	// served `budget_exhausted` steer (PRD #1226 M4, D3): the post-attempt live-worker
	// interlocked rows past their wall budget that SweepRunningTimeout's carve-out spared, now
	// stamped so their live lead is steered into the completion hold. Set from the execrows
	// count. Normally 0 (the completion interlock is rollout-OFF and this only fires on a spared,
	// budget-exhausted run).
	CompletionBudgetExhausted int64
	// CustodyReleased is the number of OPEN custody holds this pass released because
	// their release was warranted (a completed run, or a ready capture) but never
	// applied — the backstop for a best-effort terminal release that failed after a
	// successful publication (PRD #1296 M4, D3). Releasing nulls the hold's live FKs so
	// normal reap can then delete the worker. Normally 0: the candidate query reads only
	// stuck holds, a set that is empty on a healthy instance.
	CustodyReleased int64
	// RecoveryStalled is the number of durable-archive captures this pass flipped from a
	// non-terminal upload state (preparing/uploading) to needs_action because they sat past
	// the UZI_RECOVERY_UPLOAD_RETRY_WINDOW (PRD #1296 D3/D4). The custody hold is NOT released
	// — the source is retained until capture succeeds or the owner explicitly discards.
	// Normally 0 (a healthy upload becomes available in seconds), and 0 when the window is
	// non-positive (the sweep disables the pass).
	RecoveryStalled int64
	// RecoveryExpired is the number of durable-archive captures this pass flipped from
	// 'available' to 'expired' because they sat past their expires_at — the enforcement of the
	// UZI_RECOVERY_READY_RETENTION window (PRD #1296 D4). The same statement also DELETES those
	// captures' encrypted chunk rows (byte reclamation); the capture metadata row is kept in
	// state 'expired'. Custody is never touched (an expired capture whose hold is still open
	// stays retained per D3). Normally 0, and 0 when the retention is non-positive (the sweep
	// disables the pass).
	RecoveryExpired int64
}

// -------------------------------------------------------------------------
// pgtype helpers
// -------------------------------------------------------------------------

// maxFailureReasonRunes bounds the worker-reported failure reason before it lands
// in runs.failure_reason. The worker already slices to 512 (reportState), but the
// API does not take its word for the length any more than for the content — this is
// generous headroom over that slice while still bounding a hostile or buggy worker.
const maxFailureReasonRunes = 2048

// PreservedPatchMaxBytes bounds runs.preserved_patch — the agent's branch diff carried
// on a workflow_scope_missing `failed` report (PRD #377 M1). Generous headroom: the
// motivating #188 workflow file was ~112 lines, but a run can touch several workflow
// files, and this is a one-off column read on a single failed card, not an index key.
// The agent already secret-scrubs and size-caps before sending; this is the server-side
// bound that does not take the worker's word for the length.
const PreservedPatchMaxBytes = 1 << 20 // 1 MiB
