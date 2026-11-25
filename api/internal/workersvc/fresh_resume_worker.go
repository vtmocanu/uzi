package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The destination worker's fresh resume routes (issue #4628, plan section 4): the bundle stream, the
// abandon, the seeded receipt and the state read. They ignore the UZI_FRESH_RESUME gate: a transfer the
// gate no longer admits must still be fetched, abandoned or reported on by the worker that holds it, and
// the sweep is what drains it.

// FreshAbandonedReason is the refusal reason the abandon route writes on the transfer row.
const FreshAbandonedReason = "abandoned"

// Errors of the destination routes; the handler maps each to its 409 / 404 / 503.
var (
	// ErrFreshNotClaimed: the transfer is not claimed by this worker at the claim generation it sent, or
	// the run is no longer its claim at that generation. Every fenced route's 409.
	ErrFreshNotClaimed = errors.New("fresh resume transfer is not claimed by this worker at this generation")
	// ErrFreshAttemptMismatch: a receipt for an attempt or generation other than the seeded one (a seeded
	// row only; a claimed row's bad attempt id is ErrFreshNotClaimed).
	ErrFreshAttemptMismatch = errors.New("fresh resume receipt does not match the seeded attempt")
	// ErrFreshNoBundle: the transfer's evidence is a checkpoint ref, which has no bundle to stream.
	ErrFreshNoBundle = errors.New("fresh resume transfer has no bundle to stream")
	// ErrFreshTransferNotFound: no such transfer for this run and destination (the state route's 404).
	ErrFreshTransferNotFound = errors.New("fresh resume transfer not found")
	// ErrFreshLocksBusy: the predecessor worker's row stayed locked by another transaction through every
	// retry; the caller retries the request.
	ErrFreshLocksBusy = errors.New("fresh resume locks busy")
)

const (
	// freshRouteLockAttempts bounds the retries of one route on SQLSTATE 55P03 from the predecessor's
	// NOWAIT lock; each retry is a whole new transaction after a short, growing pause.
	freshRouteLockAttempts = 5
	freshRouteLockPause    = 25 * time.Millisecond
)

// freshLocked is what a route's function sees: the transaction with the locks taken and the locked run
// and transfer row.
type freshLocked struct {
	tx  pgx.Tx
	q   *store.Queries
	run store.Run
	row store.RunFreshResume
}

// fenced reports whether the calling worker holds the transfer's claim: the row is claimed for this
// owner's run at the destination and generation the caller sent, and the run is still that claim.
func (l freshLocked) fenced(wkr store.Worker, gen int64) bool {
	return l.row.State == "claimed" && l.row.RunID == l.run.ID && l.row.UserID == wkr.UserID &&
		l.row.DestinationWorkerID == pgconv.UUID(wkr.ID) &&
		l.row.ClaimGeneration.Valid && l.row.ClaimGeneration.Int64 == gen &&
		l.run.UserID == wkr.UserID && l.run.WorkerID == pgconv.UUID(wkr.ID) && l.run.ClaimGeneration == gen
}

// withFreshLocks runs fn in one READ COMMITTED transaction holding, in this order: the calling worker's
// row (workerLocks), the transfer's saved predecessor FOR KEY SHARE NOWAIT (workerLocks, a claimed
// transfer only: the caller already holds a worker lock), the run, then the transfer row. The whole
// transaction is retried on 55P03. An fn error rolls back; nil commits.
func (s *Service) withFreshLocks(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, workerLocks bool, fn func(freshLocked) error) error {
	if s.txBeginner == nil {
		return ErrFreshResumeUnavailable
	}
	for attempt := 0; attempt < freshRouteLockAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * freshRouteLockPause):
			}
		}
		err := s.freshLocksOnce(ctx, wkr, runID, frid, workerLocks, fn)
		if !isLockNotAvailable(err) {
			return err
		}
	}
	return ErrFreshLocksBusy
}

