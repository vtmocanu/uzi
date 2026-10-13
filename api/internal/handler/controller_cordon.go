package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5"
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
	id, ok := httpx.PathUUID(w, r, "workerID", "worker")
	if !ok {
		return
	}
	reason, ok := controllerDrainReason(w, r)
	if !ok {
		return
	}
	found, err := h.hsvc.Cordon(r.Context(), id, reason)
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

// ControllerUncordonWorker requests that a hosted worker stop draining (issue #458).
//
// This is the symmetric mirror of ControllerCordonWorker, and the same CONTROL-WRITE
// class: it MUTATES desired state (it clears draining_since so the claim gate resumes
// scheduling the worker), unlike the display-only ControllerStatus report. It exists
// for the reverted-drift recovery — a worker cordoned on drift whose drift is then
// reverted before it rolls has nothing left to roll, and draining_since is otherwise
// cleared ONLY by RegisterWorker on an actual roll, so it would stay cordoned forever.
//
// Authenticated by mw.RequireController on the route group — the same fleet-scoped
// bearer credential cordon uses, so no new auth mechanism. The Decision 8 blast-radius
// note applies symmetrically: this endpoint is scoped to exactly the uncordon action
// and nothing more — it can clear draining, and it can do nothing else.
//
// 204 No Content on success; 404 when no hosted worker has that id (a controller
// uncordoning a worker the api has since dropped); 400 on a malformed workerID; 500 on
// a store error.
func (h *Handler) ControllerUncordonWorker(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.PathUUID(w, r, "workerID", "worker")
	if !ok {
		return
	}
	reason, ok := controllerDrainReason(w, r)
	if !ok {
		return
	}
	var found bool
	var err error
	if reason == "pressure_recycle" {
		if h.pool == nil {
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		tx, beginErr := h.pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if beginErr != nil {
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
		qtx := h.q.WithTx(tx)
		// Match ClaimRun's worker-first lock order. The separate release statement gets
		// a fresh READ COMMITTED snapshot after any custody writer ahead of us commits.
		_, err = qtx.GetWorkerForUpdate(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.Error(w, http.StatusNotFound, "no such hosted worker")
			return
		}
		if err == nil {
			found, err = h.hsvc.WithStore(qtx).Uncordon(r.Context(), id, reason)
		}
		if err == nil {
			err = tx.Commit(r.Context())
		}
	} else {
		found, err = h.hsvc.Uncordon(r.Context(), id, reason)
	}
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

func controllerDrainReason(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req struct {
		Reason string `json:"reason,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid drain request")
		return "", false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid drain request")
		return "", false
	}
	if req.Reason != "" && req.Reason != "image_roll" && req.Reason != "pressure_recycle" {
		httpx.Error(w, http.StatusBadRequest, "invalid drain reason")
		return "", false
	}
	return req.Reason, true
}
