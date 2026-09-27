package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// validateDiskParkPreventive enforces StateRequest.DiskParkPreventive's scope (PRD #1809 M5,
// D6): the flag only qualifies a 'data_volume_full' park, so a report carrying it with any
// other cause, or with no cause at all, is a protocol error (ErrInvalidState → 400) rather than
// a silently ignored field. It runs with SetState's other cause validation, before any SQL.
func validateDiskParkPreventive(req StateRequest) error {
	if req.DiskParkPreventive == nil {
		return nil
	}
	if req.RecoveryCause == nil || *req.RecoveryCause != recoveryCauseDataVolumeFull {
		return fmt.Errorf("%w: disk_park_preventive requires recovery_cause %q", ErrInvalidState, recoveryCauseDataVolumeFull)
	}
	return nil
}

// parkDataVolumeFull is SetState's data-volume-full park transaction (PRD #1809 M5, D6). A worker
// whose write to its data volume failed disk-full after a reclaim and one retry (or that stopped
// the run preventively before the volume filled) parks the run on 'recovery_wait' with the typed
// cause 'data_volume_full', or — past UZI_RUN_DISK_PARK_MAX counted parks — fails it with the
// server-derived fail_origin 'data_volume_full'. It returns (run, rows, err) in the
// parkForgeUnreachable shape:
//
//   - rows == 1, err == nil: an APPLIED transition (park or cap-fail). SetState re-reads the run
//     and runs the shared post-switch fan-out (broadcast; the judge skips the cap-fail origin).
//   - rows == 0, err == nil: a NO-OP the caller maps to 409 {run}: the idempotent duplicate of an
//     already-applied disk park (status recovery_wait, cause data_volume_full — never counted
//     twice), or a stale report onto a run that is no longer 'running'.
//   - err == ErrStaleClaim: the report's generation is not the locked run's, or the claim was
//     released. Nothing is mutated; the returned run is the locked row, and the handler answers
//     the generic 409 {run, disposition: stale_claim}.
//
// Fencing is the forge park's: the claim generation is REQUIRED (absent is a 400, since a
// mismatch needs a value to compare), the run row is locked FOR UPDATE and the generation
// checked FIRST, then the idempotent already-parked case, then the park-or-fail. Unlike the
// pre-clone forge park it settles NO custody hold: a disk park can land mid-run with committed
// work only this worker holds, so the generation's hold stays open and the worker keeps its
// local custody (D6, "a data_volume_full park still holds custody"). It has no stamped-stop
// branch either: like the ordinary recovery park (SetRunRecoveryWait) it parks whatever
// stop_kind the row carries.
//
// A PREVENTIVE park (req.DiskParkPreventive == true) parks WITHOUT bumping disk_park_count and
// is never capped, so a run the worker keeps stopping before the volume fills cannot be failed
// by it. Every disk park, counted or not, uses the ordinary recovery-wait backoff (shaped by
// recovery_wait_count and capped at RunRecoveryMaxPark), so repeated parks back off to a bounded
// ceiling rather than spinning.
func (s *Service) parkDataVolumeFull(ctx context.Context, wkr store.Worker, owned store.Run, req StateRequest, sessionID pgtype.Text) (store.Run, int64, error) {
	if req.ClaimGeneration == nil {
		return store.Run{}, 0, fmt.Errorf("%w: data_volume_full park requires claim_generation", ErrInvalidState)
	}
	if s.txBeginner == nil {
		// The park-or-fail decision reads the counter under the row lock, which needs a
		// transaction. Production always wires one; refuse (500) rather than park uncounted, so
		// the worker keeps the run and its custody and retries the report.
		return store.Run{}, 0, fmt.Errorf("data_volume_full park unavailable: no tx beginner wired for run %s", owned.ID)
	}
	counted := req.DiskParkPreventive == nil || !*req.DiskParkPreventive

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, 0, err
	}
	// A no-op after a successful Commit; on every early return it releases the FOR UPDATE lock.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	workerID := pgconv.UUID(wkr.ID)
	run, err := qtx.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: owned.ID, WorkerID: workerID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, 0, ErrRunNotOwned
		}
		return store.Run{}, 0, err
	}

	// Precedence 1 — stale claim, nothing mutated (#1247): a reclaim moved the generation past
	// the reported one, or a switch/wall park released this claim.
	if run.ClaimGeneration != *req.ClaimGeneration || run.ClaimReleasedAt.Valid {
		return run, 0, ErrStaleClaim
	}

	// Precedence 2 — the idempotent duplicate of an applied disk park (a lost ack): answer the
	// parked run as a no-op, never counting it twice.
	if run.Status == "recovery_wait" && run.RecoveryWaitCause.Valid && run.RecoveryWaitCause.String == recoveryCauseDataVolumeFull {
		return run, 0, nil
	}
	// A stale report onto a run that is no longer 'running' is a plain no-op → 409 {run}.
	if run.Status != "running" {
		return run, 0, nil
	}

	// Park-or-fail. disk_park_count is read from the LOCKED row (the value BEFORE this park); a
	// COUNTED park that would take it past the cap fails the run instead. RunDiskParkMax == 0 is
	// unlimited. A preventive park never reaches the cap branch.
	maxParks := s.p.RunDiskParkMax
	if counted && maxParks != 0 && int(run.DiskParkCount)+1 > maxParks {
		reason := fmt.Sprintf("the worker's data volume stayed full across %d parks", run.DiskParkCount+1)
		n, ferr := qtx.SetRunFailed(ctx, store.SetRunFailedParams{
			FailureReason: pgconv.TextOrNull(reason),
			// SERVER-DERIVED, set directly — NOT through CoerceFailOrigin's worker-reportable gate
			// (data_volume_full is server-only). neverJudgeFailOrigins skips the judge for it.
			FailOrigin: pgconv.TextOrNull(recoveryCauseDataVolumeFull),
			SessionID:  sessionID,
			ID:         run.ID,
			WorkerID:   workerID,
			// Already fenced by the FOR UPDATE lock plus the generation check above, as in
			// parkForgeUnreachable's cap-fail: explicit nil.
			ClaimGeneration: pgtype.Int8{},
		})
		if ferr != nil {
			return store.Run{}, 0, ferr
		}
		if n != 1 {
			// The status='running' guard did not match under the lock; commit nothing.
			return run, 0, nil
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, 0, err
		}
		return run, 1, nil
	}

	retryNotBefore := s.now().Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter())
	parked, perr := qtx.ParkRunDataVolumeFull(ctx, store.ParkRunDataVolumeFullParams{
		Counted:         counted,
		RetryNotBefore:  pgconv.Time(retryNotBefore),
		SessionID:       sessionID,
		ID:              run.ID,
		WorkerID:        workerID,
		ClaimGeneration: pgconv.Int8Ptr(req.ClaimGeneration),
	})
	if perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			// A guard did not match under the lock — impossible given the checks above, but
			// answer the locked row as a no-op rather than commit anything.
			return run, 0, nil
		}
		return store.Run{}, 0, perr
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, 0, err
	}
	return parked, 1, nil
}
