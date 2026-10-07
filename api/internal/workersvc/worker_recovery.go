package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func recoveryDispositionCount(rows []store.WorkerRecoveryDisposition, status string) int {
	count := 0
	for _, row := range rows {
		if row.Status == status {
			count++
		}
	}
	return count
}

// workerRecoveryLockSet is transaction-local. Only a successful capture constructs
// an evaluated ledger; the private serialized tuple and parent union are never
// reconstructed from worker input.
type workerRecoveryLockSet struct {
	evaluated  bool
	frozenJSON string
	parentIDs  []uuid.UUID
}

func (locks workerRecoveryLockSet) parameters() ([]byte, []uuid.UUID, error) {
	if !locks.evaluated {
		return nil, nil, errors.New("worker recovery lock set was not evaluated")
	}
	return []byte(locks.frozenJSON), append([]uuid.UUID{}, locks.parentIDs...), nil
}

func validatedSnapshotIDs(entries []validatedEntry) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.runID)
	}
	return ids
}

func captureWorkerRecoveryLocks(ctx context.Context, qtx *store.Queries, workerID uuid.UUID, entries []validatedEntry, attested validatedFinalizeResume) (workerRecoveryLockSet, error) {
	rows, err := qtx.LockWorkerRecoveryParents(ctx, store.LockWorkerRecoveryParentsParams{
		WorkerID: pgconv.UUID(workerID), SnapshotRunIds: validatedSnapshotIDs(entries),
		AttestedRunIds: attested.ids, AttestedClaimGenerations: attested.generations,
	})
	if err != nil {
		return workerRecoveryLockSet{}, err
	}
	return workerRecoveryLocksFromRows(rows)
}

func workerRecoveryLocksFromRows(rows []store.LockWorkerRecoveryParentsRow) (workerRecoveryLockSet, error) {
	if rows == nil {
		rows = []store.LockWorkerRecoveryParentsRow{}
	}
	// The SQL returns its full lock array once, on the first row. Aggregate every
	// actual returned array, rather than inferring locks from parent associations.
	seen := make(map[uuid.UUID]bool)
	parents := make([]uuid.UUID, 0)
	for _, row := range rows {
		for _, id := range row.LockedParentIds {
			if !seen[id] {
				seen[id] = true
				parents = append(parents, id)
			}
		}
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i].String() < parents[j].String() })
	frozen, err := json.Marshal(rows)
	if err != nil {
		return workerRecoveryLockSet{}, err
	}
	return workerRecoveryLockSet{evaluated: true, frozenJSON: string(frozen), parentIDs: parents}, nil
}

func (s *Service) runFrozenAttested(ctx context.Context, qtx *store.Queries, workerID uuid.UUID, max int32, reason store.FrozenFailWorkerRunsOverCapParams, finalize validatedFinalizeResume, valid bool, locks workerRecoveryLockSet) ([]store.WorkerRecoveryDisposition, []uuid.UUID, int, error) {
	frozen, parents, err := locks.parameters()
	if err != nil {
		return nil, nil, 0, err
	}
	if !valid {
		return nil, nil, 0, nil
	}
	failed, err := qtx.FrozenFailAttestedFinalizeRunsOverCap(ctx, store.FrozenFailAttestedFinalizeRunsOverCapParams{
		ReleasedWorkerNonceOverride: reason.ReleasedWorkerNonceOverride,
		FailureReason:               reason.FailureReason, WorkerID: pgconv.UUID(workerID), MaxRequeues: max,
		RunIds: finalize.ids, ClaimGenerations: finalize.generations, FrozenTargets: frozen, LockedParentIds: parents,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	rows, err := qtx.FrozenRequeueAttestedFinalizeRuns(ctx, store.FrozenRequeueAttestedFinalizeRunsParams{
		WorkerID: pgconv.UUID(workerID), MaxRequeues: max, RunIds: finalize.ids, ClaimGenerations: finalize.generations,
		FrozenTargets: frozen, LockedParentIds: parents,
	})
	if err != nil {
		return nil, nil, 0, err
	}
	queued := make([]uuid.UUID, 0, len(rows))
	allowance := 0
	for _, row := range rows {
		queued = append(queued, row.ID)
		if row.AllowanceUsed {
			allowance++
		}
	}
	return failed, queued, allowance, nil
}
