package workersvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// reserveMarkQueries is the optional store surface of the marker pass. *store.Queries satisfies it;
// a fake Store does not, and the pass is then a no-op.
type reserveMarkQueries interface {
	ListReserveMarkOwners(ctx context.Context) ([]uuid.UUID, error)
	ListQueuedRunsForReserve(ctx context.Context, userID uuid.UUID) ([]store.Run, error)
	ListReserveWorkers(ctx context.Context, userID uuid.UUID) ([]store.ListReserveWorkersRow, error)
	SetRunReserveHoldReason(ctx context.Context, arg store.SetRunReserveHoldReasonParams) (int64, error)
	UserHasAutoEligibleAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
}

// markReserveHolds is the sweeper pass that keeps a queued run's persisted weekly-reserve reason
// (runs.reserve_hold_reason) in step with the reserve (PRD #3522 M3). ClaimRun's own gate is what
// actually withholds the run; this marker is what the run DTO, the queue_reason, the health pill
// and the ephemeral provisioner's demand queries read, so a held run is visible without anyone
// re-deriving the verdict.
//
// For each owner with a threshold set and a queued run, and for each owner whose queued run still
// carries a marker after the threshold was cleared, it recomputes every queued run's verdict and
// writes the reason ONLY on change (guarded status = 'queued', so a claim that won the race is
// never overwritten). A cleared threshold, a toggle flipped off for a started run, a chat run and an
// active manual-resume override all evaluate to "no reason", which drops the marker regardless of
// how fresh any reading is: releasing a hold never depends on the poller being healthy.
//
// A run is marked only when EVERY lane that could claim it is blocked: the lane of the worker its
// resume affinity pins it to when that pin would hold, else every registered persistent worker's lane
// plus the lane a fresh ephemeral worker would get (auto when the owner has a pooled token, else
// default). One owner's failure is logged and skipped, never blocking a sibling owner's pass; the
// loop is bounded by the owners with queued runs, one tick at a time.
func (s *Service) markReserveHolds(ctx context.Context, now time.Time) (int64, error) {
	q, ok := s.q.(reserveMarkQueries)
	if !ok {
		return 0, nil
	}
	if _, ok := s.q.(reserveQueries); !ok {
		return 0, nil
	}
	owners, err := q.ListReserveMarkOwners(ctx)
	if err != nil {
		return 0, err
	}
	var changed int64
	for _, uid := range owners {
		n, oerr := s.markReserveHoldsForOwner(ctx, q, uid, now)
		changed += n
		if oerr != nil {
			slog.Warn("sweeper: weekly reserve marker pass failed for an owner", "user_id", uid.String(), "error", oerr)
		}
	}
	return changed, nil
}

func (s *Service) markReserveHoldsForOwner(ctx context.Context, q reserveMarkQueries, uid uuid.UUID, now time.Time) (int64, error) {
	snap, err := s.reserveSnapshot(ctx, uid, now)
	if err != nil {
		return 0, err
	}
	runs, err := q.ListQueuedRunsForReserve(ctx, uid)
	if err != nil {
		return 0, err
	}
	var workers []store.ListReserveWorkersRow
	hasPool := false
	if snap.on() {
		if workers, err = q.ListReserveWorkers(ctx, uid); err != nil {
			return 0, err
		}
		if hasPool, err = q.UserHasAutoEligibleAnthropicToken(ctx, uid); err != nil {
			return 0, err
		}
	}
	var changed int64
	for _, run := range runs {
		want := ""
		if d := s.reserveRunDecision(run, snap, workers, hasPool, now); d.held {
			want = d.reason
		}
		cur := ""
		if run.ReserveHoldReason.Valid {
			cur = run.ReserveHoldReason.String
		}
		if want == cur {
			continue
		}
		n, werr := q.SetRunReserveHoldReason(ctx, store.SetRunReserveHoldReasonParams{
			ID: run.ID, Reason: pgconv.TextOrNull(want),
		})
		if werr != nil {
			return changed, werr
		}
		if n > 0 {
			changed++
			s.publishSwept(run.ID, "queued")
		}
	}
	return changed, nil
}

// reserveRunDecision decides whether the reserve holds one queued run across every lane that could
// claim it. The reason of the first lane's decision is shown (the lanes agree on a run whose
// credential is not chosen by the worker, and differ only by which blocked token they name otherwise).
func (s *Service) reserveRunDecision(run store.Run, snap *reserveSnap, workers []store.ListReserveWorkersRow, hasPool bool, now time.Time) reserveDecision {
	if !reserveEnforced(run, snap, now) {
		return reserveDecision{}
	}
	lanes := s.reserveCandidateLanes(run, workers, hasPool, now)
	var first reserveDecision
	for i, lane := range lanes {
		d := reserveHeld(run, snap, lane, now)
		if !d.held {
			return reserveDecision{}
		}
		if i == 0 {
			first = d
		}
	}
	return first
}

// reserveCandidateLanes lists the Anthropic lanes through which a queued run could be claimed. A run
// whose resume affinity still pins it to a worker (that worker row exists and is draining, fenced or
// heartbeat-fresh, or holds it inside the stale-requeue grace, and the pin has not outlived the
// affinity ceiling; ClaimRun's pin arm) is claimable only on that worker's lane. Otherwise any of the
// owner's persistent workers, a worker already bound to this very run, or a freshly provisioned
// ephemeral worker may claim it.
func (s *Service) reserveCandidateLanes(run store.Run, workers []store.ListReserveWorkersRow, hasPool bool, now time.Time) []reserveLane {
	laneOf := func(w store.ListReserveWorkersRow) reserveLane {
		l := reserveLane{mode: w.AnthropicBindMode}
		if w.AnthropicBindMode == BindModePinned && w.AnthropicSecretID.Valid {
			id := uuid.UUID(w.AnthropicSecretID.Bytes)
			l.secretID = &id
		}
		return l
	}
	if run.WorkerID.Valid && run.Kind != runkind.CrossCheck {
		for _, w := range workers {
			if w.ID != uuid.UUID(run.WorkerID.Bytes) {
				continue
			}
			ceilingOK := !run.UpdatedAt.Valid || !run.UpdatedAt.Time.Before(now.Add(-s.p.WorkerAffinityCeiling))
			heartbeatFresh := w.LastHeartbeatAt.Valid && !w.LastHeartbeatAt.Time.Before(now.Add(-s.p.WorkerHeartbeatStale))
			staleRequeuePin := s.p.WorkerStaleRequeueGrace > 0 && run.StaleRequeueGeneration.Valid &&
				run.StaleRequeueGeneration.Int64 == run.ClaimGeneration && run.StatusSince.Valid &&
				run.StatusSince.Time.After(now.Add(-s.p.WorkerStaleRequeueGrace))
			if ceilingOK && (w.DrainingSince.Valid || w.MaintenanceFenced || heartbeatFresh || staleRequeuePin) {
				return []reserveLane{laneOf(w)}
			}
		}
	}
	lanes := make([]reserveLane, 0, len(workers)+1)
	for _, w := range workers {
		if w.Ephemeral && (!w.EphemeralRunID.Valid || uuid.UUID(w.EphemeralRunID.Bytes) != run.ID) {
			continue // an ephemeral worker serves only its own bound run
		}
		lanes = append(lanes, laneOf(w))
	}
	return append(lanes, laneEphemeral(hasPool))
}
