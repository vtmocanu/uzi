package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Maintenance endpoint errors (PRD #4416 M2). The handler maps them to 409 vault_locked and 409
// run_env_history_unavailable.
var (
	ErrRunEnvVaultLocked        = errors.New("vault locked")
	ErrRunEnvHistoryUnavailable = errors.New("run env history unavailable")
)

type runEnvReceiptReader interface {
	ListRunEnvSecretReceiptVersionsForRun(ctx context.Context, runID uuid.UUID) ([]store.ListRunEnvSecretReceiptVersionsForRunRow, error)
}

// RunEnvRedact answers the worker's run-env-redact maintenance read: every secret version the run
// holds a receipt for (live or retired), opened, so a worker that has finished with a run can still
// scrub its history. The run must be this worker's (held or last held, ownedRunOrRefusal) and of
// the worker's user; otherwise ErrRunNotOwned. Protected is false when the run has no secret
// receipts. A receipted version that is missing or does not open is ErrRunEnvHistoryUnavailable,
// never a shorter list; a locked vault is ErrRunEnvVaultLocked. No value is logged.
func (s *Service) RunEnvRedact(ctx context.Context, wkr store.Worker, runID uuid.UUID) (RunEnvRedactResponse, error) {
	run, err := s.runOwnedByWorker(ctx, runID, wkr)
	if err != nil {
		return RunEnvRedactResponse{}, err
	}
	if run.UserID != wkr.UserID {
		return RunEnvRedactResponse{}, ErrRunNotOwned
	}
	rows, err := s.q.ListRunEnvSecretReceiptVersionsForRun(ctx, run.ID)
	if err != nil {
		return RunEnvRedactResponse{}, err
	}
	if len(rows) == 0 {
		return RunEnvRedactResponse{Protected: false}, nil
	}
	out := RunEnvRedactResponse{Protected: true, Versions: make([]ClaimRunEnvRedact, 0, len(rows))}
	for _, row := range rows {
		if row.Ciphertext == nil || !row.SealedWith.Valid || !row.AadVersion.Valid {
			return RunEnvRedactResponse{}, ErrRunEnvHistoryUnavailable
		}
		value, err := s.openRunEnvSealed(run.UserID, int(row.AadVersion.Int16), row.Scope, row.RepoID, true, row.Name, row.VarID, row.Rev, row.SealedWith.String, row.Ciphertext)
		switch {
		case err == nil:
		case errors.Is(err, errVaultLocked):
			return RunEnvRedactResponse{}, ErrRunEnvVaultLocked
		default:
			return RunEnvRedactResponse{}, ErrRunEnvHistoryUnavailable
		}
		out.Versions = append(out.Versions, ClaimRunEnvRedact{VarID: row.VarID, Scope: row.Scope, Rev: int(row.Rev), Name: row.Name, Value: value})
	}
	return out, nil
}
