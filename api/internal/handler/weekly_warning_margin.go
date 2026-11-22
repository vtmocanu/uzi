package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Bounds of the weekly-limit warning margin in percentage points (PRD #4123 module 6); the
// users_weekly_warning_margin_pts_check constraint repeats them.
const (
	minWeeklyWarningMarginPts = 0
	maxWeeklyWarningMarginPts = 50
)

// SetWeeklyWarningMargin sets the caller's weekly-limit warning margin (PRD #4123): how many percentage
// points below its effective weekly limit a credential must be before the early banner shows; 0 turns
// the early warnings off (park explanations are unaffected). Body: {"margin_pts": <integer 0..50>}.
// The field is required, unknown fields, fractions, strings and trailing values are a 400, and a
// rejected request writes nothing.
//
// The response is {"weekly_warning_margin_pts": <stored value>}; GET /api/me/settings reads the same
// value back. Identity is the session user, never the body. RequireUser, so a uzc_ CLI Bearer works
// (`uzi settings warning-margin`) and a cookie write needs CSRF.
func (h *Handler) SetWeeklyWarningMargin(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// RawMessage distinguishes an absent field (nil) from a present null.
	var req struct {
		MarginPts json.RawMessage `json:"margin_pts"`
	}
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MarginPts == nil {
		httpx.Error(w, http.StatusBadRequest, "margin_pts is required (an integer 0..50)")
		return
	}
	// Decoding into int64 rejects a string ("15"), a fraction (1.5) and a number with an exponent; null is refused above.
	var n int64
	if bytes.Equal(bytes.TrimSpace(req.MarginPts), []byte("null")) {
		httpx.Error(w, http.StatusBadRequest, "margin_pts must be an integer from 0 to 50")
		return
	}
	if err := json.Unmarshal(req.MarginPts, &n); err != nil || n < minWeeklyWarningMarginPts || n > maxWeeklyWarningMarginPts {
		httpx.Error(w, http.StatusBadRequest, "margin_pts must be an integer from 0 to 50")
		return
	}
	stored, err := h.q.SetUserWeeklyWarningMargin(r.Context(), store.SetUserWeeklyWarningMarginParams{
		ID:        user.ID,
		MarginPts: int16(n),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The session names a user row that no longer exists.
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err != nil {
		slog.Error("set user weekly warning margin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"weekly_warning_margin_pts": stored})
}
