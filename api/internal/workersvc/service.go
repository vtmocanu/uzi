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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/jointoken"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// anthropicTokenKind mirrors the secret kind PRD #3's handler stores under.
const anthropicTokenKind = "anthropic_token"

// Terminal run statuses. A run in any of these is finished and immutable.
var terminalStatuses = map[string]bool{"completed": true, "failed": true, "cancelled": true}

// Sentinel errors mapped to HTTP status codes by the handlers.
var (
	ErrRunNotFound     = errors.New("run not found")
	ErrRunNotOwned     = errors.New("run not owned by worker")
	ErrRepoNotFound    = errors.New("repo not found")
	ErrIssueNotFound   = errors.New("issue not found")
	ErrNoPRDLink       = errors.New("issue has no PRD link")
	ErrActiveRunExists = errors.New("a non-terminal run already exists for this issue")
	ErrRunTerminal     = errors.New("run has already finished")
	ErrInvalidState    = errors.New("invalid run state")
	ErrInvalidMessage  = errors.New("invalid run message")
	ErrWorkerNotFound  = errors.New("worker not found")
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
	MarkStaleWorkersOffline(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)

	// Runs.
	CreateRun(ctx context.Context, arg store.CreateRunParams) (store.Run, error)
	GetRunByIDForUser(ctx context.Context, arg store.GetRunByIDForUserParams) (store.Run, error)
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
	SweepClaimedNeverStarted(ctx context.Context, cutoff pgtype.Timestamptz) (int64, error)
	SweepRunningTimeout(ctx context.Context, arg store.SweepRunningTimeoutParams) (int64, error)
	FailRunsOfStaleWorkersOverCap(ctx context.Context, arg store.FailRunsOfStaleWorkersOverCapParams) (int64, error)
	RequeueRunsOfStaleWorkers(ctx context.Context, arg store.RequeueRunsOfStaleWorkersParams) (int64, error)
	FailWorkerRunsOverCap(ctx context.Context, arg store.FailWorkerRunsOverCapParams) (int64, error)
	RequeueWorkerRuns(ctx context.Context, arg store.RequeueWorkerRunsParams) (int64, error)

	// Messages + inputs.
	InsertRunMessage(ctx context.Context, arg store.InsertRunMessageParams) (int64, error)
	ListRunMessagesAfter(ctx context.Context, arg store.ListRunMessagesAfterParams) ([]store.RunMessage, error)
	CreateRunInput(ctx context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error)
	ConsumeRunInputs(ctx context.Context, runID uuid.UUID) ([]store.ConsumeRunInputsRow, error)

	// Cross-cutting reads for run creation + claim.
	GetRepoForUser(ctx context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error)
	GetIssueByIID(ctx context.Context, arg store.GetIssueByIIDParams) (store.Issue, error)
	GetUserSecretCiphertext(ctx context.Context, arg store.GetUserSecretCiphertextParams) ([]byte, error)
	ListAgentTemplates(ctx context.Context) ([]store.AgentTemplate, error)
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
}

// Service holds the store, the secret cipher, and the runtime params.
type Service struct {
	q   Store
	box *secretbox.Box
	p   Params
	// now is time.Now in production; overridable in tests for deterministic
	// cutoffs.
	now func() time.Time
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
func (s *Service) Register(ctx context.Context, wkr store.Worker, version string) (store.Worker, error) {
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
	return s.q.RegisterWorker(ctx, store.RegisterWorkerParams{Version: pgText(version), ID: wkr.ID})
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
		// A missing/undecryptable credential is not retryable: fail the run and
		// report idle. The failure reason never carries secret bytes.
		if errors.Is(err, errCredentialUnavailable) {
			if _, ferr := s.q.MarkRunFailedByID(ctx, store.MarkRunFailedByIDParams{
				ID:            run.ID,
				FailureReason: pgText(err.Error()),
			}); ferr != nil {
				return nil, ferr
			}
			return nil, nil // idle; the run now shows failed in the UI
		}
		// The run vanished between claim and payload assembly (its forge
		// connection was deleted, cascading repo → run away). Nothing to fail or
		// hand out; report idle.
		if errors.Is(err, errRunVanished) {
			return nil, nil
		}
		return nil, err
	}
	return payload, nil
}

