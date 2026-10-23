package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// CheckpointCoordinates is an immutable server-authorized pair. Provenance is
// the validated path of at most 64 actual run rows, starting with RunID.
type CheckpointCoordinates struct {
	Version       int         `json:"version"`
	WorkBranch    string      `json:"work_branch"`
	CheckpointKey string      `json:"checkpoint_key"`
	RunID         uuid.UUID   `json:"run_id"`
	UserID        uuid.UUID   `json:"user_id"`
	RepoID        uuid.UUID   `json:"repo_id"`
	Kind          string      `json:"kind"`
	IssueIID      *int64      `json:"issue_iid"`
	Provenance    []uuid.UUID `json:"provenance"`
}

func frozenCheckpointCoordinates(run store.Run) (*CheckpointCoordinates, error) {
	if len(run.CheckpointCoordinates) == 0 {
		return nil, nil
	}
	var pair CheckpointCoordinates
	if err := json.Unmarshal(run.CheckpointCoordinates, &pair); err != nil {
		return nil, err
	}
	if pair.Version != 1 || pair.RunID != run.ID || pair.UserID != run.UserID ||
		!run.RepoID.Valid || pair.RepoID != uuid.UUID(run.RepoID.Bytes) || pair.Kind != run.Kind ||
		pair.WorkBranch == "" || pair.CheckpointKey == "" ||
		len(pair.Provenance) == 0 || len(pair.Provenance) > 64 || pair.Provenance[0] != run.ID ||
		(run.Branch.Valid && run.Branch.String != pair.WorkBranch) ||
		(run.IssueIid.Valid != (pair.IssueIID != nil)) ||
		(pair.IssueIID != nil && *pair.IssueIID != run.IssueIid.Int64) {
		return nil, fmt.Errorf("checkpoint coordinates do not match run %s", run.ID)
	}
	return &pair, nil
}

// claimCheckpointRun owns a savepoint for each of three attempts. Lineage lock
// contention or drift rolls back custody, enrollment and the claim together.
// Exhaustion returns idle so another normal admission can try later.
func (s *Service) claimCheckpointRun(ctx context.Context, tx pgx.Tx, params store.ClaimRunParams) (store.Run, error) {
	for attempt := 0; attempt < 3; attempt++ {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return store.Run{}, err
		}
		q := store.New(sp)
		if err = q.OptInCheckpointCoordinates(ctx); err != nil {
			_ = sp.Rollback(ctx)
			return store.Run{}, err
		}
		run, err := q.ClaimRun(ctx, params)
		if err == nil && run.Kind == "task" {
			run.CheckpointCoordinates, err = q.EnrollCheckpointCoordinates(ctx, store.EnrollCheckpointCoordinatesParams{
				RunID: run.ID, WorkerID: uuid.UUID(params.WorkerID.Bytes), Generation: run.ClaimGeneration,
			})
		}
		if err == nil {
			if err = sp.Commit(ctx); err != nil {
				return store.Run{}, err
			}
			return run, nil
		}
		if rollbackErr := sp.Rollback(ctx); rollbackErr != nil {
			return store.Run{}, rollbackErr
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40001") {
			continue
		}
		return store.Run{}, err
	}
	return store.Run{}, pgx.ErrNoRows
}

func checkpointCoordinatesForWorker(worker store.Worker, run store.Run) (*CheckpointCoordinates, error) {
	pair, err := frozenCheckpointCoordinates(run)
	if err != nil {
		return nil, err
	}
	if pair != nil && !slices.Contains(worker.ProtocolCapabilities, capability.TaskCheckpointCoordinatesV1) {
		return nil, fmt.Errorf("worker cannot consume enrolled checkpoint coordinates")
	}
	return pair, nil
}
