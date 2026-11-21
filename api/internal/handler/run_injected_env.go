package handler

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// overlayInjectedEnv adds the names (never values) of the run environment variables the run's
// current claim generation delivered, from the delivery receipts. It belongs only on
// authorized single-run detail and only to the run's owner: GetRunForViewer permits admins,
// and an admin who is not the owner sees nothing. It opens no ciphertext and no vault key.
// A lookup error is logged and leaves the field absent.
func (h *Handler) overlayInjectedEnv(ctx context.Context, viewerID uuid.UUID, run store.Run, dto *apitypes.RunDTO) {
	if run.UserID != viewerID {
		return
	}
	injected, err := h.wsvc.InjectedRunEnv(ctx, run.ID)
	if err != nil {
		slog.Warn("resolve injected run environment", "run_id", run.ID)
		return
	}
	dto.InjectedEnv = injected
}
