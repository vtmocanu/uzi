package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// job_result.go is the worker's job-result ingest (PRD #1908): the one structured result a
// `job` run posts before it reports `completed`, and the no-result invariant that backs it.

// Job-result bounds. The job_results / job_findings CHECKs (migration 00275) are the backstop;
// these are the ingest caps the handler enforces before anything is persisted.
const (
	JobResultMaxFindings      = 200
	JobResultReportMaxBytes   = 1 << 20  // job_results.report_md CHECK
	JobFindingMessageMaxBytes = 64 << 10 // job_findings.message_md CHECK
	JobFindingURLMaxBytes     = 2048     // job_findings.url CHECK
	JobFindingFileMaxBytes    = 1024     // job_findings.file CHECK
	JobFindingMaxLine         = 1 << 24  // a sane ceiling well inside int4
	// JobResultMaxRefusedOutputs bounds the worker-reported dropped outputs of one result: the
	// worker's own file ceiling. The service further bounds what it records by OutputsMaxFiles.
	JobResultMaxRefusedOutputs   = 64
	jobNoResultFailureReasonText = "The job finished without submitting a result."
)

// JobFindingSeverities is the closed severity enum of a job finding (the job_findings CHECK).
var JobFindingSeverities = map[string]bool{"info": true, "warning": true, "error": true}

var (
	// ErrNotJobRun: the run named by a job-only route is not a kind='job' run (403).
	ErrNotJobRun = errors.New("run is not a job run")
	// errJobResultNoTransaction: a live store without a transaction beginner cannot ingest a
	// result atomically.
	errJobResultNoTransaction = errors.New("workersvc: job result needs a transaction beginner")
)

// JobFindingSubmission is one already-validated, scrubbed finding.
type JobFindingSubmission struct {
	Severity  string
	MessageMD string
	URL       *string
	File      *string
	Line      *int32
}

// JobRefusedOutput is an output the worker reports it dropped itself (see WorkerRefusalReasons):
// DisplayName already sanitised, Reason from the allowlist.
type JobRefusedOutput struct {
	DisplayName string
	Reason      string
}

// JobResultSubmission is a validated, scrubbed job result.
type JobResultSubmission struct {
	Status   string
	ReportMD string
	Findings []JobFindingSubmission
	// RefusedOutputs are the worker-side drops, deduped and bounded by the handler.
	RefusedOutputs []JobRefusedOutput
}

