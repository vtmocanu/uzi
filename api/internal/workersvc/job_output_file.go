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
	"runtime/debug"
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

// Bounds of the generated-output storage (startJobResultOutputs). The ingest reply waits for it at
// most generatedOutputsReplyBound, well inside the server's write timeout and the worker's http
// timeout, and the storage itself is cut off after generatedOutputsTimeout.
const (
	generatedOutputsReplyBound = 3 * time.Second
	generatedOutputsTimeout    = 60 * time.Second
	// generatedOutputsDrainBound is how long shutdown waits for generations in flight. The HTTP
	// drain takes up to 10 s and the default termination grace is 30 s (Kubernetes; Compose's stop
	// grace is only 10 s), so this stays well inside the remainder.
	generatedOutputsDrainBound = 15 * time.Second
)

// The reasons recorded, under a reserved name, for a generated output. The refusal rows of a run
// are what its result listing shows as refused files (PRD #1909 M5 reads job_output_refusals), so
// the three generation reasons are a STATE MACHINE the listing must present accordingly:
//
//   - generation_pending: NOT a refusal. The result transaction (SubmitJobResult) writes one per
//     file the server will generate; it means "report.md / findings.json is being generated and is
//     not stored yet". It is deleted when the file is stored, and replaced by the outcome below when
//     the generation gives up. A row that is still pending when the run is read is either a
//     generation in flight (seconds) or one that was abandoned (the process stopped or crashed, or
//     shutdown did not wait for it), which leaves the marker behind on purpose: the file is not
//     there, and nothing else would say so. The result listing should show it as "not available
//     yet / may never arrive", never as a refusal by policy, and never hide it.
//   - generation_timeout: the storage did not finish within its bound.
//   - generation_failed: the storage failed for a non-refusal reason (an error, a panic, shutdown,
//     or too many generations in flight).
//
// A file the generation could not store for a policy reason carries that refusal's own reason
// (file_too_large, a quota) in place of the marker. Rows with a worker_* reason and the
// reserved_name refusal are the worker's, never the generation's (the queries tell them apart).
const (
	RefusalGenerationPending = "generation_pending"
	RefusalGenerationTimeout = "generation_timeout"
	RefusalGenerationFailed  = "generation_failed"
)

// generatedOutputNames are the two names the server generates. The markers of both are rewritten on
// every post, so a re-post that no longer generates report.md (an empty report) drops its marker.
var generatedOutputNames = []string{JobOutputReportName, JobOutputFindingsName}

// genJob is one result's generated-output storage: everything storeJobResultOutputs needs, and the
// channel closed when the job is finished or superseded.
type genJob struct {
	wkr  store.Worker
	run  store.Run
	gen  int64
	sub  JobResultSubmission
	done chan struct{}
}

// runGen is the per-run generation state. Its presence in Service.genRuns means one generation of
// the run is running; pending is the single queued one behind it.
type runGen struct {
	pending *genJob
}

// generatedNames lists the names a submission would generate: report.md only for a non-empty
// report, findings.json always.
func generatedNames(sub JobResultSubmission) []string {
	if sub.ReportMD != "" {
		return []string{JobOutputReportName, JobOutputFindingsName}
	}
	return []string{JobOutputFindingsName}
}

// recordGenerationRefusals turns the generation_pending marker of each name the job would have
// generated into generation_failed, for a job that never ran (or panicked). Best effort: it runs on
// its own short detached context.
func (s *Service) recordGenerationRefusals(job *genJob) {
	for _, name := range generatedNames(job.sub) {
		s.jobFiles.settleGenerated(context.Background(), job.run.ID, job.gen, name, 0, RefusalGenerationFailed)
	}
}

// markGenerationPending writes, inside the result transaction, the generation_pending marker of
// each file the server will generate for sub, after clearing the generation-owned rows of both
// reserved names (an earlier post's marker, failure or refusal): a re-post rewrites its own markers
// and leaves no stale one. Because the markers commit WITH the result, a crash or kill between the
// commit and the storage leaves them behind instead of silence. They go first, ahead of the
// worker-reported refusals, so the per-run row cap cannot crowd them out.
func (j *JobFiles) markGenerationPending(ctx context.Context, q *store.Queries, runID uuid.UUID, gen int64, sub JobResultSubmission) error {
	if _, err := q.DeleteGeneratedOutputRefusals(ctx, store.DeleteGeneratedOutputRefusalsParams{
		RunID: runID, DisplayNames: generatedOutputNames, ClaimGeneration: gen,
	}); err != nil {
		return err
	}
	for _, name := range generatedNames(sub) {
		if _, err := q.InsertJobOutputRefusal(ctx, store.InsertJobOutputRefusalParams{
			RunID: runID, DisplayName: name, Reason: RefusalGenerationPending,
			ClaimGeneration: gen, MaxRows: int64(j.limits.OutputsMaxFiles),
		}); err != nil {
			return err
		}
	}
	return nil
}

