package handler

import (
	"errors"
	"log/slog"
	"net/http"

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
	rows, err := h.q.ListRunFetches(r.Context(), store.ListRunFetchesParams{RunID: runID, MaxRows: fetchctl.MaxListedFetches})
	if err != nil {
		slog.Error("run fetches: list", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := apitypes.RunFetchesDTO{Fetches: make([]apitypes.RunFetchDTO, 0, len(rows))}
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
