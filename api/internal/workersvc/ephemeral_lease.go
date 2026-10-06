package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2006: the ephemeral worker lease. A finished ephemeral worker keeps its row for a bounded
// lease and may claim a same-owner, same-repository, same-branch follow-up run. The SQL lives in
// store (EnterEphemeralLease, RebindLeasedEphemeralWorker, the ClaimRun lease arm); this file is the
// transactional glue the terminal report (setState's fence tx and completeRunWithPermitLease) and Claim
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

// LeaseInterval converts a configured lease to the SQL interval the lease-aware queries take. Zero
// or negative yields the invalid (NULL) interval, which the SQL reads as "no lease is live", so a
// lease-off caller passes every lease-aware statement the value that reproduces the pre-lease
// behaviour. Exported so the ephemeral provisioner and reaper (hostedsvc) share this one conversion.
func LeaseInterval(d time.Duration) pgtype.Interval {
	if d <= 0 {
		return pgtype.Interval{}
	}
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// enterEphemeralLeaseTx runs inside the transaction that wrote a run's terminal state, with the
// worker row ALREADY locked (before the run's, the canonical order). EVERY statement it issues runs
// inside one SAVEPOINT, the terminal-run read included, so a statement error rolls back to the
// savepoint and reports nothing done: the terminal transition is never lost, and the caller's
// post-commit release and teardown run as before. The one failure the savepoint cannot contain is
// RELEASE SAVEPOINT itself failing (sp.Commit): pgx closes the handle even on that error, so there
// is nothing left to roll back to, the outer transaction is the server's to abort, and the caller's
// own commit then fails and surfaces the error as it would for any failed terminal transaction.
//
// Only a run whose FINAL status is completed or failed enters a lease (the plan decision: only
// worker-reported completed/failed terminals with no open hold enter a lease). A report of failed
// that setState routed to cancelled (the operator stop and cancel routes) ends cancelled and gets
// no lease, as does a worker-reported cancelled. A plan-rejected run ends failed
// (SetRunFailedPlanRejected), so it may lease. For a completed run it first releases the
// completing generation's custody hold (the same ReleaseCustodyHoldExact call, with publication
// evidence, SetState makes post-commit), then starts the worker's lease with EnterEphemeralLease.
//
// generation is the completing claim generation read from the LOCKED run row. A zero-row
// EnterEphemeralLease (no identity, a profile-bound or repo-less run, a draining or isolated
// worker, another live run, a hold still open) leaves a successful release in place: that release
// is the one SetState would have made post-commit, and only the lease is skipped.
func (s *Service) enterEphemeralLeaseTx(ctx context.Context, tx pgx.Tx, wkr store.Worker, runID uuid.UUID, generation int64) terminalLeaseOutcome {
	sp, err := tx.Begin(ctx)
	if err != nil {
		slog.Warn("ephemeral lease: savepoint", "run", runID, "worker", wkr.ID, "error", err)
		return terminalLeaseOutcome{}
	}
	sq := store.New(sp)
	run, err := sq.GetRunByID(ctx, runID)
	if err != nil {
		slog.Warn("ephemeral lease: read terminal run", "run", runID, "worker", wkr.ID, "error", err)
		_ = sp.Rollback(ctx)
		return terminalLeaseOutcome{}
	}
	if run.Status != "completed" && run.Status != "failed" {
		_ = sp.Rollback(ctx) // nothing was written; leave the savepoint
		return terminalLeaseOutcome{}
	}
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

// claimRunInTx runs ClaimRun inside tx, which the caller holds with the worker row locked and
// re-read (reread is that locked row). It reports (run, true, nil) for a claim, (_, false, nil) for
// idle (no candidate, or a lease claim refused), and an error otherwise; the caller commits tx for
// both non-error outcomes, because whatever it did before the claim (a snapshot replace) must
// persist even when the claim is refused.
//
// PRD #2006: a LEASED ephemeral claimant (the lease on, lease columns set on reread) may claim a
// same-owner, same-repository, same-branch follow-up. The lease params come from reread, not the
// stale RequireWorker row, and the admission instant is a FRESH clock reading taken here, after
// every lock and guard the caller waited on: a stale transaction-start now() would admit through a
// lease that expired meanwhile. The claim runs in a SAVEPOINT, then the worker is rebound to the
// claimed run and the lease ended; the rebind re-checks the lease against its OWN clock_timestamp(),
// so a lease that expired after the admission instant refuses. A refusal (any count other than 1, or
// a unique violation because the provisioner bound the run to another ephemeral worker first) rolls
// the claim (its run update and custody hold) back to the savepoint and reports idle, exactly the
// no-candidate outcome. A rebind racing the provisioner has two outcomes, not one. When the
// provisioner's insert lands first the rebind hits the unique violation above. When ClaimRun has
// already locked the run FOR UPDATE and the provisioner's CreateEphemeralHostedWorker has inserted
// its uq_workers_ephemeral_run entry, the provisioner's FK check blocks on the run while the rebind
// waits on the uncommitted unique entry, and Postgres aborts one side with 40P01 (deadlock): the
// claim then returns an error (a 500; the agent's next claim poll retries) or provisionOne errors
// (the next provisioning pass retries). Neither corrupts state, and there is no retry here. A lease-off service, a non-ephemeral worker or an unleased one passes the
// no-lease params (NULL columns), where ClaimRun's lease arm admits nothing.
func (s *Service) claimRunInTx(ctx context.Context, tx pgx.Tx, qtx *store.Queries, workerID uuid.UUID, reread store.Worker, params store.ClaimRunParams) (store.Run, bool, error) {
	params.ClaimantDraining = reread.DrainingSince.Valid || maintenancePending(reread)
	if reread.MaintenanceFenced {
		return store.Run{}, false, nil
	}
	leased := s.ephemeralLease > 0 && reread.Ephemeral && reread.LeaseSince.Valid &&
		reread.LeaseRepoID.Valid && reread.LeaseBranch.Valid
	if !leased {
		run, err := qtx.ClaimRun(ctx, params)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Run{}, false, nil
			}
			return store.Run{}, false, err
		}
		return run, true, nil
	}
	params.EphemeralLease = LeaseInterval(s.ephemeralLease)
	params.LeaseSince = reread.LeaseSince
	params.LeaseRepoID = reread.LeaseRepoID
	params.LeaseBranch = reread.LeaseBranch
	params.ClaimantDraining = reread.DrainingSince.Valid
	params.EphemeralRunID = reread.EphemeralRunID
	at, err := s.claimLeaseAt(ctx, qtx)
	if err != nil {
		return store.Run{}, false, err
	}
	params.LeaseAt = at
	// A SAVEPOINT before ClaimRun: a refused rebind below rolls the claim back without discarding
	// what the caller did earlier in tx.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return store.Run{}, false, err
	}
	run, err := store.New(sp).ClaimRun(ctx, params)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = sp.Rollback(ctx) // nothing claimed; leave the savepoint
			s.probeLeaseClaim(false, false)
			return store.Run{}, false, nil
		}
		return store.Run{}, false, err
	}
	if run.ID != uuid.UUID(reread.EphemeralRunID.Bytes) {
		if s.leaseClaimWindow != nil {
			s.leaseClaimWindow()
		}
		n, rerr := store.New(sp).RebindLeasedEphemeralWorker(ctx, store.RebindLeasedEphemeralWorkerParams{
			NewRunID:       run.ID,
			WorkerID:       workerID,
			OldRunID:       uuid.UUID(reread.EphemeralRunID.Bytes),
			EphemeralLease: params.EphemeralLease,
		})
		if rerr != nil && !uniqueViolationOn(rerr, workersEphemeralRunUnique) {
			return store.Run{}, false, rerr
		}
		if rerr != nil || n != 1 {
			s.probeLeaseClaim(true, false)
			if err := sp.Rollback(ctx); err != nil {
				return store.Run{}, false, err
			}
			return store.Run{}, false, nil // idle: the lease ended or the run was taken
		}
		s.probeLeaseClaim(true, true)
	}
	if err := sp.Commit(ctx); err != nil {
		return store.Run{}, false, err
	}
	return run, true, nil
}

// claimRunLeasedNoSnapshot is Claim's no-snapshot path for an ephemeral worker with the lease on: a
// short transaction with the same discipline as the snapshot path (worker row locked first, the row
// re-read under that lock, the lease claim through claimRunInTx), minus the snapshot work a
// snapshot-less claim has none of. GetWorkerForUpdate returns the row as locked, which is the
// re-read the lease params need.
func (s *Service) claimRunLeasedNoSnapshot(ctx context.Context, wkr store.Worker, params store.ClaimRunParams) (*ClaimPayload, error) {
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := store.New(tx)
	locked, err := qtx.GetWorkerForUpdate(ctx, wkr.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // the worker row was deleted (reaper) since RequireWorker: idle
		}
		return nil, err
	}
	run, claimed, err := s.claimRunInTx(ctx, tx, qtx, wkr.ID, locked, params)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	committed = true
	if !claimed {
		return nil, nil // idle
	}
	return s.assembleAndFinishRunClaim(ctx, wkr, run, params.RecoveryCapable)
}
