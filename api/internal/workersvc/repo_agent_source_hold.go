package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	HoldReasonRepoAgentSourceUnavailable = "repo_agent_source_unavailable"
	RepoAgentSourceFeature               = "repo_agent_source_v1"
)

func (s *Service) SetRunRepoAgentSourceHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) (store.Run, bool, error) {
	if generation <= 0 {
		return store.Run{}, false, fmt.Errorf("%w: positive claim_generation required", ErrInvalidState)
	}
	gen := generation
	locked, _, tx, err := s.beginClaimMutation(ctx, wkr, runID, &gen)
	if err != nil {
		return store.Run{}, false, err
	}
	if tx != nil {
		defer func() { _ = tx.Rollback(ctx) }()
	}
	run, err := locked.q.SetRunRepoAgentSourceHold(ctx, store.SetRunRepoAgentSourceHoldParams{
		ID:              runID,
		WorkerID:        pgconv.UUID(wkr.ID),
		ClaimGeneration: generation,
	})
	applied := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		run, err = locked.runOwnedByWorker(ctx, runID, wkr)
		if err != nil {
			return store.Run{}, false, err
		}
		applied = run.Status == "paused" && run.HoldReason.Valid &&
			run.HoldReason.String == HoldReasonRepoAgentSourceUnavailable &&
			run.ClaimGeneration == generation && !run.ClaimReleasedAt.Valid
	} else if err != nil {
		return store.Run{}, false, err
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, false, err
		}
	}
	return run, applied, nil
}
