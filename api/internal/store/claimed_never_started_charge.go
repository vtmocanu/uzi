package store

import (
	"context"

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

// ChargeClaimedNeverStartedContinuations (issue #3859) charges, and at the episode cap parks,
// a claimed run whose worker flight departed without ever reporting running, when it is a
// retained-custody continuation (an open hold of an earlier generation plus a departure
// witness bound to this generation). It locks workers then runs, classifies the exhaustion
// evidence once, and rewrites on a fresh snapshot, all in one transaction (a savepoint when
// the caller already holds one). Run it BEFORE SweepClaimedNeverStarted, which excludes
// exactly the runs this statement owns.
func (q *Queries) ChargeClaimedNeverStartedContinuations(ctx context.Context, arg ChargeClaimedNeverStartedContinuationsParams) ([]ChargeClaimedNeverStartedContinuationsRow, error) {
	return withWorkerLostLocks(ctx, q, func(qtx *Queries) ([]ChargeClaimedNeverStartedContinuationsRow, error) {
		ids, err := qtx.LockClaimedNeverStartedContinuations(ctx, arg.Cutoff)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		evidence, err := qtx.classifyWorkerExhaustion(ctx, ids, nil)
		if err != nil {
			return nil, err
		}
		return qtx.chargeClaimedNeverStartedContinuationsLocked(ctx, chargeClaimedNeverStartedContinuationsLockedParams{
			MaxRequeues: arg.MaxRequeues, ExhaustionEvidence: evidence, LockedRunIds: ids, Cutoff: arg.Cutoff,
		})
	})
}
