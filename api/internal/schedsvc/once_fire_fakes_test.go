package schedsvc

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4512: the in-memory stand-in for the once-fire ledger and the summary writer, so the
// unit tests exercise the scheduler's progress handling without a database. The PostgreSQL 17
// tests (once_fire_retry_livedb_test.go) run the real admission and summary transactions.

type onceKey struct {
	schedule uuid.UUID
	epoch    int64
}

type fakeOnceLedger struct {
	caps map[onceKey]pgtype.Int4
	data map[onceKey][]store.ListOnceFireProgressRow
	// stale makes every admission and summary report a re-armed occurrence.
	stale bool
}

func (l *fakeOnceLedger) rows(id uuid.UUID, epoch int64) []store.ListOnceFireProgressRow {
	if l == nil {
		return nil
	}
	return append([]store.ListOnceFireProgressRow(nil), l.data[onceKey{id, epoch}]...)
}

func (l *fakeOnceLedger) cap(id uuid.UUID, epoch int64) (pgtype.Int4, error) {
	if l == nil {
		return pgtype.Int4{}, pgx.ErrNoRows
	}
	c, ok := l.caps[onceKey{id, epoch}]
	if !ok {
		return pgtype.Int4{}, pgx.ErrNoRows
	}
	return c, nil
}

// seed pre-records a start of an earlier attempt, with the header cap (nil = unlimited).
func (l *fakeOnceLedger) seed(id uuid.UUID, epoch int64, cap *int, iid int64, title string) uuid.UUID {
	run := uuid.New()
	if err := l.admit(&workersvc.OnceFireProgress{ScheduleID: id, FireEpoch: epoch, IssueIID: iid, EffectiveCap: cap}); err != nil {
		panic(err) // a fixture seeds each iid once into an unspent ledger
	}
	l.record(&workersvc.OnceFireProgress{ScheduleID: id, FireEpoch: epoch, IssueIID: iid, Title: title, WebURL: "https://forge/" + title}, run)
	return run
}

// admit is the admission's header, recorded and cap-spent checks (workersvc.admitOnceFire).
func (l *fakeOnceLedger) admit(p *workersvc.OnceFireProgress) error {
	if l == nil || p == nil {
		return nil
	}
	if l.stale {
		return workersvc.ErrOnceFireStale
	}
	k := onceKey{p.ScheduleID, p.FireEpoch}
	if l.caps == nil {
		l.caps, l.data = map[onceKey]pgtype.Int4{}, map[onceKey][]store.ListOnceFireProgressRow{}
	}
	if _, ok := l.caps[k]; !ok {
		c := pgtype.Int4{}
		if p.EffectiveCap != nil {
			c = pgtype.Int4{Int32: int32(*p.EffectiveCap), Valid: true} //nolint:gosec // test caps are tiny
		}
		l.caps[k] = c
	}
	for _, r := range l.data[k] {
		if r.IssueIid == p.IssueIID {
			return workersvc.ErrOnceFireCandidateRecorded
		}
	}
	if c := l.caps[k]; c.Valid && len(l.data[k]) >= int(c.Int32) {
		return workersvc.ErrOnceFireCapSpent
	}
	return nil
}

func (l *fakeOnceLedger) record(p *workersvc.OnceFireProgress, run uuid.UUID) {
	k := onceKey{p.ScheduleID, p.FireEpoch}
	if l.caps == nil {
		l.caps, l.data = map[onceKey]pgtype.Int4{}, map[onceKey][]store.ListOnceFireProgressRow{}
	}
	l.data[k] = append(l.data[k], store.ListOnceFireProgressRow{IssueIid: p.IssueIID, IssueTitle: p.Title, WebUrl: p.WebURL, RunUuid: run})
	sort.Slice(l.data[k], func(i, j int) bool { return l.data[k][i].IssueIid < l.data[k][j].IssueIid })
}

// admitOnce stands in for the admission inside the create transaction; nil progress is every
// other fire.
func (f *fakeRuns) admitOnce(p *workersvc.OnceFireProgress) error {
	if p == nil {
		return nil
	}
	f.progressSeen = append(f.progressSeen, *p)
	return f.ledger.admit(p)
}

// finishOnce commits the ledger row with the run (same transaction) and then, for a scripted
// lost ack, fails the call.
func (f *fakeRuns) finishOnce(p *workersvc.OnceFireProgress, iid int64, run store.Run) (store.Run, error) {
	if p != nil && f.ledger != nil {
		f.ledger.record(p, run.ID)
		if err := f.lostAck[iid]; err != nil {
			return store.Run{}, err
		}
	}
	return run, nil
}

// WriteOnceFireSummary is the in-memory summary transaction: the snapshot is the due row and the
// ledger, and the chosen write lands in the fakeStore's usual buckets.
func (f *fakeRuns) WriteOnceFireSummary(ctx context.Context, scheduleID, userID uuid.UUID, fireEpoch int64, compose func(workersvc.OnceFireSnapshot) (workersvc.OnceFireWrite, error)) (workersvc.OnceFireSummaryResult, error) {
	f.summaryCalls++
	if f.summaryErr != nil {
		return workersvc.OnceFireSummaryResult{}, f.summaryErr
	}
	if f.st == nil {
		return workersvc.OnceFireSummaryResult{}, errors.New("fake summary writer is not wired to a store")
	}
	if f.ledger != nil && f.ledger.stale {
		return workersvc.OnceFireSummaryResult{Stale: true}, nil
	}
	sched := store.RunSchedule{ID: scheduleID, UserID: userID, Timing: "once", Target: "sweep", Status: "active", FireEpoch: fireEpoch}
	for _, d := range f.st.due {
		if d.ID == scheduleID {
			sched = d
		}
	}
	w, err := compose(workersvc.OnceFireSnapshot{Schedule: sched, Progress: f.ledger.rows(scheduleID, fireEpoch)})
	if err != nil {
		return workersvc.OnceFireSummaryResult{}, err
	}
	switch w.Kind {
	case workersvc.OnceFireWritePartial:
		f.st.partialCalls = append(f.st.partialCalls, store.RecordOnceFirePartialParams{LastFire: w.LastFire, ID: scheduleID, FireEpoch: fireEpoch})
		for i := range f.st.due {
			if f.st.due[i].ID == scheduleID {
				f.st.due[i].LastFire = w.LastFire
			}
		}
	case workersvc.OnceFireWriteHeld:
		if _, err := f.st.RecordScheduleHeldFire(ctx, store.RecordScheduleHeldFireParams{LastFire: w.LastFire, ID: scheduleID, FireEpoch: fireEpoch}); err != nil {
			return workersvc.OnceFireSummaryResult{}, err
		}
	case workersvc.OnceFireWriteAdvance:
		if _, err := f.st.AdvanceOnceScheduleGuarded(ctx, store.AdvanceOnceScheduleGuardedParams{
			LastFiredAt: w.LastFiredAt, NextFireAt: w.NextFireAt, Status: w.Status, LastFire: w.LastFire, ID: scheduleID, FireEpoch: fireEpoch,
		}); err != nil {
			return workersvc.OnceFireSummaryResult{}, err
		}
	default:
		return workersvc.OnceFireSummaryResult{}, nil
	}
	return workersvc.OnceFireSummaryResult{Wrote: true}, nil
}
