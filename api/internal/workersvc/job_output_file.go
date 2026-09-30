package workersvc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_output_file.go is the worker's job-output upload (PRD #1909 M4, D4): the worker uploads the
// report, the findings JSON and the files it chose to keep before it posts the job result. A file
// over a cap or quota, or of a type the allowlist refuses, is REFUSED and recorded in
// job_output_refusals; the job itself is never failed by it.

// JobOutputParams describe one output the worker declares.
type JobOutputParams struct {
	ClaimGeneration int64
	DisplayName     string // already sanitised.
	Size            int64
	SHA256          string // required: lowercase hex, verified against the streamed bytes.
}

// JobOutputResult is a stored (or, on an idempotent retry, already stored) output.
type JobOutputResult struct {
	File store.JobFile
	// Existing is true when a retry found the file its first attempt stored and stored nothing.
	Existing bool
}

// fenceJobOutput is the pre-body fence of the output route, identical to SubmitJobResult's checks
// for a plain read: the run is held by wkr (ErrRunNotFound otherwise, so a foreign worker learns
// nothing), kind='job' (ErrNotJobRun), the claim generation is the run's CURRENT one and the claim
// is not released (ErrStaleClaim), and the run is not terminal (ErrRunTerminal).
func (s *Service) fenceJobOutput(ctx context.Context, wkr store.Worker, runID uuid.UUID, claimGeneration int64) (store.Run, error) {
	run, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}
	if run.Kind != runkind.Job {
		return store.Run{}, ErrNotJobRun
	}
	if run.ClaimGeneration != claimGeneration || run.ClaimReleasedAt.Valid {
		return store.Run{}, ErrStaleClaim
	}
	if terminalStatuses[run.Status] {
		return store.Run{}, ErrRunTerminal
	}
	return run, nil
}

// StoreJobOutput admits, streams and commits one output file of a job run for the worker that
// holds it.
//
// Order, each step before anything is read from body: the fence (fenceJobOutput; the job-file
// store must be configured, checked after it so a foreign worker learns nothing), the idempotency
// lookup, an upload slot (ErrJobFileUploadsBusy), Reserve at exactly the declared size (the
// per-file, per-job and owner/instance/shared-budget caps) and only then Write, which streams body
// through opt's Inspector and refuses a size or sha256 that is not the declared one, releasing the
// reservation in the same call. Any *JobFileRefusedError from Reserve or Write is recorded in
// job_output_refusals before it is returned (an *http.MaxBytesError from the body is a size
// mismatch); a body, timeout or database failure is not a refusal and records nothing.
//
// Idempotency: a retried upload of the same sha256 under the same display name, run and claim
// generation returns the file its first attempt stored (Existing) and stores nothing, so a worker
// that lost the response can repeat the request without double-storing or being charged against
// the per-job caps twice. The output is committed 'attached' to the run; the sweep moves it to
// 'available' when the run is terminal.
//
// After the write the fence is re-read: a claim that went stale during the upload (a re-claim
// cleared this run's outputs while the write was in flight, or the claim was released) deletes the
// file and returns ErrStaleClaim, so no file of an older flight survives into the next one.
func (s *Service) StoreJobOutput(ctx context.Context, wkr store.Worker, runID uuid.UUID, p JobOutputParams, body io.Reader, opt WriteOptions) (JobOutputResult, error) {
	run, err := s.fenceJobOutput(ctx, wkr, runID, p.ClaimGeneration)
	if err != nil {
		return JobOutputResult{}, err
	}
	jf := s.jobFiles
	if jf == nil {
		return JobOutputResult{}, ErrJobFilesUnavailable
	}
	if p.SHA256 != "" {
		if existing, ok, ferr := jf.findOutput(ctx, runID, p); ferr != nil {
			return JobOutputResult{}, ferr
		} else if ok {
			return JobOutputResult{File: existing, Existing: true}, nil
		}
	}
	release, err := jf.AcquireWrite(run.UserID)
	if err != nil {
		return JobOutputResult{}, err
	}
	defer release()

	gen := p.ClaimGeneration
	row, err := jf.Reserve(ctx, ReserveParams{
		UserID:          run.UserID,
		ProductID:       nil,
		RunID:           &runID,
		Direction:       JobFileOutput,
		ClaimGeneration: &gen,
		DisplayName:     p.DisplayName,
		DeclaredSize:    p.Size,
		DeclaredSHA256:  p.SHA256,
	})
	if err != nil {
		return JobOutputResult{}, jf.recordRefusal(ctx, runID, p, err)
	}
	stored, err := jf.Write(ctx, row.ID, run.UserID, body, opt)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// The body outran its declared size (Write has already released the reservation).
			err = refusal(RefusalInvalid, RefusalSizeMismatch)
		}
		return JobOutputResult{}, jf.recordRefusal(ctx, runID, p, err)
	}
	if _, ferr := s.fenceJobOutput(ctx, wkr, runID, p.ClaimGeneration); ferr != nil && !errors.Is(ferr, ErrRunTerminal) {
		// A terminal run keeps its output (the sweep settles it); a stale claim or a lost run does
		// not: the file belongs to a flight that no longer owns the run.
		jf.dropStaleOutput(ctx, runID, stored.ID, p.ClaimGeneration)
		if errors.Is(ferr, ErrRunNotFound) || errors.Is(ferr, ErrNotJobRun) {
			return JobOutputResult{}, ErrStaleClaim
		}
		return JobOutputResult{}, ferr
	}
	return JobOutputResult{File: stored}, nil
}

