package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// overlayPlanCrossCheckSummary belongs only on authorized single-run detail.
// GetRunForViewer permits admins; this metadata remains exclusive to the owner.
func (h *Handler) overlayPlanCrossCheckSummary(ctx context.Context, viewerID uuid.UUID, run store.Run, dto *apitypes.RunDTO) {
	if run.UserID != viewerID {
		return
	}
	summary, err := h.wsvc.PlanCrossCheckSummary(ctx, viewerID, run.ID)
	if err != nil {
		// Do not log query error text: a malformed legacy row can contain prose.
		slog.Warn("resolve plan cross-check summary", "run_id", run.ID)
	} else {
		dto.PlanCrossCheckSummary = summary
	}
	// Old plan-only responses and consent-off runs need no code lookup.
	if !run.CodeCrossCheckRequired {
		return
	}
	cc, err := h.q.GetCodeCrossCheckForOwner(ctx, store.GetCodeCrossCheckForOwnerParams{LeadRunID: run.ID, UserID: viewerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Warn("resolve code cross-check summary", "run_id", run.ID)
		return
	}
	response := codeCrossCheckResponse(cc)
	dto.CodeCrossCheckSummary = &response
}
