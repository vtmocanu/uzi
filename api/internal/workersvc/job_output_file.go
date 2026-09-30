package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_output_file.go is the job-output side of PRD #1909 M4 (D4): the worker uploads the files it
// chose to keep before it posts the job result, and the SERVER stores the report and the findings
// JSON itself from the result it stored (storeJobResultOutputs), so those two can neither bypass the
// result scrub nor be spoofed by a worker upload. A file over a cap or quota, or of a type the
// allowlist refuses, is REFUSED and recorded in job_output_refusals; the job itself is never failed
// by it.

// The display names of the two files the server generates from a stored job result. They are
// RESERVED: the worker output route refuses them, so an output carrying one is always the server's,
// and they do not count against the per-job output caps (SumRunJobFiles).
const (
	JobOutputReportName   = "report.md"
	JobOutputFindingsName = "findings.json"
)

// IsReservedJobOutputName reports whether name is one of the server-generated output names,
// compared case-insensitively so a differently cased name cannot pass for one.
func IsReservedJobOutputName(name string) bool {
	return strings.EqualFold(name, JobOutputReportName) || strings.EqualFold(name, JobOutputFindingsName)
}

// RefusalReservedName is the reason recorded when the worker uploads a file under a reserved name.
const RefusalReservedName = "reserved_name"

// The reasons the worker may report for an output it dropped itself (JobResultSubmission
// .RefusedOutputs): a fixed allowlist, so a worker cannot write free text into a refusal row.
const (
	WorkerRefusalEmpty        = "worker_empty"         // the file was empty
	WorkerRefusalTooLarge     = "worker_too_large"     // over the worker's own size or count ceilings
	WorkerRefusalUnreadable   = "worker_unreadable"    // missing, not a regular file, changed or unreadable
	WorkerRefusalUploadFailed = "worker_upload_failed" // retries exhausted or the upload phase ran out of time
	WorkerRefusalBusy         = "worker_busy"          // the api kept answering uploads_busy
)

// WorkerRefusalReasons is the allowlist of WorkerRefusal* reasons.
var WorkerRefusalReasons = map[string]bool{
	WorkerRefusalEmpty: true, WorkerRefusalTooLarge: true, WorkerRefusalUnreadable: true,
	WorkerRefusalUploadFailed: true, WorkerRefusalBusy: true,
}

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
	if IsReservedJobOutputName(p.DisplayName) {
		// report.md and findings.json are the server's own (storeJobResultOutputs).
		return JobOutputResult{}, jf.recordRefusal(ctx, runID, p.ClaimGeneration, p, refusal(RefusalInvalid, RefusalReservedName))
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
		if errors.Is(err, errJobOutputDuplicate) {
			// The uq_job_files_output_content index refused a second copy: an earlier attempt of this
			// very upload stored (or is still storing) it. The stored file is the answer; while the
			// first attempt is still streaming there is none yet, and the worker retries shortly.
			if existing, ok, ferr := jf.findOutput(ctx, runID, p); ferr != nil {
				return JobOutputResult{}, ferr
			} else if ok {
				return JobOutputResult{File: existing, Existing: true}, nil
			}
			return JobOutputResult{}, ErrJobFileUploadsBusy
		}
		return JobOutputResult{}, jf.recordRefusal(ctx, runID, p.ClaimGeneration, p, err)
	}
	stored, err := jf.Write(ctx, row.ID, run.UserID, body, opt)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			// The body outran its declared size (Write has already released the reservation).
			err = refusal(RefusalInvalid, RefusalSizeMismatch)
		}
		return JobOutputResult{}, jf.recordRefusal(ctx, runID, p.ClaimGeneration, p, err)
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
// answers with. The insert is fenced on gen, the claim generation of the flight that met the
// refusal: a stale flight's refusal that lands after a re-claim is dropped by the statement.
func (j *JobFiles) recordRefusal(ctx context.Context, runID uuid.UUID, gen int64, p JobOutputParams, err error) error {
	var ref *JobFileRefusedError
	if !errors.As(err, &ref) {
		return err
	}
	j.insertRefusal(ctx, runID, gen, p.DisplayName, max(p.Size, 0), ref.Reason)
	return err
}

// insertRefusal is the fenced, bounded refusal insert shared by the output route, the generated
// outputs and the worker-reported drops. Best effort: a failure is logged only.
func (j *JobFiles) insertRefusal(ctx context.Context, runID uuid.UUID, gen int64, displayName string, byteSize int64, reason string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, ierr := store.New(j.db).InsertJobOutputRefusal(rctx, store.InsertJobOutputRefusalParams{
		RunID:           runID,
		DisplayName:     displayName,
		ByteSize:        byteSize,
		Reason:          reason,
		ClaimGeneration: gen,
		MaxRows:         int64(j.limits.OutputsMaxFiles),
	}); ierr != nil {
		slog.Warn("job files: recording a refused output", "run", runID.String(), "error", ierr)
	}
}

// generatedFindingJSON is one finding in the generated findings.json.
type generatedFindingJSON struct {
	Severity  string  `json:"severity"`
	MessageMD string  `json:"message_md"`
	URL       *string `json:"url,omitempty"`
	File      *string `json:"file,omitempty"`
	Line      *int32  `json:"line,omitempty"`
}

