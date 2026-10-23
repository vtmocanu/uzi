package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WorkerRunPublicationContext streams at most 4KiB, then accepts one strict object.
func (h *Handler) WorkerRunPublicationContext(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req workersvc.PublicationContextRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, 4096); err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "request too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.Valid() {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result := h.wsvc.ResolvePublicationContext(r.Context(), worker, id, req)
	data, err := json.Marshal(result)
	if err != nil || len(data) > 16384 {
		data = []byte(`{"version":1,"status":"unknown","reason":"authority_unknown"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, bytes.NewReader(data))
}
