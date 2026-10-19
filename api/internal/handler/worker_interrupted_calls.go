package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// interruptedCallsDeadline also bounds the existing job-refusal read preceding
// this route. The service independently bounds its authoritative snapshot read.
func interruptedCallsDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WorkerInterruptedCalls exposes advisory evidence, never execution authority.
func (h *Handler) WorkerInterruptedCalls(w http.ResponseWriter, r *http.Request) {
	worker, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	id, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	gen, err := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if err != nil || gen < 2 {
		httpx.Error(w, http.StatusBadRequest, "invalid claim generation")
		return
	}
	raw, err := h.wsvc.InterruptedCalls(r.Context(), worker, id, r.Header.Get("X-Worker-Register-Nonce"), gen)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, workersvc.ErrInvalidState) {
			status = http.StatusBadRequest
		}
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			status = http.StatusNotFound
		}
		httpx.Error(w, status, "interrupted calls unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, json.RawMessage(raw))
}
