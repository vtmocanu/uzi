package workersvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// parkRunningCodexAccountUnavailable retries only NOWAIT contention, at most
// finishRunClaimAttempts times. Exhaustion uses a fresh transaction without alias/account
// locks. The run lock itself obeys the request context; all writes use its transaction.
// No transition settles custody.
func (s *Service) parkRunningCodexAccountUnavailable(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, session pgtype.Text) (store.Run, int64, error) {
	if req.ClaimGeneration == nil {
		return store.Run{}, 0, fmt.Errorf("%w: account park requires claim_generation", ErrInvalidState)
	}
	if s.txBeginner == nil {
		return store.Run{}, 0, errors.New("account park unavailable: no tx beginner")
	}
	for attempt := 0; attempt < finishRunClaimAttempts; attempt++ {
		run, rows, err := s.parkRunningCodexAccountTx(ctx, wkr, owned, req, session, true)
		if !isLockNotAvailable(err) {
			return run, rows, err
		}
		if attempt+1 < finishRunClaimAttempts {
			timer := time.NewTimer(finishRunClaimRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return run, 0, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return s.parkRunningCodexAccountTx(ctx, wkr, owned, req, session, false)
}

func (s *Service) parkRunningCodexAccountTx(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, session pgtype.Text, classify bool) (store.Run, int64, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	workerID := pgconv.UUID(wkr.ID)
	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Run{}, 0, ErrRunNotOwned
	}
	if err != nil {
		return store.Run{}, 0, err
	}
	if run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return run, 0, ErrStaleClaim
	}
	if run.Status != "running" {
		return run, 0, nil
	}
	var rows int64
	if run.StopKind.Valid && run.StopKind.String != "" {
		rows, err = qtx.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: run.ID, WorkerID: workerID})
	} else {
		hold := false
		if classify {
			hold, _, err = classifyLockedCodexClaim(ctx, qtx, run)
			if err != nil {
				return run, 0, err
			}
		}
		hold = hold && run.Harness == harnessCodex &&
			run.CodexAuthMode.Valid && run.CodexAuthMode.String == codexAuthModeSubscription &&
			runClaimOpenedCustody(run, true)
		if hold {
			rows, err = qtx.ParkRunningCodexAccountUnavailable(ctx, store.ParkRunningCodexAccountUnavailableParams{
				ID: run.ID, WorkerID: workerID, ClaimGeneration: *req.ClaimGeneration, SessionID: session,
			})
		} else {
			rows, err = s.setRecoveryWait(ctx, qtx, pgtype.Text{}, run, wkr, req, session)
		}
	}
	if err != nil || rows == 0 {
		return run, rows, err
	}
	if err := tx.Commit(ctx); err != nil {
		return run, 0, err
	}
	return run, rows, nil
}
