package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// recoveryWaitCauses is the server enum for StateRequest.RecoveryCause (PRD #1392 M1, D9),
// matching migration 00232's runs_recovery_wait_cause_check. SetState validates a non-nil
// cause against this set BEFORE any state SQL, so an unknown value is a 400 rather than a
// constraint violation at the park write. NULL (an absent cause) is the LEGACY/untyped park
// and is not a member here — the empty-turn park writes NULL. Only 'forge_unreachable' drives
// the dedicated park transaction today; 'empty_turn'/'provider_outage' are reserved (D9,
// #1088 adopts provider_outage) and, if reported, take the ordinary untyped park.
var recoveryWaitCauses = map[string]bool{
	"forge_unreachable": true,
	"empty_turn":        true,
	"provider_outage":   true,
}

// parkForgeUnreachable is SetState's forge pre-clone park transaction (PRD #1392 M1, D2/D3/D4).
// A transient forge failure at clone parks the run on 'recovery_wait' with a typed cause,
// settling its exact-generation custody hold in the SAME locked transaction, or fails it past
// the forge cap, or — if an owner cancel was stamped during the retries — cancels it (still
// releasing the hold). It returns (run, rows, err):
//
//   - rows == 1, err == nil: an APPLIED transition (park, cap-fail, or cancel). The committed
//     run is returned; SetState re-reads it and runs the shared terminal fan-out (the pre-clone
//     origin skips the judge). The caller maps this to 200.
//   - rows == 0, err == nil: a NO-OP the caller maps to 409 {run}: the idempotent duplicate of
//     an already-applied forge park (status recovery_wait, cause forge_unreachable — never
//     double-incremented), or a stale report onto a run that is no longer 'running'.
//   - err == ErrForgeParkStaleClaim / ErrForgeParkCustodyUnsettled: a precedence refusal the
//     caller returns as a 409 {run, reason}. The returned run is the locked row (nothing
//     committed — the transaction rolled back).
//
// Precedence (strict): a claim-generation mismatch is stale_claim, checked FIRST with nothing
// mutated; then the already-parked idempotent case answers recovery_wait (no reason); then a
// current, unreleased generation whose exact-hold cardinality or release step cannot settle
// rolls back as custody_unsettled.
func (s *Service) parkForgeUnreachable(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, sessionID pgtype.Text) (store.Run, int64, error) {
	// A forge park report MUST carry the claim generation it holds — it is the #1247 fence the
	// stale_claim precedence keys on. Absent is a protocol error (400), not a stale claim: a
	// stale_claim is a generation MISMATCH, which needs a value to compare.
	if req.ClaimGeneration == nil {
		return store.Run{}, 0, fmt.Errorf("%w: forge_unreachable park requires claim_generation", ErrInvalidState)
	}

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, 0, err
	}
	// A no-op after a successful Commit; on every early return it undoes the FOR UPDATE lock and
	// any release/transition, so a refused park never settles custody or moves state.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	workerID := pgconv.UUID(wkr.ID)
	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The worker no longer owns the run (a cross-worker reclaim landed between the pre-tx
			// snapshot and this lock) → 404, the same as every other not-owned report.
			return store.Run{}, 0, ErrRunNotOwned
		}
		return store.Run{}, 0, err
	}

	// Precedence 1 — stale_claim (checked FIRST, nothing mutated, #1247): the report's generation
	// must equal the LOCKED run's claim_generation. A mismatch means a newer claim superseded this
	// worker's, so the park is refused with the run's real (mismatched-generation) row.
	if run.ClaimGeneration != *req.ClaimGeneration {
		return run, 0, ErrForgeParkStaleClaim
	}

	// Precedence 2 — idempotent already-parked (409 recovery_wait, no reason, NOT double-counted):
	// a duplicate of an already-applied forge park (a lost ack). The generation matched above (the
	// park preserves it), so this is the same claim re-reporting. Return the parked run as a no-op.
	if run.Status == "recovery_wait" && run.RecoveryWaitCause.Valid && run.RecoveryWaitCause.String == "forge_unreachable" {
		return run, 0, nil
	}

	// A stale report onto a run that is no longer 'running' (terminal, queued, empty-turn parked
	// with a NULL cause, …) is a plain no-op → 409 {run} with the run's real status. The park
	// writers below guard on status='running' too; this is the explicit, non-mutating branch.
	if run.Status != "running" {
		return run, 0, nil
	}

	// Precedence 3 — settle the exact-generation custody hold. Lock (FOR UPDATE) and count the
	// open holds for (run, this worker, this generation): EXACTLY ONE is required. The exact
	// release trusts the supplied generation and zero/several is an unsettleable custody state,
	// so anything but one refuses the park (rollback → 409 custody_unsettled). Holding the row
	// lock across the release stops a concurrent reconciler release from turning a verified single
	// hold into zero between the count and the release.
	holdIDs, err := qtx.LockOpenCustodyHoldsForRunWorkerGeneration(ctx, store.LockOpenCustodyHoldsForRunWorkerGenerationParams{
		RunID:      run.ID,
		Generation: *req.ClaimGeneration,
		WorkerID:   wkr.ID,
	})
	if err != nil {
		return store.Run{}, 0, err
	}
	if len(holdIDs) != 1 {
		return run, 0, ErrForgeParkCustodyUnsettled
	}
	released, err := qtx.ReleaseCustodyHoldExact(ctx, store.ReleaseCustodyHoldExactParams{
		RunID:      run.ID,
		Generation: *req.ClaimGeneration,
		WorkerID:   wkr.ID,
		// D3: a generation that never adopted a source has nothing to prove against the forge.
		ReleaseEvidence: pgconv.TextOrNull("no_adopted_source"),
	})
	if err != nil {
		return store.Run{}, 0, err
	}
	if released != 1 {
		return run, 0, ErrForgeParkCustodyUnsettled
	}

	// A stop verdict may have been stamped concurrently during the clone retries (fact 4: the
	// stamp has no status guard). If one is present, this is a deliberate wind-down, not a park:
	// transition to 'cancelled' (the hold is already released above, so the cancelled generation
	// leaves no open hold) and commit. An owner cancel is the realistic pre-clone verdict; a
	// graceful 'stopped' converges to 'cancelled' the same way the failed arm does
	// (CancelRunByWorker). Only a run with NO stamped verdict proceeds to park-or-fail.
	if run.StopKind.Valid && run.StopKind.String != "" {
		n, cerr := qtx.CancelRunByWorker(ctx, store.CancelRunByWorkerParams{ID: run.ID, WorkerID: workerID})
		if cerr != nil {
			return store.Run{}, 0, cerr
		}
		if n != 1 {
			// The run moved out from under the lock in a way we cannot cancel; refuse safely.
			return run, 0, ErrForgeParkCustodyUnsettled
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, 0, err
		}
		return run, 1, nil
	}

	// Park-or-fail. forge_park_count is read from the LOCKED row (the value BEFORE this park); the
	// (count+1)th park past the cap fails the run with the server-derived origin instead of parking
	// again. RUN_FORGE_UNREACHABLE_MAX_PARKS == 0 disables the cap (unlimited).
	maxParks := s.p.RunForgeUnreachableMaxParks
	if maxParks != 0 && int(run.ForgeParkCount)+1 > maxParks {
		reason := fmt.Sprintf("the forge stayed unreachable at clone across %d parks", run.ForgeParkCount+1)
		n, ferr := qtx.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: pgconv.TextOrNull(reason),
			// SERVER-DERIVED, set directly — NOT through CoerceFailOrigin's worker-reportable gate
			// (forge_unreachable is server-only). The pre-clone origin excludes it from the judge.
			FailOrigin: pgconv.TextOrNull("forge_unreachable"),
			SessionID:  sessionID,
			ID:         run.ID,
			WorkerID:   workerID,
		})
		if ferr != nil {
			return store.Run{}, 0, ferr
		}
		if n != 1 {
			return run, 0, ErrForgeParkCustodyUnsettled
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, 0, err
		}
		return run, 1, nil
	}

	// Park. The retry stamp is computed EXACTLY as setRecoveryWait does — now + capped
	// exponential backoff (shaped by the LOCKED run's recovery_wait_count, the prior count) +
	// jitter — so the forge park shares the empty-turn park's promotion cadence.
	retryNotBefore := s.now().Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter())
	parked, perr := qtx.ParkRunForgeUnreachable(ctx, store.ParkRunForgeUnreachableParams{
		RetryNotBefore: pgconv.Time(retryNotBefore),
		SessionID:      sessionID,
		ID:             run.ID,
		WorkerID:       workerID,
	})
	if perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			// The status='running' guard did not match under the lock — impossible given the check
			// above, but refuse safely rather than commit a partial (released) transaction.
			return run, 0, ErrForgeParkCustodyUnsettled
		}
		return store.Run{}, 0, perr
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, 0, err
	}
	return parked, 1, nil
}

