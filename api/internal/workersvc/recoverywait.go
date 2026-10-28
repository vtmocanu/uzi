package workersvc

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Ordinary recovery parks retry with capped backoff. Codex idle stalls also
// count a park-observed streak and hold for the owner after RunCodexIdleStallMaxParks.

// Recovery-park jitter bounds. Smaller than the usage-limit park's 60-180s because the
// recovery base is minutes-scale, not hours: this spreads a promoted wave of recovering
// runs across ticks so PromoteRecoveryWaitRuns — a SINGLE UPDATE that releases every
// eligible row in one tick — does not re-converge them, without dominating a one-minute
// base. The floor is non-zero so a park always defers at least a few seconds past `now`.
const (
	recoveryParkJitterMin = 5 * time.Second
	recoveryParkJitterMax = 30 * time.Second
)

// recoveryParkFallbackMaxShift bounds the doubling so it can never overflow a
// time.Duration (an int64 of NANOSECONDS).
//
// 🔴 UNLIKE limitParkFallbackMaxShift THIS GUARD IS LOAD-BEARING, NOT BELT-AND-BRACES.
// recovery_wait_count is UNBOUNDED — there is no lifetime cap, so a long-recovering run
// parks arbitrarily many times. Without this bound a large count shifts base past 63 bits
// and yields a NEGATIVE duration, i.e. a stamp in the past and a promote->re-park loop that
// spins the sweeper. The cap is reached at a small count for any sane base, so every value
// at or above this bound is capped anyway and the guard costs nothing.
const recoveryParkFallbackMaxShift = 20

// recoveryParkFallbackFor returns the backoff for a park, given how many times this run has
// ALREADY parked. Mirrors limitParkFallbackFor: the argument is the PRIOR count (0 on the
// first park), because SetRunRecoveryWait bumps recovery_wait_count in the same statement as
// the transition, so this code sees the value BEFORE the park.
//
// It is a capped exponential from the configured base (RunRecoveryParkBase), doubling per
// prior park, clamped at RunRecoveryMaxPark. It ALWAYS returns a finite positive duration —
// there is no terminal or hold branch. base/cap come from Params so an operator can tune the
// cadence (RUN_RECOVERY_PARK_BASE / RUN_RECOVERY_MAX_PARK).
//
//	priorParks:      0    1    2    3     4     5+
//	wait (1m / 30m): 1m   2m   4m   8m    16m   30m (capped)
func (s *Service) recoveryParkFallbackFor(priorParks int32) time.Duration {
	base, maxPark := s.p.RunRecoveryParkBase, s.p.RunRecoveryMaxPark
	if priorParks <= 0 {
		if maxPark > 0 && base > maxPark {
			return maxPark
		}
		return base
	}
	if priorParks >= recoveryParkFallbackMaxShift {
		return maxPark
	}
	d := base << uint(priorParks)
	// d < base catches an overflow wrap (a correct doubling is always >= base for a
	// non-negative shift); d > maxPark is the ordinary clamp.
	if d < base || (maxPark > 0 && d > maxPark) {
		return maxPark
	}
	return d
}

// recoveryParkJitter rolls one jitter value. The ONLY nondeterminism on the recovery-park
// path, isolated here so recoveryParkFallbackFor stays a pure function of the count and
// every case in its test is an exact-value assertion. math/rand rather than crypto/rand
// deliberately, exactly as limitParkJitter: this is stampede avoidance, not a secret.
func recoveryParkJitter() time.Duration {
	return recoveryParkJitterMin + time.Duration(rand.Int63n(int64(recoveryParkJitterMax-recoveryParkJitterMin+1))) //nolint:gosec // G404: retry jitter spreads promotion waves; not a security token
}

// setRecoveryWait opens the claim mutation transaction. Typed idle accounting
// requires this transaction; callers already holding it use setRecoveryWaitLocked.
type recoveryWaitWriter interface {
	SetRunRecoveryWait(context.Context, store.SetRunRecoveryWaitParams) (int64, error)
}

func (s *Service) setRecoveryWait(ctx context.Context, writer recoveryWaitWriter, cause pgtype.Text, run store.Run, wkr store.Worker, req StateRequest, sessionID pgtype.Text) (int64, error) {
	if cause.Valid && cause.String == recoveryCauseCodexIdleStall && s.txBeginner == nil {
		return 0, errors.New("idle stall park unavailable: no tx beginner")
	}
	if s.txBeginner != nil {
		locked, current, tx, err := s.beginClaimMutation(ctx, wkr, run.ID, req.ClaimGeneration)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		rows, err := locked.setRecoveryWaitLocked(ctx, locked.q, cause, current, wkr, req, sessionID)
		if err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return rows, nil
	}
	return s.setRecoveryWaitLocked(ctx, writer, cause, run, wkr, req, sessionID)
}

// setRecoveryWaitLocked stays on an existing fenced run transaction. Its caller
// must already have checked claim generation and release under the run lock.
func (s *Service) setRecoveryWaitLocked(ctx context.Context, writer recoveryWaitWriter, cause pgtype.Text, run store.Run, wkr store.Worker, req StateRequest, sessionID pgtype.Text) (int64, error) {
	idle := cause.Valid && cause.String == recoveryCauseCodexIdleStall
	if idle {
		if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
			return 0, ErrMissingClaimGeneration
		}
		if run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
			return 0, ErrStaleClaim
		}
		if run.Status != "running" {
			return 0, nil
		}
		if run.StopKind.Valid && run.StopKind.String != "" {
			return s.q.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: run.ID, WorkerID: pgconv.UUID(wkr.ID)})
		}
	}
	retry := pgconv.Time(s.now().Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter()))
	var idleCount pgtype.Int4
	if idle {
		count := run.IdleStallParkCount
		if req.ProviderProgressTurns != nil && *req.ProviderProgressTurns > 0 {
			count = 1
		} else if count < math.MaxInt32 {
			count++
		}
		idleCount = pgtype.Int4{Int32: count, Valid: true}
		if int64(count) > int64(s.p.RunCodexIdleStallMaxParks) {
			retry = pgtype.Timestamptz{}
		}
	}
	return writer.SetRunRecoveryWait(ctx, store.SetRunRecoveryWaitParams{
		ResetProviderStreak: req.ProviderProgressTurns != nil && *req.ProviderProgressTurns > 0 &&
			(idle || req.RecoveryCause == nil || *req.RecoveryCause == "empty_turn"),
		RecoveryCause:      cause,
		RetryNotBefore:     retry,
		IdleStallParkCount: idleCount,
		SessionID:          sessionID,
		ID:                 run.ID,
		WorkerID:           pgconv.UUID(wkr.ID),
		// PRD #1247 M5a-1 rework (reviewer NB1): the reported generation, threaded to the fence in
		// SetRunRecoveryWait. A stale/reclaimed old flight's park matches 0 rows; nil parks unfenced.
		ClaimGeneration: pgconv.Int8Ptr(req.ClaimGeneration),
	})
}

// recoveryCauseStored retains ordinary causes with distinct owner controls.
// Other typed parks have separate writers; empty_turn remains untyped.
func recoveryCauseStored(cause *string) pgtype.Text {
	if cause != nil && (*cause == recoveryCauseVaultLocked || *cause == recoveryCauseCodexIdleStall) {
		return pgtype.Text{String: *cause, Valid: true}
	}
	return pgtype.Text{}
}
