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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
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

// Terminal run statuses. A run in any of these is finished and immutable.
var terminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// Sentinel errors mapped to HTTP status codes by the handlers.
var (
	ErrRunNotFound         = errors.New("run not found")
	ErrRunNotOwned         = errors.New("run not owned by worker")
	ErrRepoNotFound        = errors.New("repo not found")
	ErrIssueNotFound       = errors.New("issue not found")
	ErrNoPRDLink           = errors.New("issue has no PRD link")
	ErrDescriptionTooLarge = errors.New("issue description is too large to run")
	ErrActiveRunExists     = errors.New("a non-terminal run already exists for this issue")
	ErrRunTerminal         = errors.New("run has already finished")
	ErrInvalidState        = errors.New("invalid run state")
	ErrInvalidMessage      = errors.New("invalid run message")
	ErrWorkerNotFound      = errors.New("worker not found")
	// ErrWorkerHasActiveRuns rejects deletion of a worker that still owns a
	// non-terminal run: the FK is ON DELETE SET NULL, so deleting would orphan the
	// run past every sweep (and the one-active-run index would then block re-runs).
	ErrWorkerHasActiveRuns = errors.New("worker has active runs")
)

// Store is the narrow set of generated queries workersvc uses. *store.Queries
// satisfies it; tests embed it and override only the methods they exercise.
type Store interface {
	// Workers.
	CreateWorker(ctx context.Context, arg store.CreateWorkerParams) (store.Worker, error)
	GetWorkerByID(ctx context.Context, id uuid.UUID) (store.Worker, error)
	GetWorkerByIDForUser(ctx context.Context, arg store.GetWorkerByIDForUserParams) (store.Worker, error)
	ListWorkersByUser(ctx context.Context, userID uuid.UUID) ([]store.ListWorkersByUserRow, error)
	RegisterWorker(ctx context.Context, arg store.RegisterWorkerParams) (store.Worker, error)
	HeartbeatWorker(ctx context.Context, id uuid.UUID) (store.Worker, error)
	DeleteWorkerForUser(ctx context.Context, arg store.DeleteWorkerForUserParams) (int64, error)
	CountWorkerNonTerminalRuns(ctx context.Context, arg store.CountWorkerNonTerminalRunsParams) (int64, error)
	MarkStaleWorkersOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)

	// Runs.
	CreateRun(ctx context.Context, arg store.CreateRunParams) (store.Run, error)
	// CI-fix runs (PRD #6).
	CreateCIFixRun(ctx context.Context, arg store.CreateCIFixRunParams) (store.Run, error)
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
	SetRunRunning(ctx context.Context, arg store.SetRunRunningParams) (int64, error)
	SetRunAwaitingApproval(ctx context.Context, arg store.SetRunAwaitingApprovalParams) (int64, error)
	SetRunCompleted(ctx context.Context, arg store.SetRunCompletedParams) (int64, error)
	SetRunFailed(ctx context.Context, arg store.SetRunFailedParams) (int64, error)
	MarkRunFailedByID(ctx context.Context, arg store.MarkRunFailedByIDParams) (int64, error)
	CancelRunServerSide(ctx context.Context, arg store.CancelRunServerSideParams) (int64, error)
	RejectRunServerSide(ctx context.Context, arg store.RejectRunServerSideParams) (int64, error)
	UpdateRunLastSeq(ctx context.Context, arg store.UpdateRunLastSeqParams) (int64, error)

	// Sweeper + register-time orphan recovery.
	SweepClaimedNeverStarted(ctx context.Context, cutoff pgtype.Timestamptz) ([]store.SweepClaimedNeverStartedRow, error)
	// RequeueClaimedRunToQueued resets one just-claimed run to queued when its
	// owner's vault locked between the claim gate and the token open (PRD #32 M3).
	RequeueClaimedRunToQueued(ctx context.Context, id uuid.UUID) (int64, error)
	SweepRunningTimeout(ctx context.Context, arg store.SweepRunningTimeoutParams) ([]store.SweepRunningTimeoutRow, error)
	FailRunsOfStaleWorkersOverCap(ctx context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) ([]store.FailRunsOfStaleWorkersOverCapRow, error)
	RequeueRunsOfStaleWorkers(ctx context.Context, arg store.RequeueRunsOfStaleWorkersParams) ([]store.RequeueRunsOfStaleWorkersRow, error)
	FailWorkerRunsOverCap(ctx context.Context, arg store.FailWorkerRunsOverCapParams) (int64, error)
	RequeueWorkerRuns(ctx context.Context, arg store.RequeueWorkerRunsParams) (int64, error)

	// Messages + inputs.
	InsertRunMessage(ctx context.Context, arg store.InsertRunMessageParams) (int64, error)
	ListRunMessagesAfter(ctx context.Context, arg store.ListRunMessagesAfterParams) ([]store.RunMessage, error)
	CreateRunInput(ctx context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error)
	CreateStopVerdictInput(ctx context.Context, arg store.CreateStopVerdictInputParams) (store.RunUserInput, error)
	ConsumeRunInputs(ctx context.Context, runID uuid.UUID) ([]store.ConsumeRunInputsRow, error)

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
	RunTimeout           time.Duration
	RunIdleTimeout       time.Duration
	RunMaxIterations     int
	RunMaxRequeues       int
	WorkerHeartbeatStale time.Duration
	WorkerAffinityGrace  time.Duration
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
}

