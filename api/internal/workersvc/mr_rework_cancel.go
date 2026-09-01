package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CancelReworkForMR aborts the active mr_rework run for (repoID, mrIID), if any,
// when its MR has left the opened state (merged/closed) — issue #853. It reuses the
// operator-cancel decision (submitInput's cancel branch, service.go) so a LIVE worker
// is actually stopped: a raw status flip would enqueue nothing into run_user_inputs and
// the worker would keep spending. No active rework → no-op (nil).
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
			return nil // no active rework for this MR — nothing to cancel
		}
		return err
	}
	live, err := s.hasLivePoller(ctx, run)
	if err != nil {
		return err
	}
	if live {
		// Live worker: enqueue a cancel verdict AND stamp stop_kind in one statement.
		// The worker consumes it, reports failed, and SetState routes off stop_kind
		// to CancelRunByWorker, finalizing to 'cancelled' (kind-agnostic).
		_, err = s.q.CreateStopVerdictInput(ctx, store.CreateStopVerdictInputParams{
			RunID:      run.ID,
			Kind:       "cancel",
			Body:       pgconv.TextOrNull(reason),
			StopKind:   pgconv.TextOrNull("cancelled"),
			StopReason: pgconv.TextOrNull(reason),
		})
		return err
	}
	// No live poller: server-side flip (owner-scoped by the run's own user_id).
	if _, err = s.q.CancelRunServerSide(ctx, store.CancelRunServerSideParams{
		ID:         run.ID,
		UserID:     run.UserID,
		StopReason: pgconv.TextOrNull(reason),
	}); err != nil {
		return err
	}
	if s.bcast != nil { // bcast is optional/post-construction — nil-guard required
		s.bcast.PublishState(run.ID, "cancelled")
	}
	s.notify(run.ID, "cancelled")
	s.maybeEnqueueJudgeByID(ctx, run.ID) // cancel is judge-filtered → no-op, but mirrors the operator path
	return nil
}
