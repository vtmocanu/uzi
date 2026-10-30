package handler

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func forgeOriginCooldownDTO(origin string, until time.Time, halfOpen string, reporting, reserved bool, now time.Time) apitypes.ForgeOriginCooldownDTO {
	d := apitypes.ForgeOriginCooldownDTO{Origin: origin, CooledUntil: until}
	switch {
	case halfOpen != "":
		d.HalfOpenRunID = &halfOpen
		if reporting {
			d.State = "half_open"
			d.Detail = "One run is checking this origin; recovery is unverified."
		} else {
			d.State = "legacy_serialized_until_upgrade"
			d.Detail = "One run is checking this origin; retries stay serialized until it finishes. Upgrade the worker for direct recovery reporting."
		}
	case reserved:
		d.State = "reserved_unverified"
		d.Detail = "An origin retry remains reserved; its current run could not be verified. Recovery is unverified."
	case now.Before(until):
		d.State = "cooling"
		d.Detail = "Retries for this origin are cooling down on this worker; recovery is unverified."
	default:
		d.State = "retry_eligible_unverified"
		d.Detail = "Cooldown expired; a retry is eligible. Recovery is unverified."
	}
	return d
}

// Each overlay makes one owner-scoped read over the response IDs, with no retry.
// Failure leaves optional fields absent and does not affect sibling decorations.
func (h *Handler) workerForgeOriginCooldowns(ctx context.Context, ids []uuid.UUID) map[uuid.UUID][]apitypes.ForgeOriginCooldownDTO {
	viewer, ok := mw.UserFromContext(ctx)
	if !ok || h.q == nil || len(ids) == 0 {
		return nil
	}
	rows, err := h.q.ListOwnerWorkerForgeOriginCooldowns(ctx, store.ListOwnerWorkerForgeOriginCooldownsParams{UserID: viewer.ID, WorkerIds: ids})
	if err != nil {
		return nil
	}
	out := make(map[uuid.UUID][]apitypes.ForgeOriginCooldownDTO)
	now := h.clock()
	for _, row := range rows {
		out[row.WorkerID] = append(out[row.WorkerID], forgeOriginCooldownDTO(row.Origin, row.CooledUntil.Time, row.HalfOpenRunID, row.SuccessReporting, row.Reserved, now))
	}
	return out
}

func (h *Handler) overlayRunForgeOriginCooldowns(ctx context.Context, dtos ...*apitypes.RunDTO) {
	viewer, ok := mw.UserFromContext(ctx)
	if !ok || h.q == nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(dtos))
	byID := make(map[uuid.UUID][]*apitypes.RunDTO)
	for _, d := range dtos {
		if d == nil {
			continue
		}
		d.ForgeOriginCooldown = nil
		id, err := uuid.Parse(d.ID)
		if err != nil {
			continue
		}
		if _, seen := byID[id]; !seen {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], d)
	}
	if len(ids) == 0 {
		return
	}
	rows, err := h.q.ListOwnerRunForgeOriginCooldowns(ctx, store.ListOwnerRunForgeOriginCooldownsParams{UserID: viewer.ID, RunIds: ids})
	if err != nil {
		return
	}
	now := h.clock()
	for _, row := range rows {
		d := forgeOriginCooldownDTO(row.Origin, row.CooledUntil.Time, row.HalfOpenRunID, row.SuccessReporting, row.Reserved, now)
		for _, target := range byID[row.RunID] {
			target.ForgeOriginCooldown = &d
		}
	}
}
