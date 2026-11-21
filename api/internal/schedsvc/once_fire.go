package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4512 (ADR-4007 T3a): the scheduler half of durable per-fire progress for one-time
// sweeps. The admission transaction and the summary transaction live in workersvc
// (once_fire.go, once_fire_summary.go); this file folds the ledger into a fire's outcome and
// composes the summaries written through workersvc.WriteOnceFireSummary.

// errOnceFireHeld is fireSweep's quiet-hold signal for a continuation of a partially admitted
// one-time sweep whose owner capacity has no room: process neither logs it as a transient
// failure nor advances the row.
var errOnceFireHeld = errors.New("once sweep continuation held: no capacity room")

// onceSummaryMode says which summary writeOnceSweepSummary composes.
type onceSummaryMode int

const (
	// onceSummaryPartial: last_fire only, while the row stays active and due (gate contention,
	// a capacity hold). A summary equal to the stored one is not rewritten.
	onceSummaryPartial onceSummaryMode = iota
	// onceSummaryHeld: a credential_disabled hold. With no progress it is the epoch-guarded
	// RecordScheduleHeldFire; with progress a last_fire-only write that keeps the Started entries.
	onceSummaryHeld
	// onceSummaryComplete: the completing advance to status 'fired'.
	onceSummaryComplete
)

// foldPriorStarts renders the ledger rows of earlier attempts as Started entries, carrying the
// stored title, URL and the original run uuid (never uuid.Nil: the ledger forbids it).
func foldPriorStarts(rows []store.ListOnceFireProgressRow) []Started {
	if len(rows) == 0 {
		return nil
	}
	out := make([]Started, 0, len(rows))
	for _, r := range rows {
		iid := r.IssueIid
		out = append(out, Started{IssueIID: &iid, RunID: r.RunUuid, Title: r.IssueTitle, WebURL: r.WebUrl})
	}
	return out
}

// composeOnceSummary builds the outcome to persist from the fire's outcome and the ledger rows
// read under the schedule row lock. Started comes ONLY from the ledger, so a delayed or lost-ack
// view never shrinks or duplicates it; the caller's skips for a recorded issue are dropped (a
// lost-ack start surfaces there as fetch_failed); Matched is len(Started)+len(Skips).
func composeOnceSummary(out FireOutcome, rows []store.ListOnceFireProgressRow) FireOutcome {
	recorded := make(map[int64]bool, len(rows))
	for _, r := range rows {
		recorded[r.IssueIid] = true
	}
	composed := out
	composed.Started = foldPriorStarts(rows)
	composed.Skips = nil
	for _, s := range out.Skips {
		if s.IssueIID != nil && recorded[*s.IssueIID] {
			continue
		}
		composed.Skips = append(composed.Skips, s)
	}
	composed.Matched = len(composed.Started) + len(composed.Skips)
	return composed
}

// summaryEquivalent reports whether stored already says exactly what next says (fired_at aside),
// so a repeated partial or held write is skipped. The capacity block's InFlight and Room are
// volatile observations (they change every tick), so they are ignored; its stable fields still
// compare.
func summaryEquivalent(stored []byte, next []byte) bool {
	if len(stored) == 0 {
		return false
	}
	var a, b lastFireRecord
	if json.Unmarshal(stored, &a) != nil || json.Unmarshal(next, &b) != nil {
		return false
	}
	a.FiredAt = b.FiredAt
	if a.Capacity != nil && b.Capacity != nil {
		ac, bc := *a.Capacity, *b.Capacity
		ac.InFlight, ac.Room = bc.InFlight, bc.Room
		a.Capacity = &ac
	}
	return reflect.DeepEqual(a, b)
}

// writeOnceSweepSummary persists a one-time sweep's summary (partial, held or completing)
// through the workersvc helper: one short transaction under the schedule row lock, composed from
// the ledger. It never blocks the retry: a failed partial or held write is logged and dropped,
// and a failed completing write leaves the row un-advanced so the next tick retries (a ledger
// that could not be read never advances). A stale occurrence writes nothing.
func (e *Scheduler) writeOnceSweepSummary(ctx context.Context, sched store.RunSchedule, out FireOutcome, mode onceSummaryMode) {
	now := e.now()
	res, err := e.runs.WriteOnceFireSummary(ctx, sched.ID, sched.UserID, sched.FireEpoch, func(snap workersvc.OnceFireSnapshot) (workersvc.OnceFireWrite, error) {
		composed := composeOnceSummary(out, snap.Progress)
		lastFire, err := marshalLastFire(composed, now)
		if err != nil {
			return workersvc.OnceFireWrite{}, err
		}
		switch mode {
		case onceSummaryComplete:
			return workersvc.OnceFireWrite{
				Kind: workersvc.OnceFireWriteAdvance, LastFire: lastFire,
				LastFiredAt: pgconv.Time(now), NextFireAt: pgtype.Timestamptz{}, Status: "fired",
			}, nil
		case onceSummaryHeld:
			if len(snap.Progress) == 0 {
				if lastFireIsHold(snap.Schedule.LastFire, composed) {
					return workersvc.OnceFireWrite{}, nil
				}
				return workersvc.OnceFireWrite{Kind: workersvc.OnceFireWriteHeld, LastFire: lastFire}, nil
			}
		default: // onceSummaryPartial
			if len(snap.Progress) == 0 {
				return workersvc.OnceFireWrite{}, nil // nothing admitted yet: nothing to record
			}
		}
		if summaryEquivalent(snap.Schedule.LastFire, lastFire) {
			return workersvc.OnceFireWrite{}, nil
		}
		return workersvc.OnceFireWrite{Kind: workersvc.OnceFireWritePartial, LastFire: lastFire}, nil
	})
	switch {
	case err != nil && mode == onceSummaryComplete:
		e.logger.Error("scheduler: advance once sweep (not advanced, will retry)", "schedule", sched.ID.String(), "error", err)
	case err != nil:
		e.logger.Warn("scheduler: once sweep summary not written", "schedule", sched.ID.String(), "error", err)
	case res.Stale:
		e.logger.Info("scheduler: once sweep summary skipped, the fire is stale", "schedule", sched.ID.String(), "fire_epoch", sched.FireEpoch)
	}
}
