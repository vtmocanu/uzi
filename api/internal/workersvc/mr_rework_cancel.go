package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// CancelReworkForMR aborts the active mr_rework run for (repoID, mrIID), if any,
// when its MR has left the opened state (merged/closed) — issue #853. It runs the fenced
// terminal-target cancel (cancelTerminalTarget, issue #4616): a LIVE worker gets a stamped
// cancel input it drains, any other non-terminal status is cancelled server-side, and a run
// that already has a stop_kind, holds a pending terminal outcome or moved since the lookup is
// left untouched (nil). reason is recorded verbatim (sanitized). No active rework → nil.
// A missing transaction beginner or a store failure is an error.
//
// GetActiveMRReworkRunForMR is :one, safe because its WHERE matches the
// uq_runs_one_active_mr_rework partial unique index (migration 00167): at most one
// non-terminal mr_rework run exists per (repo, MR).
func (s *Service) CancelReworkForMR(ctx context.Context, repoID uuid.UUID, mrIID int64, reason string) error {
	run, err := s.q.GetActiveMRReworkRunForMR(ctx, store.GetActiveMRReworkRunForMRParams{
		RepoID: repoID,
		MrIid:  pgtype.Int8{Int64: mrIID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // no active rework for this MR: nothing to cancel
		}
		return err
	}
	// The watcher identified the MR by IID alone, so the observation is built from the looked-up
	// row (its own pipeline_ref is the source branch) and the fenced cancel re-validates it under
	// row locks: a run reclaimed, stopped by an operator or holding a pending outcome since the
	// lookup is left untouched. A live worker gets the cancel input; any other non-terminal status
	// (parked included) is cancelled server-side.
	return s.cancelTerminalTarget(ctx, TerminalTargetObservation{
		RunID: run.ID, RepoID: repoID, Kind: run.Kind, Status: run.Status, WorkerID: run.WorkerID,
		ClaimGeneration: run.ClaimGeneration, TargetMRIID: mrIID, SourceBranch: run.PipelineRef.String,
	}, terminalTargetPolicyLegacyRework, reason)
}
