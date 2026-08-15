package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/toolprofile"
	"gitlab.example.com/vtmocanu/uzi/api/internal/toolseed"
)

// maxToolNoteBytes bounds the optional admin note on an allowlist entry.
const maxToolNoteBytes = 500

// pgTextOrNull maps "" to a NULL text column and any other string to a value.
func pgTextOrNull(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

// toolAllowlistDTO is the JSON view of a tool_allowlist row (PRD #18 M4).
type toolAllowlistDTO struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	PinnedVersion *string   `json:"pinned_version"`
	Note          *string   `json:"note"`
	UpdatedBy     *string   `json:"updated_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func toolAllowlistToDTO(e store.ToolAllowlist) toolAllowlistDTO {
	dto := toolAllowlistDTO{
		ID:            e.ID.String(),
		Name:          e.Name,
		PinnedVersion: textPtrValue(e.PinnedVersion.Valid, e.PinnedVersion.String),
		Note:          textPtrValue(e.Note.Valid, e.Note.String),
		CreatedAt:     e.CreatedAt.Time,
		UpdatedAt:     e.UpdatedAt.Time,
	}
	if e.UpdatedBy.Valid {
		u := uuid.UUID(e.UpdatedBy.Bytes).String()
		dto.UpdatedBy = &u
	}
	return dto
}

// toolAllowlistWriteRequest is the create/update body. On update, name is
// immutable and ignored.
type toolAllowlistWriteRequest struct {
	Name          string `json:"name"`
	PinnedVersion string `json:"pinned_version"`
	Note          string `json:"note"`
}

// validateAllowlistWrite sanitizes + validates the version policy + note shared by
// create and update. The name is validated separately (create only).
func validateAllowlistWrite(req toolAllowlistWriteRequest) (pinned, note string, err error) {
	pinned = strings.TrimSpace(req.PinnedVersion)
	if pinned != "" && !toolprofile.WellFormedVersion(pinned) {
		return "", "", errors.New("pinned_version must be a simple version token")
	}
	note = strings.TrimSpace(req.Note)
	if len(note) > maxToolNoteBytes {
		return "", "", errors.New("note is too long")
	}
	if hasControlChar(note) {
		return "", "", errors.New("note must not contain control characters")
	}
	return pinned, note, nil
}

// ListToolAllowlist returns the full allowlist. Readable by any authenticated user
// (the repo package picker needs it to know which packages are selectable); writes
// are admin-only (see the routes).
func (h *Handler) ListToolAllowlist(w http.ResponseWriter, r *http.Request) {
	if _, ok := mw.UserFromContext(r.Context()); !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := h.q.ListToolAllowlist(r.Context())
	if err != nil {
		slog.Error("list tool allowlist", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]toolAllowlistDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, toolAllowlistToDTO(e))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"allowlist": out})
}

// CreateToolAllowlistEntry adds an allowed package (admin only). name is a bare
// package base name (no @version — the version policy is pinned_version).
func (h *Handler) CreateToolAllowlistEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !actor.IsAdmin { // defense-in-depth beside the RequireAdmin route group
		httpx.Error(w, http.StatusForbidden, "admin only")
		return
	}
	var req toolAllowlistWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !toolprofile.WellFormed(name) || strings.Contains(name, "@") {
		httpx.Error(w, http.StatusBadRequest, "name must be a bare package name (no version); pin the version with pinned_version")
		return
	}
	// Decision 6: a credential-bearing CLI may never be allowlisted, even by an
	// admin — it would give the agent a pre-authenticated tool.
	if toolprofile.Denied(name) {
		httpx.Error(w, http.StatusBadRequest, "that package ships a credential-bearing CLI and may not be allowlisted")
		return
	}
	// PRD #123 M3 (SC2): gate the allowlist to the baked worker toolchain. A
	// package that is not baked is permitted but unprovisionable behind the worker
	// egress block, so allowlisting it would surface as a run-time hang. Reject at
	// admin time instead.
	if !toolseed.Covered(name) {
		httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("%q is not in the baked worker toolchain; it must be added to the image (agent/devbox-global/devbox.json) and the image rolled before it can be allowlisted", name))
		return
	}
	pinned, note, err := validateAllowlistWrite(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	row, err := h.q.CreateToolAllowlistEntry(r.Context(), store.CreateToolAllowlistEntryParams{
		Name:          name,
		PinnedVersion: pgTextOrNull(pinned),
		Note:          pgTextOrNull(note),
		UpdatedBy:     pgUUID(actor.ID),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Error(w, http.StatusConflict, "that package is already on the allowlist")
			return
		}
		slog.Error("create tool allowlist entry", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"entry": toolAllowlistToDTO(row)})
}

// UpdateToolAllowlistEntry edits an entry's version policy + note (admin only).
// name is immutable.
func (h *Handler) UpdateToolAllowlistEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !actor.IsAdmin { // defense-in-depth beside the RequireAdmin route group
		httpx.Error(w, http.StatusForbidden, "admin only")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid allowlist entry id")
		return
	}
	var req toolAllowlistWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	pinned, note, err := validateAllowlistWrite(req)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}
	row, err := h.q.UpdateToolAllowlistEntry(r.Context(), store.UpdateToolAllowlistEntryParams{
		ID:            id,
		PinnedVersion: pgTextOrNull(pinned),
		Note:          pgTextOrNull(note),
		UpdatedBy:     pgUUID(actor.ID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "allowlist entry not found")
			return
		}
		slog.Error("update tool allowlist entry", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"entry": toolAllowlistToDTO(row)})
}

// DeleteToolAllowlistEntry removes an allowed package (admin only). A profile that
// still lists the removed package will fail its next claim (claim-time
// re-validation) — that is the intended, visible consequence of shrinking the list.
func (h *Handler) DeleteToolAllowlistEntry(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !actor.IsAdmin { // defense-in-depth beside the RequireAdmin route group
		httpx.Error(w, http.StatusForbidden, "admin only")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid allowlist entry id")
		return
	}
	n, err := h.q.DeleteToolAllowlistEntry(r.Context(), id)
	if err != nil {
		slog.Error("delete tool allowlist entry", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "allowlist entry not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
