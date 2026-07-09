package handler

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// WorkerRegister brings the worker online (and recovers any runs it orphaned by
// restarting). Accepts an optional {version} body.
func (h *Handler) WorkerRegister(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	var req struct {
		Version string `json:"version"`
		// Name is accepted for wire compatibility (the M2 worker announces both
		// name and version) but deliberately ignored: the authoritative worker
		// name is the user-chosen label set at token issuance, not something the
		// worker may overwrite. DecodeJSON rejects unknown fields, so this must be
		// declared even though nothing reads it.
		Name string `json:"name"`
		// Template is the worker's self-reported image template (PRD #18). Unlike
		// Name, it IS read + persisted (as template_reported): it is observability
		// the server surfaces and badges drift on, never an authn/authz input.
		// Optional — an older image omits it and the column stays NULL.
		Template string `json:"template"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// template is untrusted worker self-report bound for the DB + web UI. Bound it
	// to a tight charset (workertmpl.WellFormed) and DROP anything else to empty
	// (persisted as NULL) rather than 400 — a soft observability field must never
	// wedge a worker's register-retry loop. Membership is NOT checked here: an
	// unknown-but-well-formed name is the drift signal, not an error.
	reported := strings.TrimSpace(req.Template)
	if reported != "" && !workertmpl.WellFormed(reported) {
		slog.Warn("worker reported a malformed template; dropping", "worker_id", wkr.ID.String())
		reported = ""
	}
	updated, err := h.wsvc.Register(r.Context(), wkr, req.Version, reported)
	if err != nil {
		slog.Error("worker register", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// worker_id is echoed for the worker's convenience; identity on every other
	// call comes from the Bearer token, never a URL path (M2 wire contract).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"worker_id": updated.ID.String(),
		"worker":    workerDTOFromWorker(updated, false),
	})
}

// WorkerHeartbeat refreshes liveness. No body.
func (h *Handler) WorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	updated, err := h.wsvc.Heartbeat(r.Context(), wkr)
	if err != nil {
		slog.Error("worker heartbeat", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": workerDTOFromWorker(updated, false)})
}

// WorkerClaim atomically claims the next run for the worker's user. 204 when the
// queue is idle; otherwise the full claim payload (never logged — it carries
// decrypted credentials).
func (h *Handler) WorkerClaim(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	payload, err := h.wsvc.Claim(r.Context(), wkr)
	if err != nil {
		slog.Error("worker claim", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if payload == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpx.JSON(w, http.StatusOK, payload)
}

// WorkerRunMessages appends a batch of seq-numbered messages (idempotent on
// (run_id, seq)).
func (h *Handler) WorkerRunMessages(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req struct {
		Messages []workersvc.IncomingMessage `json:"messages"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.wsvc.AppendMessages(r.Context(), wkr, runID, req.Messages); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidMessage):
			httpx.Error(w, http.StatusBadRequest, "each message needs a positive seq, a kind, and a JSON payload")
		default:
			slog.Error("worker run messages", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WorkerRunState applies a state transition and echoes the run's resulting
// status, so the worker learns if the run was cancelled out from under it.
func (h *Handler) WorkerRunState(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req workersvc.StateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, applied, err := h.wsvc.SetState(r.Context(), wkr, runID, req)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.Error(w, http.StatusBadRequest, "state must be one of running, awaiting_approval, completed, failed")
		default:
			slog.Error("worker run state", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if !applied {
		// The run was already terminal (e.g. cancelled out from under the worker):
		// 409 with the run's real status. The worker treats 409 as success and
		// stops (M2 wire contract).
		httpx.JSON(w, http.StatusConflict, map[string]any{"run": runToDTO(run)})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run)})
}

// WorkerRunInputs consumes and returns any pending steering inputs, FIFO.
func (h *Handler) WorkerRunInputs(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	inputs, err := h.wsvc.ConsumeInputs(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run inputs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"inputs": inputs})
}
