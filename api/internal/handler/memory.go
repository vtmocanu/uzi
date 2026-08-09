package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// ListMyMemory returns the authenticated user's agent memory across ALL of their
// repos, newest first (PRD #90 M6). This is the owner's visibility control: a
// poisoned entry can outlive the repo injection that planted it, so seeing every
// entry (with its repo + provenance) is what makes the per-entry delete a real
// recourse, not a nicety. Scoped to the caller by user_id — never another user's
// memory.
func (h *Handler) ListMyMemory(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListAgentMemoryForUser(r.Context(), user.ID)
	if err != nil {
		slog.Error("list agent memory", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.AgentMemoryDTO, 0, len(rows))
	for _, m := range rows {
		dto := apitypes.AgentMemoryDTO{
			ID:        m.ID.String(),
			RepoID:    m.RepoID.String(),
			RepoName:  m.RepoName,
			Title:     m.Title,
			Body:      m.Body,
			Basis:     normalizeMemoryBasis(m.Basis),
			Evidence:  memoryEvidence(m.Evidence),
			CreatedAt: m.CreatedAt.Time,
		}
		if m.RunID.Valid {
			dto.RunID = uuid.UUID(m.RunID.Bytes).String()
		}
		out = append(out, dto)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"memories": out})
}

// DeleteMyMemory deletes one of the caller's memory entries — the owner's purge
// (PRD #90 S1). Owner-scoped by user_id, so a foreign or unknown id is a 404, never
// a cross-user delete.
func (h *Handler) DeleteMyMemory(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid memory id")
		return
	}
	n, err := h.q.DeleteAgentMemory(r.Context(), store.DeleteAgentMemoryParams{ID: id, UserID: user.ID})
	if err != nil {
		slog.Error("delete agent memory", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "memory not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
