package handler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// SetUsageReserve sets the caller's weekly usage reserve (PRD #3522): the 7-day utilization
// percent at which uzi stops spending their subscription, and whether a running run is also
// paused. Body: {"threshold_pct": <integer 1..100>|null, "pause_running": <bool>}. BOTH
// fields are required (an absent field is a 400, never "unchanged"), so a write is always the
// whole setting; null threshold_pct turns the reserve off.
//
// The response is {"user": <UserDTO>}, like the other per-user toggles (SetUserWaitOnLimit,
// SetUserNotifyEarlyReset), so the weekly_reserve_pct and weekly_reserve_pause_running
// fields come back with the rest of the user.
//
// Identity is the session user, never the body. The threshold is bounded 1..100 here and by
// the users_weekly_reserve_pct_check constraint.
func (h *Handler) SetUsageReserve(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// RawMessage distinguishes an absent field (nil) from a present null (the bytes `null`).
	var req struct {
		ThresholdPct json.RawMessage `json:"threshold_pct"`
		PauseRunning json.RawMessage `json:"pause_running"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ThresholdPct == nil {
		httpx.Error(w, http.StatusBadRequest, "threshold_pct is required (an integer 1..100, or null to turn the reserve off)")
		return
	}
	if req.PauseRunning == nil {
		httpx.Error(w, http.StatusBadRequest, "pause_running is required (true or false)")
		return
	}
	var pct pgtype.Int2
	if !bytes.Equal(bytes.TrimSpace(req.ThresholdPct), []byte("null")) {
		// Decoding into int64 rejects a string ("70") and a fraction (1.5) alike.
		var n int64
		if err := json.Unmarshal(req.ThresholdPct, &n); err != nil || n < 1 || n > 100 {
			httpx.Error(w, http.StatusBadRequest, "threshold_pct must be an integer from 1 to 100, or null")
			return
		}
		pct = pgtype.Int2{Int16: int16(n), Valid: true}
	}
	var pause bool
	if err := json.Unmarshal(req.PauseRunning, &pause); err != nil || bytes.Equal(bytes.TrimSpace(req.PauseRunning), []byte("null")) {
		httpx.Error(w, http.StatusBadRequest, "pause_running must be true or false")
		return
	}
	updated, err := h.q.SetUserUsageReserve(r.Context(), store.SetUserUsageReserveParams{
		ID:                        user.ID,
		WeeklyReservePct:          pct,
		WeeklyReservePauseRunning: pause,
	})
	if err != nil {
		slog.Error("set user usage reserve", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"user": toDTO(updated)})
}