// Broadcaster receives run events after they are persisted, for live fan-out to
// browsers. It is the seam onto the WS hub; nil in tests and any deployment that
// serves no live channel. Every method is best-effort and must never block or
// error the persistence path (the DB write is authoritative).
type Broadcaster interface {
	// PublishMessage forwards one newly-persisted run message.
	PublishMessage(runID uuid.UUID, seq int32, kind, agent string, payload []byte, createdAt time.Time)
	// PublishState signals that a run's status changed.
	PublishState(runID uuid.UUID, status string)
}

// MultiBroadcaster fans each event out to several Broadcasters — the WS hub AND
// the Slack notifier (PRD #25 M3). Each is best-effort and non-blocking by its
// own contract, so the fan-out is a plain iteration. A nil or empty value is a
// valid no-op broadcaster.
type MultiBroadcaster []Broadcaster

func (m MultiBroadcaster) PublishMessage(runID uuid.UUID, seq int32, kind, agent string, payload []byte, createdAt time.Time) {
	for _, b := range m {
		b.PublishMessage(runID, seq, kind, agent, payload, createdAt)
	}
}

func (m MultiBroadcaster) PublishState(runID uuid.UUID, status string) {
	for _, b := range m {
		b.PublishState(runID, status)
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
}

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
	return &Service{q: q, box: box, p: p, now: time.Now}
}

// -------------------------------------------------------------------------
// Worker protocol
// -------------------------------------------------------------------------

// Register brings a worker online and recovers its orphaned runs. A registering
// worker has, by definition, just started fresh and is executing nothing, so any
// run it still holds (claimed/running/awaiting_approval) is orphaned: over the
// re-queue budget it is failed, otherwise it is re-queued to this same worker
// (affinity) to be re-claimed and resumed from the persisted session. This is
// what makes `docker compose down && up` recover — the out-of-process worker's
// fresh-start signal, which the server cannot infer from heartbeats alone.
func (s *Service) Register(ctx context.Context, wkr store.Worker, version, template string) (store.Worker, error) {
	max := int32(s.p.RunMaxRequeues)
	if _, err := s.q.FailWorkerRunsOverCap(ctx, store.FailWorkerRunsOverCapParams{
		FailureReason: pgText("worker restarted; run orphaned and out of re-queue budget"),
		WorkerID:      pgUUID(wkr.ID),
		MaxRequeues:   max,
	}); err != nil {
		return store.Worker{}, err
	}
	if _, err := s.q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{
		WorkerID:    pgUUID(wkr.ID),
		MaxRequeues: max,
	}); err != nil {
		return store.Worker{}, err
	}
	// template is the worker's self-reported image template (PRD #18); empty →
	// NULL (older image sends none). Soft signal only; never rejected here.
	return s.q.RegisterWorker(ctx, store.RegisterWorkerParams{
		Version:          pgText(version),
		TemplateReported: pgText(template),
		ID:               wkr.ID,
	})
}

