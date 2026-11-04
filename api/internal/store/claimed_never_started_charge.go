package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// ChargeClaimedNeverStartedContinuationsParams selects claimed runs older than Cutoff.
// MaxRequeues is the per-episode requeue budget (RUN_MAX_REQUEUES).
type ChargeClaimedNeverStartedContinuationsParams struct {
	Cutoff      pgtype.Timestamptz `json:"cutoff"`
	MaxRequeues int32              `json:"max_requeues"`
}

// ChargeClaimedNeverStartedContinuationsRow is the committed disposition of one run: status is
// queued for a charged requeue and recovery_wait for the exhausted park.
type ChargeClaimedNeverStartedContinuationsRow = chargeClaimedNeverStartedContinuationsLockedRow

// ParkUnwitnessedContinuationsRow is the committed disposition of one run parked at
// recovery_wait / retained_continuation_unwitnessed.
type ParkUnwitnessedContinuationsRow = parkUnwitnessedContinuationsLockedRow

// chargeSelected classifies the exhaustion evidence once for the locked runs and rewrites them
// on a fresh snapshot. The caller holds the worker locks and a transaction.
func (q *Queries) chargeSelected(ctx context.Context, workerIDs []uuid.UUID, arg ChargeClaimedNeverStartedContinuationsParams) ([]ChargeClaimedNeverStartedContinuationsRow, error) {
	ids, err := q.LockClaimedNeverStartedContinuations(ctx, LockClaimedNeverStartedContinuationsParams{Cutoff: arg.Cutoff, LockedWorkerIds: workerIDs})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	evidence, err := q.classifyWorkerExhaustion(ctx, ids, nil)
	if err != nil {
		return nil, err
	}
	return q.chargeClaimedNeverStartedContinuationsLocked(ctx, chargeClaimedNeverStartedContinuationsLockedParams{
		MaxRequeues: arg.MaxRequeues, ExhaustionEvidence: evidence, LockedRunIds: ids, Cutoff: arg.Cutoff,
	})
}

// ChargeClaimedNeverStartedContinuations (issue #3859) charges, and at the episode cap parks,
// a claimed run whose worker flight departed without ever reporting running, when it is a
// retained-custody continuation (an open hold of an earlier generation plus a departure
// witness bound to this generation). It locks workers then runs, classifies the exhaustion
// evidence once, and rewrites on a fresh snapshot, all in one transaction (a savepoint when
// the caller already holds one). The production sweep uses SweepClaimedNeverStartedCohort,
// which runs this step and the passes around it atomically.
func (q *Queries) ChargeClaimedNeverStartedContinuations(ctx context.Context, arg ChargeClaimedNeverStartedContinuationsParams) ([]ChargeClaimedNeverStartedContinuationsRow, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]ChargeClaimedNeverStartedContinuationsRow, error) {
		workers, err := qtx.LockClaimedNeverStartedWorkers(ctx, arg.Cutoff)
		if err != nil || len(workers) == 0 {
			return nil, err
		}
		return qtx.chargeSelected(ctx, workers, arg)
	})
}

// SweepClaimedNeverStartedCohortParams drives one claimed-never-started cohort. Cutoff is the
// claim grace cutoff; WitnessCutoff is now minus WitnessWindowSeconds (twice the grace), the
// bounded wait for a departure witness.
type SweepClaimedNeverStartedCohortParams struct {
	Cutoff               pgtype.Timestamptz
	WitnessCutoff        pgtype.Timestamptz
	WitnessWindowSeconds float64
	MaxRequeues          int32
}

// SweepClaimedNeverStartedCohortResult lists what each pass committed.
type SweepClaimedNeverStartedCohortResult struct {
	Charged     []ChargeClaimedNeverStartedContinuationsRow
	Unwitnessed []ParkUnwitnessedContinuationsRow
	Reset       []SweepClaimedNeverStartedRow
}

// SweepClaimedNeverStartedCohort (issue #3859) runs, in ONE transaction and in lock order
// workers, runs, cross_checks: the worker locks, the charge pass (witnessed departures are
// charged, or parked at the episode cap), the unwitnessed park (window passed, presence, no
// stamp) and the uncharged reset, which defers a continuation still inside its witness window.
// Nothing is failed. A failure rolls the whole cohort back.
func (q *Queries) SweepClaimedNeverStartedCohort(ctx context.Context, arg SweepClaimedNeverStartedCohortParams) (SweepClaimedNeverStartedCohortResult, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) (SweepClaimedNeverStartedCohortResult, error) {
		var res SweepClaimedNeverStartedCohortResult
		workers, err := qtx.LockClaimedNeverStartedWorkers(ctx, arg.Cutoff)
		if err != nil {
			return res, err
		}
		if len(workers) > 0 {
			if res.Charged, err = qtx.chargeSelected(ctx, workers, ChargeClaimedNeverStartedContinuationsParams{Cutoff: arg.Cutoff, MaxRequeues: arg.MaxRequeues}); err != nil {
				return res, err
			}
			if res.Unwitnessed, err = qtx.parkUnwitnessedSelected(ctx, workers, arg); err != nil {
				return res, err
			}
		}
		res.Reset, err = qtx.SweepClaimedNeverStarted(ctx, SweepClaimedNeverStartedParams{Cutoff: arg.Cutoff, WitnessCutoff: arg.WitnessCutoff})
		return res, err
	})
}

func (q *Queries) parkUnwitnessedSelected(ctx context.Context, workerIDs []uuid.UUID, arg SweepClaimedNeverStartedCohortParams) ([]ParkUnwitnessedContinuationsRow, error) {
	ids, err := q.LockUnwitnessedContinuations(ctx, LockUnwitnessedContinuationsParams{Cutoff: arg.Cutoff, LockedWorkerIds: workerIDs, WitnessCutoff: arg.WitnessCutoff})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	evidence, err := q.classifyWorkerExhaustion(ctx, ids, nil)
	if err != nil {
		return nil, err
	}
	return q.parkUnwitnessedContinuationsLocked(ctx, parkUnwitnessedContinuationsLockedParams{
		ExhaustionEvidence: evidence, WitnessWindowSeconds: arg.WitnessWindowSeconds,
		LockedRunIds: ids, Cutoff: arg.Cutoff, WitnessCutoff: arg.WitnessCutoff,
	})
}