// settleGenerated records the outcome of a generated output that was not stored: the marker is
// updated in place to reason (SettleGeneratedOutputRefusal), or, when there is none, the refusal is
// inserted. Best effort on a fresh short context, like insertRefusal: a failure is logged and the
// marker stays pending.
func (j *JobFiles) settleGenerated(ctx context.Context, runID uuid.UUID, gen int64, name string, byteSize int64, reason string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	n, err := store.New(j.db).SettleGeneratedOutputRefusal(rctx, store.SettleGeneratedOutputRefusalParams{
		RunID: runID, DisplayName: name, ByteSize: byteSize, Reason: reason, ClaimGeneration: gen,
	})
	if err != nil {
		slog.Warn("job files: settling a generated output marker", "run", runID.String(), "file", name, "error", err)
		return
	}
	if n == 0 {
		j.insertRefusal(ctx, runID, gen, name, byteSize, reason)
	}
}

// clearGenerated deletes the generation-owned rows of name once its file is stored. Idempotent and
// best effort (a failure is logged and leaves a stale pending marker next to a stored file, which a
// re-post or re-claim clears). It runs right after the file's commit, not in it: a crash between
// the two leaves the file and a pending marker, never a missing file without a marker.
func (j *JobFiles) clearGenerated(ctx context.Context, runID uuid.UUID, gen int64, name string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := store.New(j.db).DeleteGeneratedOutputRefusals(rctx, store.DeleteGeneratedOutputRefusalsParams{
		RunID: runID, DisplayNames: []string{name}, ClaimGeneration: gen,
	}); err != nil {
		slog.Warn("job files: clearing a generated output marker", "run", runID.String(), "file", name, "error", err)
	}
}

// generatedOutputsMax is the process-wide cap on runs with a generation in flight: the upload write
// slot bound (already clamped to the database pool), at least 1.
func (s *Service) generatedOutputsMax() int {
	if s.genMax > 0 {
		return s.genMax
	}
	return max(1, s.jobFiles.limits.MaxConcurrentWrites)
}