// Heartbeat refreshes liveness and returns the (possibly updated) worker.
func (s *Service) Heartbeat(ctx context.Context, wkr store.Worker) (store.Worker, error) {
	return s.q.HeartbeatWorker(ctx, wkr.ID)
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

	run, err := s.q.ClaimRun(ctx, store.ClaimRunParams{
		WorkerID:       pgUUID(wkr.ID),
		UserID:         wkr.UserID,
		AffinityCutoff: pgTime(s.now().Add(-s.p.WorkerAffinityGrace)),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // idle
		}
		return nil, err
	}

	payload, err := s.assembleClaim(ctx, run)
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

// openAnthropic opens the owner's decrypted Anthropic token — the one secret both
// the run lane and the chat lane deliver. With the vault wired (production), route
// through vault.Open on the row's sealed_with: a 'dek' row needs the owner's vault
// unlocked, and a lock that landed after the claim gate surfaces as vault.ErrLocked
// → errVaultLocked (requeue, never fail). A legacy 'master' row opens under the
// master box without an unlock (the claim gate is what withholds a locked owner's
// runs; the master box is not DEK-protected). Nil vault (tests) opens under the
// master box directly. A missing/undecryptable token is errCredentialUnavailable.
func (s *Service) openAnthropic(ctx context.Context, userID uuid.UUID) ([]byte, error) {
	secret, err := s.q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: userID,
		Kind:   store.KindAnthropicToken,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
		}
		return nil, fmt.Errorf("anthropic secret lookup: %w", err)
	}
	var anthropic []byte
	if s.vlt != nil {
		anthropic, err = s.vlt.Open(userID, store.KindAnthropicToken, secret.SealedWith, secret.Ciphertext)
		if errors.Is(err, vault.ErrLocked) {
			return nil, errVaultLocked
		}
	} else {
		anthropic, err = s.box.Open(secret.Ciphertext)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: Anthropic token could not be decrypted", errCredentialUnavailable)
	}
	return anthropic, nil
}

