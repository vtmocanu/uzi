package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// shortObjectID shortens a git object id for display.
func shortObjectID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// overlayRunPublicationIntents sets the publication park diagnostics and the intent summary on
// runs that hold an active publication intent (issue #1324 D10). Best-effort and display only:
// one read for the whole page, and a failed read leaves the fields absent. A run without an
// active intent carries none of them.
func (h *Handler) overlayRunPublicationIntents(ctx context.Context, dtos ...*apitypes.RunDTO) {
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
	rows, err := h.q.ListOwnerRunPublicationIntents(ctx, store.ListOwnerRunPublicationIntentsParams{UserID: viewer.ID, RunIds: ids})
	if err != nil {
		return
	}
	var limit *int
	if bound := h.cfg.RunProviderParkMaxDuration; bound > 0 {
		secs := int(bound / time.Second)
		limit = &secs
	}
	for _, row := range rows {
		parkTotal, episode := int(row.ParkTotal), int(row.Episode)
		for _, d := range byID[row.RunID] {
			d.PublicationWaitStartedAt = timePtr(row.WaitStartedAt.Valid, row.WaitStartedAt.Time)
			d.PublicationWaitLimitSeconds = limit
			d.PublicationHoldReason = textPtrValue(row.HoldReason.Valid, row.HoldReason.String)
			d.PublicationFirstWaitAt = timePtr(row.FirstWaitAt.Valid, row.FirstWaitAt.Time)
			d.PublicationParkTotal = &parkTotal
			d.PublicationEpisode = &episode
			intent := &apitypes.PublicationIntentDTO{
				Branch: row.Branch, OriginalHead: shortObjectID(row.OriginalHead),
				MrIID: d.MrIID, State: row.State, EnrollmentSeq: int(row.EnrollmentSeq),
			}
			if row.Candidate != "" {
				c := shortObjectID(row.Candidate)
				intent.Candidate = &c
			}
			d.PublicationIntent = intent
		}
	}
}
