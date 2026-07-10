package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// RunKindChat is the third run kind (PRD #39): a conversational session with the
// in-app uzi agent. It rides the run machinery for token delivery, message
// persistence/replay, liveness, and vault gating, but has no repo, issue, or
// branch (runs_kind_shape enforces repo_id/issue_iid/branch all NULL).
const RunKindChat = "chat"

// MaxChatMessageBytes bounds a single chat message (create prompt or follow-up).
// Generous for a conversational turn, tight enough to bound abuse; the whole-body
// cap DecodeJSON already enforces is the outer guard.
const MaxChatMessageBytes = 64 * 1024

// maxChatTitleRunes bounds the derived conversation title.
const maxChatTitleRunes = 80

// Chat sentinel errors, mapped to HTTP status by the handlers.
var (
	ErrEmptyChatMessage    = errors.New("chat message is empty")
	ErrChatMessageTooLarge = errors.New("chat message is too large")
	// ErrChatTurnCapReached is returned when a chat has hit CHAT_MAX_TURNS server
	// follow-ups; the client surfaces it as "start a new chat" (Decision 3).
	ErrChatTurnCapReached = errors.New("chat has reached its turn limit")
	// ErrChatNotEnded rejects a Continue on a still-active conversation (Decision 11
	// resumes an ENDED chat).
	ErrChatNotEnded = errors.New("chat has not ended")
	// ErrProposalNotFound covers an unknown proposal, one on another user's chat, or
	// one on a non-chat run (all indistinguishable to the caller by design).
	ErrProposalNotFound = errors.New("proposal not found")
	// ErrProposalNotPending is returned when a confirm/dismiss targets a proposal
	// that was already resolved (idempotency guard against a double click / race).
	ErrProposalNotPending = errors.New("proposal already resolved")
)

// -------------------------------------------------------------------------
// Chat claim payload (worker wire) — the chat lane's counterpart to ClaimPayload
// -------------------------------------------------------------------------

// ChatClaimPayload is the self-contained handoff a worker receives on the CHAT
// claim lane (POST /api/worker/runs/claim?lane=chat). It is a distinct, narrower
// shape than the run-lane ClaimPayload (Decision 9/10): it carries the Anthropic
// token ONLY — no forge PAT, no forge username, no repo, no agent templates, no
// skills — so the "chat claims omit the forge PAT" guarantee is STRUCTURAL (the
// field does not exist here to be populated), not an omit-empty on a shared struct.
// Like ClaimPayload it is returned only over the claim response and must never be
// logged.
type ChatClaimPayload struct {
	RunID string `json:"run_id"`
	// Kind is always "chat" — the worker's chat lane asserts it, and it keeps the
	// two lanes' payloads self-describing.
	Kind string `json:"kind"`
	// Title is the conversation's display title. There is deliberately no prompt
	// field (pinned M4 contract): the user's first message is delivered as a seeded
	// run_user_inputs follow_up (see CreateChatRun), so the worker consumes it through
	// the same input path as every later turn — one code path, uniform user_message
	// emission. A Continue run (Decision 11) carries no seeded input; the worker
	// resumes the prior session and parks awaiting the next message.
	Title  string `json:"title"`
	Status string `json:"status"`
	// SessionID is the SDK session to resume: the run's own session (a
	// requeued/resumed chat) or, for a Continue run, the resumed-from run's session
	// (Decision 11). Null for a fresh chat. ResumeOfRunID is set only for a Continue
	// run, so the worker can honestly say "continuing without prior context" when the
	// session is gone.
	SessionID     *string `json:"session_id"`
	ResumeOfRunID *string `json:"resume_of_run_id"`
	LastSeq       int32   `json:"last_seq"` // resume: continue message numbering
	RequeueCount  int32   `json:"requeue_count"`

	Secrets ChatClaimSecrets `json:"secrets"`
	Config  ChatClaimConfig  `json:"config"`
}

// ChatClaimSecrets are the decrypted secrets a chat run needs — the Anthropic
// token and nothing else (Decision 9/12). There is deliberately no ForgePAT /
// ForgeUsername field: a chat agent holds no forge credential, one tier narrower
// than a run.
type ChatClaimSecrets struct {
	AnthropicOAuthToken string `json:"anthropic_oauth_token"`
}

// ChatClaimConfig are the chat lifecycle caps the worker enforces, delivered so the
// worker's own clocks match what the server configured (Decision 3, the no-drift
// principle). IdleTimeoutSeconds is when the worker completes an idle chat;
// TurnTimeoutSeconds is the per-turn wall-clock; MaxTurns is the turn ceiling the
// worker also enforces (the server counts persisted follow_ups independently).
// DefaultModel is the owner's per-user default model (omitted when unset).
type ChatClaimConfig struct {
	IdleTimeoutSeconds int     `json:"idle_timeout_seconds"`
	TurnTimeoutSeconds int     `json:"turn_timeout_seconds"`
	MaxTurns           int     `json:"max_turns"`
	DefaultModel       *string `json:"default_model,omitempty"`
}

// -------------------------------------------------------------------------
// Chat claim lane
// -------------------------------------------------------------------------