// assembleClaim builds the claim payload for an already-claimed run.
func (s *Service) assembleClaim(ctx context.Context, run store.Run) (*ClaimPayload, error) {
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

	anthropic, err := s.openAnthropic(ctx, run.UserID)
	if err != nil {
		return nil, err
	}

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
		Repo: ClaimRepo{
			ID:            uuid.UUID(run.RepoID.Bytes).String(),
			URL:           rc.RepoWebUrl,
			CloneURL:      rc.RepoWebUrl + ".git",
			DefaultBranch: textPtr(rc.DefaultBranch),
			SkillsEnabled: rc.RepoSkillsEnabled,
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
			RunTimeoutSeconds:  int(s.p.RunTimeout.Seconds()),
			IdleTimeoutSeconds: int(s.p.RunIdleTimeout.Seconds()),
			MaxIterations:      s.p.RunMaxIterations,
			DefaultModel:       textPtr(defaultModel),
			SkillMaxBytes:      s.p.SkillMaxBytes,
			SkillsMaxPerRun:    s.p.SkillsMaxPerRun,
			ToolPackages:       toolPackages,
			RepoDevboxOptIn:    rc.RepoDevboxOptIn,
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

// IncomingMessage is one seq-numbered message a worker appends.
type IncomingMessage struct {
	Seq     int32           `json:"seq"`
	Kind    string          `json:"kind"`
	Agent   string          `json:"agent"`
	Payload json.RawMessage `json:"payload"`
}

// AppendMessages persists a worker's batched messages (idempotent on
// (run_id, seq)) and advances the run's last_seq high-water mark. The worker
// must own the run.
func (s *Service) AppendMessages(ctx context.Context, wkr store.Worker, runID uuid.UUID, msgs []IncomingMessage) error {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return err
	}
	// Validate the whole batch before persisting any of it: a single invalid
	// message rejects the batch with nothing written (all-or-nothing), so a
	// [valid, valid, invalid] batch never leaves the first two half-persisted.
	var maxSeq int32
	for _, m := range msgs {
		if m.Seq <= 0 || m.Kind == "" || len(m.Payload) == 0 || !json.Valid(m.Payload) {
			return ErrInvalidMessage
		}
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	inserted := make([]IncomingMessage, 0, len(msgs))
	for _, m := range msgs {
		rows, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID:   runID,
			Seq:     m.Seq,
			Kind:    m.Kind,
			Agent:   pgText(m.Agent),
			Payload: []byte(m.Payload),
		})
		if err != nil {
			return err
		}
		// rows == 0 means a duplicate (run_id, seq) — a worker re-delivery. Only
		// broadcast genuinely new messages so a retry never double-emits over WS.
		if rows > 0 {
			inserted = append(inserted, m)
		}
	}
	if maxSeq > run.LastSeq {
		if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: maxSeq}); err != nil {
			return err
		}
	}
	// Fan out after the log + high-water mark are durably advanced, so a browser
	// that reacts by replaying from last_seq sees a consistent state.
	if s.bcast != nil {
		now := s.now()
		for _, m := range inserted {
			s.bcast.PublishMessage(runID, m.Seq, m.Kind, m.Agent, []byte(m.Payload), now)
		}
	}
	return nil
}

// StateRequest is the worker's report of a run's new state. Only the fields
// relevant to State are read. The wire key is `status` (matches the runs.status
// column, the M2 worker client, and multica's protocol); the Go field stays
// `State` to avoid churn in the switch below.
type StateRequest struct {
	State          string  `json:"status"` // running|awaiting_approval|completed|failed
	PlanMd         *string `json:"plan_md"`
	Branch         *string `json:"branch"`
	MrIID          *int64  `json:"mr_iid"`
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
}

