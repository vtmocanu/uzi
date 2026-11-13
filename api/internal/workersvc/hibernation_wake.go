package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// WorkerWakeTimeout is the fixed worker_wake_timeout of PRD #4205 (not a setting): how long a
// recorded demand wake suppresses ephemeral provisioning for its run, measured from the FIRST
// wake. It is HibernationWakeTimeout, named for the demand-wake consumer.
const WorkerWakeTimeout = HibernationWakeTimeout

// DemandWakeResult is what one DemandWake call did.
type DemandWakeResult int

const (
	// DemandWakeNone: nothing was recorded (the run already has a wake, is no longer a valid
	// demand, or a concurrent pass won).
	DemandWakeNone DemandWakeResult = iota
	// DemandWakeWoke: a wake row was recorded and the worker moved to waking with a fresh op id.
	DemandWakeWoke
	// DemandWakeAttached: a wake row was recorded against a worker that was already waking.
	DemandWakeAttached
)

// DemandWake records that run runID demands worker workerID and moves the worker to waking
// (PRD #4205 M5).
//
// One READ COMMITTED transaction. The candidate worker is locked FOR UPDATE, then the run row FOR
// SHARE (worker-then-run, as ClaimRun), and the run's status is re-read under that lock: a run that
// is no longer queued is never woken for.
//
//   - A run that already has a wake row never gets another while that row's worker is waking or
//     settled awake: one wake per pending run, and no sibling is woken. The row lives until the run
//     leaves 'queued' (ReleaseStaleWake). A row whose worker is hibernated or preparing again (a
//     recovery path: HibernateWorker refuses to re-hibernate under a queued run's wake) is deleted
//     under that worker's lock and the run is treated as fresh demand.
//   - A waking candidate whose wake_error is set is refused: a failed wake must not capture demand.
//   - A candidate that is already waking only gets the row (DemandWakeAttached). Otherwise it moves
//     to waking through moveToWaking with a fresh op id (a preparing worker is withdrawn the same
//     way) and the result is DemandWakeWoke. INSERT .. ON CONFLICT DO NOTHING admits exactly one
//     winner among concurrent passes; a loser rolls back.
//
// A Commit error is an UNKNOWN outcome: it is returned as is and never retried here. The next
// pass reads the real state, so a commit that did land is reused, never repeated.
func (s *Service) DemandWake(ctx context.Context, runID, workerID uuid.UUID) (DemandWakeResult, error) {
	if s.txBeginner == nil {
		return DemandWakeNone, ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return DemandWakeNone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	// The row's worker (when it is not the candidate) is locked together with the candidate, in id
	// order, so two passes touching the same pair cannot deadlock. The row is read again under the
	// locks: one re-pointed meanwhile is left to the next pass.
	ids := []uuid.UUID{workerID}
	if prior, perr := q.GetHibernationWake(ctx, runID); perr == nil {
		if prior.WorkerID != workerID {
			ids = append(ids, prior.WorkerID)
		}
	} else if !errors.Is(perr, pgx.ErrNoRows) {
		return DemandWakeNone, perr
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	locked := make(map[uuid.UUID]store.Worker, len(ids))
	for _, id := range ids {
		lw, lerr := q.GetWorkerForUpdate(ctx, id)
		if errors.Is(lerr, pgx.ErrNoRows) {
			if id == workerID {
				return DemandWakeNone, nil // the candidate was deleted
			}
			continue // the row's worker was deleted; its row went with it
		}
		if lerr != nil {
			return DemandWakeNone, lerr
		}
		locked[id] = lw
	}
	w := locked[workerID]

	status, err := q.LockRunForShare(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DemandWakeNone, nil
	}
	if err != nil {
		return DemandWakeNone, err
	}
	if status != "queued" {
		return DemandWakeNone, nil
	}

	if cur, gerr := q.GetHibernationWake(ctx, runID); gerr == nil {
		rw, held := locked[cur.WorkerID]
		if !held || (rw.HibernationState != "hibernated" && rw.HibernationState != "preparing") {
			// Waking: reuse. Settled awake: the run already woke once, and no sibling is woken.
			return DemandWakeNone, nil
		}
		// The row's worker went back to sleep while the run was still queued (a recovery path:
		// HibernateWorker refuses this while the run is queued). The wake can no longer serve the
		// run, so the row is dropped and the run is fresh demand.
		if _, derr := q.DeleteHibernationWake(ctx, store.DeleteHibernationWakeParams{RunID: runID, WorkerID: cur.WorkerID}); derr != nil {
			return DemandWakeNone, derr
		}
	} else if !errors.Is(gerr, pgx.ErrNoRows) {
		return DemandWakeNone, gerr
	}

	run, err := q.GetRunByID(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DemandWakeNone, nil
	}
	if err != nil {
		return DemandWakeNone, err
	}
	if run.UserID != w.UserID || w.Kind != "hosted" || w.Ephemeral {
		return DemandWakeNone, nil
	}
	if w.HibernationState != "hibernated" && w.HibernationState != "preparing" && w.HibernationState != "waking" {
		return DemandWakeNone, nil
	}
	if w.HibernationState == "waking" && w.WakeError.Valid {
		return DemandWakeNone, nil
	}

	now := s.now()
	if _, ierr := q.InsertHibernationWake(ctx, store.InsertHibernationWakeParams{
		RunID: runID, WorkerID: workerID, FirstWokenAt: pgconv.Time(now),
	}); errors.Is(ierr, pgx.ErrNoRows) {
		return DemandWakeNone, nil // another pass recorded this run's wake first
	} else if ierr != nil {
		return DemandWakeNone, ierr
	}
	res := DemandWakeAttached
	if moveToWaking(&w, now, "demand: run "+runID.String()) {
		res = DemandWakeWoke
		if _, werr := writeHibernation(ctx, q, w); werr != nil {
			return DemandWakeNone, werr
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DemandWakeNone, fmt.Errorf("demand wake commit (outcome unknown): %w", err)
	}
	return res, nil
}

// ReleaseStaleWake deletes run runID's wake row when the run is no longer queued (or no longer
// exists), or when the row's worker is hibernated or preparing again (the wake can no longer serve
// the run). A row whose worker settled awake is kept while the run is queued: it already woke once.
// The worker is locked first and the staleness is re-checked under the lock, so a wake being
// recorded concurrently is never torn. It reports whether a row was deleted.
func (s *Service) ReleaseStaleWake(ctx context.Context, runID uuid.UUID) (bool, error) {
	if s.txBeginner == nil {
		return false, ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	prior, err := q.GetHibernationWake(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w, err := q.GetWorkerForUpdate(ctx, prior.WorkerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // the cascade already removed the row
	}
	if err != nil {
		return false, err
	}
	cur, err := q.GetHibernationWake(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cur.WorkerID != w.ID {
		return false, nil // re-pointed while we waited for the lock
	}
	run, err := q.GetRunByID(ctx, runID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	stale := errors.Is(err, pgx.ErrNoRows) || run.Status != "queued" ||
		w.HibernationState == "hibernated" || w.HibernationState == "preparing"
	if !stale {
		return false, nil
	}
	n, err := q.DeleteHibernationWake(ctx, store.DeleteHibernationWakeParams{RunID: runID, WorkerID: w.ID})
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("release stale wake commit (outcome unknown): %w", err)
	}
	return n > 0, nil
}
