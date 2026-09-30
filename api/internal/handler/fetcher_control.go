package handler

// fetcher_control.go is the api side of uzi-fetcher's control protocol (PRD #1906 M3):
// POST /api/fetcher/v1/begin and /api/fetcher/v1/complete, the contract in the package doc
// of api/internal/fetcher ("Fetcher -> api"). Both are authenticated by the fetcher's own
// service Bearer (mw.RequireFetcher), never a cookie. The RUN is taken only from the
// per-run credential in the body (fetchctl hashes it and looks the row up): the bodies are
// decoded strictly into apitypes shapes that have no run field, so a request naming a run
// is a 400, not an instruction.

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountFetcherRoutes registers the fetcher control routes. Called by BOTH routers, like
// mountControllerRoutes. Mounted ONLY when UZI_FETCHER_TOKEN_SHA256 is configured: unset
// (the compose default), the routes do not exist rather than existing-and-refusing.
func (h *Handler) mountFetcherRoutes(r chi.Router) {
	if len(h.cfg.FetcherTokenSHA256) == 0 {
		return
	}
	r.Group(func(r chi.Router) {
		r.Use(mw.RequireFetcher(h.cfg.FetcherTokenSHA256))
		r.Post(fetchctl.BeginPath[len("/api"):], h.FetcherBegin)
		r.Post(fetchctl.CompletePath[len("/api"):], h.FetcherComplete)
	})
}

func (h *Handler) fetchControl() (*fetchctl.Service, bool) {
	if h.pool == nil || h.settings == nil {
		return nil, false
	}
	return fetchctl.New(h.pool, h.settings), true
}

func writeFetcherControlError(w http.ResponseWriter, status int, msg, reason string) {
	httpx.JSON(w, status, apitypes.FetcherControlErrorDTO{Error: msg, Reason: reason})
}

// FetcherBegin validates the forwarded run credential and admits one fetch: 200 with the
// reservation and the run's claim-time site-list snapshot, 403 credential_invalid, or 429
// with the cap that refused it.
func (h *Handler) FetcherBegin(w http.ResponseWriter, r *http.Request) {
	var req apitypes.FetcherBeginRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid begin request")
		return
	}
	svc, ok := h.fetchControl()
	if !ok {
		httpx.Error(w, http.StatusServiceUnavailable, "fetch control is not configured")
		return
	}
	adm, err := svc.Begin(r.Context(), req.Credential)
	var refused *fetchctl.AdmissionRefusedError
	switch {
	case err == nil:
		httpx.JSON(w, http.StatusOK, apitypes.FetcherBeginResponse{
			ReservationID: adm.ReservationID.String(),
			Entries:       adm.Entries,
			MaxBytes:      adm.MaxBytes,
		})
	case errors.Is(err, fetchctl.ErrCredentialInvalid):
		writeFetcherControlError(w, http.StatusForbidden, "the run fetch credential is invalid", fetchctl.ReasonCredentialInvalid)
	case errors.As(err, &refused):
		writeFetcherControlError(w, http.StatusTooManyRequests, "a run fetch limit is used up", refused.Reason)
	default:
		slog.Error("fetcher begin", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}

// FetcherComplete records one admitted attempt in the run's source log and settles its
// reservation. 204 when logged (or already logged); anything else means nothing was
// written, and the fetcher then returns no content.
func (h *Handler) FetcherComplete(w http.ResponseWriter, r *http.Request) {
	var req apitypes.FetcherCompleteRequest
	if err := httpx.DecodeJSONStrict(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid complete request")
		return
	}
	svc, ok := h.fetchControl()
	if !ok {
		httpx.Error(w, http.StatusServiceUnavailable, "fetch control is not configured")
		return
	}
	err := svc.Complete(r.Context(), req)
	var invalid *fetchctl.InvalidRequestError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, fetchctl.ErrCredentialInvalid):
		writeFetcherControlError(w, http.StatusForbidden, "the run fetch credential is invalid", fetchctl.ReasonCredentialInvalid)
	case errors.Is(err, fetchctl.ErrReservationNotFound):
		writeFetcherControlError(w, http.StatusNotFound, "no such reservation for this run", "reservation_not_found")
	case errors.As(err, &invalid):
		writeFetcherControlError(w, http.StatusBadRequest, invalid.Msg, "invalid_record")
	default:
		slog.Error("fetcher complete", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