// SetState applies a worker's state transition and returns the run's resulting
// row plus whether the transition was applied. All transitions are guarded
// against terminal statuses, so a report that lands on an already-terminal run
// (e.g. a cancel raced in) is a no-op: applied is false and the run's real
// status is returned. The handler maps applied==false to 409 (the worker treats
// "already terminal" as success and learns it was cancelled), per the M2 wire
// contract.
func (s *Service) SetState(ctx context.Context, wkr store.Worker, runID uuid.UUID, req StateRequest) (run store.Run, applied bool, err error) {
	if _, err = s.runOwnedByWorker(ctx, runID, wkr); err != nil {
		return store.Run{}, false, err
	}
	sessionID := textParam(req.SessionID)
	var rows int64
	switch req.State {
	case "running":
		rows, err = s.q.SetRunRunning(ctx, store.SetRunRunningParams{
			IterationCount: req.IterationCount, SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "awaiting_approval":
		rows, err = s.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: textParam(req.PlanMd), SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "completed":
		rows, err = s.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch: textParam(req.Branch), MrIid: int8Param(req.MrIID), SessionID: sessionID,
			FixVerdict: clampWireFixVerdict(req.FixVerdict), ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "failed":
		rows, err = s.q.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: textParam(req.FailureReason), SessionID: sessionID, ID: runID, WorkerID: pgUUID(wkr.ID),
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
	}
	return run, rows > 0, err
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
	for _, row := range rows {
		out = append(out, InputDTO{ID: row.ID, Kind: row.Kind, Body: textPtr(row.Body), CreatedAt: row.CreatedAt.Time})
	}
	return out, nil
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

// -------------------------------------------------------------------------
// Web-facing worker + run management
// -------------------------------------------------------------------------

// CreateWorker issues a worker for the user and returns the plaintext join token
// exactly once (only its hash is stored).
func (s *Service) CreateWorker(ctx context.Context, userID uuid.UUID, name, templateDeclared string) (store.Worker, string, error) {
	token, hash, err := jointoken.Generate()
	if err != nil {
		return store.Worker{}, "", err
	}
	// templateDeclared is the UI-chosen worker template (PRD #18), validated
	// against the registry by the caller; empty → NULL (no choice made).
	wkr, err := s.q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID:           userID,
		Name:             name,
		TokenHash:        hash,
		TemplateDeclared: pgText(templateDeclared),
	})
	if err != nil {
		return store.Worker{}, "", err
	}
	return wkr, token, nil
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
func (s *Service) CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, allowWithoutPRD bool) (store.Run, error) {
	return s.createRun(ctx, userID, repoID, issueIID, description, false, allowWithoutPRD)
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
	return s.createRun(ctx, userID, repoID, issueIID, description, true, allowWithoutPRD)
}

func (s *Service) createRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string, autoApprove, allowWithoutPRD bool) (store.Run, error) {
	// The description cap is enforced HERE, once, so the manual (handler → 422) and
	// autopilot (poller → too-large comment) paths cannot drift (PRD #19 M5). Checked
	// first: it is pure input validation, independent of the repo/issue gates below.
	if len(description) > MaxIssueDescriptionBytes {
		return store.Run{}, ErrDescriptionTooLarge
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

// SubmitInputResult reports how a steering input was handled.
type SubmitInputResult struct {
	// ServerSide is true when a cancel/reject was applied directly because no
	// live poller would ever consume it.
	ServerSide bool
}

// SubmitInput records a steering input (approve/reject/follow-up/cancel) for a
// run the user owns. When the target is a cancel or plan rejection and no live
// poller exists (the run is still queued, or its worker has gone stale), the
// transition is applied server-side so the input is never stranded waiting for a
// GET /inputs poll that will never come. Otherwise the input is enqueued for the
// worker to consume.
func (s *Service) SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string) (SubmitInputResult, error) {
	run, err := s.GetRun(ctx, userID, runID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if terminalStatuses[run.Status] {
		return SubmitInputResult{}, ErrRunTerminal
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

	// A plain steering input (approve_plan / follow_up): enqueue for the worker with
	// no stop signal and no runs-row touch.
	if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: kind, Body: pgText(body),
	}); err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false}, nil
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
	if run.Status == "queued" || !run.WorkerID.Valid {
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
}

// Sweep enforces the liveness rules the workers cannot: stale workers go offline
// and their non-terminal runs are re-queued (or failed past the re-queue cap);
// claimed-but-never-started runs are reclaimed; runs past RUN_TIMEOUT are failed.
// It is called on a ticker and once immediately at boot (the orphan sweep).
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	now := s.now()
	staleCutoff := pgTime(now.Add(-s.p.WorkerHeartbeatStale))
	claimCutoff := pgTime(now.Add(-s.p.ClaimGrace))
	runCutoff := pgTime(now.Add(-s.p.RunTimeout))
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
	}

	timedOut, err := s.q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
		FailureReason: pgText("run exceeded RUN_TIMEOUT"),
		Cutoff:        runCutoff,
	})
	if err != nil {
		return res, fmt.Errorf("sweep running-timeout: %w", err)
	}
	res.RunningTimeout = int64(len(timedOut))
	for _, r := range timedOut {
		s.publishSwept(r.ID, r.Status)
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

func int8Param(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

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

// isUniqueViolation reports whether err is a Postgres unique-constraint failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
