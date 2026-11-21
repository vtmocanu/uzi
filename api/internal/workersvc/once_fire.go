package workersvc

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4512 (ADR-4007 T3a): durable per-fire progress for one-time sweeps. A scheduled
// once-sweep fire passes an OnceFireProgress to CreateScheduledRun / CreateScheduledAutopilotRun,
// and the run, the fire header and the ledger row commit in the SAME transaction.

var (
	// ErrOnceFireStale: the fire's schedule is gone, no longer an active once sweep, was
	// re-armed (fire_epoch moved), or its owner changed. Nothing was written.
	ErrOnceFireStale = errors.New("once fire: the schedule occurrence is stale")
	// ErrOnceFireIdentity: the progress context disagrees with the create arguments or with the
	// locked schedule row. A bug guard: nothing was written.
	ErrOnceFireIdentity = errors.New("once fire: progress identity does not match the create")
	// ErrOnceFireCandidateRecorded: this issue already holds a slot of the fire. No run was created.
	ErrOnceFireCandidateRecorded = errors.New("once fire: candidate already recorded for this fire")
	// ErrOnceFireCapSpent: the fire's stored cap is already reached. No run was created.
	ErrOnceFireCapSpent = errors.New("once fire: the fire's cap is spent")
	// errOnceFireRequiresTx: the admission needs one transaction covering the locks, the run
	// insert and the ledger row; with no transaction there is no safe path.
	errOnceFireRequiresTx = errors.New("once fire run creation requires an atomic transaction")
)

// onceFireRunsPkey is the ledger's explicitly named primary key (migration 00502). A 23505 on
// it is a lost race for the same issue, not an active-run conflict.
const onceFireRunsPkey = "schedule_once_fire_runs_pkey"

// OnceFireProgress is the scheduler's per-admission context for a scheduled one-time sweep
// fire. Nil (every other caller: RunNow, recurring sweeps, the issue target) is exactly
// today's behaviour.
type OnceFireProgress struct {
	ScheduleID uuid.UUID
	UserID     uuid.UUID
	RepoID     uuid.UUID
	FireEpoch  int64
	IssueIID   int64
	Title      string
	WebURL     string
	// EffectiveCap is the fire's cap, recorded once in the header; nil is unlimited. An
	// existing header's stored cap wins over this value.
	EffectiveCap *int
}

// checkOnceFireArgs fails closed before any lock when the progress context disagrees with the
// create arguments (a mismatched optional context is never trusted).
func checkOnceFireArgs(p *OnceFireProgress, userID, repoID uuid.UUID, issueIID int64) error {
	if p.UserID != userID || p.RepoID != repoID || p.IssueIID != issueIID {
		return ErrOnceFireIdentity
	}
	_, err := onceFireCapArg(p.EffectiveCap)
	return err
}

// onceFireCapArg converts the fire's cap to the header's int4. nil is unlimited (a NULL cap); a
// value outside 0..MaxInt32 is never narrowed and fails closed as ErrOnceFireIdentity.
func onceFireCapArg(c *int) (pgtype.Int4, error) {
	if c == nil {
		return pgtype.Int4{}, nil
	}
	v := *c
	if v < 0 || v > math.MaxInt32 {
		return pgtype.Int4{}, ErrOnceFireIdentity
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}, nil
}

// isOnceFireLedgerConflict reports a 23505 on the ledger's primary key.
func isOnceFireLedgerConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == onceFireRunsPkey
}

// admitOnceFire is steps 6-9 of the create transaction's lock order (after the pinned secret
// share lock, before CreateRun): the repo row FOR SHARE (repo deletion locks the repo and then
// cascades to its schedules, so the schedule must come second), the schedule row FOR UPDATE, the
// recheck of the locked row, then the header, the stored cap and the spent-slot checks. Any
// refusal returns before anything is created.
func admitOnceFire(ctx context.Context, q *store.Queries, p *OnceFireProgress, userID, repoID uuid.UUID) error {
	if _, err := q.LockRepoForCIWatch(ctx, repoID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOnceFireStale // the repo (and with it the schedule) is gone
		}
		return err
	}
	sched, err := q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: p.ScheduleID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOnceFireStale // deleted, or no longer this owner's
		}
		return err
	}
	if sched.Timing != "once" || sched.Target != "sweep" || sched.Status != "active" || sched.FireEpoch != p.FireEpoch {
		return ErrOnceFireStale
	}
	if sched.RepoID != repoID {
		return ErrOnceFireIdentity
	}
	capArg, err := onceFireCapArg(p.EffectiveCap) // already vetted pre-lock by checkOnceFireArgs
	if err != nil {
		return err
	}
	if err := q.InsertOnceFireHeader(ctx, store.InsertOnceFireHeaderParams{ScheduleID: p.ScheduleID, FireEpoch: p.FireEpoch, EffectiveCap: capArg}); err != nil {
		return err
	}
	stored, err := q.GetOnceFireCap(ctx, store.GetOnceFireCapParams{ScheduleID: p.ScheduleID, FireEpoch: p.FireEpoch})
	if err != nil {
		return err
	}
	recorded, err := q.OnceFireRunExists(ctx, store.OnceFireRunExistsParams{ScheduleID: p.ScheduleID, FireEpoch: p.FireEpoch, IssueIid: p.IssueIID})
	if err != nil {
		return err
	}
	if recorded {
		return ErrOnceFireCandidateRecorded
	}
	count, err := q.CountOnceFireRuns(ctx, store.CountOnceFireRunsParams{ScheduleID: p.ScheduleID, FireEpoch: p.FireEpoch})
	if err != nil {
		return err
	}
	// NULL means unlimited: the nil check is explicit, never a comparison against a zero value.
	if stored.Valid && count >= int64(stored.Int32) {
		return ErrOnceFireCapSpent
	}
	return nil
}

// recordOnceFire is step 11: the ledger row, carrying the id CreateRun returned in this
// transaction. A 23505 on the ledger's primary key is returned unmapped; createRun maps it
// ahead of the generic unique-violation arm.
func recordOnceFire(ctx context.Context, q *store.Queries, p *OnceFireProgress, run store.Run) error {
	return q.InsertOnceFireRun(ctx, store.InsertOnceFireRunParams{
		ScheduleID: p.ScheduleID, FireEpoch: p.FireEpoch, RepoID: p.RepoID, IssueIid: p.IssueIID,
		IssueTitle: p.Title, WebUrl: p.WebURL, RunUuid: run.ID,
	})
}

// onceFireTxReady reports whether the service can run an admission atomically: a transaction
// beginner and a live *store.Queries, like CreateCIRepairRun.
func (s *Service) onceFireTxReady() bool {
	if s.txBeginner == nil {
		return false
	}
	_, ok := s.q.(*store.Queries)
	return ok
}

// beforeCommit runs the test-only pre-commit seams on a created run, right before a create
// transaction's COMMIT. createBeforeCommitErrHook may fail the create (the caller then returns
// without committing, and the deferred rollback discards everything).
func (s *Service) beforeCommit(run store.Run) error {
	if s.createBeforeCommitHook != nil {
		s.createBeforeCommitHook(run)
	}
	if s.createBeforeCommitErrHook != nil {
		return s.createBeforeCommitErrHook(run)
	}
	return nil
}
