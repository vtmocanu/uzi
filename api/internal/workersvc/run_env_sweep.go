package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Run environment retired-version sweeper (PRD #4416 M2), the always-registered sweeper.Pass
// "run_env_retired_sweep".
//
// A retired or pending-deletion secret version stays sealed only while a HELD run holds a secret
// receipt for it (fn_run_env_run_held, migration 00543: not terminal, terminal for less than 24
// hours, or holding any recovery custody). Once no held run does, the version is deleted; then
// the variable identities that no row and no receipt references are deleted too.
//
// Lock order: the pass takes nothing but the owner's users row (FOR SHARE NOWAIT) and then the
// run_env_vars / run_env_var_ids / run_env_receipts rows it deletes. It never locks a run or a
// custody row, so it sits after nothing and before nothing in the run -> users order of the
// claim finisher. A busy owner is skipped until the next pass. Every batch is one transaction,
// so a failure rolls back one batch, and a re-run finds the same work (the pass is idempotent).
// It is effect-inert when there is nothing to do: one indexed owner scan.
const (
	// runEnvSweepPassBudget bounds one pass.
	runEnvSweepPassBudget = 10 * time.Second
	// runEnvSweepMaxUsers bounds the owners one pass looks at.
	runEnvSweepMaxUsers = 25
	// runEnvSweepBatch bounds the rows one transaction deletes.
	runEnvSweepBatch = 200
	// runEnvSweepMaxBatches bounds the transactions one owner gets per pass.
	runEnvSweepMaxBatches = 5
)

// runEnvSweepState is the owner cursor, so owners whose versions are all held do not starve the
// ones behind them. The pass is serial; the mutex covers a manual call from a test or a console.
type runEnvSweepState struct {
	mu    sync.Mutex
	after uuid.UUID
}

// runEnvSweepQueries is the owner scan the pass needs beyond Store, satisfied by *store.Queries.
type runEnvSweepQueries interface {
	ListRunEnvSweepUsers(context.Context, store.ListRunEnvSweepUsersParams) ([]uuid.UUID, error)
}

// SweepRunEnvRetired runs one pass and returns the number of rows it deleted.
func (s *Service) SweepRunEnvRetired(ctx context.Context) (int64, error) {
	rq, ok := s.q.(runEnvSweepQueries)
	if !ok || s.txBeginner == nil {
		return 0, errors.New("run env sweep: queries are not wired")
	}
	ctx, cancel := context.WithTimeout(ctx, runEnvSweepPassBudget)
	defer cancel()

	s.runEnvSweep.mu.Lock()
	defer s.runEnvSweep.mu.Unlock()
	users, err := rq.ListRunEnvSweepUsers(ctx, store.ListRunEnvSweepUsersParams{After: s.runEnvSweep.after, MaxUsers: runEnvSweepMaxUsers})
	if err != nil {
		return 0, fmt.Errorf("run env sweep: list owners: %w", err)
	}
	if len(users) < runEnvSweepMaxUsers {
		s.runEnvSweep.after = uuid.Nil // the scan reached the end: start over next pass
	} else {
		s.runEnvSweep.after = users[len(users)-1]
	}
	var deleted int64
	var errs []error
	for _, user := range users {
		if ctx.Err() != nil {
			break
		}
		n, err := s.sweepRunEnvOwner(ctx, user)
		deleted += n
		if err != nil {
			slog.Error("run env sweep: owner failed", "user", user, "error", err)
			errs = append(errs, err)
		}
	}
	return deleted, errors.Join(errs...)
}

// sweepRunEnvOwner deletes one owner's sweepable rows in bounded batches, one transaction each.
func (s *Service) sweepRunEnvOwner(ctx context.Context, user uuid.UUID) (int64, error) {
	var total int64
	for range runEnvSweepMaxBatches {
		n, more, err := s.sweepRunEnvBatch(ctx, user)
		total += n
		if err != nil || !more || ctx.Err() != nil {
			return total, err
		}
	}
	return total, nil
}

// sweepRunEnvBatch is one transaction: lock the owner, delete one batch of versions, then one
// batch of identities. more reports that a batch came back full, so another may follow.
func (s *Service) sweepRunEnvBatch(ctx context.Context, user uuid.UUID) (n int64, more bool, err error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := store.New(tx)
	if _, err := q.LockUserForRunEnvSweepNowait(ctx, user); err != nil {
		if isLockNotAvailable(err) || errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil // busy or gone: the next pass looks again
		}
		return 0, false, err
	}
	versions, err := q.DeleteRunEnvSweepVersions(ctx, store.DeleteRunEnvSweepVersionsParams{UserID: user, Batch: runEnvSweepBatch})
	if err != nil {
		return 0, false, err
	}
	// The identities are scanned after the versions, so one that just lost its last row goes too.
	idents, err := q.DeleteRunEnvSweepIdentities(ctx, store.DeleteRunEnvSweepIdentitiesParams{UserID: user, Batch: runEnvSweepBatch})
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return versions + idents, versions == runEnvSweepBatch || idents == runEnvSweepBatch, nil
}
