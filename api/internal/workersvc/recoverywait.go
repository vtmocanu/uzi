package workersvc

import (
	"context"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Transient-recovery park (issue #1197): the server's half of the 'recovery_wait' status.
//
// It is modelled CLOSELY on the usage-limit park (limitwait.go's setLimitWait +
// limitParkFallbackFor + limitParkJitter) but with two deliberate differences that are the
// whole point of it being a RECOVERY park rather than a LIMIT park:
//
//   - NO lifetime cap. There is no RUN_RECOVERY_MAX_WAITS and no budget-exhausted branch:
//     recoveryParkFallbackFor ALWAYS returns a finite positive duration, so a park always
//     becomes promotable again. The sweeper (PromoteRecoveryWaitRuns) auto-resumes the run
//     at the capped cadence until it recovers or the owner cancels (CancelRunServerSide's
//     negative admit-set covers a recovery_wait run for free).
//   - NO terminal / opt-out / gauge branch. Unlike decideLimitPark there is no opt-out,
//     no credential cross-check and no failure path here — a recovery_wait report always
//     parks. recovery_wait_count shapes the backoff curve ONLY; it is never a limit.
//
// This is the reusable transient-recovery park PRIMITIVE: any transient cause can report
// `recovery_wait` and reuse the capture->park->promote->reclaim lifecycle, so issue #1088's
// provider classifier can adopt it later without building a competing mechanism.

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

// setRecoveryWait is SetState's `recovery_wait` arm: the impure half of the transient park.
//
// It mirrors setLimitWait's signature/shape but is far simpler — there is no decision to
// make. Every recovery_wait report parks: the server computes retry_not_before = now +
// capped-exponential-backoff + jitter and calls SetRunRecoveryWait, whose POSITIVE source
// guard (status='running') makes a re-delivered/out-of-order report a 0-row no-op that
// SetState maps to applied=false / 409 (idempotent, like the limit park). It returns the
// row count SetState maps to `applied`.
//
// The StateRequest parameter is unused today — recovery_wait carries no dedicated DTO
// fields (the pool_wait precedent) — but is kept for symmetry with the park family so a
// later transient-recovery field lands here without changing the call shape.
func (s *Service) setRecoveryWait(ctx context.Context, run store.Run, wkr store.Worker, _ StateRequest, sessionID pgtype.Text) (int64, error) {
	retryNotBefore := s.now().Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter())
	return s.q.SetRunRecoveryWait(ctx, store.SetRunRecoveryWaitParams{
		RetryNotBefore: pgconv.Time(retryNotBefore),
		SessionID:      sessionID,
		ID:             run.ID,
		WorkerID:       pgconv.UUID(wkr.ID),
	})
}
