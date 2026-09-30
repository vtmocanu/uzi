package workersvc

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// OpenJobInputFile is the worker download of one job input (PRD #1909 D8): it returns the file's
// metadata and a reader over its verified plaintext. The guard mirrors CheckJobResultTarget and
// SubmitJobResult's fence, for a read:
//
//   - the run is held by wkr (ErrRunNotFound otherwise, so a foreign worker learns nothing);
//   - the run is kind='job' (ErrNotJobRun);
//   - claimGeneration is the run's CURRENT generation and the claim is not released
//     (ErrStaleClaim), and the run is not terminal (ErrRunTerminal);
//   - the job-file store is configured (ErrJobFilesUnavailable), checked only after the fence;
//   - the file is an input, state 'attached', attached to THIS run (run_id match) and owned by the
//     run's user; anything else reads as ErrJobFileNotFound, so an output, an unattached upload, or
//     another run's file is indistinguishable from an unknown id.
//
// The reader fails with ErrJobFileIntegrity in place of io.EOF when the stored bytes do not verify
// (see JobFiles.Open); the caller must abort the response rather than end it cleanly.
func (s *Service) OpenJobInputFile(ctx context.Context, wkr store.Worker, runID, fileID uuid.UUID, claimGeneration int64) (store.JobFile, io.Reader, error) {
	run, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, nil, ErrRunNotFound
		}
		return store.JobFile{}, nil, err
	}
	if run.Kind != runkind.Job {
		return store.JobFile{}, nil, ErrNotJobRun
	}
	if run.ClaimGeneration != claimGeneration || run.ClaimReleasedAt.Valid {
		return store.JobFile{}, nil, ErrStaleClaim
	}
	if terminalStatuses[run.Status] {
		return store.JobFile{}, nil, ErrRunTerminal
	}
	// The store check follows the fence so a worker that does not hold the run learns nothing from
	// it (a foreign worker reads 404 whether or not files are configured).
	if s.jobFiles == nil {
		return store.JobFile{}, nil, ErrJobFilesUnavailable
	}
	f, err := s.jobFiles.Get(ctx, fileID, run.UserID)
	if err != nil {
		return store.JobFile{}, nil, err
	}
	if f.Direction != JobFileInput || f.State != JobFileAttached || !f.RunID.Valid || uuid.UUID(f.RunID.Bytes) != runID {
		return store.JobFile{}, nil, ErrJobFileNotFound
	}
	return s.jobFiles.Open(ctx, fileID, run.UserID)
}