// DrainGeneratedOutputs is the shutdown half of startJobResultOutputs: it makes the service refuse
// new generations (recorded as generation_failed) and waits for the ones already started, at most
// generatedOutputsDrainBound, and reports whether they all finished. The caller
// MUST run it after the HTTP server has drained (no request can start one any more) and BEFORE the
// database pool closes (a generation stores through the pool). A pending generation behind a
// running one is recorded generation_failed instead of started, so the wait is one storage long.
// The wait is capped at generatedOutputsDrainBound (far below generatedOutputsTimeout) so shutdown
// fits the default termination grace; a generation still running then is left to its
// generation_pending marker (written in the result transaction), which a hard stop leaves visible.
func (s *Service) DrainGeneratedOutputs() bool {
	s.genMu.Lock()
	s.genClosed = true
	s.genMu.Unlock()
	done := make(chan struct{})
	go func() { s.genWG.Wait(); close(done) }()
	bound := generatedOutputsDrainBound
	if s.genDrainBound > 0 {
		bound = s.genDrainBound
	}
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// WaitForGeneratedOutputs blocks until every generated-output storage started by SubmitJobResult
// has finished. The storage runs detached from the request, so a caller that must observe its
// effect (a test) waits here; shutdown uses DrainGeneratedOutputs, which also refuses new work.
func (s *Service) WaitForGeneratedOutputs() { s.genWG.Wait() }

// startJobResultOutputs stores report.md (the stored, scrubbed report_md) and findings.json (the
// stored, scrubbed findings) as output files of the run. It runs from SubmitJobResult AFTER the
// result transaction committed, so the files are built from what the result store holds (the api's
// scrub), never from a worker upload. The worker-reported dropped outputs are recorded in the
// result transaction itself, independent of this.
//
// Bounded design: per run at most ONE generation runs and at most ONE waits behind it, and a newer
// post replaces the waiting one (latest wins; the replaced one is superseded, recorded nowhere,
// because the newer post carries the content the result store now holds). Process-wide at most
// generatedOutputsMax runs have a generation in flight; a post beyond that is recorded as
// generation_failed. So a worker re-posting with varying content holds at most two submissions
// (about 4 MiB each) per run and one goroutine per run, never a queue of them. The per-run state
// also serialises the replace-then-insert of a run's files, so two posts cannot leave two report.md
// rows.
//
// Each generation runs in a goroutine tracked by the service (genWG) on a context DETACHED from the
// request (context.WithoutCancel) with its own timeout (generatedOutputsTimeout): a worker that
// disconnects does not cancel it, and a slow one does not fail the request. The caller waits for
// its job at most generatedOutputsReplyBound (or until its own ctx ends) and then answers
// regardless, because the result is already committed and a late reply would only make the worker
// treat a stored result as failed. An output it cannot store (a refusal, the timeout, a panic, any
// other failure) is recorded in job_output_refusals (RefusalGenerationTimeout /
// RefusalGenerationFailed for the last two), never dropped silently and never failing the ingest.
// After DrainGeneratedOutputs, a post is refused with generation_failed instead of started.
//
// Lock order: the result transaction (runs row FOR UPDATE, job_results, job_findings) is COMMITTED
// before anything here starts, so no runs or result row lock is held while Reserve takes the
// stored-files advisory keys (owner, then shared); Reserve's own run read is a plain read taken
// before the keys, exactly as for a worker upload. genMu is only ever held for map bookkeeping,
// never across a database call. The fence is re-read after each write instead (fenceJobOutput), and
// a file whose claim went stale meanwhile is dropped, as StoreJobOutput does. The writes take no
// upload slot: the body is in memory, so the write holds a connection only for the insert, never
// for a slow client.
//
// Idempotent: a re-post with the same content stores nothing new (the unique output index, or the
// lookup); a re-post with different content replaces the earlier file of the same name.
func (s *Service) startJobResultOutputs(ctx context.Context, wkr store.Worker, run store.Run, gen int64, sub JobResultSubmission) {
	if s.jobFiles == nil {
		return
	}
	job := &genJob{wkr: wkr, run: run, gen: gen, sub: sub, done: make(chan struct{})}
	s.genMu.Lock()
	switch st := s.genRuns[run.ID]; {
	case s.genClosed:
		s.genMu.Unlock()
		s.recordGenerationRefusals(job)
		return
	case st != nil:
		if st.pending != nil {
			close(st.pending.done) // superseded by the newer post
		}
		st.pending = job
		s.genMu.Unlock()
	case len(s.genRuns) >= s.generatedOutputsMax():
		s.genMu.Unlock()
		slog.Warn("job files: too many generations in flight, output not stored", "run", run.ID.String())
		s.recordGenerationRefusals(job)
		return
	default:
		if s.genRuns == nil {
			s.genRuns = map[uuid.UUID]*runGen{}
		}
		s.genRuns[run.ID] = &runGen{}
		s.genWG.Add(1)
		s.genMu.Unlock()
		go s.runGenerations(job)
	}
	t := time.NewTimer(s.generatedOutputsReplyBound())
	defer t.Stop()
	select {
	case <-job.done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// runGenerations runs a run's generation and then the one pending behind it, until none is left;
// it owns the run's entry in genRuns and the genWG count taken by startJobResultOutputs. A pending
// job met after shutdown began is recorded generation_failed instead of run.
func (s *Service) runGenerations(job *genJob) {
	defer s.genWG.Done()
	closed := false
	for {
		if closed {
			s.recordGenerationRefusals(job)
			close(job.done)
		} else {
			s.runGeneration(job)
		}
		s.genMu.Lock()
		st := s.genRuns[job.run.ID]
		next := st.pending
		st.pending = nil
		if next == nil {
			delete(s.genRuns, job.run.ID)
			s.genMu.Unlock()
			return
		}
		closed = s.genClosed
		s.genMu.Unlock()
		job = next
	}
}

// runGeneration runs one job under its own timeout and closes its done channel. A panic is
// recovered (the goroutine is detached, so nothing above it would catch it and the api would
// exit), logged, and recorded generation_failed for every name the job would have generated.
func (s *Service) runGeneration(job *genJob) {
	gctx, cancel := context.WithTimeout(context.Background(), s.generatedOutputsTimeout())
	defer cancel()
	defer close(job.done)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("job files: panic storing generated outputs", "run", job.run.ID.String(), "panic", r, "stack", string(debug.Stack()))
			s.recordGenerationRefusals(job)
		}
	}()
	if s.genHook != nil {
		s.genHook(job)
	}
	s.storeJobResultOutputs(gctx, job.wkr, job.run, job.gen, job.sub)
}

func (s *Service) generatedOutputsTimeout() time.Duration {
	if s.genTimeout > 0 {
		return s.genTimeout
	}
	return generatedOutputsTimeout
}

func (s *Service) generatedOutputsReplyBound() time.Duration {
	if s.genReplyBound > 0 {
		return s.genReplyBound
	}
	return generatedOutputsReplyBound
}

// storeJobResultOutputs is the body of one generation (runGeneration); ctx carries the storage
// deadline. Name, content type and scrubbing rules: see startJobResultOutputs.
func (s *Service) storeJobResultOutputs(ctx context.Context, wkr store.Worker, run store.Run, gen int64, sub JobResultSubmission) {
	jf := s.jobFiles
	if jf == nil {
		return
	}
	// Record a generation that did not finish: report.md and findings.json are the only names
	// this path writes. A failure observed after ctx expired is labelled generation_timeout even
	// when the underlying error merely raced the deadline: the deadline is the likelier cause and
	// the label is an operator hint, not a diagnosis.
	record := func(name string, err error) {
		if err == nil {
			return
		}
		reason := RefusalGenerationFailed
		if ctx.Err() != nil {
			reason = RefusalGenerationTimeout
		}
		slog.Warn("job files: generated output not stored", "run", run.ID.String(), "file", name, "reason", reason, "error", err)
		jf.settleGenerated(ctx, run.ID, gen, name, 0, reason)
	}
	if sub.ReportMD != "" {
		record(JobOutputReportName, s.storeGeneratedOutput(ctx, wkr, run, gen, JobOutputReportName, "text/markdown", []byte(sub.ReportMD)))
	}
	findings := make([]generatedFindingJSON, 0, len(sub.Findings))
	for _, f := range sub.Findings {
		findings = append(findings, generatedFindingJSON(f))
	}
	// SetEscapeHTML(false): json.Marshal would write <, > and & as \u003c and friends (six bytes for
	// one), inflating findings full of markup past the per-file cap for no reason.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(findings); err != nil {
		record(JobOutputFindingsName, err)
	} else {
		record(JobOutputFindingsName, s.storeGeneratedOutput(ctx, wkr, run, gen, JobOutputFindingsName, "application/json", buf.Bytes()))
	}
}