// storeJobResultOutputs stores report.md (the stored, scrubbed report_md) and findings.json (the
// stored, scrubbed findings) as output files of the run, and records the outputs the worker
// reported dropping. It runs from SubmitJobResult AFTER the result transaction committed, so the
// files are built from what the result store holds (the api's scrub), never from a worker upload.
// It never fails the ingest: a cap or quota refusal is recorded in job_output_refusals, any other
// failure is logged, and the next post of the result tries again.
//
// Lock order: the result transaction (runs row FOR UPDATE, job_results, job_findings) is COMMITTED
// before anything here starts, so no runs or result row lock is held while Reserve takes the
// stored-files advisory keys (owner, then shared); Reserve's own run read is a plain read taken
// before the keys, exactly as for a worker upload. The fence is re-read after each write instead
// (fenceJobOutput), and a file whose claim went stale meanwhile is dropped, as StoreJobOutput does.
// The writes take no upload slot: the body is in memory, so the write holds a connection only for
// the insert, never for a slow client.
//
// Idempotent: a re-post with the same content stores nothing new (the unique output index, or the
// lookup); a re-post with different content replaces the earlier file of the same name.
func (s *Service) storeJobResultOutputs(ctx context.Context, wkr store.Worker, run store.Run, gen int64, sub JobResultSubmission) {
	jf := s.jobFiles
	if jf == nil {
		return
	}
	if sub.ReportMD != "" {
		s.storeGeneratedOutput(ctx, wkr, run, gen, JobOutputReportName, "text/markdown", []byte(sub.ReportMD))
	}
	findings := make([]generatedFindingJSON, 0, len(sub.Findings))
	for _, f := range sub.Findings {
		findings = append(findings, generatedFindingJSON(f))
	}
	if raw, err := json.MarshalIndent(findings, "", "  "); err != nil {
		slog.Warn("job files: encoding findings.json", "run", run.ID.String(), "error", err)
	} else {
		s.storeGeneratedOutput(ctx, wkr, run, gen, JobOutputFindingsName, "application/json", raw)
	}
	for i, r := range sub.RefusedOutputs {
		if i >= jf.limits.OutputsMaxFiles {
			break
		}
		jf.insertRefusal(ctx, run.ID, gen, r.DisplayName, 0, r.Reason)
	}
}

// storeGeneratedOutput stores one server-generated output (see storeJobResultOutputs).
func (s *Service) storeGeneratedOutput(ctx context.Context, wkr store.Worker, run store.Run, gen int64, name, contentType string, content []byte) {
	jf := s.jobFiles
	sum := sha256.Sum256(content)
	p := JobOutputParams{ClaimGeneration: gen, DisplayName: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	warn := func(msg string, err error) {
		slog.Warn("job files: "+msg, "run", run.ID.String(), "file", name, "error", err)
	}
	if _, err := store.New(jf.db).DeleteStaleGeneratedJobOutput(ctx, store.DeleteStaleGeneratedJobOutputParams{
		RunID: pgconv.UUID(run.ID), ClaimGeneration: int8Ptr(&gen), DisplayName: name, Sha256: p.SHA256,
	}); err != nil {
		warn("replacing a generated output", err)
		return
	}
	if _, ok, err := jf.findOutput(ctx, run.ID, p); err != nil {
		warn("looking up a generated output", err)
		return
	} else if ok {
		return
	}
	runID := run.ID
	row, err := jf.Reserve(ctx, ReserveParams{
		UserID:          run.UserID,
		RunID:           &runID,
		Direction:       JobFileOutput,
		ClaimGeneration: &gen,
		DisplayName:     name,
		DeclaredSize:    p.Size,
		DeclaredSHA256:  p.SHA256,
	})
	if err != nil {
		if errors.Is(err, errJobOutputDuplicate) {
			return // a concurrent post of the same result is storing it
		}
		if rerr := jf.recordRefusal(ctx, run.ID, gen, p, err); !isRefusal(rerr) {
			warn("reserving a generated output", rerr)
		}
		return
	}
	stored, err := jf.Write(ctx, row.ID, run.UserID, bytes.NewReader(content), WriteOptions{ContentType: contentType})
	if err != nil {
		if rerr := jf.recordRefusal(ctx, run.ID, gen, p, err); !isRefusal(rerr) {
			warn("writing a generated output", rerr)
		}
		return
	}
	if _, ferr := s.fenceJobOutput(ctx, wkr, run.ID, gen); ferr != nil && !errors.Is(ferr, ErrRunTerminal) {
		jf.dropStaleOutput(ctx, run.ID, stored.ID, gen)
	}
}

func isRefusal(err error) bool {
	var ref *JobFileRefusedError
	return errors.As(err, &ref)
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
// The deletes SKIP a row an in-flight write of the earlier flight holds FOR UPDATE instead of
// waiting on it, so the claim response never blocks on an old flight's write; that write's own
// post-write fence (StoreJobOutput, dropStaleOutput) drops the file as soon as it commits.
//
// Lock order: it takes NO stored-files advisory key. It only deletes rows, which can only lower the
// sums an admission reads, so it needs no admission serialisation, and holding the keys while it
// waits on a row would stall every admission behind one slow writer. It runs as its own short
// transaction, after the claim transaction has committed (assembly is outside it), and takes only
// job_files row locks on this run's output rows and job_output_refusals rows. The paths that take
// the keys (Reserve, Sweep, recovery bind) never lock those rows while waiting on a key: Reserve
// only inserts, the Sweep's release SKIPs locked rows, and its settle and expire statements select
// input, unattached and terminal-run rows, never an output of a run that is being claimed. It
// waits on no row lock at all (FOR UPDATE SKIP LOCKED).
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
