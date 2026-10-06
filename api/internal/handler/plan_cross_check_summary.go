package handler

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
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
		return
	}
	dto.PlanCrossCheckSummary = summary
}
