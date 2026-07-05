package handler

import (
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// userSettingsDTO is the current user's own (non-secret) settings. Today that is
// just the default worker model (PRD #17); null means inherit (the lead
// template's model, else the account/SDK default).
type userSettingsDTO struct {
	DefaultModel *string `json:"default_model"`
}

// GetMySettings returns the current user's own settings. Session-authenticated
// and own-user only (no admin path — a user's model choice is theirs).
func (h *Handler) GetMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	model, err := h.q.GetUserDefaultModel(r.Context(), user.ID)
	if err != nil {
		slog.Error("get user default model", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{DefaultModel: textPtrValue(model.Valid, model.String)},
	})
}

// PutMySettings updates the current user's default worker model. The value is
// validated by the same validateModel used for agent templates (PRD #17
// Decision 4), so the two write surfaces cannot drift. A null or blank value
// clears it back to inherit.
func (h *Handler) PutMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		DefaultModel *string `json:"default_model"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	model, err := validateModel(req.DefaultModel)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, err := h.q.SetUserDefaultModel(r.Context(), store.SetUserDefaultModelParams{
		ID:           user.ID,
		DefaultModel: model,
	})
	if err != nil {
		slog.Error("set user default model", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{DefaultModel: textPtrValue(updated.Valid, updated.String)},
	})
}