// ClaimChat is the chat lane's counterpart to Claim (Decision 4): it claims the
// oldest queued CHAT run for the worker's user and assembles the repo-less chat
// payload. A nil payload with a nil error means the chat queue is idle. It shares
// Claim's vault gate and assembly-error recovery, so a locked owner's chats wait
// and a chat with no Anthropic token fails cleanly instead of wedging the loop.
func (s *Service) ClaimChat(ctx context.Context, wkr store.Worker) (*ChatClaimPayload, error) {
	if s.vlt != nil && !s.vlt.Unlocked(wkr.UserID) {
		return nil, nil // idle: owner locked
	}
	run, err := s.q.ClaimChatRun(ctx, store.ClaimChatRunParams{
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
	payload, err := s.assembleChatClaim(ctx, run)
	if err != nil {
		return nil, s.recoverClaimAssembly(ctx, run, err)
	}
	return payload, nil
}

// assembleChatClaim builds the chat claim payload for an already-claimed chat run.
// It joins NO repo and NO forge connection (Decision 12): the only context it needs
// beyond the run row is the resumed-from session for a Continue run, and the only
// secret it opens is the Anthropic token.
func (s *Service) assembleChatClaim(ctx context.Context, run store.Run) (*ChatClaimPayload, error) {
	resumeSession, err := s.q.GetChatRunClaimContext(ctx, run.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errRunVanished
		}
		return nil, fmt.Errorf("chat claim context: %w", err)
	}

	anthropic, err := s.openAnthropic(ctx, run.UserID)
	if err != nil {
		return nil, err
	}

	defaultModel, err := s.q.GetUserDefaultModel(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("default model lookup: %w", err)
	}

	// Resume the run's own session if it has one (a requeued/resumed chat), else the
	// resumed-from run's session (a Continue) — so the worker resumes uniformly from
	// SessionID regardless of which case produced it.
	sessionID := run.SessionID
	if !sessionID.Valid {
		sessionID = resumeSession
	}

	return &ChatClaimPayload{
		RunID:         run.ID.String(),
		Kind:          run.Kind,
		Title:         run.Title.String,
		Status:        run.Status,
		SessionID:     textPtr(sessionID),
		ResumeOfRunID: uuidPtr(run.ResumeOfRunID),
		LastSeq:       run.LastSeq,
		RequeueCount:  run.RequeueCount,
		Secrets: ChatClaimSecrets{
			AnthropicOAuthToken: string(anthropic),
		},
		Config: ChatClaimConfig{
			IdleTimeoutSeconds: int(s.p.WorkerChatIdleTimeout.Seconds()),
			TurnTimeoutSeconds: int(s.p.WorkerChatTurnTimeout.Seconds()),
			MaxTurns:           s.p.ChatMaxTurns,
			DefaultModel:       textPtr(defaultModel),
		},
	}, nil
}

// -------------------------------------------------------------------------
// Chat CRUD (session-authenticated, owner-scoped)
// -------------------------------------------------------------------------

// CreateChatRun queues a chat run whose first message is the initial prompt
// (Decision 2). The message becomes both the run's stored prompt (issue_description)
// and, derived down to a short line, its conversation title.
func (s *Service) CreateChatRun(ctx context.Context, userID uuid.UUID, message string) (store.Run, error) {
	message = strings.TrimSpace(message)
	if err := validateChatMessage(message); err != nil {
		return store.Run{}, err
	}
	title := deriveChatTitle(message)
	return s.q.CreateChatRun(ctx, store.CreateChatRunParams{
		RunID:            uuid.New(),
		UserID:           userID,
		IssueTitle:       title,
		IssueDescription: message,
		Title:            pgText(title),
	})
}

// ListChatRuns returns the user's chat conversations, newest first.
func (s *Service) ListChatRuns(ctx context.Context, userID uuid.UUID) ([]store.Run, error) {
	return s.q.ListChatRunsForUser(ctx, userID)
}

// GetChatRun returns a CHAT run owned by the user. A non-chat run (or another
// user's run, or an unknown id) is ErrRunNotFound — a non-chat run is simply not
// addressable through the /api/chats surface.
func (s *Service) GetChatRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error) {
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	if run.Kind != RunKindChat {
		return store.Run{}, ErrRunNotFound
	}
	return run, nil
}

// SubmitChatMessage records a follow-up turn on a live chat run (Decision 2): it
// rides the existing run_user_inputs steering wire as a follow_up, which the worker
// picks up between turns. It rejects a terminal chat (ErrRunTerminal → the client
// shows Continue) and enforces the server-side turn ceiling (Decision 3) BEFORE
// enqueuing, counting persisted follow_ups so a compromised worker can't burn spend
// past CHAT_MAX_TURNS.
func (s *Service) SubmitChatMessage(ctx context.Context, userID, runID uuid.UUID, message string) (SubmitInputResult, error) {
	message = strings.TrimSpace(message)
	if err := validateChatMessage(message); err != nil {
		return SubmitInputResult{}, err
	}
	run, err := s.GetChatRun(ctx, userID, runID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if terminalStatuses[run.Status] {
		return SubmitInputResult{}, ErrRunTerminal
	}
	count, err := s.q.CountChatFollowUps(ctx, runID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if int(count) >= s.p.ChatMaxTurns {
		return SubmitInputResult{}, ErrChatTurnCapReached
	}
	if _, err := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: runID, Kind: "follow_up", Body: pgText(message),
	}); err != nil {
		return SubmitInputResult{}, err
	}
	return SubmitInputResult{ServerSide: false}, nil
}

