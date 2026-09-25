package workersvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrInputReceiptConflict = errors.New("input receipt conflicts with current claim or input state")

// Why a receipt's claim is inactive (issue #1673). The worker keeps polling on
// switch_pending, since the switch signal arrives on its next GET and the claim may
// return; on released or stale it ends the old flight, whose GETs would never see
// the rows again.
const (
	ReceiptSwitchPending = "switch_pending"
	ReceiptReleased      = "released"
	ReceiptStale         = "stale"
)

// InputReceiptConflictError is ErrInputReceiptConflict with the claim's inactive
// reason, empty when the claim is active and the rows themselves conflict.
type InputReceiptConflictError struct{ Reason string }

func (e *InputReceiptConflictError) Error() string { return ErrInputReceiptConflict.Error() }
func (e *InputReceiptConflictError) Unwrap() error { return ErrInputReceiptConflict }

var ErrInputReceiptInvalid = errors.New("input ids must belong to this run and be worker inputs")

// InputReceiptResult includes whether this claim can still consume pending inputs.
type InputReceiptResult struct {
	Inputs []InputDTO `json:"inputs"`
	Active bool       `json:"active"`
	// Reason is set when Active is false: ReceiptSwitchPending, ReceiptReleased or ReceiptStale.
	Reason string `json:"reason,omitempty"`
}

// receiptInactiveReason reports why this worker's claim generation cannot consume inputs,
// or "" when it can. A terminal run keeps its worker_id and generation, and a requeued run
// keeps worker_id for affinity, so status is part of the fence: an ACK in flight across a
// completion or a requeue must not mark a late cancel or verdict delivered.
func receiptInactiveReason(run store.LockRunForInputReceiptRow, wkr store.Worker, generation int64) string {
	switch {
	case !run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != wkr.ID || run.ClaimGeneration != generation,
		terminalStatuses[run.Status], run.Status == "queued":
		return ReceiptStale
	case run.ClaimReleasedAt.Valid:
		return ReceiptReleased
	case run.CredentialSwitchRequestedAt.Valid && run.CredentialSwitchGeneration.Valid && run.CredentialSwitchGeneration.Int64 == generation:
		return ReceiptSwitchPending
	}
	return ""
}

func validInputIDs(ids []int64) bool {
	if len(ids) == 0 || len(ids) > 1000 {
		return false
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return false
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func (s *Service) inputReceipt(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64, applied bool) (InputReceiptResult, error) {
	if !slices.Contains(wkr.ProtocolCapabilities, capability.InputReceiptsV1) || !validInputIDs(ids) {
		return InputReceiptResult{}, ErrInputReceiptInvalid
	}
	if s.txBeginner == nil {
		return InputReceiptResult{}, errors.New("input receipt transaction unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return InputReceiptResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transactions cannot be rolled back
	q := store.New(tx)
	run, err := q.LockRunForInputReceipt(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return InputReceiptResult{}, ErrRunNotOwned
	}
	if err != nil {
		return InputReceiptResult{}, err
	}
	reason := receiptInactiveReason(run, wkr, generation)
	active := reason == ""
	conflict := &InputReceiptConflictError{Reason: reason}
	rows, err := q.ListInputReceiptRows(ctx, store.ListInputReceiptRowsParams{RunID: runID, Ids: ids})
	if err != nil {
		return InputReceiptResult{}, err
	}
	if len(rows) != len(ids) {
		return InputReceiptResult{}, ErrInputReceiptInvalid
	}
	toAck := make([]int64, 0, len(ids))
	out := make([]InputDTO, 0, len(ids))
	followUp := false
	for _, row := range rows {
		ownReceipt := row.ConsumedAt.Valid && row.ConsumedClaimGeneration.Valid && row.ConsumedClaimGeneration.Int64 == generation &&
			row.ConsumedWorkerID.Valid && uuid.UUID(row.ConsumedWorkerID.Bytes) == wkr.ID
		switch {
		case applied:
			// A retried APPLIED for rows this claim already applied succeeds even after the
			// claim released: the first reply was lost, and the worker already routed them.
			if !ownReceipt || (!active && !row.AppliedAt.Valid) {
				return InputReceiptResult{}, conflict
			}
		case !row.ConsumedAt.Valid:
			if !active {
				return InputReceiptResult{}, conflict
			}
			toAck = append(toAck, row.ID)
		case !ownReceipt:
			if !active || row.AppliedAt.Valid {
				return InputReceiptResult{}, conflict
			}
			// Transfer an unapplied receipt to the current active claim.
			toAck = append(toAck, row.ID)
		}
		if row.Kind == "follow_up" && !row.ConsumedAt.Valid {
			followUp = true
		}
		out = append(out, InputDTO{ID: row.ID, Kind: row.Kind, Body: textPtr(row.Body), CreatedAt: row.CreatedAt.Time})
	}
	if len(toAck) > 0 {
		stamped, err := q.AckRunInputRows(ctx, store.AckRunInputRowsParams{RunID: runID, Ids: toAck, ClaimGeneration: pgtype.Int8{Int64: generation, Valid: true}, WorkerID: pgconv.UUID(wkr.ID)})
		if err != nil {
			return InputReceiptResult{}, err
		}
		if len(stamped) != len(toAck) {
			return InputReceiptResult{}, fmt.Errorf("input ACK changed concurrently")
		}
	}
	if applied {
		_, err = q.ApplyRunInputRows(ctx, store.ApplyRunInputRowsParams{RunID: runID, Ids: ids, ClaimGeneration: pgtype.Int8{Int64: generation, Valid: true}, WorkerID: pgconv.UUID(wkr.ID)})
		if err != nil {
			return InputReceiptResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return InputReceiptResult{}, err
	}
	if followUp && s.bcast != nil {
		s.bcast.PublishInput(runID)
	}
	return InputReceiptResult{Inputs: out, Active: active, Reason: reason}, nil
}

func (s *Service) AckInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64) (InputReceiptResult, error) {
	return s.inputReceipt(ctx, wkr, runID, generation, ids, false)
}

func (s *Service) ApplyInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64) (InputReceiptResult, error) {
	return s.inputReceipt(ctx, wkr, runID, generation, ids, true)
}
