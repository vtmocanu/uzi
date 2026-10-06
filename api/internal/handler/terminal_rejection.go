package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const terminalRejectionMaxBytes = 128 << 10

func (h *Handler) WorkerTerminalRejections(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	var req struct {
		Rejections []workersvc.TerminalRejection `json:"rejections"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, terminalRejectionMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF || req.Rejections == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := workersvc.ValidateTerminalRejections(req.Rejections); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid terminal rejection")
		return
	}
	out, err := h.wsvc.ReportTerminalRejections(r.Context(), wkr, req.Rejections)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > terminalRejectionMaxBytes {
		httpx.Error(w, http.StatusInternalServerError, "rejection response unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *Handler) WorkerTerminalRejectionCustody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	rawID := chi.URLParam(r, "id")
	runID, err := uuid.Parse(rawID)
	if err != nil || runID.String() != rawID {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	values := r.URL.Query()["generation"]
	if len(values) != 1 {
		httpx.Error(w, http.StatusBadRequest, "invalid generation")
		return
	}
	generation, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || generation < 0 || generation > workersvc.MaxTerminalRejectionGeneration {
		httpx.Error(w, http.StatusBadRequest, "invalid generation")
		return
	}
	out, err := h.wsvc.TerminalRejectionCustody(r.Context(), wkr, runID, generation)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > terminalRejectionMaxBytes {
		httpx.Error(w, http.StatusInternalServerError, "custody response unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