// errCredentialUnavailable marks a claim that cannot proceed because a required
// secret is absent or cannot be decrypted. Its message is safe to store as a
// run failure reason (it never includes secret bytes).
var errCredentialUnavailable = errors.New("credential unavailable")

// errRunVanished marks a claim whose run disappeared before its payload could be
// assembled (a cascading delete of the forge connection).
var errRunVanished = errors.New("run vanished before claim assembly")

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

	sealed, err := s.q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: run.UserID,
		Kind:   anthropicTokenKind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: no Anthropic token configured for this user", errCredentialUnavailable)
		}
		return nil, fmt.Errorf("anthropic secret lookup: %w", err)
	}
	anthropic, err := s.box.Open(sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: Anthropic token could not be decrypted", errCredentialUnavailable)
	}

	templates, err := s.q.ListAgentTemplates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent templates: %w", err)
	}

	return &ClaimPayload{
		RunID:            run.ID.String(),
		IssueIID:         run.IssueIid,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		Status:           run.Status,
		Branch:           textPtr(run.Branch),
		SessionID:        textPtr(run.SessionID),
		LastSeq:          run.LastSeq,
		IterationCount:   run.IterationCount,
		RequeueCount:     run.RequeueCount,
		PlanMd:           textPtr(run.PlanMd),
		Repo: ClaimRepo{
			ID:            run.RepoID.String(),
			URL:           rc.RepoWebUrl,
			CloneURL:      rc.RepoWebUrl + ".git",
			DefaultBranch: textPtr(rc.DefaultBranch),
		},
		Secrets: ClaimSecrets{
			ForgeUsername:       rc.BotUsername,
			ForgePAT:            string(botPAT),
			AnthropicOAuthToken: string(anthropic),
		},
		Agents: agentsFromTemplates(templates),
		Config: ClaimConfig{
			RunTimeoutSeconds:  int(s.p.RunTimeout.Seconds()),
			IdleTimeoutSeconds: int(s.p.RunIdleTimeout.Seconds()),
			MaxIterations:      s.p.RunMaxIterations,
		},
	}, nil
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
	var maxSeq int32
	for _, m := range msgs {
		if m.Seq <= 0 || m.Kind == "" || len(m.Payload) == 0 || !json.Valid(m.Payload) {
			return ErrInvalidMessage
		}
		if _, err := s.q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID:   runID,
			Seq:     m.Seq,
			Kind:    m.Kind,
			Agent:   pgText(m.Agent),
			Payload: []byte(m.Payload),
		}); err != nil {
			return err
		}
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	if maxSeq > run.LastSeq {
		if _, err := s.q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{ID: runID, Seq: maxSeq}); err != nil {
			return err
		}
	}
	return nil
}

