package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// confirmLivePublishAttempt confirms only the run tip and consumes the exact ready attempt.
// Forge IO has already finished. This short transaction locks the run before the attempt and
// retention row, serializing terminal transitions; it never inserts or changes retention.
// Other-run claims are checked again in the final UPDATE's snapshot, without a reservation
// against claims that may be inserted after that statement.
func (s *Service) confirmLivePublishAttempt(ctx context.Context, a store.CheckpointPublishAttempt, observedRun store.Run, observedRecord store.CheckpointRetention, hadRecord bool, fence func(context.Context) error) (bool, error) {
	if s.txBeginner == nil {
		return false, errors.New("live checkpoint confirmation requires a transaction")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin live checkpoint confirmation: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := retentionBookkeepingCtx(ctx)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	q := store.New(tx)
	run, err := q.LockRunForLiveCheckpointConfirmation(ctx, a.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock live checkpoint run: %w", err)
	}
	if terminalStatuses[run.Status] {
		return false, nil
	}
	current, err := q.LockCheckpointPublishAttemptForConfirmation(ctx, a.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock live checkpoint attempt: %w", err)
	}
	if !current.ReconcileReadyAt.Valid || current.RunID != a.RunID ||
		current.Ref != a.Ref || current.Tip != a.Tip {
		return false, nil
	}
	rec, err := q.LockCheckpointRetentionForConfirmation(ctx, a.RunID)
	hasRecord := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("lock live checkpoint retention: %w", err)
	}
	if hasRecord != hadRecord || (hasRecord &&
		(!recordTracksPublish(rec, a.Ref) || rec.Ref != observedRecord.Ref ||
			rec.RecoveryRef != observedRecord.RecoveryRef || rec.State != observedRecord.State ||
			rec.Tip != observedRecord.Tip)) {
		return false, nil
	}
	if err := s.beforeRetentionWrite(ctx, a.RunID, "attempt-confirm", fence); err != nil {
		return false, err
	}
	n, err := q.ConfirmLiveCheckpointPublishAttempt(ctx, store.ConfirmLiveCheckpointPublishAttemptParams{
		RunID: a.RunID, AttemptID: a.ID, Ref: a.Ref, Tip: a.Tip,
		ExpectedTip: observedRun.CheckpointTip, HadRetention: hadRecord,
		ObservedRef: observedRecord.Ref, ObservedRecoveryRef: observedRecord.RecoveryRef,
		ObservedState: observedRecord.State, ObservedRecordTip: observedRecord.Tip,
	})
	if err != nil {
		return false, fmt.Errorf("confirm live checkpoint tip: %w", err)
	}
	if n != 1 {
		return false, nil
	}
	n, err = q.DeleteConfirmedLiveCheckpointPublishAttempt(ctx, store.DeleteConfirmedLiveCheckpointPublishAttemptParams{
		ID: a.ID, RunID: a.RunID, Ref: a.Ref, Tip: a.Tip,
	})
	if err != nil {
		return false, fmt.Errorf("consume confirmed live checkpoint attempt: %w", err)
	}
	if n != 1 {
		// The deferred rollback also undoes the run confirmation.
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit live checkpoint confirmation: %w", err)
	}
	return true, nil
}