// findOutput looks up the output an earlier attempt of the same upload stored.
func (j *JobFiles) findOutput(ctx context.Context, runID uuid.UUID, p JobOutputParams) (store.JobFile, bool, error) {
	row, err := store.New(j.db).FindJobOutputFile(ctx, store.FindJobOutputFileParams{
		RunID:           pgconv.UUID(runID),
		ClaimGeneration: int8Ptr(&p.ClaimGeneration),
		Sha256:          pgconv.Text(p.SHA256),
		DisplayName:     p.DisplayName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JobFile{}, false, nil
		}
		return store.JobFile{}, false, err
	}
	return row, true, nil
}

// recordRefusal records a refused output and returns err unchanged. Only a *JobFileRefusedError
// is recorded (bounded per run by OutputsMaxFiles; see InsertJobOutputRefusal). Recording is best
// effort on a fresh short context: a failure is logged and never replaces the refusal the caller
// answers with.
func (j *JobFiles) recordRefusal(ctx context.Context, runID uuid.UUID, p JobOutputParams, err error) error {
	var ref *JobFileRefusedError
	if !errors.As(err, &ref) {
		return err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, ierr := store.New(j.db).InsertJobOutputRefusal(rctx, store.InsertJobOutputRefusalParams{
		RunID:       runID,
		DisplayName: p.DisplayName,
		ByteSize:    max(p.Size, 0),
		Reason:      ref.Reason,
		MaxRows:     int64(j.limits.OutputsMaxFiles),
	}); ierr != nil {
		slog.Warn("job files: recording a refused output", "run", runID.String(), "error", ierr)
	}
	return err
}

// dropStaleOutput deletes an output whose claim went stale mid-upload. Best effort: a failure is
// logged; the next re-claim's ClearRunOutputs (or the run's deletion) removes it.
func (j *JobFiles) dropStaleOutput(ctx context.Context, runID, fileID uuid.UUID, gen int64) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := store.New(j.db).DeleteJobOutputFile(rctx, store.DeleteJobOutputFileParams{
		ID: fileID, RunID: pgconv.UUID(runID), ClaimGeneration: int8Ptr(&gen),
	}); err != nil {
		slog.Warn("job files: dropping a stale output", "run", runID.String(), "file", fileID.String(), "error", err)
	}
}

// ClearRunOutputs deletes a job run's output files (their chunks cascade) and refusal rows: the
// re-claim reset (assembleJobClaim, next to ClearJobResultForRun) so a new flight starts with none
// of an earlier flight's.
//
// Lock order: it takes NO stored-files advisory key. It only deletes rows, which can only lower the
// sums an admission reads, so it needs no admission serialisation, and holding the keys while it
// waits on a row would stall every admission behind one slow writer. It runs as its own short
// transaction, after the claim transaction has committed (assembly is outside it), and takes only
// job_files row locks on this run's output rows and job_output_refusals rows. The paths that take
// the keys (Reserve, Sweep, recovery bind) never lock those rows while waiting on a key: Reserve
// only inserts, the Sweep's release SKIPs locked rows, and its settle and expire statements select
// input, unattached and terminal-run rows, never an output of a run that is being claimed. The one
// wait is on an in-flight Write's row lock (LockReservedJobFile) of an earlier flight, which holds
// no key and ends when that write's transaction does (bounded by the request deadline).
func (j *JobFiles) ClearRunOutputs(ctx context.Context, runID uuid.UUID) error {
	tx, err := j.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := q.DeleteJobOutputFilesForRun(ctx, pgconv.UUID(runID)); err != nil {
		return err
	}
	if _, err := q.DeleteJobOutputRefusalsForRun(ctx, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
