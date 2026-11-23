package handler

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// weeklyLimitSource is what the handler needs of weeklylimit.Loader; the interface is also the unit-test seam.
type weeklyLimitSource interface {
	ForRuns(ctx context.Context, ownerID uuid.UUID, runs []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO
}

// SetWeeklyLimit wires the weekly-limit loader (PRD #4123). Unset (or a nil loader) leaves every run
// without a weekly_limit block.
func (h *Handler) SetWeeklyLimit(l weeklyLimitSource) {
	if l == nil {
		h.weeklyLimit = nil
		return
	}
	h.weeklyLimit = l
}

// weeklyLimitForViewer is the block for the viewer's OWN runs only: it names credentials and
// alternatives, so, like harnessDecisionForViewer, it is never built for another user's run, admin or not.
func (h *Handler) weeklyLimitForViewer(ctx context.Context, viewer store.User, runs []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO {
	if h.weeklyLimit == nil || viewer.ID == uuid.Nil {
		return nil
	}
	own := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		if r.UserID == viewer.ID {
			own = append(own, r)
		}
	}
	if len(own) == 0 {
		return nil
	}
	return h.weeklyLimit.ForRuns(ctx, viewer.ID, own)
}

// overlayWeeklyLimit sets weekly_limit on the viewer's own listed runs. rows are the run rows the items
// were built from (the DTOs do not carry the bindings the block's structural exclusion reads).
func (h *Handler) overlayWeeklyLimit(r *http.Request, viewer store.User, rows []store.Run, items []apitypes.RunListItemDTO) {
	blocks := h.weeklyLimitForViewer(r.Context(), viewer, rows)
	if len(blocks) == 0 {
		return
	}
	for i := range items {
		id, err := uuid.Parse(items[i].ID)
		if err != nil {
			continue
		}
		items[i].WeeklyLimit = blocks[id]
	}
}
