package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ListRunFetches is GET /api/runs/{id}/fetches (PRD #1906 M3, Decision 8): the run's
// source log, every fetch attempt allowed or refused, oldest first. RequireUser (cookie or
// uzc_/uza_ Bearer) so `uzi run fetches` reaches it; STRICT owner-or-404 through
// h.wsvc.GetRun, like the recovery archives (an admin viewing a foreign run is refused
// too). A run that never fetched has an empty list.
//
// Keyset-paginated, because a log can hold up to fetch_max_run_attempts rows: ?limit= is
// 1..fetchctl.FetchesPageSize (default that, larger values clamped to it), ?after= is the
// next_cursor of the previous page (a row id of THIS run; anything else is 400). The page
// asks for one row more than it returns to know whether next_cursor is due.
func (h *Handler) ListRunFetches(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	if _, err := h.wsvc.GetRun(r.Context(), user.ID, runID); err != nil {
		if errors.Is(err, workersvc.ErrRunNotFound) {
			httpx.Error(w, http.StatusNotFound, "run not found")
			return
		}
		slog.Error("run fetches: get run", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	limit := int32(fetchctl.FetchesPageSize)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n < 1 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = int32(min(n, fetchctl.FetchesPageSize))
	}
	params := store.ListRunFetchesParams{
		RunID:          runID,
		AfterCreatedAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
		AfterID:        uuid.Nil,
		MaxRows:        limit + 1,
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		after, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "after must be the next_cursor of a previous page")
			return
		}
		cur, err := h.q.GetRunFetchCursor(r.Context(), store.GetRunFetchCursorParams{RunID: runID, ID: after})
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusBadRequest, "after is not a fetch of this run")
			return
		}
		if err != nil {
			slog.Error("run fetches: cursor", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		params.AfterCreatedAt, params.AfterID = cur.CreatedAt, cur.ID
	}
	rows, err := h.q.ListRunFetches(r.Context(), params)
	if err != nil {
		slog.Error("run fetches: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.RunFetchesDTO{}
	if len(rows) > int(limit) {
		rows = rows[:limit]
		out.NextCursor = rows[len(rows)-1].ID.String()
	}
	out.Fetches = make([]apitypes.RunFetchDTO, 0, len(rows))
	for _, f := range rows {
		out.Fetches = append(out.Fetches, apitypes.RunFetchDTO{
			ID:          f.ID.String(),
			URL:         f.Url,
			FinalURL:    f.FinalUrl,
			Verdict:     f.Verdict,
			Reason:      f.Reason,
			HTTPStatus:  int(f.HttpStatus),
			ContentType: f.ContentType,
			Bytes:       f.Bytes,
			SHA256:      f.Sha256,
			StartedAt:   f.StartedAt.Time,
			FinishedAt:  f.FinishedAt.Time,
			CreatedAt:   f.CreatedAt.Time,
		})
	}
	httpx.JSON(w, http.StatusOK, out)
}