// SubmitJobResult persists a job run's result for the worker that holds it. The run row is locked
// FOR UPDATE and fenced (owned by wkr, kind='job', the stamped claim generation still current and
// unreleased, not terminal) and then the result is upserted, the run's earlier findings deleted
// and the new ones inserted, all in ONE transaction: a retried POST replaces the earlier body and
// a failure part-way leaves the earlier result untouched.
//
// The generation_pending markers and the outputs the worker reported dropping are recorded in the
// same transaction. After the commit
// the server stores the result's report.md and findings.json as output files (startJobResultOutputs;
// its comment has the lock order and the bounds: those writes run OUTSIDE this transaction, on a
// context detached from the request, and the reply waits for them at most a few seconds). They never
// fail the ingest.
//
// Errors: ErrRunNotFound (not held by this worker), ErrNotJobRun, ErrStaleClaim (generation
// mismatch or released claim), ErrRunTerminal.
func (s *Service) SubmitJobResult(ctx context.Context, wkr store.Worker, runID uuid.UUID, claimGeneration int64, sub JobResultSubmission) (err error) {
	if s.txBeginner == nil {
		return errJobResultNoTransaction
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := store.New(tx)
	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRunNotFound
		}
		return err
	}
	if run.Kind != runkind.Job {
		return ErrNotJobRun
	}
	if run.ClaimGeneration != claimGeneration || run.ClaimReleasedAt.Valid {
		return ErrStaleClaim
	}
	if terminalStatuses[run.Status] {
		return ErrRunTerminal
	}
	if err = qtx.UpsertJobResult(ctx, store.UpsertJobResultParams{RunID: runID, Status: sub.Status, ReportMd: sub.ReportMD}); err != nil {
		return err
	}
	if err = qtx.DeleteJobFindingsForRun(ctx, runID); err != nil {
		return err
	}
	for i, f := range sub.Findings {
		if err = qtx.InsertJobFinding(ctx, store.InsertJobFindingParams{
			RunID:     runID,
			Ordinal:   int32(i), // #nosec G115 -- bounded by JobResultMaxFindings
			Severity:  f.Severity,
			MessageMd: f.MessageMD,
			Url:       jobTextPtr(f.URL),
			File:      jobTextPtr(f.File),
			Line:      pgconv.Int4Ptr32(f.Line),
		}); err != nil {
			return err
		}
	}
	// The generation_pending markers of the files the server will generate, and the outputs the
	// worker reported dropping, are recorded here, in the result transaction, so neither depends on
	// the detached generation below (fenced on the claim generation and bounded by the per-run
	// output file cap in the statement itself). The markers make a generation that never finishes
	// (crash, kill) visible instead of silent: see RefusalGenerationPending.
	if s.jobFiles != nil {
		if err = s.jobFiles.markGenerationPending(ctx, qtx, runID, claimGeneration, sub); err != nil {
			return err
		}
		for i, r := range sub.RefusedOutputs {
			if i >= s.jobFiles.limits.OutputsMaxFiles {
				break
			}
			if _, err = qtx.InsertJobOutputRefusal(ctx, store.InsertJobOutputRefusalParams{
				RunID: runID, DisplayName: r.DisplayName, Reason: r.Reason,
				ClaimGeneration: claimGeneration, MaxRows: int64(s.jobFiles.limits.OutputsMaxFiles),
			}); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	s.startJobResultOutputs(ctx, wkr, run, claimGeneration, sub)
	return nil
}

// CheckJobResultTarget is the cheap pre-body check of the job-result route: the run must be held
// by wkr (ErrRunNotFound otherwise) and be a kind='job' run (ErrNotJobRun). It is a plain read;
// SubmitJobResult's FOR UPDATE fence remains the authority for claim generation and status.
func (s *Service) CheckJobResultTarget(ctx context.Context, wkr store.Worker, runID uuid.UUID) error {
	run, err := s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRunNotFound
		}
		return err
	}
	if run.Kind != runkind.Job {
		return ErrNotJobRun
	}
	return nil
}

func jobTextPtr(p *string) pgtype.Text {
	if p == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *p, Valid: true}
}

// RunIsJobForRoute reports whether the run a worker route names is a kind='job' run, for the
// not_for_job refusal (PRD #1908). It fails open to "not a job" whenever the run is not visible to
// the worker (unknown, held by another worker, another user's), so the refusal never tells a
// worker anything about a run it could not already touch and every non-job response is unchanged.
//
//   - reviewed=false: {id} is the run the worker HOLDS (publish, memory, forge): the existing
//     GetRunOwnedByWorker lookup.
//   - reviewed=true: {id} is the REVIEWED run of a judge/review flight (trace, review,
//     task-review), which the reviewing worker does not hold: GetRunByID, visible only to a
//     worker of the run's own user (the same owner equality authorizeTaskReviewTarget asserts).
func (s *Service) RunIsJobForRoute(ctx context.Context, wkr store.Worker, runID uuid.UUID, reviewed bool) (bool, error) {
	var (
		run store.Run
		err error
	)
	if reviewed {
		run, err = s.q.GetRunByID(ctx, runID)
	} else {
		run, err = s.q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: runID, WorkerID: pgconv.UUID(wkr.ID)})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if reviewed && run.UserID != wkr.UserID {
		return false, nil
	}
	return run.Kind == runkind.Job, nil
}
