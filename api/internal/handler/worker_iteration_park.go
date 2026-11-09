package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// decodeIterationPark bounds the entire body, including trailing whitespace,
// before decoding. Pointers distinguish required false/empty values from null.
func decodeIterationPark(r *http.Request) (workersvc.IterationParkEvidence, error) {
	var e workersvc.IterationParkEvidence
	raw, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 || !utf8.Valid(raw) {
		return e, workersvc.ErrIterationParkEvidence
	}
	var body struct {
		ClaimGeneration          *int64          `json:"claim_generation"`
		IterationCount           *int32          `json:"iteration_count"`
		MaxIterations            *int32          `json:"max_iterations"`
		Head                     *string         `json:"head"`
		Published                *bool           `json:"published"`
		CheckpointContainsLatest json.RawMessage `json:"checkpoint_contains_latest"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil {
		return e, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return e, workersvc.ErrIterationParkEvidence
	}
	if body.ClaimGeneration == nil || body.IterationCount == nil || body.MaxIterations == nil ||
		body.Head == nil || body.Published == nil {
		return e, workersvc.ErrIterationParkEvidence
	}
	e = workersvc.IterationParkEvidence{ClaimGeneration: *body.ClaimGeneration,
		IterationCount: *body.IterationCount, MaxIterations: *body.MaxIterations, Head: *body.Head, Published: *body.Published}
	if body.CheckpointContainsLatest != nil {
		var value *bool
		if err := json.Unmarshal(body.CheckpointContainsLatest, &value); err != nil || value == nil {
			return e, workersvc.ErrIterationParkEvidence
		}
		e.CheckpointContainsLatest = value
	}
	return e, e.Validate()
}

// WorkerRunIterationPark returns only the bounded claim ACK. Every validated
// refusal is a conflict; it must never be interpreted as a request to redrive.
func (h *Handler) WorkerRunIterationPark(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	evidence, err := decodeIterationPark(r)
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid iteration park evidence")
		return
	}
	run, applied, reason, err := h.wsvc.ReportIterationPark(r.Context(), wkr, id, evidence)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker iteration park", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	code := http.StatusOK
	if !applied {
		code = http.StatusConflict
	}
	httpx.JSON(w, code, struct {
		Applied         bool   `json:"applied"`
		Status          string `json:"status"`
		ClaimGeneration int64  `json:"claim_generation"`
		HoldReason      string `json:"hold_reason"`
		Reason          string `json:"reason,omitempty"`
	}{applied, run.Status, run.ClaimGeneration, run.HoldReason.String, reason})
}
