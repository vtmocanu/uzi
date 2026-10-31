package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// MemoryParkReference is the claim body's memory_park_reference (issue #3639): the settled
// memory-pressure park an owner Resume lifted for exactly the claiming worker incarnation.
type MemoryParkReference struct {
	ClaimGeneration  int64     `json:"claim_generation"`
	MemoryEpisode    int64     `json:"memory_episode"`
	WorkerID         uuid.UUID `json:"worker_id"`
	RegisterNonce    string    `json:"register_nonce"`
	SettlementSha256 string    `json:"settlement_sha256"`
}

// MemoryParkSettlementResult answers an accepted (or exactly replayed) receipt.
type MemoryParkSettlementResult struct {
	SettlementSha256 string `json:"settlement_sha256"`
}

// RecordMemoryParkSettlement stores the worker's settlement receipt for the run's current
// worker_memory_pressure park. The record is decoded strictly before any lock; currency is then judged
// under the memory lock (worker, parents, run) and every failure is a 409 with no write. The receipt
// transaction never updates runs and never writes intervention rows: the history only has to be
// consistent with the park's binding (a missing row is accepted, a foreign one is a conflict).
// The first stored receipt is immutable; the same bytes replay as success while the park is current.
func (s *Service) RecordMemoryParkSettlement(ctx context.Context, w store.Worker, runID uuid.UUID, raw []byte) (MemoryParkSettlementResult, error) {
	rec, interventionIDs, err := decodeMemoryParkReceipt(raw, runID, w.ID)
	if err != nil {
		return MemoryParkSettlementResult{}, err
	}
	sum := sha256.Sum256(raw)
	result := MemoryParkSettlementResult{SettlementSha256: hex.EncodeToString(sum[:])}
	b := MemoryBinding{RunID: runID, WorkerID: w.ID, RegisterNonce: rec.Binding.RegisterNonce,
		ClaimGeneration: rec.Binding.ClaimGeneration, MemoryEpisode: rec.Binding.MemoryEpisode}
	tx, q, locked, run, err := s.memoryLock(ctx, w, b)
	if err != nil {
		return MemoryParkSettlementResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rel, err := s.memoryParkCurrentRelease(ctx, q, locked, run, b)
	if err != nil {
		return MemoryParkSettlementResult{}, err
	}
	for _, id := range interventionIDs {
		row, err := q.GetMemoryIntervention(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return MemoryParkSettlementResult{}, err
		}
		ib := b
		ib.InterventionID = id
		if !memoryBindingMatches(row, ib, locked.UserID) {
			return MemoryParkSettlementResult{}, ErrMemoryBinding
		}
	}
	if rel.Settlement != nil {
		if !bytes.Equal(rel.SettlementSha256, sum[:]) {
			return MemoryParkSettlementResult{}, ErrMemoryBinding
		}
		return result, nil
	}
	n, err := q.SettleMemoryParkRelease(ctx, store.SettleMemoryParkReleaseParams{
		RunID: runID, ClaimGeneration: b.ClaimGeneration, Settlement: raw, SettlementSha256: sum[:]})
	if err != nil {
		return MemoryParkSettlementResult{}, err
	}
	if n != 1 {
		// The row is locked FOR UPDATE, so this means an invariant broke; never claim success.
		return MemoryParkSettlementResult{}, fmt.Errorf("memory park settlement not stored for run %s generation %d", runID, b.ClaimGeneration)
	}
	if err := tx.Commit(ctx); err != nil {
		return MemoryParkSettlementResult{}, err
	}
	return result, nil
}

// memoryParkCurrentRelease proves, under the caller's memory lock, that the receipt's binding is the
// run's CURRENT unlifted worker_memory_pressure park and returns its locked release row. Each failed
// proof is ErrMemoryStale.
func (s *Service) memoryParkCurrentRelease(ctx context.Context, q *store.Queries, locked store.Worker, run store.Run, b MemoryBinding) (store.MemoryParkRelease, error) {
	if !slices.Contains(locked.ProtocolCapabilities, capability.MemoryParkSettlementV1) ||
		!locked.SnapshotRegisterNonce.Valid || locked.SnapshotRegisterNonce.String != b.RegisterNonce ||
		run.Status != "recovery_wait" || !run.RecoveryWaitCause.Valid ||
		run.RecoveryWaitCause.String != recoveryCauseWorkerMemoryPressure || !run.ClaimReleasedAt.Valid ||
		!run.ReleasedWorkerID.Valid || uuid.UUID(run.ReleasedWorkerID.Bytes) != b.WorkerID ||
		!run.ReleasedWorkerNonce.Valid || run.ReleasedWorkerNonce.String != b.RegisterNonce ||
		run.ClaimGeneration != b.ClaimGeneration || run.MemoryEpisode != b.MemoryEpisode {
		return store.MemoryParkRelease{}, ErrMemoryStale
	}
	parent, err := memoryParentCurrent(ctx, q, run)
	if err != nil {
		return store.MemoryParkRelease{}, err
	}
	if !parent {
		return store.MemoryParkRelease{}, ErrMemoryStale
	}
	rel, err := q.GetMemoryParkReleaseForUpdate(ctx, store.GetMemoryParkReleaseForUpdateParams{
		RunID: run.ID, ClaimGeneration: b.ClaimGeneration})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MemoryParkRelease{}, ErrMemoryStale // a legacy park has no release row
	}
	if err != nil {
		return store.MemoryParkRelease{}, err
	}
	if rel.WorkerID != b.WorkerID || rel.RegisterNonce != b.RegisterNonce || rel.MemoryEpisode != b.MemoryEpisode ||
		!rel.ReleasedAt.Valid || !rel.ReleasedAt.Time.Equal(run.ClaimReleasedAt.Time) || rel.LiftedAt.Valid {
		return store.MemoryParkRelease{}, ErrMemoryStale
	}
	return rel, nil
}

