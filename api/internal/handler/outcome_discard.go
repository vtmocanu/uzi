package handler

// Owner-confirmed discard of a worker-held run_not_owned terminal outcome (issue #2984). The
// worker-authenticated half (report, directive list, begin, complete, reconcile) is mounted under
// /api/worker; the owner half under /api/workers/{id} on the RequireUser group, so a uzc_ CLI
// token reaches it without CSRF and a cookie session needs the CSRF header. The service
// (workersvc/outcome_discard.go) owns every authorization and lock decision.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/outcomeloss"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const outcomeDiscardSmallBodyMax = 8 << 10

var (
	errBlockedOutcomesTooLarge = errors.New("blocked outcomes body too large")
	errBlockedOutcomesInvalid  = errors.New("invalid blocked outcomes body")
)

// decodeBlockedOutcomesPut reads a PUT body under the 6 MiB cap, counting outcomes and bytes
// while decoding: at most 8 outcomes, each at most outcomeloss.MaxReportBytes of raw JSON, each
// decoded strictly (unknown fields refused), nothing after the single top-level object.
func decodeBlockedOutcomesPut(w http.ResponseWriter, r *http.Request) ([]apitypes.BlockedOutcomeReport, error) {
	body := http.MaxBytesReader(w, r.Body, outcomeloss.MaxPutBodyBytes)
	dec := json.NewDecoder(body)
	fail := func(err error) ([]apitypes.BlockedOutcomeReport, error) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errBlockedOutcomesTooLarge
		}
		return nil, errBlockedOutcomesInvalid
	}
	expect := func(want json.Delim) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); !ok || d != want {
			return errBlockedOutcomesInvalid
		}
		return nil
	}
	if err := expect('{'); err != nil {
		return fail(err)
	}
	tok, err := dec.Token()
	if err != nil {
		return fail(err)
	}
	if key, ok := tok.(string); !ok || key != "outcomes" {
		return nil, errBlockedOutcomesInvalid
	}
	if err := expect('['); err != nil {
		return fail(err)
	}
	reports := []apitypes.BlockedOutcomeReport{}
	for dec.More() {
		if len(reports) >= outcomeloss.MaxOutcomesPerPut {
			return nil, errBlockedOutcomesInvalid
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fail(err)
		}
		if len(raw) > outcomeloss.MaxReportBytes {
			return nil, errBlockedOutcomesTooLarge
		}
		item := json.NewDecoder(bytes.NewReader(raw))
		item.DisallowUnknownFields()
		var rep apitypes.BlockedOutcomeReport
		if err := item.Decode(&rep); err != nil {
			return nil, errBlockedOutcomesInvalid
		}
		if _, err := item.Token(); !errors.Is(err, io.EOF) {
			return nil, errBlockedOutcomesInvalid
		}
		reports = append(reports, rep)
	}
	if err := expect(']'); err != nil {
		return fail(err)
	}
	if err := expect('}'); err != nil {
		return fail(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errBlockedOutcomesInvalid
	}
	return reports, nil
}

// outcomeDiscardError maps a service error onto the response. It returns false for an error it
// does not own, leaving the caller to answer 500.
func outcomeDiscardError(w http.ResponseWriter, err error) bool {
	var ref *workersvc.OutcomeDiscardRefusal
	switch {
	case errors.As(err, &ref):
		httpx.JSON(w, http.StatusConflict, apitypes.OutcomeDiscardRefusal{Error: "the outcome cannot be discarded: " + ref.Code, Code: ref.Code})
	case errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found")
	case errors.Is(err, workersvc.ErrWorkerNotFound), errors.Is(err, workersvc.ErrOutcomeWorkerGone):
		httpx.Error(w, http.StatusNotFound, "worker not found")
	case errors.Is(err, workersvc.ErrOutcomeDiscardNotFound):
		httpx.Error(w, http.StatusNotFound, "outcome discard not found")
	case errors.Is(err, workersvc.ErrOutcomeDiscardConflict):
		httpx.JSON(w, http.StatusConflict, apitypes.OutcomeDiscardRefusal{Error: "the directive is not in a state that allows this", Code: "state_conflict"})
	case errors.Is(err, workersvc.ErrInvalidOutcomeDiscardRequest), errors.Is(err, workersvc.ErrInvalidBlockedOutcome):
		httpx.Error(w, http.StatusBadRequest, "invalid request")
	default:
		return false
	}
	return true
}

func outcomeDiscardInternal(w http.ResponseWriter, what string, err error) {
	slog.Error("outcome discard: "+what, "error", err)
	httpx.Error(w, http.StatusInternalServerError, "internal error")
}

// WorkerBlockedOutcomesPut is PUT /api/worker/blocked-outcomes: the holding worker's wholesale
// report of its blocked journals.
func (h *Handler) WorkerBlockedOutcomesPut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	reports, err := decodeBlockedOutcomesPut(w, r)
	switch {
	case errors.Is(err, errBlockedOutcomesTooLarge):
		httpx.Error(w, http.StatusRequestEntityTooLarge, "blocked outcomes body too large")
		return
	case err != nil:
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.wsvc.ReplaceBlockedOutcomes(r.Context(), wkr, reports); err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "report", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.BlockedOutcomesPutResponse{Stored: len(reports)})
}

