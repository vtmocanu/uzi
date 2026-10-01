package workersvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2006: the ephemeral worker lease. A finished ephemeral worker keeps its row for a bounded
// lease and may claim a same-owner, same-repository, same-branch follow-up run. The SQL lives in
// store (EnterEphemeralLease, RebindLeasedEphemeralWorker, the ClaimRun lease arm); this file is the
// transactional glue the terminal report (setState's fence tx and completeRunWithPermit) and Claim
// share.

// workersEphemeralRunUnique is the unique index that holds one ephemeral worker per bound run.
const workersEphemeralRunUnique = "uq_workers_ephemeral_run"

// terminalLeaseOutcome is what the transaction that committed a terminal report did for the
// ephemeral lease. The zero value means nothing: SetState's post-commit steps (the custody release,
// the teardown) run exactly as they did before the lease.
type terminalLeaseOutcome struct {
	// released is true when the completed run's custody hold was released inside the terminal
	// transaction, so the post-commit ReleaseCustodyHoldExact is skipped as a duplicate.
	released bool
	// leased is true when the worker's lease was entered in the terminal transaction, so the
	// post-commit teardown is skipped.
	leased bool
}

// leaseInterval is the configured lease as the SQL interval the lease-aware queries take. Zero
// yields the invalid (NULL) interval, which the SQL reads as "no lease is live", so a lease-off
// service passes every lease-aware statement the value that reproduces the pre-lease behaviour.
func (s *Service) leaseInterval() pgtype.Interval {
	if s.ephemeralLease <= 0 {
		return pgtype.Interval{}
	}
	return pgtype.Interval{Microseconds: s.ephemeralLease.Microseconds(), Valid: true}
}

// enterEphemeralLeaseTx runs inside the transaction that wrote a run's terminal state, with the
// worker row ALREADY locked (before the run's, the canonical order). For a completed run it first
// releases the completing generation's custody hold (the same ReleaseCustodyHoldExact call, with
// publication evidence, SetState makes post-commit), then starts the worker's lease with
// EnterEphemeralLease. Both run in one SAVEPOINT: on any error it rolls back to the savepoint, so
// the terminal transition is never lost, and reports nothing done, so the caller's post-commit
// release and teardown run as before.
//
// generation is the completing claim generation read from the LOCKED run row. A zero-row
// EnterEphemeralLease (no identity, a profile-bound or repo-less run, a draining or isolated
// worker, another live run, a hold still open) leaves a successful release in place: that release
// is the one SetState would have made post-commit, and only the lease is skipped.
func (s *Service) enterEphemeralLeaseTx(ctx context.Context, tx pgx.Tx, qtx *store.Queries, wkr store.Worker, runID uuid.UUID, generation int64) terminalLeaseOutcome {
	run, err := qtx.GetRunByID(ctx, runID)
	if err != nil || !terminalStatuses[run.Status] {
		if err != nil {
			slog.Warn("ephemeral lease: read terminal run", "run", runID, "worker", wkr.ID, "error", err)
		}
		return terminalLeaseOutcome{}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Warn("ephemeral lease: savepoint", "run", runID, "worker", wkr.ID, "error", err)
		return terminalLeaseOutcome{}
	}
	sq := store.New(sp)
	var out terminalLeaseOutcome
	if run.Status == "completed" {
		if _, err := sq.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
			RunID:           runID,
			Generation:      generation,
			WorkerID:        wkr.ID,
			ReleaseEvidence: pgconv.TextOrNull("publication"),
		}); err != nil {
			slog.Warn("ephemeral lease: release custody in terminal tx", "run", runID, "worker", wkr.ID, "generation", generation, "error", err)
			_ = sp.Rollback(ctx)
			return terminalLeaseOutcome{}
		}
		out.released = true
	}
	n, err := sq.EnterEphemeralLease(ctx, store.EnterEphemeralLeaseParams{WorkerID: wkr.ID, RunID: runID})
	if err != nil {
		slog.Warn("ephemeral lease: enter", "run", runID, "worker", wkr.ID, "error", err)
		_ = sp.Rollback(ctx)
		return terminalLeaseOutcome{}
	}
	out.leased = n == 1
	if err := sp.Commit(ctx); err != nil {
		slog.Warn("ephemeral lease: release savepoint", "run", runID, "worker", wkr.ID, "error", err)
		return terminalLeaseOutcome{}
	}
	return out
}

// claimLeaseAt is the instant a claim passes ClaimRun as @lease_at: a FRESH database clock reading
// the caller takes immediately before ClaimRun, after every lock and guard it waits on, so a lease
// that expired while the claim waited is never admitted through. leaseAtOverride (test-only, zero
// in production) substitutes a pinned instant.
func (s *Service) claimLeaseAt(ctx context.Context, qtx *store.Queries) (pgtype.Timestamptz, error) {
	if s.leaseAtOverride.Valid {
		return s.leaseAtOverride, nil
	}
	return qtx.LeaseClockNow(ctx)
}

// probeLeaseClaim reports a lease claim's outcome to the test hook; a no-op in production.
func (s *Service) probeLeaseClaim(admitted, rebound bool) {
	if s.leaseClaimProbe != nil {
		s.leaseClaimProbe(admitted, rebound)
	}
}
