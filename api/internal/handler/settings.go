package handler

import (
	"fmt"
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// updateSettingsRequest is a partial map of setting key → value. Any subset of
// the known keys may be sent; unknown keys are rejected. The web Settings page
// sends both label keys together, but the API tolerates a single-key update.
type updateSettingsRequest struct {
	Settings map[string]string `json:"settings"`
}

// GetSettings returns every known setting with its effective value (admin only).
// The shape is stable — one entry per known key — so a missing row reads as its
// compiled-in default rather than an absent field.
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	all, err := h.settings.All(r.Context())
	if err != nil {
		slog.Error("get settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"settings": all})
}

// UpdateSettings writes one or more settings (admin only), validating per
// Decision 8: each value non-empty, ≤ 64 chars, no comma; unknown keys
// rejected; and the cross-key rule prd_label != autopilot_label checked against
// the effective post-update state. The writes commit in a single transaction so
// a two-key swap can never leave the two labels transiently equal (the exact
// state the cross-key rule forbids).
func (h *Handler) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req updateSettingsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Settings) == 0 {
		httpx.Error(w, http.StatusBadRequest, "no settings provided")
		return
	}

	// Per-key validation: known key + per-value rules.
	for key, value := range req.Settings {
		if !settings.Known(key) {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("unknown setting: %s", key))
			return
		}
		if err := settings.ValidateLabel(value); err != nil {
			httpx.Error(w, http.StatusBadRequest, fmt.Sprintf("%s: %s", key, err))
			return
		}
	}

	// Cross-key rule on the effective post-update state: the current values
	// overlaid with the submitted ones, so a single-key PUT is still checked
	// against the other key's stored value.
	current, err := h.settings.All(r.Context())
	if err != nil {
		slog.Error("update settings: read current", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	merged := make(map[string]string, len(current))
	for k, v := range current {
		merged[k] = v
	}
	for k, v := range req.Settings {
		merged[k] = v
	}
	if err := settings.ValidateMerged(merged); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		slog.Error("update settings: begin tx", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful Commit
	qtx := h.q.WithTx(tx)
	for key, value := range req.Settings {
		if _, err := qtx.UpsertAppSetting(ctx, store.UpsertAppSettingParams{
			Key:       key,
			Value:     value,
			UpdatedBy: pgUUID(actor.ID),
		}); err != nil {
			// Deliberately omit the key/value from the log (audit pre-flag): settings
			// values are not secrets today, but nothing in app_settings should ever
			// be assumed loggable.
			slog.Error("update settings: upsert failed", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("update settings: commit", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Drop the read cache so the next read (poller or this handler's response)
	// reflects the write immediately rather than lagging by the TTL.
	h.settings.Invalidate()

	all, err := h.settings.All(ctx)
	if err != nil {
		slog.Error("update settings: read back", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"settings": all})
}
