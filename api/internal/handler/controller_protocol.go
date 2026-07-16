package handler

import (
	"errors"
	"log/slog"
	"net/http"

	"gitlab.example.com/vtmocanu/uzi/api/internal/hostedsvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
)

// ControllerPoll serves the hosted-worker controller's desired-state poll (PRD
// #58 Decision 2). Authenticated by mw.RequireController on the route group; the
// credential is whole-fleet scoped, so there is no per-row authz here — this
// endpoint exists only when hosting is enabled, and only the controller can reach
// it.
//
// A POST, not a GET, because the request carries the controller's materialization
// acks (see hostedsvc.PollRequest): the poll and the ack are one round trip
// against one endpoint. No CSRF step — the route group is bearer-authenticated,
// not cookie-authenticated, exactly like /api/worker/*.
func (h *Handler) ControllerPoll(w http.ResponseWriter, r *http.Request) {
	var req hostedsvc.PollRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := h.hsvc.Poll(r.Context(), req)
	if err != nil {
		if errors.Is(err, hostedsvc.ErrBadWorkerID) {
			httpx.Error(w, http.StatusBadRequest, "materialized must contain worker uuids")
			return
		}
		slog.Error("controller poll", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, resp)
}
