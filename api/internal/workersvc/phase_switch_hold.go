package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The two phases a phase-switch hold can name: the phase the worker could not bind.
const (
	PhaseSwitchPlan      = "plan"
	PhaseSwitchImplement = "implement"
)

// SetRunPhaseSwitchHold parks the worker's own live claim on the phase-switch hold (PRD #3252 m4).
// The claim generation is mandatory: the hold keeps the claim through an owner resume, so it must
// never land on a claim this flight no longer holds.
//
// phase must be PhaseSwitchPlan or PhaseSwitchImplement (ErrInvalidState otherwise). An implement
// hold also requires an approved plan under the same row lock (auto_approve, a human or
// cross-check-bound approval, or a seeded plan, the park_run.go precedent): an unapproved run has
// not reached implement, so the hold is refused and the row returned unchanged.
//
// It returns applied=true for the transition and for a repeat of the same generation whose hold
// already landed (a lost ACK retried), answered from the locked re-read so status_since and the
// budgets stay as the first request left them. Any other refusal, including a run the owner has
// since resumed to queued with the marker, returns the owned row with applied=false (the handler's
// 409 carries its real status and hold_reason); a reclaimed run is ErrRunNotOwned (404).
func (s *Service) SetRunPhaseSwitchHold(ctx context.Context, wkr store.Worker, runID uuid.UUID, capturedHead *string, generation int64, phase string) (store.Run, bool, error) {
	if generation <= 0 {
		return store.Run{}, false, fmt.Errorf("%w: positive claim_generation required", ErrInvalidState)
	}
	if phase != PhaseSwitchPlan && phase != PhaseSwitchImplement {
		return store.Run{}, false, fmt.Errorf("%w: phase must be %q or %q", ErrInvalidState, PhaseSwitchPlan, PhaseSwitchImplement)
	}
	gen := generation
	locked, current, tx, err := s.beginClaimMutation(ctx, wkr, runID, &gen)
	if err != nil {
		return store.Run{}, false, err
	}
	if tx != nil {
		defer func() { _ = tx.Rollback(ctx) }()
	}
	if phase == PhaseSwitchImplement && (current.Status == "running" || current.Status == "awaiting_approval") {
		rc, err := locked.q.GetRunClaimContext(ctx, runID)
		if err != nil {
			return store.Run{}, false, err
		}
		if !current.AutoApprove && !rc.HumanPlanApproved && current.PlanSource != planSourceSeeded {
			return current, false, nil
		}
	}
	var head pgtype.Text
	if capturedHead != nil {
		// NUL-strip before the trim, as setRunCompletionHoldLocked does for the same worker field.
		clean, _ := stripNUL(*capturedHead)
		head = pgconv.TextOrNull(strings.TrimSpace(clean))
	}
	run, err := locked.q.SetRunPhaseSwitchHold(ctx, store.SetRunPhaseSwitchHoldParams{
		ID:               runID,
		WorkerID:         pgconv.UUID(wkr.ID),
		HoldCapturedHead: head,
		ClaimGeneration:  generation,
	})
	applied := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		run, err = locked.runOwnedByWorker(ctx, runID, wkr)
		if err != nil {
			return store.Run{}, false, err
		}
		applied = phaseSwitchHeldFor(run, generation)
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

// phaseSwitchHeldFor reports whether run is already held on the phase switch for exactly this live
// claim generation (the idempotent repeat). A queued run carrying the marker is the owner's resume
// and is NOT held.
func phaseSwitchHeldFor(run store.Run, generation int64) bool {
	return run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == HoldReasonPhaseSwitchUnavailable &&
		run.ClaimGeneration == generation && !run.ClaimReleasedAt.Valid
}
