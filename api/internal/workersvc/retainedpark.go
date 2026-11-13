package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const recoveryCauseRetainedRecoveryExhausted = "retained_recovery_exhausted"
const recoveryCauseRetainedRecoveryAdoptionFailed = "retained_recovery_adoption_failed"
const recoveryCauseRetainedContinuationUnwitnessed = "retained_continuation_unwitnessed"

// RetainedCapacityHoldFeature is the register feature token an api advertises once it accepts
// StateRequest.RetainedCapacityOnly (issue #4046). A worker sends the marker only after seeing it.
const RetainedCapacityHoldFeature = "retained_recovery_capacity_hold_v1"

// validateRetainedCapacityOnly refuses a retained_capacity_only marker unless it rides a
// recovery_wait park with cause retained_recovery_exhausted, a positive claim_generation
// and the value true. It runs before any SQL or registration-evidence write.
func validateRetainedCapacityOnly(req StateRequest) error {
	if req.RetainedCapacityOnly == nil {
		return nil
	}
	if !*req.RetainedCapacityOnly {
		return fmt.Errorf("%w: retained_capacity_only must be true when present", ErrInvalidState)
	}
	if req.State != "recovery_wait" {
		return fmt.Errorf("%w: retained_capacity_only requires state recovery_wait", ErrInvalidState)
	}
	if req.RecoveryCause == nil || *req.RecoveryCause != recoveryCauseRetainedRecoveryExhausted {
		return fmt.Errorf("%w: retained_capacity_only requires recovery_cause %q", ErrInvalidState, recoveryCauseRetainedRecoveryExhausted)
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return fmt.Errorf("%w: retained_capacity_only requires positive claim_generation", ErrInvalidState)
	}
	return nil
}

// RecoveryDetailFeature is the register feature token an api advertises once it accepts
// StateRequest.RecoveryDetail (issue #4177). A worker sends the field only after seeing it.
const RecoveryDetailFeature = "recovery_detail_adoption_refusal_v1"

// Closed set of recovery_wait_detail values for a retained_recovery_adoption_failed park.
const (
	// RecoveryDetailSecretRemediationActive: retrying unchanged cannot help; Resume is refused.
	RecoveryDetailSecretRemediationActive = "secret_remediation_active"
	// RecoveryDetailRemediationProofUnknown: a retry may help.
	RecoveryDetailRemediationProofUnknown = "remediation_proof_unknown"
)

// validateRecoveryDetail refuses a recovery_detail unless it rides a recovery_wait park with
// cause retained_recovery_adoption_failed, a positive claim_generation and a closed-set value.
// A nil field passes. It runs before any SQL or registration-evidence write.
func validateRecoveryDetail(req StateRequest) error {
	if req.RecoveryDetail == nil {
		return nil
	}
	switch *req.RecoveryDetail {
	case RecoveryDetailSecretRemediationActive, RecoveryDetailRemediationProofUnknown:
	default:
		return fmt.Errorf("%w: unknown recovery_detail", ErrInvalidState)
	}
	if req.State != "recovery_wait" {
		return fmt.Errorf("%w: recovery_detail requires state recovery_wait", ErrInvalidState)
	}
	if req.RecoveryCause == nil || *req.RecoveryCause != recoveryCauseRetainedRecoveryAdoptionFailed {
		return fmt.Errorf("%w: recovery_detail requires recovery_cause %q", ErrInvalidState, recoveryCauseRetainedRecoveryAdoptionFailed)
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return fmt.Errorf("%w: recovery_detail requires positive claim_generation", ErrInvalidState)
	}
	return nil
}

// claimRecoveryEpisode projects harness recovery restarts without changing the stored
// worker-death allowance or its finalize/requeue predicates.
func claimRecoveryEpisode(run store.Run) int64 {
	return run.WorkerRecoveryEpisode + int64(run.RetainedParkCount)
}

// parkRetainedRecovery releases execution, never custody. One locked transaction makes
// the counter, retry decision and late-report fence indivisible. Refusals write nothing.
func (s *Service) parkRetainedRecovery(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, sessionID pgtype.Text) (store.Run, int64, error) {
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return owned, 0, fmt.Errorf("%w: retained recovery park requires positive claim_generation", ErrInvalidState)
	}
	if s.txBeginner == nil {
		return owned, 0, fmt.Errorf("retained recovery park unavailable: no tx beginner")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return owned, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	workerID := pgconv.UUID(wkr.ID)
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return owned, 0, ErrRunNotOwned
	}
	if err != nil {
		return owned, 0, err
	}
	if run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return run, 0, ErrStaleClaim
	}
	if run.Status != "running" || (run.StopKind.Valid && run.StopKind.String != "") {
		return run, 0, nil
	}
	switch run.Kind {
	case "issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework":
	default:
		return run, 0, nil
	}
	// A validated capacity-only marker takes the at-cap branch: no counter increment and
	// a NULL retry, an owner-held recovery_wait (issue #4046).
	counted := *req.RecoveryCause == recoveryCauseRetainedRecoveryExhausted && req.RetainedCapacityOnly == nil &&
		int64(run.RetainedParkCount) < int64(s.p.RunRetainedRecoveryMaxParks)
	retry := pgtype.Timestamptz{}
	if counted {
		retry = pgconv.Time(s.now().Add(s.recoveryParkFallbackFor(run.RetainedParkCount) + recoveryParkJitter()))
	}
	// Preserve the existing tri-state park display field in this transaction:
	// post-park claim mutations cannot pass a reconciled run's release fence.
	var checkpoint pgtype.Bool
	if req.CheckpointContainsLatest != nil {
		checkpoint = pgtype.Bool{Bool: *req.CheckpointContainsLatest, Valid: true}
	}
	parked, err := q.ParkRunRetainedRecovery(ctx, store.ParkRunRetainedRecoveryParams{
		ID: run.ID, WorkerID: workerID, ClaimGeneration: *req.ClaimGeneration,
		RecoveryCause: *req.RecoveryCause, Counted: counted, RetryNotBefore: retry, SessionID: sessionID,
		CheckpointContainsLatest: checkpoint,
		RecoveryDetail:           pgconv.TextPtr(req.RecoveryDetail),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return run, 0, nil
	}
	if err != nil {
		return run, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return run, 0, err
	}
	return parked, 1, nil
}
