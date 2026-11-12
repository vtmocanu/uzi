package workersvc

import (
	"context"
	"errors"
	"fmt"

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
//   - A run that already has a wake row never gets another: one wake per pending run. The row
//     lives until the run leaves 'queued' (ReleaseStaleWake), so a worker that settled or was
//     re-hibernated does not trigger a second wake, and no sibling is woken.
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

	w, err := q.GetWorkerForUpdate(ctx, workerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return DemandWakeNone, nil // the candidate was deleted
	}
	if err != nil {
		return DemandWakeNone, err
	}

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

	if _, gerr := q.GetHibernationWake(ctx, runID); gerr == nil {
		return DemandWakeNone, nil // already woke once for this run: reuse or no second wake
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

// ReleaseStaleWake deletes run runID's wake row only when the run is no longer queued (or no longer
// exists). A row whose worker settled or was re-hibernated is kept: the run already woke once.
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
	stale := errors.Is(err, pgx.ErrNoRows) || run.Status != "queued"
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
