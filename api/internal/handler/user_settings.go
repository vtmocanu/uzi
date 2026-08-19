package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/theme"
)

// userSettingsDTO is the current user's own (non-secret) settings: the default
// worker model (PRD #17), the per-user judge model override (PRD #69) and
// run-summary model override (PRD #362 M2), the UI theme override (PRD #21), and
// the sidebar token-meter choice (migration 00123). null default_model means
// inherit (the lead template's model, else the account/SDK default); null
// judge_model / summary_model means inherit the instance value; null theme means
// "use the instance default". sidebar_token_ids lists the NON-default Anthropic
// tokens whose rate meters the user surfaced on the sidebar rail — the default
// token always shows and is never listed; empty means default-only.
type userSettingsDTO struct {
	DefaultModel    *string  `json:"default_model"`
	JudgeModel      *string  `json:"judge_model"`
	SummaryModel    *string  `json:"summary_model"`
	Theme           *string  `json:"theme"`
	SidebarTokenIds []string `json:"sidebar_token_ids"`
}

// userSettingsResponse reads the user's settings row (one GetUserSettings query,
// summary_model folded in) and writes the settings body, shared by the GET and PUT
// handlers so the two responses never drift.
func (h *Handler) userSettingsResponse(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	s, err := h.q.GetUserSettings(r.Context(), userID)
	if err != nil {
		slog.Error("get user settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// summary_model rides the GetUserSettings one-row read (it is the same users row),
	// so the settings surface reads it from `s` — no separate query. GetUserSummaryModel
	// stays the narrow read for issue-run claim assembly, where no other user field is
	// needed (PRD #362 M2).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"settings": userSettingsDTO{
			DefaultModel:    textPtrValue(s.DefaultModel.Valid, s.DefaultModel.String),
			JudgeModel:      textPtrValue(s.JudgeModel.Valid, s.JudgeModel.String),
			SummaryModel:    textPtrValue(s.SummaryModel.Valid, s.SummaryModel.String),
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
		JudgeModel      json.RawMessage `json:"judge_model"`
		SummaryModel    json.RawMessage `json:"summary_model"`
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

	if req.JudgeModel != nil {
		var raw *string
		if err := json.Unmarshal(req.JudgeModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid judge_model")
			return
		}
		// Same validator as default_model (PRD #69 M2, Decision 4): nil/blank
		// trims to NULL = inherit the instance judge_model; a bad model is a 400.
		model, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.q.SetUserJudgeModel(r.Context(), store.SetUserJudgeModelParams{
			ID:         user.ID,
			JudgeModel: model,
		}); err != nil {
			slog.Error("set user judge model", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if req.SummaryModel != nil {
		var raw *string
		if err := json.Unmarshal(req.SummaryModel, &raw); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid summary_model")
			return
		}
		// Same validator as judge_model (PRD #362 M2): nil/blank trims to NULL =
		// inherit the instance summary_model; a bad model is a 400.
		model, err := validateModel(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.q.SetUserSummaryModel(r.Context(), store.SetUserSummaryModelParams{
			ID:           user.ID,
			SummaryModel: model,
		}); err != nil {
			slog.Error("set user summary model", "error", err)
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
			// A store fault during the ownership check is OUR failure, not the
			// client's: 500, like every other store-error path in this handler.
			// The 400 arm is reserved for genuine request defects (a non-UUID
			// entry, an oversized list).
			if errors.Is(err, errSidebarStore) {
				httpx.Error(w, http.StatusInternalServerError, "internal error")
			} else {
				httpx.Error(w, http.StatusBadRequest, err.Error())
			}
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

// errSidebarStore marks a store fault inside validateSidebarTokenIds, so the
// caller can answer 500 for it while every other validation error stays a 400.
// The message is what a 500 body would say anyway; the identity is what matters.
var errSidebarStore = errors.New("internal error")

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
		slog.Error("list secrets for sidebar-token filter", "error", err)
		return nil, errSidebarStore
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
