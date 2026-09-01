package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// chatMessageRequest is the body of chat create and message-post: a single user
// message. Create derives the conversation title from it; message posts it as a
// follow-up turn.
type chatMessageRequest struct {
	Message string `json:"message"`
}

// CreateChat queues a new chat conversation (PRD #39). The first message becomes
// the initial prompt and the derived title; the run is queued for the user's
// worker to claim on the chat lane. Owner-scoped (the run belongs to the caller),
// behind the per-user chat rate limiter.
func (h *Handler) CreateChat(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req chatMessageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, err := h.wsvc.CreateChatRun(r.Context(), user.ID, req.Message)
	if err != nil {
		if h.writeChatInputError(w, err) {
			return
		}
		slog.Error("create chat", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// chatListDTO is one conversation in the Chat page's list: the display + activity
// fields the list needs, distinct from the full apitypes.RunDTO (a chat has no repo/issue/MR
// context to carry). turn_count is the user-turn count (persisted follow_ups incl.
// the seeded first message); last_message_at is the newest run_message time (null
// until the worker emits one) the list sorts on.
type chatListDTO struct {
	ID            string     `json:"id"`
	Title         *string    `json:"title"`
	Status        string     `json:"status"`
	TurnCount     int64      `json:"turn_count"`
	LastMessageAt *time.Time `json:"last_message_at"`
	ResumeOfRunID *string    `json:"resume_of_run_id"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ListChats returns the current user's chat conversations, ordered by last activity.
// The instance-wide turn cap rides the envelope (a constant, not per-chat).
func (h *Handler) ListChats(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.wsvc.ListChatRuns(r.Context(), user.ID)
	if err != nil {
		slog.Error("list chats", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]chatListDTO, 0, len(rows))
	for _, c := range rows {
		dto := chatListDTO{
			ID:            c.ID.String(),
			Title:         textPtrValue(c.Title.Valid, c.Title.String),
			Status:        c.Status,
			TurnCount:     c.TurnCount,
			LastMessageAt: timePtr(c.LastMessageAt.Valid, c.LastMessageAt.Time),
			CreatedAt:     c.CreatedAt.Time,
			UpdatedAt:     c.UpdatedAt.Time,
		}
		if c.ResumeOfRunID.Valid {
			s := uuid.UUID(c.ResumeOfRunID.Bytes).String()
			dto.ResumeOfRunID = &s
		}
		out = append(out, dto)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"chats": out, "max_turns": h.cfg.ChatMaxTurns})
}

// PostChatMessage adds a follow-up turn to a live conversation. A terminal chat is
// 409 (the client shows Continue); the turn cap is 409 with a clear message.
// Owner-scoped, behind the per-user chat limiter.
func (h *Handler) PostChatMessage(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.chatUserAndID(w, r)
	if !ok {
		return
	}
	var req chatMessageRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.wsvc.SubmitChatMessage(r.Context(), userID, id, req.Message)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "chat not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "this conversation has ended; continue it to keep chatting")
		case errors.Is(err, workersvc.ErrChatTurnCapReached):
			httpx.Error(w, http.StatusConflict, "this conversation has reached its turn limit; start a new chat")
		default:
			if h.writeChatInputError(w, err) {
				return
			}
			slog.Error("post chat message", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}

// EndChat gracefully ends a conversation (Decision 3). A terminal chat is 409.
func (h *Handler) EndChat(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.chatUserAndID(w, r)
	if !ok {
		return
	}
	res, err := h.wsvc.EndChat(r.Context(), userID, id)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "chat not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "this conversation has already ended")
		default:
			slog.Error("end chat", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}

// ContinueChat resumes an ended conversation as a new queued chat run (Decision 11).
func (h *Handler) ContinueChat(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.chatUserAndID(w, r)
	if !ok {
		return
	}
	run, err := h.wsvc.ContinueChat(r.Context(), userID, id)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "chat not found")
		case errors.Is(err, workersvc.ErrChatNotEnded):
			httpx.Error(w, http.StatusConflict, "this conversation is still active")
		default:
			slog.Error("continue chat", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// startChatRunRequest is the body of the chat start-run card's Start click (PRD #191
// M5): the repo (by the human path the card showed) and the issue iid. The run is
// gated exactly as the board start button (StartRunForUser), so an issue with no PRD is
// refused with the same message.
type startChatRunRequest struct {
	RepoPath string `json:"repo_path"`
	IssueIID int64  `json:"issue_iid"`
}

// StartChatRun starts an agent run from a chat's start-run card. Owner-scoped through
// the repo path resolve; behind the per-user forge limiter (it does a forge GetIssue).
func (h *Handler) StartChatRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req startChatRunRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.RepoPath) == "" {
		httpx.Error(w, http.StatusBadRequest, "repo_path is required")
		return
	}
	if req.IssueIID <= 0 {
		httpx.Error(w, http.StatusBadRequest, "issue_iid must be a positive integer")
		return
	}
	run, err := h.wsvc.StartRunForUserByPath(r.Context(), user.ID, req.RepoPath, req.IssueIID, nil, nil)
	if err != nil {
		h.writeStartRunError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// cancelChatRunRequest is the body of the chat cancel card's Cancel click (PRD #322):
// the target run id. The run is re-resolved and terminality-guarded server-side by
// SubmitInput; the card value is untrusted, exactly like the start-run card.
type cancelChatRunRequest struct {
	RunID string `json:"run_id"`
}

// CancelChatRun cancels a live run from a chat's cancel card (PRD #322). Owner-scoped
// and terminality-guarded through workersvc.SubmitInput(cancel); the card's run_id is
// untrusted. No forge call, so it wears no limiter (mounted like /{id}/end).
func (h *Handler) CancelChatRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req cancelChatRunRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	runID, err := uuid.Parse(strings.TrimSpace(req.RunID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "run_id is required")
		return
	}
	// Map errors like CreateRunInput (workers.go), NOT writeStartRunError: a stale/forged
	// card must degrade to a clear 404/409, never a 500.
	res, err := h.wsvc.SubmitInput(r.Context(), user.ID, runID, "cancel", "", nil)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		default:
			slog.Error("cancel chat run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}

// steerChatRunRequest is the body of the chat steer card's Send click (PRD #322): the
// target run id and the (human-edited) follow-up message. The run is re-resolved and
// terminality-guarded server-side by SubmitInput(follow_up); the card values are
// untrusted. A chat-run target is refused (steering is for issue runs).
type steerChatRunRequest struct {
	RunID   string `json:"run_id"`
	Message string `json:"message"`
}

// SteerChatRun sends a follow-up to steer a live issue run from a chat's steer card
// (PRD #322). Owner-scoped + terminality-guarded through SubmitInput(follow_up); a chat
// run is refused with an issue-runs-only message. Under chatLimiter (it induces agent
// spend), mirroring the chat message endpoint.
func (h *Handler) SteerChatRun(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req steerChatRunRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	runID, err := uuid.Parse(strings.TrimSpace(req.RunID))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		httpx.Error(w, http.StatusBadRequest, "message must not be empty")
		return
	}
	// Map errors like CreateRunInput (workers.go), NOT writeStartRunError: a stale/forged
	// card or a chat-run target must degrade to a clear 404/409, never a 500.
	res, err := h.wsvc.SubmitInput(r.Context(), user.ID, runID, "follow_up", req.Message, nil)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotFound):
			httpx.Error(w, http.StatusNotFound, "run not found")
		case errors.Is(err, workersvc.ErrRunTerminal):
			httpx.Error(w, http.StatusConflict, "run has already finished")
		case errors.Is(err, workersvc.ErrChatInputNotAllowed):
			httpx.Error(w, http.StatusConflict, "steering applies to issue runs, not chats")
		default:
			slog.Error("steer chat run", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"server_side": res.ServerSide})
}

// createdIssueDTO is the confirm response: the real forge issue the click created.
type createdIssueDTO struct {
	IID    int64  `json:"iid"`
	WebURL string `json:"web_url"`
	Title  string `json:"title"`
}

// ConfirmProposal executes a proposed issue on the forge (Decision 8): the ONLY
// path that turns a proposal into a real GitLab issue. Forge-first via the caller's
// own connection, behind the per-user forge limiter. Owner-scoped through the chat
// run; a proposal that is not pending is 409.
func (h *Handler) ConfirmProposal(w http.ResponseWriter, r *http.Request) {
	userID, runID, propID, ok := h.chatProposalIDs(w, r)
	if !ok {
		return
	}
	// The whole claim-first / forge-write / settle-or-revert composite lives in
	// workersvc.ConfirmProposalForUser (PRD #191 M1) so the Slack proposal card calls
	// the identical path; this handler only maps its sentinels to HTTP.
	created, err := h.wsvc.ConfirmProposalForUser(r.Context(), userID, runID, propID)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrProposalRepoGone):
			httpx.Error(w, http.StatusNotFound, "the proposal's target repo is no longer available")
		case errors.Is(err, workersvc.ErrForgeIssueWrite):
			// err wraps the driver's already-redacted message.
			httpx.Error(w, http.StatusBadGateway, err.Error())
		case errors.Is(err, workersvc.ErrProposalNotFound), errors.Is(err, workersvc.ErrProposalNotPending):
			h.writeProposalLookupError(w, err)
		default:
			slog.Error("confirm proposal", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"issue": createdIssueDTO{IID: created.IID, WebURL: created.WebURL, Title: created.Title},
	})
}

// DismissProposal marks a proposal dismissed. It NEVER touches the forge
// (Decision 8: dismissing provably writes nothing).
func (h *Handler) DismissProposal(w http.ResponseWriter, r *http.Request) {
	userID, runID, propID, ok := h.chatProposalIDs(w, r)
	if !ok {
		return
	}
	// Ownership + pending are enforced by GetChatProposal before the status flip.
	if _, err := h.wsvc.GetChatProposal(r.Context(), userID, runID, propID); err != nil {
		h.writeProposalLookupError(w, err)
		return
	}
	if err := h.wsvc.DismissProposal(r.Context(), propID); err != nil {
		if errors.Is(err, workersvc.ErrProposalNotPending) {
			httpx.Error(w, http.StatusConflict, "this proposal has already been resolved")
			return
		}
		slog.Error("dismiss proposal", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -------------------------------------------------------------------------
// helpers
// -------------------------------------------------------------------------

// chatUserAndID resolves the authenticated user id and the chat run id from the
// request, writing the error response itself on failure.
func (h *Handler) chatUserAndID(w http.ResponseWriter, r *http.Request) (userID, id uuid.UUID, ok bool) {
	u, authed := mw.UserFromContext(r.Context())
	if !authed {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return uuid.Nil, uuid.Nil, false
	}
	id, ok = httpx.PathUUID(w, r, "id", "chat")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return u.ID, id, true
}

// chatProposalIDs resolves the user id plus the chat run id and proposal id.
func (h *Handler) chatProposalIDs(w http.ResponseWriter, r *http.Request) (userID, runID, propID uuid.UUID, ok bool) {
	uid, id, valid := h.chatUserAndID(w, r)
	if !valid {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	propID, ok = httpx.PathUUID(w, r, "pid", "proposal")
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return uid, id, propID, true
}

// writeChatInputError maps the input-validation chat errors to a status and
// reports whether it handled err.
func (h *Handler) writeChatInputError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, workersvc.ErrEmptyChatMessage):
		httpx.Error(w, http.StatusBadRequest, "message must not be empty")
		return true
	case errors.Is(err, workersvc.ErrChatMessageTooLarge):
		httpx.Error(w, http.StatusUnprocessableEntity, "message is too large")
		return true
	default:
		return false
	}
}

// writeProposalLookupError maps the proposal lookup errors to a status.
func (h *Handler) writeProposalLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workersvc.ErrProposalNotFound):
		httpx.Error(w, http.StatusNotFound, "proposal not found")
	case errors.Is(err, workersvc.ErrProposalNotPending):
		httpx.Error(w, http.StatusConflict, "this proposal has already been resolved")
	default:
		slog.Error("load chat proposal", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
