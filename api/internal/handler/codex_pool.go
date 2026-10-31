package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The two stored values of users.codex_account_mode (migration 00387's CHECK).
const (
	codexModeDefault = "default"
	codexModeAuto    = "auto"
)

// SetCodexAccountMode sets the caller's Codex account mode (issue #3719). Body:
// {"codex_account_mode": "default"|"auto"}; any other value, or an absent field, is a 400.
// The response is {"codex_account_mode": <mode>}. Identity is the session user, never the
// body. See the route comment in routes_me.go for why this is Bearer-reachable.
func (h *Handler) SetCodexAccountMode(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Mode *string `json:"codex_account_mode"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Mode == nil || (*req.Mode != codexModeDefault && *req.Mode != codexModeAuto) {
		httpx.Error(w, http.StatusBadRequest, `codex_account_mode must be "default" or "auto"`)
		return
	}
	n, err := h.q.SetUserCodexAccountMode(r.Context(), store.SetUserCodexAccountModeParams{
		Mode: *req.Mode, ID: user.ID,
	})
	if err != nil {
		slog.Error("set codex account mode", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n == 0 {
		httpx.Error(w, http.StatusNotFound, "user not found")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"codex_account_mode": *req.Mode})
}

// PatchCodexAuthAutoEligible toggles whether one of the caller's codex_auth aliases is in
// the Codex auto-selection pool (issue #3719). It is PatchAnthropicTokenAutoEligible's
// sibling, with the same ownership discipline: the exclusive secret-mutation lock (the
// run-creation fence in workersvc depends on pool consent being written under it), the
// owner-scoped GetUserSecretForUpdate, and a 404 (never 403) for a foreign id or an id of
// any other kind (an openai_api_key or anthropic_token id is not a Codex pool member).
// Pooling a DISABLED alias is refused with 409, as for the Anthropic toggle; un-pooling one
// is allowed.
func (h *Handler) PatchCodexAuthAutoEligible(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	secretID, ok := httpx.PathUUID(w, r, "id", "secret")
	if !ok {
		return
	}
	// A POINTER, so an omitted field is a 400 rather than a silent opt-out.
	var req struct {
		AutoEligible *bool `json:"auto_eligible"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AutoEligible == nil {
		httpx.Error(w, http.StatusBadRequest, "auto_eligible is required")
		return
	}

	var out store.SetUserSecretAutoEligibleRow
	var disabledAt pgtype.Timestamptz
	var found bool
	err := h.withSecretLock(r.Context(), user.ID, nil, func(q *store.Queries) error {
		cur, gerr := q.GetUserSecretForUpdate(r.Context(), store.GetUserSecretForUpdateParams{
			ID: secretID, UserID: user.ID,
		})
		if gerr != nil {
			if errors.Is(gerr, pgx.ErrNoRows) {
				return nil // found stays false → 404
			}
			return gerr
		}
		if cur.Kind != store.KindCodexAuth {
			return nil // another kind's id: 404, indistinguishable from a foreign id
		}
		found = true
		disabledAt = cur.DisabledAt
		if cur.DisabledAt.Valid && *req.AutoEligible {
			return errSecretDisabled
		}
		row, serr := q.SetUserSecretAutoEligible(r.Context(), store.SetUserSecretAutoEligibleParams{
			ID: secretID, UserID: user.ID, Kind: store.KindCodexAuth,
			AutoEligible: *req.AutoEligible,
		})
		if serr != nil {
			return serr
		}
		out = row
		return nil
	})
	if err != nil {
		if errors.Is(err, errSecretDisabled) {
			httpx.Error(w, http.StatusConflict, err.Error())
			return
		}
		slog.Error("patch codex auth auto-eligible", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "codex login not found")
		return
	}
	dto := secretMeta(out.ID, out.Kind, out.Label, out.IsDefault, out.AutoEligible, out.CreatedAt, out.UpdatedAt)
	dto.Enabled = !disabledAt.Valid
	if disabledAt.Valid {
		dto.DisabledAt = &disabledAt.Time
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"secret": dto})
}