func (s *Service) freshLocksOnce(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, workerLocks bool, fn func(freshLocked) error) error {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	// A plain unlocked read first: a transfer of another owner (or, on a fenced route, another destination)
	// is refused before any lock is taken, so a foreign worker can never queue on the run or worker locks.
	// The under-lock checks below stay the authority.
	pre, err := q.GetFreshResume(ctx, frid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (pre.RunID != runID || pre.UserID != wkr.UserID)) {
		return ErrFreshTransferNotFound
	}
	if err != nil {
		return err
	}
	if workerLocks && pre.DestinationWorkerID != pgconv.UUID(wkr.ID) {
		return ErrFreshNotClaimed
	}
	if workerLocks {
		if _, err := q.GetWorkerForUpdate(ctx, wkr.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrFreshNotClaimed
			}
			return err
		}
	}
	unlocked, err := q.GetFreshResume(ctx, frid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && unlocked.RunID != runID) {
		return ErrFreshTransferNotFound
	}
	if err != nil {
		return err
	}
	if workerLocks && unlocked.State == "claimed" && unlocked.SavedWorkerID.Valid {
		if _, err := q.LockWorkerKeyShareNoWait(ctx, uuid.UUID(unlocked.SavedWorkerID.Bytes)); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	run, err := q.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFreshTransferNotFound
	}
	if err != nil {
		return err
	}
	row, err := q.GetFreshResumeForUpdate(ctx, frid)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFreshTransferNotFound
	}
	if err != nil {
		return err
	}
	if err := fn(freshLocked{tx: tx, q: q, run: run, row: row}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FreshResumeBundleCapture is the bundle route's fence: the transfer is claimed by this worker at gen and
// its evidence is a capture. It returns the capture to stream. The fence is checked under the locks and
// released before the stream starts; the capture itself stays protected from discard and expiry while the
// transfer is claimed (CaptureHasActiveFreshResume).
func (s *Service) FreshResumeBundleCapture(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, gen int64) (uuid.UUID, error) {
	var capture uuid.UUID
	err := s.withFreshLocks(ctx, wkr, runID, frid, true, func(l freshLocked) error {
		if !l.fenced(wkr, gen) {
			return ErrFreshNotClaimed
		}
		if l.row.EvidenceKind != apitypes.FreshResumeKindCapture || !l.row.CaptureID.Valid {
			return ErrFreshNoBundle
		}
		capture = uuid.UUID(l.row.CaptureID.Bytes)
		return nil
	})
	return capture, err
}

// AbandonFreshResume is the destination giving the transfer back: refused with reason "abandoned", the
// run's saved tuple restored to its held recovery_wait. The caller holds its own worker row, so the
// predecessor lock is NOWAIT (RefuseNoWait).
func (s *Service) AbandonFreshResume(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, gen int64) error {
	restored := false
	err := s.withFreshLocks(ctx, wkr, runID, frid, true, func(l freshLocked) error {
		restored = false
		if !l.fenced(wkr, gen) {
			return ErrFreshNotClaimed
		}
		outcome, err := RefuseFreshResumeInTx(ctx, l.tx, frid, FreshAbandonedReason, RefuseNoWait())
		if err != nil {
			return err
		}
		switch outcome {
		case FreshRefusedRestored:
			restored = true
		case FreshRefusedRunMoved:
		default:
			return ErrFreshNotClaimed
		}
		return nil
	})
	if err == nil && restored {
		s.publishSwept(runID, "recovery_wait")
	}
	return err
}

// FreshResumeReceipt is the destination's seeded receipt (plan section 4, "Receipt"). Under the locks:
// a claimed transfer at the caller's G' whose attempt id carries -g<G'>- supersedes the run's earlier
// seeded row and is marked seeded (the first fenced commit); a replay of the same attempt and generation
// is a success; a different attempt or generation on a seeded row is ErrFreshAttemptMismatch; anything
// else, including a claimed row whose attempt id does not carry -g<G'>-, is ErrFreshNotClaimed. The reply carries current_authority, judged under the run lock against liveGen.
func (s *Service) FreshResumeReceipt(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, gen int64, attemptID string, liveGen int64) (apitypes.FreshResumeStateDTO, error) {
	var dto apitypes.FreshResumeStateDTO
	err := s.withFreshLocks(ctx, wkr, runID, frid, true, func(l freshLocked) error {
		row := l.row
		mine := row.UserID == wkr.UserID && row.RunID == runID && row.DestinationWorkerID == pgconv.UUID(wkr.ID)
		switch {
		case row.State == "claimed" && l.fenced(wkr, gen):
			if _, err := l.q.SupersedePriorSeeded(ctx, store.SupersedePriorSeededParams{RunID: runID, ID: frid}); err != nil {
				return err
			}
			marked, err := l.q.MarkFreshResumeSeeded(ctx, store.MarkFreshResumeSeededParams{
				ID: frid, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: pgtype.Int8{Int64: gen, Valid: true}, AttemptID: attemptID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				// Every other fact was just checked under the locks, so the attempt id is what failed. A
				// claimed row is not yet seeded, so the plan's receipt table answers not-claimed here;
				// the attempt mismatch is only for an already seeded row.
				return ErrFreshNotClaimed
			}
			if err != nil {
				return err
			}
			row = marked
		case mine && row.SeededAt.Valid && row.State == "seeded":
			if row.DestinationAttemptID.String != attemptID || row.SeededGeneration.Int64 != gen {
				return ErrFreshAttemptMismatch
			}
		default:
			return ErrFreshNotClaimed
		}
		authority, err := l.q.FreshResumeCurrentAuthority(ctx, store.FreshResumeCurrentAuthorityParams{
			RunID: runID, WorkerID: wkr.ID, LiveGeneration: liveGen,
		})
		if err != nil {
			return err
		}
		dto = freshStateDTO(row.State, row.SeededAt, row.DestinationAttemptID, row.SeededGeneration, authority)
		return nil
	})
	return dto, err
}

// FreshResumeState is GET .../fresh-resume/{frid}/state: fenced only on the run, its owner and the
// destination being the caller, never on the transfer's state or generation, so a worker can always ask
// what became of a transfer (a refused one answers "refused", seeded_at null). No such transfer for this
// caller is ErrFreshTransferNotFound.
func (s *Service) FreshResumeState(ctx context.Context, wkr store.Worker, runID, frid uuid.UUID, liveGen int64) (apitypes.FreshResumeStateDTO, error) {
	var dto apitypes.FreshResumeStateDTO
	err := s.withFreshLocks(ctx, wkr, runID, frid, false, func(l freshLocked) error {
		if l.run.UserID != wkr.UserID {
			return ErrFreshTransferNotFound
		}
		st, err := l.q.GetFreshResumeState(ctx, store.GetFreshResumeStateParams{ID: frid, RunID: runID, WorkerID: pgconv.UUID(wkr.ID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFreshTransferNotFound
		}
		if err != nil {
			return err
		}
		authority, err := l.q.FreshResumeCurrentAuthority(ctx, store.FreshResumeCurrentAuthorityParams{
			RunID: runID, WorkerID: wkr.ID, LiveGeneration: liveGen,
		})
		if err != nil {
			return err
		}
		dto = freshStateDTO(st.State, st.SeededAt, st.DestinationAttemptID, st.SeededGeneration, authority)
		return nil
	})
	return dto, err
}

func freshStateDTO(state string, seededAt pgtype.Timestamptz, attempt pgtype.Text, seededGen pgtype.Int8, authority bool) apitypes.FreshResumeStateDTO {
	dto := apitypes.FreshResumeStateDTO{State: state, CurrentAuthority: authority}
	if seededAt.Valid {
		at := seededAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
		dto.SeededAt = &at
	}
	if attempt.Valid {
		dto.DestinationAttemptID = &attempt.String
	}
	if seededGen.Valid {
		dto.SeededGeneration = &seededGen.Int64
	}
	return dto
}