// WorkerOutcomeDiscardsList is GET /api/worker/outcome-discards.
func (h *Handler) WorkerOutcomeDiscardsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	out, err := h.wsvc.ListWorkerOutcomeDiscards(r.Context(), wkr)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "list", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.WorkerOutcomeDiscardsResponse{Discards: out})
}

// workerOutcomeDiscardAction wraps the three worker POST routes that act on one directive.
func (h *Handler) workerOutcomeDiscardAction(w http.ResponseWriter, r *http.Request, what string,
	act func(wkr store.Worker, id uuid.UUID) (apitypes.OutcomeDiscard, error), decode func(*http.Request) error) {
	w.Header().Set("Cache-Control", "no-store")
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "outcome discard")
	if !ok {
		return
	}
	if decode != nil {
		if err := decode(r); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	out, err := act(wkr, id)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, what, err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// WorkerOutcomeDiscardBegin is POST /api/worker/outcome-discards/{id}/begin.
func (h *Handler) WorkerOutcomeDiscardBegin(w http.ResponseWriter, r *http.Request) {
	h.workerOutcomeDiscardAction(w, r, "begin", func(wkr store.Worker, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
		return h.wsvc.BeginOutcomeDiscard(r.Context(), wkr, id)
	}, nil)
}

// WorkerOutcomeDiscardComplete is POST /api/worker/outcome-discards/{id}/complete.
func (h *Handler) WorkerOutcomeDiscardComplete(w http.ResponseWriter, r *http.Request) {
	var req apitypes.OutcomeDiscardCompleteRequest
	h.workerOutcomeDiscardAction(w, r, "complete", func(wkr store.Worker, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
		return h.wsvc.CompleteOutcomeDiscard(r.Context(), wkr, id, req)
	}, func(r *http.Request) error { return httpx.DecodeJSONStrictBounded(r, &req, outcomeDiscardSmallBodyMax) })
}

// WorkerOutcomeDiscardReconcile is POST /api/worker/outcome-discards/{id}/reconcile.
func (h *Handler) WorkerOutcomeDiscardReconcile(w http.ResponseWriter, r *http.Request) {
	var req apitypes.OutcomeDiscardReconcileRequest
	h.workerOutcomeDiscardAction(w, r, "reconcile", func(wkr store.Worker, id uuid.UUID) (apitypes.OutcomeDiscard, error) {
		return h.wsvc.ReconcileOutcomeDiscard(r.Context(), wkr, id, req)
	}, func(r *http.Request) error { return httpx.DecodeJSONStrictBounded(r, &req, outcomeDiscardSmallBodyMax) })
}

// ListWorkerBlockedOutcomes is GET /api/workers/{id}/blocked-outcomes: the worker's reported
// outcomes of runs the caller owns. The caller must own the worker.
func (h *Handler) ListWorkerBlockedOutcomes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	workerID, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	out, err := h.wsvc.ListBlockedOutcomesForOwner(r.Context(), user.ID, workerID)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "owner list", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.OwnerBlockedOutcomesResponse{Outcomes: out})
}

// DiscardWorkerBlockedOutcome is POST /api/workers/{id}/blocked-outcomes/discard?confirm=discard.
// Order: confirm (400, before any SQL), strict body and grammar (400), evidence (400), then the
// service: run owner (404), worker owner (404), and the 409 refusals in their fixed order.
func (h *Handler) DiscardWorkerBlockedOutcome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if r.URL.Query().Get("confirm") != "discard" {
		httpx.Error(w, http.StatusBadRequest, "outcome discard requires ?confirm=discard")
		return
	}
	workerID, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	var req apitypes.OutcomeDiscardRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, outcomeDiscardSmallBodyMax); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	out, err := h.wsvc.AuthorizeOutcomeDiscard(r.Context(), user.ID, workerID, req)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "authorize", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// GetWorkerOutcomeDiscard is GET /api/workers/{id}/outcome-discards/{discardID}.
func (h *Handler) GetWorkerOutcomeDiscard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	workerID, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "discardID", "outcome discard")
	if !ok {
		return
	}
	out, err := h.wsvc.GetOutcomeDiscardForOwner(r.Context(), user.ID, workerID, id)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "owner status", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// CancelWorkerOutcomeDiscard is POST /api/workers/{id}/outcome-discards/{discardID}/cancel.
func (h *Handler) CancelWorkerOutcomeDiscard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	workerID, ok := httpx.PathUUID(w, r, "id", "worker")
	if !ok {
		return
	}
	id, ok := httpx.PathUUID(w, r, "discardID", "outcome discard")
	if !ok {
		return
	}
	out, err := h.wsvc.CancelOutcomeDiscard(r.Context(), user.ID, workerID, id)
	if err != nil {
		if !outcomeDiscardError(w, err) {
			outcomeDiscardInternal(w, "owner cancel", err)
		}
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
