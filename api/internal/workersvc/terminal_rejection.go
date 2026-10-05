package workersvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const MaxTerminalRejectionGeneration int64 = 9007199254740991

type TerminalRejection struct {
	RunID           string `json:"run_id"`
	ClaimGeneration *int64 `json:"claim_generation"`
	Reason          string `json:"reason"`
}

var ErrInvalidTerminalRejection = errors.New("invalid terminal rejection")

// ValidateTerminalRejections validates the entire bounded batch before any transaction begins.
func ValidateTerminalRejections(batch []TerminalRejection) error {
	if len(batch) > 256 {
		return ErrInvalidTerminalRejection
	}
	for _, item := range batch {
		id, err := uuid.Parse(item.RunID)
		if err != nil || id.String() != item.RunID || item.ClaimGeneration == nil ||
			*item.ClaimGeneration < 0 || *item.ClaimGeneration > MaxTerminalRejectionGeneration || item.Reason != "mac_failure" {
			return ErrInvalidTerminalRejection
		}
	}
	return nil
}

type TerminalRejectionDisposition struct {
	RunID           string `json:"run_id"`
	ClaimGeneration int64  `json:"claim_generation"`
	Reason          string `json:"reason"`
	Disposition     string `json:"disposition"`
}

type TerminalRejectionResponse struct {
	Dispositions []TerminalRejectionDisposition `json:"dispositions"`
}

// ReportTerminalRejections processes at most 256 tuples sequentially, bounded by ctx.
// A store failure stops the batch; earlier commits remain annotated for an idempotent retry.
// The diagnostic ACK never authorizes cleanup.
func (s *Service) ReportTerminalRejections(ctx context.Context, wkr store.Worker, batch []TerminalRejection) (TerminalRejectionResponse, error) {
	if err := ValidateTerminalRejections(batch); err != nil {
		return TerminalRejectionResponse{}, err
	}
	out := TerminalRejectionResponse{Dispositions: make([]TerminalRejectionDisposition, 0, len(batch))}
	if len(batch) == 0 {
		return out, nil
	}
	if s.txBeginner == nil {
		return out, errors.New("terminal rejection transaction unavailable")
	}
	for _, item := range batch {
		recorded, err := s.reportTerminalRejection(ctx, wkr, item)
		if err != nil {
			return out, err
		}
		disposition := "skipped"
		if recorded {
			disposition = "recorded"
		}
		out.Dispositions = append(out.Dispositions, TerminalRejectionDisposition{
			RunID: item.RunID, ClaimGeneration: *item.ClaimGeneration, Reason: item.Reason, Disposition: disposition,
		})
	}
	return out, nil
}

func (s *Service) reportTerminalRejection(ctx context.Context, wkr store.Worker, item TerminalRejection) (bool, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	id := uuid.MustParse(item.RunID) // Validated before any writes.
	// Lock the unfiltered assignment first, including a newer or foreign assignment.
	_, err = q.GetRunByIDForUpdate(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	holds, err := q.LockTerminalRejectionHolds(ctx, store.LockTerminalRejectionHoldsParams{
		UserID: wkr.UserID, WorkerID: wkr.ID, RunID: id, Generation: *item.ClaimGeneration,
	})
	if err != nil {
		return false, err
	}
	for _, hold := range holds {
		if err := q.AnnotateTerminalRejectionHold(ctx, hold); err != nil {
			return false, err
		}
	}
	if len(holds) > 0 {
		if err := q.RetroTerminalRejectionReason(ctx, store.RetroTerminalRejectionReasonParams{
			RunID: id, UserID: wkr.UserID, WorkerID: pgconv.UUID(wkr.ID), Generation: *item.ClaimGeneration,
		}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return len(holds) > 0, nil
}

type TerminalRejectionCustody struct {
	RunID           string          `json:"run_id"`
	WorkerID        string          `json:"worker_id"`
	Generation      int64           `json:"generation"`
	ExactHolds      json.RawMessage `json:"exact_holds"`
	SiblingHolds    json.RawMessage `json:"sibling_holds"`
	ExactCount      int64           `json:"exact_count"`
	SiblingCount    int64           `json:"sibling_count"`
	ExactComplete   bool            `json:"exact_complete"`
	SiblingComplete bool            `json:"sibling_complete"`
	Complete        bool            `json:"complete"`
	Outcome         string          `json:"outcome"`
}

// TerminalRejectionCustody observes custody only; none of its outcomes authorizes cleanup.
func (s *Service) TerminalRejectionCustody(ctx context.Context, wkr store.Worker, runID uuid.UUID, generation int64) (TerminalRejectionCustody, error) {
	row, err := s.q.TerminalRejectionCustodySnapshot(ctx, store.TerminalRejectionCustodySnapshotParams{
		UserID: wkr.UserID, WorkerID: wkr.ID, RunID: runID, Generation: generation,
	})
	if err != nil {
		return TerminalRejectionCustody{}, err
	}
	out := TerminalRejectionCustody{
		RunID: runID.String(), WorkerID: wkr.ID.String(), Generation: generation,
		ExactHolds: row.ExactHolds, SiblingHolds: row.SiblingHolds,
		ExactCount: row.ExactCount, SiblingCount: row.SiblingCount,
		ExactComplete: row.ExactCount <= 256, SiblingComplete: row.SiblingCount <= 256,
		Outcome: "unknown",
	}
	out.Complete = out.ExactComplete && out.SiblingComplete
	if row.OpenCount > 0 {
		out.Outcome = "retained"
	} else if out.Complete && row.ExactCount > 0 && row.UnsettledCount == 0 && row.SiblingCount == 0 {
		out.Outcome = "settled"
	}
	return out, nil
}
