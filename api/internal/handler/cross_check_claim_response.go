package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Bound the checker body by its decoded kind before adding the transport marker.
// The service also checks this envelope before its fenced claim delivery commits.
func writeWorkerRunClaim(w http.ResponseWriter, payload *workersvc.ClaimPayload) {
	if payload != nil && payload.Kind != runkind.CrossCheck {
		httpx.JSON(w, http.StatusOK, payload)
		return
	}
	raw, err := workersvc.MarshalCrossCheckClaim(payload)
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "checker claim refused")
		return
	}
	w.Header().Set("X-Uzi-Claim-Kind", "cross_check")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(raw); err != nil {
		// The response may already be committed. Never log its credential envelope.
		slog.Error("write checker claim response", "error", err)
	}
}
