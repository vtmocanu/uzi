package handler

import (
	"context"
	"errors"
	"log/slog"
	"math"

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
	if cc.CheckerRunID.Valid {
		childID := uuid.UUID(cc.CheckerRunID.Bytes)
		child, childErr := h.q.GetRunByID(ctx, childID)
		// Usage is detail evidence only for the same owner's actual checker child.
		if childErr == nil && child.UserID == viewerID && child.Kind == "cross_check" &&
			child.TargetRunID.Valid && uuid.UUID(child.TargetRunID.Bytes) == run.ID {
			totals, usageErr := h.wsvc.RunUsageTotalsForRuns(ctx, []uuid.UUID{childID})
			if usageErr == nil {
				row, present := totals[childID]
				response.Usage = usageFromTotals(row, present)
				if u := response.Usage; u != nil {
					if u.CostStatus != "metered" || math.IsNaN(u.CostUSD) || math.IsInf(u.CostUSD, 0) || u.CostUSD < 0 {
						if u.CostStatus != "subscription" {
							u.CostStatus = "unreported"
						}
						u.CostUSD = 0
					}
				}
			} else {
				slog.Warn("resolve code cross-check usage", "run_id", run.ID)
			}
		}
	}
	dto.CodeCrossCheckSummary = &response
}
