package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runprogress"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// EffectiveNowSummary reports whether the model-written Now summary is on for a run OWNED by
// ownerID (PRD #2603): the instance switch (settings.KeyNowSummaryEnabled, default on) AND the
// owner's own switch (users.now_summary_enabled, NULL reads as on). Pass the RUN OWNER's id,
// never the viewer's: an admin reads other users' runs, and a viewer's preference must neither
// show nor hide a note on someone else's run.
//
// A read error is returned and callers treat it as off (a summary is optional, so it fails
// closed). A missing owner row is off.
func (h *Handler) EffectiveNowSummary(ctx context.Context, ownerID uuid.UUID) (bool, error) {
	if h.settings == nil {
		return false, nil
	}
	on, err := h.settings.NowSummaryEnabled(ctx)
	if err != nil || !on {
		return false, err
	}
	v, err := h.q.GetUserNowSummaryEnabled(ctx, ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return !v.Valid || v.Bool, nil
}

// overlayNowNote attaches RunProgress.NowNote (PRD #2603) on the GetRun response only, and only
// when the run is eligible right now: progress.state is percent (an executing, healthy issue
// run with a non-empty frozen list, so a waiting, parked, queued or stalled run never shows a
// note, even one written for its still-active milestone), the active milestone id is
// non-empty, and the effective setting is on for the run owner. The query is bound to the
// active milestone id, so a note written for an earlier milestone is never shown. Best-effort:
// an error is logged and the field stays absent.
func (h *Handler) overlayNowNote(ctx context.Context, run store.Run, dto *apitypes.RunDTO) {
	p := dto.Progress
	if p == nil || p.State != runprogress.StatePercent || p.ActiveMilestoneID == "" {
		return
	}
	on, err := h.EffectiveNowSummary(ctx, run.UserID)
	if err != nil {
		slog.Error("now summary setting", "run_id", run.ID, "error", err)
		return
	}
	if !on {
		return
	}
	row, err := h.q.GetLatestProgressNote(ctx, store.GetLatestProgressNoteParams{RunID: run.ID, MilestoneID: p.ActiveMilestoneID})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("latest progress note", "run_id", run.ID, "error", err)
		}
		return
	}
	// Defence in depth: the text was normalised on ingest, but rows are read back through the
	// same rule so a row written by an older or buggy api can never reach a surface unclean.
	text := workersvc.SanitizeProgressNoteText(row.Text)
	if text == "" || !row.CreatedAt.Valid {
		return
	}
	p.NowNote = &apitypes.ProgressNote{Text: text, At: row.CreatedAt.Time}
}
