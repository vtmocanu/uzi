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

// receiptMode is which receipt a worker sends: ACK (received), APPLIED (acted on), or
// DISCARDED (issue #1604: an approve_plan the worker received but dropped as stale, sent
// against an earlier plan).
type receiptMode int

const (
	receiptAck receiptMode = iota
	receiptApplied
	receiptDiscarded
)

// dispositionSuperseded marks an approve_plan settled by a DISCARDED receipt (issue #1604):
// applied_at is set so the row leaves the replay list, and the approval readers
// (GetRunClaimContext, SetRunRunning) skip it, so it never counts as a human approval.
const dispositionSuperseded = "superseded"

func (s *Service) inputReceipt(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64, mode receiptMode) (InputReceiptResult, error) {
	applied := mode == receiptApplied
	discarded := mode == receiptDiscarded
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
	// Issue #1604: only an approve_plan can be discarded. Every other kind is either acted on
	// (APPLIED) or replayed to the next claim; settling it without acting would drop it.
	if discarded && slices.ContainsFunc(rows, func(row store.ListInputReceiptRowsRow) bool { return row.Kind != "approve_plan" }) {
		return InputReceiptResult{}, ErrInputReceiptInvalid
	}
	// Issue #1604: the worker sends APPLIED for a revise_plan only once its disposition is
	// final: after the plan answering it was persisted (or the budget-exhausted re-gate), or
	// when it was stale or empty and never acted on. So marking it applied under a pending
	// credential switch is truthful; refusing it would leave the row unapplied and the resumed
	// claim would replay a revision that is already settled. Only a switch_pending claim qualifies (the claim is still this
	// worker's, at this generation, not released), and only when every requested row is a
	// revise_plan this claim itself received: a mixed batch or a released/stale claim is still
	// refused. Persistence and APPLIED are separate transactions, so an interruption between
	// them can still repeat a revision (at-least-once, never exactly-once).
	applyUnderSwitch := applied && reason == ReceiptSwitchPending &&
		!slices.ContainsFunc(rows, func(row store.ListInputReceiptRowsRow) bool { return row.Kind != "revise_plan" })
	toAck := make([]int64, 0, len(ids))
	out := make([]InputDTO, 0, len(ids))
	followUp := false
	toDiscard := 0
	for _, row := range rows {
		ownReceipt := row.ConsumedAt.Valid && row.ConsumedClaimGeneration.Valid && row.ConsumedClaimGeneration.Int64 == generation &&
			row.ConsumedWorkerID.Valid && uuid.UUID(row.ConsumedWorkerID.Bytes) == wkr.ID
		superseded := row.Disposition.Valid && row.Disposition.String == dispositionSuperseded
		switch {
		case discarded:
			// Issue #1604: the same fences as APPLIED, with the switch_pending allowance a
			// revise-only APPLIED has: a discard is truthful while the claim is still this
			// worker's (the rows are all approve_plan, checked above). A retry of rows this
			// claim already discarded succeeds even after the claim ended (the first reply was
			// lost). A row already applied as a REAL approval is a conflict: the worker acted
			// on it, and a discard must not rewrite that.
			switch {
			case !ownReceipt:
				return InputReceiptResult{}, conflict
			case row.AppliedAt.Valid:
				if !superseded {
					return InputReceiptResult{}, conflict
				}
			case !active && reason != ReceiptSwitchPending:
				return InputReceiptResult{}, conflict
			default:
				toDiscard++
			}
		case applied && superseded:
			// A row this claim discarded is not an approval; answering APPLIED 200 for it would
			// tell the worker the approve counted when the server never will.
			return InputReceiptResult{}, conflict
		case applied:
			// A retried APPLIED for rows this claim already applied succeeds even after the
			// claim released: the first reply was lost, and the worker already routed them.
			if !ownReceipt || (!active && !row.AppliedAt.Valid && !applyUnderSwitch) {
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
	if discarded && toDiscard > 0 {
		settled, err := q.DiscardRunInputRows(ctx, store.DiscardRunInputRowsParams{RunID: runID, Ids: ids, ClaimGeneration: pgtype.Int8{Int64: generation, Valid: true}, WorkerID: pgconv.UUID(wkr.ID)})
		if err != nil {
			return InputReceiptResult{}, err
		}
		if settled != int64(toDiscard) {
			return InputReceiptResult{}, fmt.Errorf("input discard changed concurrently")
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
	return s.inputReceipt(ctx, wkr, runID, generation, ids, receiptAck)
}

func (s *Service) ApplyInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64) (InputReceiptResult, error) {
	return s.inputReceipt(ctx, wkr, runID, generation, ids, receiptApplied)
}

// DiscardInputs settles approve_plan rows the worker received and dropped as stale (issue
// #1604): applied_at is set so they leave the replay list, which is oldest-first and capped,
// and disposition 'superseded' keeps them from counting as a human plan approval.
func (s *Service) DiscardInputs(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64, ids []int64) (InputReceiptResult, error) {
	return s.inputReceipt(ctx, wkr, runID, generation, ids, receiptDiscarded)
}
