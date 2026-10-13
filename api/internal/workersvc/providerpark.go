package workersvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Provider-transient park (PRD #1733): the server's half of a typed recovery_cause
// 'provider_outage' report. A provider error the harness classed as transient parks the run on
// 'recovery_wait' exactly as before, but the park is now COUNTED in an unbroken STREAK
// (runs.provider_park_count / provider_park_streak_started_at) and the streak is BOUNDED in
// elapsed time: the first provider park at or past RunProviderParkMaxDuration since the streak
// began becomes a paused owner hold ('provider_unavailable') instead of another retry. The policy
// never fails a run.

// validateProviderPark enforces StateRequest.ProductiveTurns' scope and the typed park's fence
// requirement (PRD #1733). Every refusal is ErrInvalidState (400 "state must be one of ..."),
// never a decode error, so a worker can tell a new api's validation refusal from an older api's
// strict-decode refusal of the unknown productive_turns field and re-send untyped only for the
// latter. It runs with SetState's other cause validation, before any SQL.
func validateProviderPark(req StateRequest) error {
	if req.ProviderProgressTurns != nil {
		allowed := req.RecoveryCause == nil ||
			*req.RecoveryCause == "empty_turn" || *req.RecoveryCause == recoveryCauseDataVolumeFull
		if req.State != "recovery_wait" || !allowed || *req.ProviderProgressTurns < 0 ||
			req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
			return fmt.Errorf("%w: provider_progress_turns requires a non-negative count and positive generation on an ordinary or disk recovery park", ErrInvalidState)
		}
	}
	typed := req.State == "recovery_wait" && req.RecoveryCause != nil && *req.RecoveryCause == recoveryCauseProviderOutage
	if req.ProductiveTurns != nil {
		if !typed {
			return fmt.Errorf("%w: productive_turns requires status recovery_wait with recovery_cause %q", ErrInvalidState, recoveryCauseProviderOutage)
		}
		if *req.ProductiveTurns < 0 {
			return fmt.Errorf("%w: productive_turns must not be negative", ErrInvalidState)
		}
	}
	// Typed accounting needs a positive generation: the nil-generation compatibility path must
	// never become authority for a new streak or a progress reset (generation 0 is a chat run).
	if typed && (req.ClaimGeneration == nil || *req.ClaimGeneration <= 0) {
		return fmt.Errorf("%w: provider_outage park requires a positive claim_generation", ErrInvalidState)
	}
	return nil
}

// nextProviderStreak computes the streak a provider park leaves, from the LOCKED row and the
// service clock only. A new streak (count 1, started now) opens when none is open or the parking
// flight reported productive turns (progress breaks the streak); otherwise the open streak
// continues with count + 1, saturating below the int32 column maximum.
func nextProviderStreak(run store.Run, productive bool, now time.Time) (int32, time.Time) {
	if !run.ProviderParkStreakStartedAt.Valid || productive {
		return 1, now
	}
	count := run.ProviderParkCount
	if count < math.MaxInt32 {
		count++
	}
	return count, run.ProviderParkStreakStartedAt.Time
}