// storeGeneratedOutput stores one server-generated output (see startJobResultOutputs). A cap or
// quota refusal is recorded here and returns nil; any other failure is returned for the caller to
// record.
func (s *Service) storeGeneratedOutput(ctx context.Context, wkr store.Worker, run store.Run, gen int64, name, contentType string, content []byte) error {
	jf := s.jobFiles
	sum := sha256.Sum256(content)
	p := JobOutputParams{ClaimGeneration: gen, DisplayName: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
	if p.Size > jf.limits.OutputFileMaxBytes {
		// Refused before any lookup or reservation: over the per-file cap, recorded like any refusal.
		jf.settleGenerated(ctx, run.ID, gen, name, p.Size, RefusalFileTooLarge)
		return nil
	}
	if _, err := store.New(jf.db).DeleteStaleGeneratedJobOutput(ctx, store.DeleteStaleGeneratedJobOutputParams{
		RunID: pgconv.UUID(run.ID), ClaimGeneration: int8Ptr(&gen), DisplayName: name, Sha256: p.SHA256,
	}); err != nil {
		return err
	}
	if _, ok, err := jf.findOutput(ctx, run.ID, p); err != nil {
		return err
	} else if ok {
		jf.clearGenerated(ctx, run.ID, gen, name)
		return nil
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
			return nil // a concurrent post of the same result is storing it; it clears the marker
		}
		return s.settleRefusal(ctx, run.ID, gen, p, err)
	}
	stored, err := jf.Write(ctx, row.ID, run.UserID, bytes.NewReader(content), WriteOptions{ContentType: contentType})
	if err != nil {
		return s.settleRefusal(ctx, run.ID, gen, p, err)
	}
	if _, ferr := s.fenceJobOutput(ctx, wkr, run.ID, gen); ferr != nil && !errors.Is(ferr, ErrRunTerminal) {
		jf.dropStaleOutput(ctx, run.ID, stored.ID, gen)
		return nil
	}
	jf.clearGenerated(ctx, run.ID, gen, name)
	return nil
}

// settleRefusal turns a refusal of a generated output into its row (replacing the pending marker)
// and returns nil; any other error is returned for the caller to record as a generation failure.
func (s *Service) settleRefusal(ctx context.Context, runID uuid.UUID, gen int64, p JobOutputParams, err error) error {
	var ref *JobFileRefusedError
	if !errors.As(err, &ref) {
		return err
	}
	s.jobFiles.settleGenerated(ctx, runID, gen, p.DisplayName, max(p.Size, 0), ref.Reason)
	return nil
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
