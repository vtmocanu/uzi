package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// jobWallBackstopGraceSeconds is how long past its wall budget a claimed or running job may
// stay before the server backstop fails it. The job runner aborts itself at budget_wall_seconds
// and reports failed, so this only fires for a runner that died or wedged; the grace keeps the
// backstop from racing a runner that is reporting its own failure at the deadline.
const jobWallBackstopGraceSeconds = 300

// Failure reasons stamped on a job whose run-bound ephemeral worker cannot serve it. They are
// user-visible through the job's failure_reason, so they name the cause and the fix.
const (
	jobFailNoCapableWorker    = "the worker provisioned for this job does not support jobs; update the worker image"
	jobFailWorkerNeverArrived = "the worker provisioned for this job never registered before the provisioning deadline"
	jobFailPastWallDeadline   = "the job exceeded its time budget"
)

// FailJobsWithUnservableEphemeral is the PRD #1908 D-A2 sweeper pass: a run-bound ephemeral
// worker that can never serve its kind='job' run fails the job instead of leaving it queued for
// the gap trigger to re-provision forever. It is wired BEFORE the ephemeral ReapPass, which would
// otherwise delete a never-booted worker silently and leave the still-queued job to be
// re-provisioned every deadline.
//
// For each ephemeral worker bound to a job that either registered without job_runner_v1 or never
// registered by the provision deadline (provisionDeadline before now), in ONE transaction: the
// job is failed by a conditional UPDATE (status='queued' AND worker_id IS NULL, so a capable
// worker that claimed the job first keeps it), then the worker row is deleted with the same
// busy and custody guards DeleteEphemeralWorkerForRun carries (the controller tears the pod down
// on row absence). A job that was claimed elsewhere only loses the stale ephemeral row. It
// returns the number of jobs failed. Like ReapPass it is not gated on the ephemeral kill-switch:
// a stack that turned the feature off must still clean its bound workers.
func (s *Service) FailJobsWithUnservableEphemeral(ctx context.Context, provisionDeadline time.Duration) (int64, error) {
	if s.txBeginner == nil {
		return 0, errors.New("workersvc: FailJobsWithUnservableEphemeral needs a transaction beginner")
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	cutoff := pgconv.Time(s.now().Add(-provisionDeadline))
	rows, err := qtx.LockUnservableEphemeralJobWorkers(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("lock unservable ephemeral job workers: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	var failed []store.FailUnservedJobRunRow
	for _, r := range rows {
		reason := jobFailNoCapableWorker
		if r.Cause == "ephemeral_worker_never_registered" {
			reason = jobFailWorkerNeverArrived
		}
		out, err := qtx.FailUnservedJobRun(ctx, store.FailUnservedJobRunParams{
			RunID:         r.RunID,
			FailureReason: pgconv.TextOrNull(reason),
			FailOrigin:    r.Cause,
		})
		if err != nil {
			return 0, fmt.Errorf("fail unserved job run %s: %w", r.RunID, err)
		}
		failed = append(failed, out...)
		// Delete in every case: when the job was failed the worker is dead weight, and when a
		// capable worker claimed the job first only the stale ephemeral row goes. The delete
		// keeps the busy and custody guards, so a held worker survives to a later tick.
		if _, err := qtx.DeleteEphemeralWorkerForRun(ctx, r.RunID); err != nil {
			return 0, fmt.Errorf("delete unservable ephemeral worker %s: %w", r.WorkerID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for _, f := range failed {
		s.publishSwept(f.ID, f.Status)
		slog.Info("sweeper: failed a job whose run-bound ephemeral worker cannot serve it", "run_id", f.ID)
	}
	return int64(len(failed)), nil
}

// FailJobsPastWallDeadline is the PRD #1908 D-E deadline backstop: a job never parks at its wall
// clock (the wall-park passes exclude it), so a claimed or running job past its budget plus a
// grace is failed with fail_origin='run_timeout'. globalTimeout is the fallback for a job row
// with no budget_wall_seconds. It returns the number of jobs failed.
func (s *Service) FailJobsPastWallDeadline(ctx context.Context, globalTimeout time.Duration) (int64, error) {
	rows, err := s.q.FailJobsPastWallDeadline(ctx, store.FailJobsPastWallDeadlineParams{
		FailureReason:        pgconv.TextOrNull(jobFailPastWallDeadline),
		Now:                  pgconv.Time(s.now()),
		GlobalTimeoutSeconds: int32(globalTimeout.Seconds()),
		GraceSeconds:         jobWallBackstopGraceSeconds,
	})
	if err != nil {
		return 0, fmt.Errorf("fail jobs past wall deadline: %w", err)
	}
	for _, r := range rows {
		s.publishSwept(r.ID, r.Status)
	}
	return int64(len(rows)), nil
}

// jobRevokeSweepBatch bounds one product-revoke pass. A backlog larger than this drains over
// the next ticks; a cancelled job leaves the selection, so the pass makes progress.
const jobRevokeSweepBatch = 200

// jobRevokeCancelReason is the operator-visible stop reason of a job cancelled by the sweep.
const jobRevokeCancelReason = "the product credential that created this job was revoked, the product was disabled, or the owner was deactivated"

// CancelRevokedProductJobs is the PRD #1908 D14 sweeper pass: it cancels every non-terminal
// job whose creating product token was explicitly revoked, whose product is disabled or deleted,
// or whose owner is deactivated (ListRevokedProductJobs holds the exact predicate, including why
// token expiry and a uzc_-created job are not selected). One idempotent rule covers every revoke
// path, so no revoke handler carries a hook.
//
// Each job is cancelled through the existing cancel path (SubmitInputWithOptions, as the owner):
// a queued job ends cancelled server-side at once, a running one gets a cancel input its job
// runner polls and honours. A job that already has a pending cancel is not selected, so a re-run
// is a no-op. A job that finished between the select and the cancel is skipped; any other
// per-job failure is logged and joined into the returned error after the rest are processed.
// It returns the number of cancels issued.
func (s *Service) CancelRevokedProductJobs(ctx context.Context) (int64, error) {
	rows, err := s.q.ListRevokedProductJobs(ctx, jobRevokeSweepBatch)
	if err != nil {
		return 0, fmt.Errorf("list revoked product jobs: %w", err)
	}
	var (
		n    int64
		errs []error
	)
	for _, r := range rows {
		_, err := s.SubmitInputWithOptions(ctx, r.UserID, r.ID, "cancel", jobRevokeCancelReason, nil, SubmitInputOptions{})
		switch {
		case err == nil:
			n++
			slog.Info("sweeper: cancelled a job whose product authorization was revoked", "run_id", r.ID)
		case errors.Is(err, ErrRunTerminal), errors.Is(err, ErrRunNotFound), errors.Is(err, ErrOutcomePendingConfirmationRequired):
			// Finished (or journaled a terminal outcome awaiting delivery) since the select.
		default:
			slog.Warn("sweeper: cancelling a revoked product job failed", "run_id", r.ID, "err", err)
			errs = append(errs, fmt.Errorf("cancel job %s: %w", r.ID, err))
		}
	}
	return n, errors.Join(errs...)
}
