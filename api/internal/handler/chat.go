package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
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
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run)})
}

// chatListDTO is one conversation in the Chat page's list: the display + activity
// fields the list needs, distinct from the full runDTO (a chat has no repo/issue/MR
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
	httpx.JSON(w, http.StatusCreated, map[string]any{"run": runToDTO(run)})
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
	prop, err := h.wsvc.GetChatProposal(r.Context(), userID, runID, propID)
	if err != nil {
		h.writeProposalLookupError(w, err)
		return
	}

	// Load the target repo + its connection PAT (the user must still own it).
	repo, err := h.q.GetRepoForUser(r.Context(), store.GetRepoForUserParams{ID: prop.RepoID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "the proposal's target repo is no longer available")
			return
		}
		slog.Error("confirm proposal: load repo", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	f, err := h.svc.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		slog.Error("build forge for connection", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	var labels []string
	if len(prop.Labels) > 0 {
		if err := json.Unmarshal(prop.Labels, &labels); err != nil {
			labels = nil
		}
	}

	created, err := f.CreateIssue(r.Context(), repo.ForgeProjectID, prop.Title, prop.Description, labels)
	if err != nil {
		// err is already PAT-redacted by the driver.
		httpx.Error(w, http.StatusBadGateway, "could not create the issue on the forge: "+err.Error())
		return
	}

	// Record the proposal as confirmed. A lost race (a concurrent confirm/dismiss
	// won between GetChatProposal and here) is rare behind the human-clicked,
	// rate-limited confirm; the issue was still created, so surface it rather than
	// dropping it, and log the race for the owner's audit trail.
	if err := h.wsvc.ConfirmProposal(r.Context(), propID, created.IID); err != nil {
		if errors.Is(err, workersvc.ErrProposalNotPending) {
			slog.Warn("confirm proposal: raced after issue creation", "proposal", propID.String(), "issue_iid", created.IID, "issue_url", created.WebURL)
		} else {
			slog.Error("confirm proposal: mark confirmed", "error", err)
		}
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid chat id")
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
	propID, err := uuid.Parse(chi.URLParam(r, "pid"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid proposal id")
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