// ResumeMemoryHold is the owner Resume of a worker_memory_pressure hold: the unchanged
// ResumeMemoryEpisode and then, in the SAME READ COMMITTED transaction opened on begin, the lift of
// the released-pair exclusion for exactly that park when its settlement receipt is stored. A zero-row
// lift is not an error: the resume commits and the exclusion stays. pgx.ErrNoRows from the resume
// is returned as is so the caller keeps its existing 409 path.
func ResumeMemoryHold(ctx context.Context, begin TxBeginner, runID, userID uuid.UUID, globalTimeoutSeconds int32) error {
	tx, err := begin.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return err
	}
	q := store.New(tx)
	if _, err := q.ResumeMemoryEpisode(ctx, store.ResumeMemoryEpisodeParams{
		ID: runID, UserID: userID, GlobalTimeoutSeconds: globalTimeoutSeconds}); err != nil {
		return err
	}
	if _, err := q.LiftSettledMemoryPark(ctx, store.LiftSettledMemoryParkParams{ID: runID, UserID: userID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// memoryParkClaimReference runs inside the claim transaction, after ClaimRun and with the worker row
// locked (reread is that locked row). It (a) retires a lifted release row the claimant is not the
// named incarnation of, so a foreign claim ends the same-incarnation exception for good, and
// (b) returns the reference for a capable claimant that IS the named incarnation of the run's latest
// lifted, unsuperseded row. Only the latest release row (highest claim generation) counts, so a
// newer park replaces an older reference. A read error fails the claim.
func (s *Service) memoryParkClaimReference(ctx context.Context, q *store.Queries, claimant store.Worker, run store.Run) (*MemoryParkReference, error) {
	rel, err := q.GetLatestMemoryParkRelease(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !rel.LiftedAt.Valid || rel.SupersededAt.Valid {
		return nil, nil
	}
	named := rel.WorkerID == claimant.ID && claimant.SnapshotRegisterNonce.Valid &&
		rel.RegisterNonce == claimant.SnapshotRegisterNonce.String
	if !named {
		if _, err := q.SupersedeMemoryParkRelease(ctx, store.SupersedeMemoryParkReleaseParams{
			RunID: run.ID, ClaimGeneration: rel.ClaimGeneration}); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if !slices.Contains(claimant.ProtocolCapabilities, capability.MemoryParkSettlementV1) || len(rel.SettlementSha256) != sha256.Size {
		return nil, nil
	}
	return &MemoryParkReference{ClaimGeneration: rel.ClaimGeneration, MemoryEpisode: rel.MemoryEpisode,
		WorkerID: rel.WorkerID, RegisterNonce: rel.RegisterNonce,
		SettlementSha256: hex.EncodeToString(rel.SettlementSha256)}, nil
}

// insertMemoryParkRelease records the park just written by ParkWorkerMemoryPressure, in its
// transaction. Zero rows would mean the row already exists for this generation or the park left no
// released pair; either is an invariant failure that rolls the park back.
func insertMemoryParkRelease(ctx context.Context, q *store.Queries, run store.Run) error {
	if _, err := q.InsertMemoryParkRelease(ctx, run.ID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("memory park release row not inserted for run %s generation %d", run.ID, run.ClaimGeneration)
		}
		return err
	}
	return nil
}
