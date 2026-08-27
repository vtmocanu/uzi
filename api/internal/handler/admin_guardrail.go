package handler

import (
	"log/slog"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
)

// AdminGuardrailImpact is the admin-only, read-only pre-flight impact count for
// PRD #66 (M3): a LIVE scan that reports how many enabled repos would be refused
// under the new guardrail (the bot can push/merge to the default branch). It
// PERSISTS NOTHING — GuardrailImpact re-sweeps the forge and writes no report,
// because M3 measures the blast radius before M1's migration NULLs the stored
// reports (R1/R2), and a jsonb query over those reports would miss protected-but
// -bot-mergeable repos and read INTERVAL=0's empty blob as "zero affected".
//
// Admin-only: it fans out across EVERY user's forge connections, so it is mounted
// in the admin READ group and, because it is a live 1 + 2×repos forge scan, wears
// the per-user forge limiter the same way /{id}/privilege-check does.
func (h *Handler) AdminGuardrailImpact(w http.ResponseWriter, r *http.Request) {
	report, err := h.pcheck.GuardrailImpact(r.Context())
	if err != nil {
		slog.Error("guardrail impact scan", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, report)
}
