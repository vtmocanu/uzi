package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// salvageStatePromoted is the run_salvage state in which the run-scoped salvage copy exists
// on the forge and has not expired (PRD #1867).
const salvageStatePromoted = "promoted"

// overlayRunSalvage sets the salvage_* fields on the single-run detail read of a failed run
// (PRD #1867 M4). runToDTO stays pure, so the lookup happens here, beside the landing_state
// overlay. Best-effort: a lookup error is logged and leaves the fields null, never failing
// the read of an otherwise-fine run; a run with no salvage row leaves them null too.
func (h *Handler) overlayRunSalvage(ctx context.Context, run store.Run, dto *apitypes.RunDTO) {
	if run.Status != "failed" {
		return
	}
	row, err := h.q.GetRunSalvage(ctx, run.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("resolve run salvage", "run_id", run.ID, "error", err)
	}
	applyRunSalvage(dto, row, err)
}

// applyRunSalvage is overlayRunSalvage's pure half: it maps a run_salvage row (or the lookup
// error) onto the DTO. Any error, pgx.ErrNoRows included, leaves every field untouched
// (null). salvage_ref is set only in the promoted state; the branch checkpoint ref is never
// surfaced, since its retention is #1810's, not salvage's.
func applyRunSalvage(dto *apitypes.RunDTO, row store.RunSalvage, lookupErr error) {
	if lookupErr != nil {
		return
	}
	state, tip := row.State, row.Tip
	dto.SalvageState = &state
	dto.SalvageTip = &tip
	if state == salvageStatePromoted {
		ref := "refs/uzi-salvage/" + row.RunID.String()
		dto.SalvageRef = &ref
	}
	if row.ExpiresAt.Valid {
		at := row.ExpiresAt.Time
		dto.SalvageExpiresAt = &at
	}
	if row.LastError.Valid {
		msg := row.LastError.String
		dto.SalvageLastError = &msg
	}
}