// failForgeUnsettleable is the fail-safe for a forge_unreachable park report when no
// transaction beginner is wired (s.txBeginner == nil), so the atomic release+park cannot run
// (PRD #1392 M1, D3). It MUST NOT do a blind untyped park (that would park while leaking the
// generation's custody hold), so it takes today's safe FAILED path — the run fails with the
// coerced/default origin exactly as a pre-#1392 forge clone failure did, and the custody-release
// reconciler remains the backstop for the still-open hold. In production txBeginner is always
// wired, so this is the tests/degraded-deployment path.
func (s *Service) failForgeUnsettleable(ctx context.Context, wkr store.Worker, runID uuid.UUID, req StateRequest, sessionID pgtype.Text) (int64, error) {
	failOrigin := "agent_failure"
	if o := CoerceFailOrigin(req.FailOrigin); o != nil {
		failOrigin = *o
	}
	return s.q.SetRunFailed(ctx, store.SetRunFailedParams{
		FailureReason:  limitAwareFailureReason(req),
		FailOrigin:     pgconv.TextOrNull(failOrigin),
		PreservedPatch: clampWirePreservedPatch(req.PreservedPatch),
		SessionID:      sessionID,
		ID:             runID,
		WorkerID:       pgconv.UUID(wkr.ID),
	})
}
