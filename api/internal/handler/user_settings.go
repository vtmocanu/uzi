package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/theme"
)

// userSettingsDTO is the current user's own (non-secret) settings: the default
// worker model (PRD #17), the UI theme override (PRD #21), and the sidebar
// token-meter choice (migration 00123). null default_model means inherit (the
// lead template's model, else the account/SDK default); null theme means "use
// the instance default". sidebar_token_ids lists the NON-default Anthropic
// tokens whose rate meters the user surfaced on the sidebar rail — the default
// token always shows and is never listed; empty means default-only.
type userSettingsDTO struct {
	DefaultModel    *string  `json:"default_model"`
	Theme           *string  `json:"theme"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
}

// userSettingsResponse reads both columns and writes the settings body, shared by
// the GET and PUT handlers so the two responses never drift.
func (h *Handler) userSettingsResponse(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	s, err := h.q.GetUserSettings(r.Context(), userID)
	if err != nil {
		slog.Error("get user settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{
			DefaultModel:    textPtrValue(s.DefaultModel.Valid, s.DefaultModel.String),
			Theme:           textPtrValue(s.Theme.Valid, s.Theme.String),
			SidebarTokenIds: uuidStrings(s.SidebarTokenIds),
		},
	})
}

// GetMySettings returns the current user's own settings. Session-authenticated
// and own-user only (no admin path — a user's model and theme are theirs).
func (h *Handler) GetMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	h.userSettingsResponse(w, r, user.ID)
}

// PutMySettings updates the current user's own settings with PATCH-like
// semantics: a field is applied only when present in the body — absent leaves
// the stored value untouched, present-null clears it. That lets the worker-model
// card and the Appearance theme picker save independently over the one endpoint
// without clobbering each other. default_model is validated by the same
// validateModel used for agent templates (PRD #17); theme by the same theme
// registry the admin default surface uses (PRD #21), so neither pair of write
// paths can drift.
func (h *Handler) PutMySettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// RawMessage distinguishes an absent field (nil) from a present null (the
	// bytes `null`); a plain *string cannot, and absent must mean "unchanged".
	var req struct {
		DefaultModel    json.RawMessage `json:"default_model"`
		Theme           json.RawMessage `json:"theme"`
		SidebarTokenIds json.RawMessage `json:"sidebar_token_ids"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.DefaultModel != nil {
		var raw *string
		if err := json.Unmarshal(req.DefaultModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid default_model")
			return
		}
		model, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.q.SetUserDefaultModel(r.Context(), store.SetUserDefaultModelParams{
			ID:           user.ID,
			DefaultModel: model,
		}); err != nil {
			slog.Error("set user default model", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if req.Theme != nil {
		var raw *string
		if err := json.Unmarshal(req.Theme, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid theme")
			return
		}
		themeVal, err := validateTheme(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.q.SetUserTheme(r.Context(), store.SetUserThemeParams{
			ID:    user.ID,
			Theme: themeVal,
		}); err != nil {
			slog.Error("set user theme", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if req.SidebarTokenIds != nil {
		var raw *[]string
		if err := json.Unmarshal(req.SidebarTokenIds, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid sidebar_token_ids")
			return
		}
		ids, err := h.validateSidebarTokenIds(r.Context(), user.ID, raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.q.SetUserSidebarTokens(r.Context(), store.SetUserSidebarTokensParams{
			ID:              user.ID,
			SidebarTokenIds: ids,
		}); err != nil {
			slog.Error("set user sidebar tokens", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	h.userSettingsResponse(w, r, user.ID)
}

// maxSidebarTokenIds bounds the request list before any per-id work; nobody
// legitimately holds hundreds of tokens, and an unbounded list is an invitation.
const maxSidebarTokenIds = 100

// validateSidebarTokenIds maps the request list to the stored set: nil or empty
// clears back to default-only; each entry must parse as a UUID, and any id that
// is not one of the CALLER'S OWN anthropic_token secrets is silently dropped
// rather than rejected — the web sends ids it read moments ago, and a token
// deleted from another tab must not fail the whole save. Duplicates collapse.
// The default token's id is dropped with the rest of the non-matching set left
// intact if present — storing it would be harmless, but the contract is that
// the stored list holds only non-default extras.
func (h *Handler) validateSidebarTokenIds(ctx context.Context, userID uuid.UUID, raw *[]string) ([]uuid.UUID, error) {
	if raw == nil || len(*raw) == 0 {
		return []uuid.UUID{}, nil
	}
	if len(*raw) > maxSidebarTokenIds {
		return nil, fmt.Errorf("sidebar_token_ids: at most %d entries", maxSidebarTokenIds)
	}
	secrets, err := h.q.ListUserSecretsForKind(ctx, store.ListUserSecretsForKindParams{
		UserID: userID,
		Kind:   "anthropic_token",
	})
	if err != nil {
		return nil, fmt.Errorf("internal error")
	}
	owned := make(map[uuid.UUID]bool, len(secrets))
	for _, s := range secrets {
		// Only non-default tokens are storable; the default always shows.
		if !s.IsDefault {
			owned[s.ID] = true
		}
	}
	ids := make([]uuid.UUID, 0, len(*raw))
	seen := make(map[uuid.UUID]bool, len(*raw))
	for _, v := range *raw {
		id, err := uuid.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("sidebar_token_ids: %q is not a valid id", v)
		}
		if owned[id] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// uuidStrings renders a stored uuid[] for the DTO; a NULL column arrives as a
// nil slice and reads as the empty (default-only) choice.
func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// validateTheme maps a nullable theme override to its storage type: a nil
// pointer or a blank value becomes NULL (use the instance default); a known
// theme becomes a set pgtype.Text; an unknown theme is rejected. Shares
// theme.Validate with the admin default write path (same discipline as
// validateModel), so a value accepted here is accepted there and vice versa.
func validateTheme(raw *string) (pgtype.Text, error) {
	if raw == nil {
		return pgtype.Text{}, nil
	}
	v := strings.TrimSpace(*raw)
	if v == "" {
		return pgtype.Text{}, nil
	}
	if err := theme.Validate(v); err != nil {
		return pgtype.Text{}, err
	}
	return pgtype.Text{String: v, Valid: true}, nil
}