// EndChat gracefully ends a conversation (Decision 3's explicit "End chat"). It
// rides SubmitInput's cancel path: a live worker consumes the cancel and completes
// cleanly; with no live poller the run is cancelled server-side. A terminal chat is
// ErrRunTerminal (already ended).
func (s *Service) EndChat(ctx context.Context, userID, runID uuid.UUID) (SubmitInputResult, error) {
	if _, err := s.GetChatRun(ctx, userID, runID); err != nil {
		return SubmitInputResult{}, err
	}
	return s.SubmitInput(ctx, userID, runID, "cancel", "")
}

// ContinueChat resumes an ENDED conversation (Decision 11): a NEW queued chat run
// carrying resume_of_run_id → the ended run, with worker_id pre-set to the ended
// run's worker for resume affinity. The claiming worker best-effort resumes the
// persisted SDK session. A still-active source is ErrChatNotEnded.
func (s *Service) ContinueChat(ctx context.Context, userID, runID uuid.UUID) (store.Run, error) {
	src, err := s.GetChatRun(ctx, userID, runID)
	if err != nil {
		return store.Run{}, err
	}
	if !terminalStatuses[src.Status] {
		return store.Run{}, ErrChatNotEnded
	}
	title := src.Title
	if !title.Valid {
		title = pgText(src.IssueTitle)
	}
	return s.q.CreateChatContinueRun(ctx, store.CreateChatContinueRunParams{
		UserID:        userID,
		IssueTitle:    src.IssueTitle,
		Title:         title,
		ResumeOfRunID: pgUUID(src.ID),
		WorkerID:      src.WorkerID, // affinity to the worker whose disk holds the session (may be NULL)
	})
}

// -------------------------------------------------------------------------
// Issue proposals (Decision 8) — the browser-side confirm/dismiss half
// -------------------------------------------------------------------------

// GetChatProposal loads a PENDING proposal for the browser confirm/dismiss flow,
// scoped to the requesting user through the owning chat run. A proposal that is not
// pending is ErrProposalNotPending (rejected before any forge write, closing the
// common double-click); an unknown/foreign/non-chat proposal is ErrProposalNotFound.
func (s *Service) GetChatProposal(ctx context.Context, userID, runID, propID uuid.UUID) (store.GetChatProposalForConfirmRow, error) {
	row, err := s.q.GetChatProposalForConfirm(ctx, store.GetChatProposalForConfirmParams{
		ID: propID, RunID: runID, UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.GetChatProposalForConfirmRow{}, ErrProposalNotFound
		}
		return store.GetChatProposalForConfirmRow{}, err
	}
	if row.Status != "pending" {
		return store.GetChatProposalForConfirmRow{}, ErrProposalNotPending
	}
	return row, nil
}

// ConfirmProposal records a proposal as confirmed with the forge issue iid the
// handler just created (forge-first — the handler does the CreateIssue, then calls
// this). The status='pending' guard means a lost race (a concurrent confirm/dismiss
// won) yields ErrProposalNotPending rather than re-recording.
func (s *Service) ConfirmProposal(ctx context.Context, propID uuid.UUID, issueIID int64) error {
	if _, err := s.q.MarkProposalConfirmed(ctx, store.MarkProposalConfirmedParams{
		ID:              propID,
		CreatedIssueIid: pgtype.Int8{Int64: issueIID, Valid: true},
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotPending
		}
		return err
	}
	return nil
}

// DismissProposal flips a proposal to dismissed. Status-only — it NEVER touches the
// forge (Decision 8: dismissing provably writes nothing).
func (s *Service) DismissProposal(ctx context.Context, propID uuid.UUID) error {
	if _, err := s.q.MarkProposalDismissed(ctx, propID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProposalNotPending
		}
		return err
	}
	return nil
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

func validateChatMessage(trimmed string) error {
	if trimmed == "" {
		return ErrEmptyChatMessage
	}
	if len(trimmed) > MaxChatMessageBytes {
		return ErrChatMessageTooLarge
	}
	return nil
}

// deriveChatTitle turns the first message into a short conversation title: the
// first non-empty line, collapsed whitespace, truncated to maxChatTitleRunes with
// an ellipsis. Empty input yields a stable fallback (the caller has already
// rejected an empty message, so this only guards a whitespace-only first line).
func deriveChatTitle(message string) string {
	line := message
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return "New chat"
	}
	if utf8.RuneCountInString(line) > maxChatTitleRunes {
		runes := []rune(line)
		line = strings.TrimSpace(string(runes[:maxChatTitleRunes])) + "…"
	}
	return line
}

// uuidPtr formats a nullable uuid column as a *string (nil when NULL), the DTO /
// wire convention for an optional id.
func uuidPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}
