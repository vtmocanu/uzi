package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// maxBoardExtraLabels caps a user's board membership extras override (PRD #196 M3).
const maxBoardExtraLabels = 64

// boardPrefsDTO is the wire shape for both GET and PUT (PRD #196 M3). ExtraLabels
// carries the "unset vs empty" sentinel across JSON: a nil slice marshals to `null`
// (not customized — the client falls back to the admin default board_extra_labels),
// an empty slice marshals to `[]` (the user's absolute empty set, Decision 9), and a
// populated slice is the absolute set. show_all is the old per-browser "show all
// other issues" boolean, now per-account.
type boardPrefsDTO struct {
	ExtraLabels []string `json:"extra_labels"`
	ShowAll     bool     `json:"show_all"`
}

// GetBoardPrefs returns the caller's board view preferences for a repo they can see
// (PRD #196 M3). repoForRequest is the same auth+repo-access gate the other board
// handlers use (401/400/404). No stored row yet ⇒ the unset defaults
// ({extra_labels: null, show_all: false}) at 200, never 404: a fresh board is not an
// error, and the null tells the client to use the admin default.
func (h *Handler) GetBoardPrefs(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	row, err := h.q.GetBoardPrefs(r.Context(), store.GetBoardPrefsParams{UserID: user.ID, RepoID: repo.ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.JSON(w, http.StatusOK, boardPrefsDTO{ExtraLabels: nil, ShowAll: false})
			return
		}
		slog.Error("get board prefs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, boardPrefsDTO{
		ExtraLabels: decodeExtraLabels(row.ExtraLabels),
		ShowAll:     row.ShowAll,
	})
}

// PutBoardPrefs replaces the caller's board view preferences for a repo they own
// (PRD #196 M3), returning the stored row. extra_labels distinguishes JSON
// null/absent (reset to "not customized", stored as SQL NULL) from an array (the
// user's absolute set, INCLUDING []) via a pointer. A non-null array is validated:
// every entry must pass settings.ValidateLabel and the count is capped. Owner-only
// via the upsert's ownership join — a non-owned/unknown repo writes nothing → 404.
//
// VISIBILITY only: this endpoint never touches run eligibility or the run gate.
func (h *Handler) PutBoardPrefs(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	repo, ok := h.repoForRequest(w, r)
	if !ok {
		return
	}
	var req struct {
		ExtraLabels *[]string `json:"extra_labels"`
		ShowAll     bool      `json:"show_all"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// NULL (not customized) unless the body carried an array. A present array — even
	// [] — is the user's absolute set and is stored verbatim as JSON.
	var raw []byte
	if req.ExtraLabels != nil {
		if len(*req.ExtraLabels) > maxBoardExtraLabels {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("at most %d extra labels", maxBoardExtraLabels))
			return
		}
		for _, label := range *req.ExtraLabels {
			if err := settings.ValidateLabel(label); err != nil {
				httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("invalid label %q: %s", label, err))
				return
			}
		}
		marshaled, err := json.Marshal(*req.ExtraLabels)
		if err != nil {
			slog.Error("marshal board extra labels", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		raw = marshaled
	}

	row, err := h.q.UpsertBoardPrefsForOwner(r.Context(), store.UpsertBoardPrefsForOwnerParams{
		UserID:      user.ID,
		RepoID:      repo.ID,
		ExtraLabels: raw,
		ShowAll:     req.ShowAll,
	})
	if err != nil {
		// No row written ⇒ the repo is not owned by the caller (the ownership join
		// matched nothing). Hide existence: 404, not 403.
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "repo not found")
			return
		}
		slog.Error("upsert board prefs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, boardPrefsDTO{
		ExtraLabels: decodeExtraLabels(row.ExtraLabels),
		ShowAll:     row.ShowAll,
	})
}

// decodeExtraLabels decodes the nullable extra_labels JSONB column into a slice.
// A NULL/empty column yields nil (→ JSON `null`, "not customized"); a stored `[]`
// yields a non-nil empty slice (→ JSON `[]`, the absolute empty set), preserving the
// unset-vs-empty distinction on the way back out. A malformed value degrades to nil.
func decodeExtraLabels(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if out == nil {
		// A stored JSON `null` decodes to a nil slice; treat it as not customized.
		return nil
	}
	return out
}
