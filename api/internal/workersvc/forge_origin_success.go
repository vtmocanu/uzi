package workersvc

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ForgeCloneSuccessFeature negotiates advisory direct clone/fetch success only.
const ForgeCloneSuccessFeature = capability.ForgeCloneSuccessV1

type ForgeOriginReservation struct {
	Incarnation        uuid.UUID `json:"incarnation"`
	Version            int64     `json:"version"`
	ObservationVersion int64     `json:"observation_version"`
}

type ForgeCloneSuccessRequest struct {
	ClaimGeneration    int64     `json:"claim_generation"`
	Incarnation        uuid.UUID `json:"incarnation"`
	Version            int64     `json:"version"`
	ObservationVersion int64     `json:"observation_version"`
}

// ReportForgeCloneSuccess makes one bounded transaction attempt. Failure drops
// telemetry; callers must not retry or use its result as execution authority.
func (s *Service) ReportForgeCloneSuccess(ctx context.Context, w store.Worker, runID uuid.UUID, req ForgeCloneSuccessRequest) (bool, error) {
	if req.ClaimGeneration <= 0 || req.Incarnation == uuid.Nil || req.Version <= 0 || req.ObservationVersion <= 0 {
		return false, ErrInvalidState
	}
	if s.txBeginner == nil {
		return false, errors.New("forge clone success transaction unavailable")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err = q.LockForgeOriginWorker(ctx, store.LockForgeOriginWorkerParams{RunID: runID, WorkerID: w.ID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrRunNotOwned
		}
		return false, err
	}
	// Authentication may precede a capability refresh. The existing worker lock
	// serializes this observation with registration updates and downgrade.
	current, err := q.GetWorkerByID(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if current.UserID != w.UserID || !slices.Contains(current.ProtocolCapabilities, ForgeCloneSuccessFeature) {
		return false, ErrInvalidState
	}
	run, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(w.ID)})
	if err != nil {
		return false, err
	}
	if run.UserID != w.UserID || run.ClaimGeneration != req.ClaimGeneration {
		return false, nil
	}
	n, err := q.ClearForgeOriginOnCloneSuccess(ctx, store.ClearForgeOriginOnCloneSuccessParams{
		RunID: runID, WorkerID: w.ID, Generation: req.ClaimGeneration,
		Incarnation: req.Incarnation, Version: req.Version, ObservationVersion: req.ObservationVersion,
	})
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *Service) forgeOriginReservation(ctx context.Context, w store.Worker, run store.Run) *ForgeOriginReservation {
	if !slices.Contains(w.ProtocolCapabilities, ForgeCloneSuccessFeature) {
		return nil
	}
	q, ok := s.q.(interface {
		GetForgeOriginReservation(context.Context, store.GetForgeOriginReservationParams) (store.GetForgeOriginReservationRow, error)
	})
	if !ok {
		return nil
	}
	row, err := q.GetForgeOriginReservation(ctx, store.GetForgeOriginReservationParams{
		RunID: run.ID, WorkerID: w.ID, Generation: run.ClaimGeneration,
	})
	if err != nil || !row.ReservationIncarnation.Valid || !row.ReservationVersion.Valid || !row.ReservationObservationVersion.Valid {
		return nil
	}
	return &ForgeOriginReservation{Incarnation: uuid.UUID(row.ReservationIncarnation.Bytes),
		Version: row.ReservationVersion.Int64, ObservationVersion: row.ReservationObservationVersion.Int64}
}
