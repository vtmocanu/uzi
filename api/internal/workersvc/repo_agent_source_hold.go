package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	HoldReasonRepoAgentSourceUnavailable = "repo_agent_source_unavailable"
	RepoAgentSourceFeature               = "repo_agent_source_v1"
	PlanAgentSourceFeature               = "plan_agent_source_v1"
)

func (s *Service) SetRunRepoAgentSourceHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, commit string) (store.Run, bool, error) {
	if generation <= 0 {
		return store.Run{}, false, fmt.Errorf("%w: positive claim_generation required", ErrInvalidState)
	}
	if commit != "" && !repoAgentSourceCommitRE.MatchString(commit) {
		return store.Run{}, false, fmt.Errorf("%w: repo_agent_source_commit must be a full lowercase commit id", ErrInvalidState)
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
		ID:                    runID,
		WorkerID:              pgconv.UUID(wkr.ID),
		ClaimGeneration:       generation,
		RepoAgentSourceCommit: pgconv.TextOrNull(commit),
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
	if applied && commit != "" && run.RepoAgentSourceCommit.String != commit {
		slog.Warn("workersvc: repo-agent source hold kept the stored revision",
			"run_id", runID, "stored", run.RepoAgentSourceCommit.String, "reported", commit)
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, false, err
		}
	}
	return run, applied, nil
}
