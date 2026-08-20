package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// ControllerCordonWorker requests that a hosted worker start draining (PRD #422 M4,
// Decision 8).
//
// This is a CONTROL-WRITE: it MUTATES desired state (it stamps draining_since so
// the claim gate idles the worker), which is a different trust/failure class from
// the display-only ControllerStatus report — that one can only UPDATE roll-health
// display rows and is deliberately structurally incapable of touching scheduling
// state. Cordon is the one thing this controller asserts that changes what the
// fleet does, so it rides its own endpoint rather than being folded into the
// status report.
//
// Authenticated by mw.RequireController on the route group — the same fleet-scoped
// bearer credential the poll and status report use, so no new auth mechanism, no
// cookie, no CSRF step. The Decision 8 blast-radius note: cordoning widens the
// controller's authority (a compromised controller could cordon the whole fleet),
// so the endpoint is scoped to exactly the cordon action and nothing more — it can
// set draining, and it can do nothing else.
//
// 204 No Content on success (nothing for the controller to read state out of, so
// this cannot grow into a second desired-state channel); 404 when no hosted worker
// has that id (a controller cordoning a worker the api has since dropped); 400 on a
// malformed workerID; 500 on a store error.
func (h *Handler) ControllerCordonWorker(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workerID"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid worker id")
		return
	}
	found, err := h.hsvc.Cordon(r.Context(), id)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !found {
		httpx.Error(w, http.StatusNotFound, "no such hosted worker")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
