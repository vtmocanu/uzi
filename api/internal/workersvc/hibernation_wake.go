package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// WorkerWakeTimeout is the fixed worker_wake_timeout of PRD #4205 (not a setting): how long a
// recorded demand wake suppresses ephemeral provisioning for its run, measured from the FIRST
// wake. It is HibernationWakeTimeout, named for the demand-wake consumer.
const WorkerWakeTimeout = HibernationWakeTimeout

// DemandWake records that run runID demands worker workerID and moves the worker to waking
// (PRD #4205 M5). It reports true only when this call recorded a NEW wake for the run.
//
// One READ COMMITTED transaction. The wake row is read first, unlocked, only to learn which
// workers to lock: the candidate and, when the run already has a wake naming a different worker,
// that worker too, both taken FOR UPDATE in id order. The row is then re-read under the locks; a
// row that moved to a worker outside the locked set abandons the attempt (the next pass retries).
//
//   - A wake whose worker is still waking is reused: no new op id, no transition, no sibling wake.
//   - A wake whose worker is no longer waking is stale: it is deleted and the run is treated as
//     fresh. The INSERT .. ON CONFLICT DO NOTHING then admits exactly one winner; a loser rolls
//     the whole transaction back.
//   - A candidate that is already waking only gets the row. Otherwise it moves to waking through
//     moveToWaking with a fresh op id (a preparing worker is withdrawn the same way).
//
// A Commit error is an UNKNOWN outcome: it is returned as is and never retried here. The next
// pass reads the real state, so a commit that did land is reused, never repeated.
func (s *Service) DemandWake(ctx context.Context, runID, workerID uuid.UUID) (bool, error) {
	if s.txBeginner == nil {
		return false, ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	ids := []uuid.UUID{workerID}
	if prior, perr := q.GetHibernationWake(ctx, runID); perr == nil {
		if prior.WorkerID != workerID {
			ids = append(ids, prior.WorkerID)
		}
	} else if !errors.Is(perr, pgx.ErrNoRows) {
		return false, perr
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	locked := make(map[uuid.UUID]store.Worker, len(ids))
	for _, id := range ids {
		w, lerr := q.GetWorkerForUpdate(ctx, id)
		if errors.Is(lerr, pgx.ErrNoRows) {
			if id == workerID {
				return false, nil // the candidate was deleted
			}
			continue // the prior wake's worker is gone; its row went with it (ON DELETE CASCADE)
		}
		if lerr != nil {
			return false, lerr
		}
		locked[id] = w
	}

	cur, cerr := q.GetHibernationWake(ctx, runID)
	hasCur := cerr == nil
	if cerr != nil && !errors.Is(cerr, pgx.ErrNoRows) {
		return false, cerr
	}
	if hasCur {
		if _, ok := locked[cur.WorkerID]; !ok {
			return false, nil // the row moved while we waited; the next pass sees it
		}
	}

	run, err := q.GetRunByID(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w := locked[workerID]
	if run.Status != "queued" || run.UserID != w.UserID || w.Kind != "hosted" || w.Ephemeral {
		return false, nil
	}
	if w.HibernationState != "hibernated" && w.HibernationState != "preparing" && w.HibernationState != "waking" {
		return false, nil
	}

	if hasCur {
		if curWorker, ok := locked[cur.WorkerID]; ok && curWorker.HibernationState == "waking" {
			return false, nil // reuse: one wake per pending run
		}
		if _, derr := q.DeleteHibernationWake(ctx, store.DeleteHibernationWakeParams{RunID: runID, WorkerID: cur.WorkerID}); derr != nil {
			return false, derr
		}
	}

	now := s.now()
	if _, ierr := q.InsertHibernationWake(ctx, store.InsertHibernationWakeParams{
		RunID: runID, WorkerID: workerID, FirstWokenAt: pgconv.Time(now),
	}); errors.Is(ierr, pgx.ErrNoRows) {
		return false, nil // another pass recorded this run's wake first
	} else if ierr != nil {
		return false, ierr
	}
	if moveToWaking(&w, now, "demand: run "+runID.String()) {
		if _, werr := writeHibernation(ctx, q, w); werr != nil {
			return false, werr
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("demand wake commit (outcome unknown): %w", err)
	}
	return true, nil
}

// ReleaseStaleWake deletes run runID's wake row when it is stale: the run is no longer queued, or
// the worker it names is no longer waking. The worker is locked first and the staleness is
// re-checked under the lock, so a wake being recorded or settled concurrently is never torn. It
// reports whether a row was deleted.
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
	stale := errors.Is(err, pgx.ErrNoRows) || run.Status != "queued" || w.HibernationState != "waking"
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
