package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// -------------------------------------------------------------------------
// Pause-all-schedules (PRD #1093 D7): a singleton pause resource under the
// RequireUser /schedules group, CLI-reachable. Owner-scoped by construction —
// the row is always the caller's own user, so there is no id to check.
// -------------------------------------------------------------------------

// normalizeSchedulePause turns the RAW pause columns into the NORMALIZED wire DTO,
// applying the same expiry rule as M1's scheduler pauseCache: paused AND (until NULL
// OR until > now). An expired `until` reads as not paused (until:null). The read-time
// normalization uses time.Now() at the call site rather than an injected clock, unlike
// the scheduler which fires against its own e.now().
func normalizeSchedulePause(paused bool, until pgtype.Timestamptz, now time.Time) apitypes.SchedulePauseDTO {
	if !paused || (until.Valid && !until.Time.After(now)) {
		return apitypes.SchedulePauseDTO{Paused: false, Until: nil}
	}
	if until.Valid {
		u := until.Time
		return apitypes.SchedulePauseDTO{Paused: true, Until: &u}
	}
	return apitypes.SchedulePauseDTO{Paused: true, Until: nil}
}

// GetSchedulePause returns the caller's normalized pause-all state
// (GET /api/schedules/pause). A missing owner row (ErrNoRows) reads as not paused
// (fail-open; the run_schedules FK makes a missing row unlikely for a session user).
func (h *Handler) GetSchedulePause(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	row, err := h.q.GetUserSchedulePause(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.JSON(w, http.StatusOK, apitypes.SchedulePauseDTO{Paused: false, Until: nil})
			return
		}
		slog.Error("get user schedule pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, normalizeSchedulePause(row.SchedulesPaused, row.SchedulesPausedUntil, time.Now()))
}

// PutSchedulePause pauses all the caller's schedules (PUT /api/schedules/pause). Body
// {"until": "<RFC3339>"|null}: a null until pauses indefinitely ("until I resume"); an
// until in the PAST is a 422 (a pause that is already over is a client error, not a
// silent no-op). Returns the normalized state, 200.
func (h *Handler) PutSchedulePause(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Until *time.Time `json:"until"`
	}
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		httpx.RespondDecodeError(w, err, "invalid request body")
		return
	}
	now := time.Now()
	if req.Until != nil && !req.Until.After(now) {
		httpx.Error(w, http.StatusUnprocessableEntity, "until must be in the future")
		return
	}
	until := pgtype.Timestamptz{}
	if req.Until != nil {
		until = pgtype.Timestamptz{Time: *req.Until, Valid: true}
	}
	set, err := h.q.SetUserSchedulePause(r.Context(), store.SetUserSchedulePauseParams{
		SchedulesPaused:      true,
		SchedulesPausedUntil: until,
		ID:                   user.ID,
	})
	if err != nil {
		slog.Error("set user schedule pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, normalizeSchedulePause(set.SchedulesPaused, set.SchedulesPausedUntil, now))
}

// DeleteSchedulePause resumes all the caller's schedules (DELETE /api/schedules/pause).
// Idempotent — resuming an already-resumed user is a clean 200. Never touches any
// per-row run_schedules.enabled, so resume restores the exact prior set. Returns the
// (resumed) normalized state, 200.
func (h *Handler) DeleteSchedulePause(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	set, err := h.q.SetUserSchedulePause(r.Context(), store.SetUserSchedulePauseParams{
		SchedulesPaused:      false,
		SchedulesPausedUntil: pgtype.Timestamptz{},
		ID:                   user.ID,
	})
	if err != nil {
		slog.Error("clear user schedule pause", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, normalizeSchedulePause(set.SchedulesPaused, set.SchedulesPausedUntil, time.Now()))
}