// parkProviderOutage is SetState's typed provider-park transaction (PRD #1733). It returns
// (run, rows, held, err) in the parkDataVolumeFull shape plus whether the applied transition was
// the owner hold:
//
//   - rows == 1, err == nil: an APPLIED transition (park, hold, or cancel). held distinguishes the
//     hold ('paused') from the ordinary 'recovery_wait' park. SetState then re-reads the run and
//     runs the shared post-switch fan-out.
//   - rows == 0, err == nil: a NO-OP the caller maps to 409 {run}: the run is no longer 'running'
//     (which covers the idempotent duplicate of an applied park) or a SQL guard did not match.
//   - err == ErrStaleClaim: the report's generation is not the locked run's, or the claim was
//     released. Nothing is mutated and the counters do not move.
//
// The transaction owns its lock (stateUsesForUpdateFence is false for recovery_wait): the run row
// is locked FOR UPDATE, the generation checked FIRST, then the not-running no-op, then a stamped
// stop, then the streak decision. A stamped stop verdict wins over the park, exactly as in
// parkDataVolumeFull: the run ends 'cancelled' through CancelRunByWorker and is never held.
//
// The hold keeps the claim (claim_released_at stays NULL, worker_id and worker_active_runs are
// kept) so a verified but unpublished capture that exists only on that worker survives, and it
// settles every pending owner pause input in the same statement (SetRunProviderHold). All time
// comes from s.now() so a test crosses the bound without sleeping.
func (s *Service) parkProviderOutage(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, sessionID pgtype.Text) (store.Run, int64, bool, error) {
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		// validateProviderPark refuses this earlier; kept so the arm is safe on its own.
		return store.Run{}, 0, false, fmt.Errorf("%w: provider_outage park requires a positive claim_generation", ErrInvalidState)
	}
	if s.txBeginner == nil {
		// The streak decision reads the counter under the row lock, which needs a transaction.
		// Production always wires one; refuse (500) rather than park uncounted, so the worker
		// keeps the run and its custody and retries the report.
		return store.Run{}, 0, false, fmt.Errorf("provider_outage park unavailable: no tx beginner wired for run %s", owned.ID)
	}

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, 0, false, err
	}
	// A no-op after a successful Commit; on every early return it releases the FOR UPDATE lock.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	workerID := pgconv.UUID(wkr.ID)
	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, 0, false, ErrRunNotOwned
		}
		return store.Run{}, 0, false, err
	}

	// Precedence 1: stale claim, nothing mutated (#1247).
	if run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return run, 0, false, ErrStaleClaim
	}
	// Precedence 2: a report onto a run that is no longer 'running' is a plain no-op, including
	// the duplicate of an already-applied park (a lost ack), which therefore never counts twice.
	if run.Status != "running" {
		return run, 0, false, nil
	}
	// Precedence 3: a stamped stop verdict is a deliberate wind-down, not an outage to retry or
	// hold: cancel through the worker path, never park.
	if run.StopKind.Valid && run.StopKind.String != "" {
		n, cerr := qtx.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: run.ID, WorkerID: workerID})
		if cerr != nil {
			return store.Run{}, 0, false, cerr
		}
		if n != 1 {
			return run, 0, false, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, 0, false, err
		}
		return run, 1, false, nil
	}

	now := s.now()
	productive := req.ProductiveTurns != nil && *req.ProductiveTurns > 0
	count, started := nextProviderStreak(run, productive, now)
	startedTS := pgconv.Time(started)

	if bound := s.p.RunProviderParkMaxDuration; bound > 0 && now.Sub(started) >= bound {
		held, herr := qtx.SetRunProviderHold(ctx, store.SetRunProviderHoldParams{
			SessionID:                   sessionID,
			ProviderParkCount:           count,
			ProviderParkStreakStartedAt: startedTS,
			ID:                          run.ID,
			WorkerID:                    workerID,
			ClaimGeneration:             *req.ClaimGeneration,
		})
		if herr != nil {
			if errors.Is(herr, pgx.ErrNoRows) {
				// A guard did not match under the lock (the kind is not parkable): no-op.
				return run, 0, false, nil
			}
			return store.Run{}, 0, false, herr
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, 0, false, err
		}
		return held, 1, true, nil
	}

	retryNotBefore := now.Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter())
	parked, perr := qtx.SetRunProviderRecoveryWait(ctx, store.SetRunProviderRecoveryWaitParams{
		RetryNotBefore:              pgconv.Time(retryNotBefore),
		ProviderParkCount:           count,
		ProviderParkStreakStartedAt: startedTS,
		SessionID:                   sessionID,
		ID:                          run.ID,
		WorkerID:                    workerID,
		ClaimGeneration:             *req.ClaimGeneration,
	})
	if perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return run, 0, false, nil
		}
		return store.Run{}, 0, false, perr
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, 0, false, err
	}
	return parked, 1, false, nil
}
