package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// HoldReasonBaseAttestationMissing (issue #3194) is the owner-actionable hold a `--require-base`
// run enters when its worker adopted retained work that no checkpoint attests was produced on the
// planned base. The agent never ran; the clone and custody stay on the worker. Only the owner
// leaves it (recovery, export, a successor run, cancel); ResumePausedRun, the extension writers and
// every promoter refuse or skip it.
const HoldReasonBaseAttestationMissing = "base_attestation_missing"

// ErrBaseAttestationHeld refuses every owner verb that would run the agent on a run held on
// HoldReasonBaseAttestationMissing (resume, extend, set-token). The hold is resolved by recovering
// the kept work or starting a successor, never by running this run again.
var ErrBaseAttestationHeld = errors.New("this run is held because its retained work is not attested against the planned base (--require-base), so it cannot resume or be extended; recover the kept work with `uzi run recovery` and `uzi run export`, or start a successor run")

// IsBaseAttestationHeld reports whether run is paused on HoldReasonBaseAttestationMissing.
func IsBaseAttestationHeld(run store.Run) bool {
	return run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == HoldReasonBaseAttestationMissing
}

// SetRunBaseAttestationHold parks the worker's own live claim on the base-attestation hold
// (issue #3194). The claim generation is mandatory: the hold refuses resume, so it must never land
// on a claim this flight no longer holds.
//
// It returns applied=true for the transition and for a repeat of the same generation whose hold
// already landed (a lost ACK retried): that repeat is answered from the locked re-read, so
// status_since and the budgets stay as the first request left them. Any other refusal returns the
// owned row with applied=false (the handler's 409 carries its real status); a reclaimed run is
// ErrRunNotOwned (404).
func (s *Service) SetRunBaseAttestationHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, capturedHead string, generation int64) (store.Run, bool, error) {
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
	// NUL-strip before the trim, as setRunCompletionHoldLocked does for the same worker field.
	clean, _ := stripNUL(capturedHead)
	run, err := locked.q.SetRunBaseAttestationHold(ctx, store.SetRunBaseAttestationHoldParams{
		ID:               runID,
		WorkerID:         pgconv.UUID(wkr.ID),
		HoldCapturedHead: pgconv.TextOrNull(strings.TrimSpace(clean)),
		ClaimGeneration:  generation,
	})
	applied := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		run, err = locked.runOwnedByWorker(ctx, runID, wkr)
		if err != nil {
			return store.Run{}, false, err
		}
		applied = baseAttestationHeldFor(run, generation)
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

// baseAttestationHeldFor reports whether run is already held on the base attestation for exactly
// this live claim generation (the idempotent repeat).
func baseAttestationHeldFor(run store.Run, generation int64) bool {
	return IsBaseAttestationHeld(run) && run.ClaimGeneration == generation && !run.ClaimReleasedAt.Valid
}
