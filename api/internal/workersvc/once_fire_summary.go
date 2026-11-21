package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4512 (ADR-4007 T3a), summary writes. Every once-sweep summary (the partial write, the
// held write and the completing advance) goes through WriteOnceFireSummary: one short
// transaction, no forge I/O, that takes the SAME schedule row lock an admission holds from its
// step 7 to its commit, so the ledger it composes from is complete for every admission that
// committed, and an admission that has not committed waits until the summary does.

// onceFireSummaryTimeout bounds the whole summary transaction (begin to commit), detached from
// the caller's context so a canceled fire still records what it did.
const onceFireSummaryTimeout = 5 * time.Second

// OnceFireWriteKind is the write WriteOnceFireSummary performs once compose has chosen one.
type OnceFireWriteKind int

const (
	// OnceFireWriteNone writes nothing (a deduplicated or empty summary).
	OnceFireWriteNone OnceFireWriteKind = iota
	// OnceFireWritePartial writes last_fire only (RecordOnceFirePartial): never recent_fires.
	OnceFireWritePartial
	// OnceFireWriteHeld is the epoch-guarded RecordScheduleHeldFire, for a hold that has no
	// progress yet (it appends to recent_fires exactly as today's hold does).
	OnceFireWriteHeld
	// OnceFireWriteAdvance is the completing AdvanceOnceScheduleGuarded.
	OnceFireWriteAdvance
)

// OnceFireSnapshot is what compose sees: the schedule row and the ledger rows of the claimed
// epoch, both read under the schedule row lock.
type OnceFireSnapshot struct {
	Schedule store.RunSchedule
	Progress []store.ListOnceFireProgressRow
}

// OnceFireWrite is compose's decision. LastFiredAt, NextFireAt and Status apply to the
// advance only.
type OnceFireWrite struct {
	Kind        OnceFireWriteKind
	LastFire    []byte
	LastFiredAt pgtype.Timestamptz
	NextFireAt  pgtype.Timestamptz
	Status      string
}

// OnceFireSummaryResult reports what WriteOnceFireSummary did. Stale: the schedule is gone, no
// longer an active once sweep, or its occurrence moved on (nothing was written; the caller must
// not notify or advance). Wrote: a row was updated.
type OnceFireSummaryResult struct {
	Stale bool
	Wrote bool
}

// WriteOnceFireSummary runs compose over the locked snapshot of (scheduleID, userID, fireEpoch)
// and performs the write it returns. compose runs with the row lock held and must do no I/O. A
// progress read failure returns the error without writing, so a completion never advances on a
// ledger it could not read. A lock wait past 5s (55P03) or any other database error is returned
// as is; the scheduler treats it as transient. Without a transaction beginner and a live
// *store.Queries it fails closed.
func (s *Service) WriteOnceFireSummary(ctx context.Context, scheduleID, userID uuid.UUID, fireEpoch int64, compose func(OnceFireSnapshot) (OnceFireWrite, error)) (OnceFireSummaryResult, error) {
	if !s.onceFireTxReady() {
		return OnceFireSummaryResult{}, errOnceFireRequiresTx
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), onceFireSummaryTimeout)
	defer cancel()
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return OnceFireSummaryResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// READ COMMITTED first: the ledger read must see an admission that committed while this
	// transaction waited on the schedule row lock (a fresh snapshot per statement).
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		return OnceFireSummaryResult{}, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return OnceFireSummaryResult{}, err
	}
	q := store.New(tx)
	sched, err := q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: scheduleID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OnceFireSummaryResult{Stale: true}, nil
		}
		return OnceFireSummaryResult{}, err
	}
	if sched.Timing != "once" || sched.Target != "sweep" || sched.Status != "active" || sched.FireEpoch != fireEpoch {
		return OnceFireSummaryResult{Stale: true}, nil
	}
	rows, err := q.ListOnceFireProgress(ctx, store.ListOnceFireProgressParams{ScheduleID: scheduleID, FireEpoch: fireEpoch})
	if err != nil {
		return OnceFireSummaryResult{}, err
	}
	if s.onceFireSummaryAfterReadHook != nil {
		s.onceFireSummaryAfterReadHook()
	}
	w, err := compose(OnceFireSnapshot{Schedule: sched, Progress: rows})
	if err != nil {
		return OnceFireSummaryResult{}, err
	}
	var n int64
	switch w.Kind {
	case OnceFireWriteNone:
		return OnceFireSummaryResult{}, nil
	case OnceFireWritePartial:
		n, err = q.RecordOnceFirePartial(ctx, store.RecordOnceFirePartialParams{LastFire: w.LastFire, ID: scheduleID, FireEpoch: fireEpoch})
	case OnceFireWriteHeld:
		n, err = q.RecordScheduleHeldFire(ctx, store.RecordScheduleHeldFireParams{LastFire: w.LastFire, ID: scheduleID, FireEpoch: fireEpoch})
	case OnceFireWriteAdvance:
		n, err = q.AdvanceOnceScheduleGuarded(ctx, store.AdvanceOnceScheduleGuardedParams{
			LastFiredAt: w.LastFiredAt, NextFireAt: w.NextFireAt, Status: w.Status, LastFire: w.LastFire,
			ID: scheduleID, FireEpoch: fireEpoch,
		})
	default:
		return OnceFireSummaryResult{}, errors.New("once fire summary: unknown write kind")
	}
	if err != nil {
		return OnceFireSummaryResult{}, err
	}
	if n == 0 {
		return OnceFireSummaryResult{Stale: true}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return OnceFireSummaryResult{}, err
	}
	return OnceFireSummaryResult{Wrote: true}, nil
}
