package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func withoutAccountCooldownHealth(health string) string {
	if health == "waiting_account_cooldown" {
		return "ok"
	}
	return health
}

func accountCooldownFields(class pgtype.Text, at pgtype.Timestamptz) (*apitypes.AccountCooldownClass, *time.Time) {
	if !class.Valid || !at.Valid || at.InfinityModifier != pgtype.Finite || !at.Time.After(time.Now()) {
		return nil, nil
	}
	c := apitypes.AccountCooldownClass(class.String)
	if apitypes.AccountCooldownLine(c, at.Time) == "" {
		return nil, nil
	}
	return &c, &at.Time
}

// overlayAccountCooldowns reads only owner-authorized current evidence. Failed reads
// leave the metadata absent; runToDTO already removed the persisted cooldown signal.
func (h *Handler) overlayAccountCooldowns(ctx context.Context, dtos ...*apitypes.RunDTO) {
	if h.q == nil {
		return
	}
	ids := make([]uuid.UUID, 0, len(dtos))
	byID := make(map[uuid.UUID][]*apitypes.RunDTO)
	for _, d := range dtos {
		if d == nil {
			continue
		}
		if d.Status != "running" && d.Status != "claimed" && d.Status != "awaiting_approval" {
			continue
		}
		id, err := uuid.Parse(d.ID)
		if err != nil {
			continue
		}
		if _, found := byID[id]; !found {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], d)
	}
	if len(ids) == 0 {
		return
	}
	viewer, _ := mw.UserFromContext(ctx)
	rows, err := h.q.ListRunAccountCooldowns(ctx, store.ListRunAccountCooldownsParams{RunIds: ids, ViewerID: viewer.ID})
	if err != nil {
		slog.Error("run account cooldown projection", "error", err)
		return
	}
	for _, row := range rows {
		for _, d := range byID[row.RunID] {
			if d.Health != "ok" {
				continue
			}
			// A wall warning outranks the cooldown, including before the next health sweep.
			if d.DeadlineAt != nil && !d.DeadlineAt.After(time.Now()) {
				continue
			}
			d.AccountCooldownClass, d.AccountCooldownEligibleAt = accountCooldownFields(pgtype.Text{String: row.AccountCooldownClass, Valid: true}, row.AccountCooldownEligibleAt)
			if d.AccountCooldownClass != nil {
				d.Health = "waiting_account_cooldown"
				reason := apitypes.AccountCooldownReason
				d.HealthReason = &reason
				if d.Progress != nil {
					d.Progress.State = "waiting"
					d.Progress.Pct = nil
				}
			}
		}
	}
}

func (dto *latestRunDTO) overlayAccountCooldown(class pgtype.Text, at pgtype.Timestamptz) {
	if !dto.IsMine || dto.Health != "ok" {
		return
	}
	if dto.DeadlineAt != nil && !dto.DeadlineAt.After(time.Now()) {
		return
	}
	dto.AccountCooldownClass, dto.AccountCooldownEligibleAt = accountCooldownFields(class, at)
	if dto.AccountCooldownClass != nil {
		dto.Health = "waiting_account_cooldown"
		reason := apitypes.AccountCooldownReason
		dto.HealthReason = &reason
	}
}
