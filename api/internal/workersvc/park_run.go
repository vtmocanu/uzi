package workersvc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RequestWorkerPause reuses the ordinary pause request under the claim's run-row
// lock. It leaves SetRunPaused and owner resume responsible for the transitions.
// A pending ordinary pause is accepted without creating another audit input;
// its mode is retained. All pending ordinary pause inputs are delivered by this
// response and consumed/applied in this transaction, never by later steering.
func (s *Service) RequestWorkerPause(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) (store.Run, bool, error) {
	if generation <= 0 {
		return store.Run{}, false, fmt.Errorf("%w: positive claim_generation required", ErrInvalidState)
	}
	if _, err := s.runOwnedByWorker(ctx, runID, wkr); err != nil {
		return store.Run{}, false, err
	}
	if s.txBeginner == nil {
		return store.Run{}, false, fmt.Errorf("worker pause transaction unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, false, ErrRunNotOwned
	}
	if err != nil {
		return store.Run{}, false, err
	}
	if !slices.Contains(wkr.ProtocolCapabilities, capability.ParkRunV1) ||
		run.ClaimGeneration != generation || run.ClaimReleasedAt.Valid ||
		run.Status != "running" || run.Kind != runkind.Issue || run.Interactive ||
		run.PauseMode.String == "wall" || run.StopKind.Valid || run.BudgetFinalizeSeconds > 0 ||
		PendingCredentialSwitchSignal(run) != nil {
		return run, false, nil
	}
	stopped, err := q.WorkerPauseHasStopInput(ctx, runID)
	if err != nil {
		return store.Run{}, false, err
	}
	if stopped {
		return run, false, nil
	}
	deadline := RunDeadline(run.StartedAt, run.BudgetWallSeconds, run.BudgetPausedSeconds, run.Kind, run.Interactive, run.Status, s.p.RunTimeout, run.BudgetExtensionSeconds, run.BudgetFinalizeSeconds)
	if deadline != nil && !s.now().Before(*deadline) {
		return run, false, ErrRunWallExpired
	}
	if !run.PlanMd.Valid || strings.TrimSpace(run.PlanMd.String) == "" {
		return run, false, nil
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil {
		return store.Run{}, false, err
	}
	if run.ScopeCeiling.Valid && int(run.ScopeCeiling.Int32) <= len(completed) {
		return run, false, nil
	}
	rc, err := q.GetRunClaimContext(ctx, runID)
	if err != nil {
		return store.Run{}, false, err
	}
	if !run.AutoApprove && !rc.HumanPlanApproved && run.PlanSource != planSourceSeeded {
		return run, false, nil
	}
	if !run.PauseRequestedAt.Valid {
		if _, err := q.CreateWorkerPauseInput(ctx, store.CreateWorkerPauseInputParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: generation}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return run, false, nil
			}
			return store.Run{}, false, err
		}
	}
	if err := q.ConsumeWorkerPauseInputs(ctx, store.ConsumeWorkerPauseInputsParams{RunID: runID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: pgconv.Int8Ptr(&generation)}); err != nil {
		return store.Run{}, false, err
	}
	run, err = q.GetRunByID(ctx, runID)
	if err != nil {
		return store.Run{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, false, err
	}
	return run, true, nil
}