// StateRequest is the worker's report of a run's new state. Only the fields
// relevant to State are read.
type StateRequest struct {
	State          string  `json:"state"` // running|awaiting_approval|completed|failed
	PlanMd         *string `json:"plan_md"`
	Branch         *string `json:"branch"`
	MrIID          *int64  `json:"mr_iid"`
	FailureReason  *string `json:"failure_reason"`
	IterationCount int32   `json:"iteration_count"`
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
	var rows int64
	switch req.State {
	case "running":
		rows, err = s.q.SetRunRunning(ctx, store.SetRunRunningParams{
			IterationCount: req.IterationCount, ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "awaiting_approval":
		rows, err = s.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: textParam(req.PlanMd), ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "completed":
		rows, err = s.q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch: textParam(req.Branch), MrIid: int8Param(req.MrIID), ID: runID, WorkerID: pgUUID(wkr.ID),
		})
	case "failed":
		rows, err = s.q.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: textParam(req.FailureReason), ID: runID, WorkerID: pgUUID(wkr.ID),
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
func (s *Service) CreateWorker(ctx context.Context, userID uuid.UUID, name string) (store.Worker, string, error) {
	token, hash, err := jointoken.Generate()
	if err != nil {
		return store.Worker{}, "", err
	}
	wkr, err := s.q.CreateWorker(ctx, store.CreateWorkerParams{UserID: userID, Name: name, TokenHash: hash})
	if err != nil {
		return store.Worker{}, "", err
	}
	return wkr, token, nil
}

// ListWorkers returns the user's workers with derived busy status.
func (s *Service) ListWorkers(ctx context.Context, userID uuid.UUID) ([]store.ListWorkersByUserRow, error) {
	return s.q.ListWorkersByUser(ctx, userID)
}

// DeleteWorker revokes a worker (its token stops authenticating).
func (s *Service) DeleteWorker(ctx context.Context, userID, workerID uuid.UUID) error {
	n, err := s.q.DeleteWorkerForUser(ctx, store.DeleteWorkerForUserParams{ID: workerID, UserID: userID})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWorkerNotFound
	}
	return nil
}

// CreateRun queues a run from a board card. The issue must be a cached PRD issue
// (with a PRD link) in a repo the user owns; its title is snapshotted from the
// cache and its description from the request, so the run is self-contained even
// if the issue cache is later evicted. The one-non-terminal-run-per-issue index
// rejects a duplicate active run.
func (s *Service) CreateRun(ctx context.Context, userID, repoID uuid.UUID, issueIID int64, description string) (store.Run, error) {
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
	if !issue.HasPrdLink {
		return store.Run{}, ErrNoPRDLink
	}
	run, err := s.q.CreateRun(ctx, store.CreateRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueIid:         issueIID,
		IssueTitle:       issue.Title,
		IssueDescription: description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActiveRunExists
		}
		return store.Run{}, err
	}
	return run, nil
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
			if kind == "cancel" {
				_, err = s.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{ID: runID, UserID: userID})
			} else {
				_, err = s.q.RejectRunServerSide(ctx, store.RejectRunServerSideParams{
					ID: runID, UserID: userID, FailureReason: pgText("plan rejected"),
				})
			}
			if err != nil {
				return SubmitInputResult{}, err
			}
			return SubmitInputResult{ServerSide: true}, nil
		}
	}

	if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: kind, Body: pgText(body),
	}); err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false}, nil
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
	WorkersOffline int64
	ClaimedReset   int64
	RunningTimeout int64
	StaleFailed    int64
	StaleRequeued  int64
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
	if res.ClaimedReset, err = s.q.SweepClaimedNeverStarted(ctx, claimCutoff); err != nil {
		return res, fmt.Errorf("sweep claimed-never-started: %w", err)
	}
	if res.RunningTimeout, err = s.q.SweepRunningTimeout(ctx, store.SweepRunningTimeoutParams{
		FailureReason: pgText("run exceeded RUN_TIMEOUT"),
		Cutoff:        runCutoff,
	}); err != nil {
		return res, fmt.Errorf("sweep running-timeout: %w", err)
	}
	// Fail-over-cap before re-queue: the two are disjoint on requeue_count, but
	// failing first keeps a run that just hit the cap from being re-queued.
	if res.StaleFailed, err = s.q.FailRunsOfStaleWorkersOverCap(ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgText("worker lost; exceeded re-queue budget"),
		MaxRequeues:   max,
		Cutoff:        staleCutoff,
	}); err != nil {
		return res, fmt.Errorf("fail stale-worker runs over cap: %w", err)
	}
	if res.StaleRequeued, err = s.q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: max,
		Cutoff:      staleCutoff,
	}); err != nil {
		return res, fmt.Errorf("re-queue stale-worker runs: %w", err)
	}
	return res, nil
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
