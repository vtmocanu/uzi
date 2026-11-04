package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CheckPublishOwnership is the read-only pre-body gate of the live checkpoint publish route
// (handler.WorkerRunPublish): it answers ErrRunNotOwned unless the run is held by wkr, before the
// handler takes an admission slot, extends the request's deadlines or reads the pack body. Without
// it a worker could hold the shared slots with run ids it does not own, since only
// PublishAttested checks ownership. It reads ownership alone (the claim generation is left to
// PublishAttested).
//
// It is a TOCTOU pre-check: the answer can be stale by the time the body has been read.
// PublishAttested's own ownership and claim-generation checks stay authoritative.
func (s *Service) CheckPublishOwnership(ctx context.Context, wkr store.Worker, runID uuid.UUID) error {
	_, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunNotOwned
	}
	return err
}
