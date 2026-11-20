package handler

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// overlayInjectedEnv sets RunDTO.InjectedEnv on the single-run read: the names the run's latest claim
// delivered (PRD #4416 M2). It belongs to the owner only; GetRunForViewer admits admins, who get none.
// It reads receipts and opens nothing. Best-effort: an error is logged without values and the field
// stays absent.
func (h *Handler) overlayInjectedEnv(ctx context.Context, viewerID uuid.UUID, run store.Run, dto *apitypes.RunDTO) {
	if run.UserID != viewerID || run.ClaimGeneration <= 0 || h.q == nil {
		return
	}
	rows, err := h.q.ListRunInjectedEnv(ctx, store.ListRunInjectedEnvParams{RunID: run.ID, ClaimGeneration: run.ClaimGeneration})
	if err != nil {
		slog.Error("run injected env", "run_id", run.ID, "error", err)
		return
	}
	for _, row := range rows {
		dto.InjectedEnv = append(dto.InjectedEnv, apitypes.InjectedEnvDTO{Name: row.Name, Scope: row.Scope, Secret: row.Secret})
	}
}
