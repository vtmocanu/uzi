package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Retired repo variable sweep (PRD #4416 M2), the always-registered sweeper.Pass
// "run_env_retired_sweep". A retired version (a rotated secret some run still holds a receipt for) or a
// pending-deletion row is deleted once every run holding a secret receipt for it is released
// (fn_run_env_run_held): terminal for over 24 hours with no open custody hold, capture, held-ref
// publication, terminal preservation, salvage, prerequisite closure, publication resume, checkpoint
// retention or unresolved checkpoint publish attempt. It then drops the variable identities nothing
// references.
//
// EFFECT-INERT WHEN EMPTY: the pass starts with one bounded read of run_env_vars_retained_user_idx and
// returns when no user owns such a row.
//
// Bounds: at most runEnvSweepMaxUsers users per tick (a cursor continues after the last user, so a user
// whose rows stay held never starves the rest), runEnvSweepBatch rows per statement, one transaction per
// user, and the pass budget runEnvSweepBudget as a whole. One user's failure is logged and does not stop
// the others; the errors are joined and returned.
//
// Per user the transaction takes the users row FOR SHARE NOWAIT first, the same lock a claim's finish
// takes, so the sweep never runs against a variable write or vault change in flight; a busy owner is
// skipped until the next tick. A claim that sees a deleted row reports history unavailable and parks, so
// the hold predicate is the only thing standing between a still-needed version and its deletion.
const (
	runEnvSweepMaxUsers = 25
	runEnvSweepBatch    = 200
	runEnvSweepBudget   = 20 * time.Second
)

// SweepRunEnvRetired runs one pass and returns the number of rows deleted (versions plus identities).
func (s *Service) SweepRunEnvRetired(ctx context.Context) (int64, error) {
	if s.txBeginner == nil {
		return 0, errors.New("run env sweep: no transaction source is wired")
	}
	ctx, cancel := context.WithTimeout(ctx, runEnvSweepBudget)
	defer cancel()
	s.runEnvSweepMu.Lock()
	after := s.runEnvSweepCursor
	s.runEnvSweepMu.Unlock()
	users, err := s.q.ListRunEnvSweepUsers(ctx, store.ListRunEnvSweepUsersParams{AfterUserID: after, MaxUsers: runEnvSweepMaxUsers})
	if err != nil {
		return 0, fmt.Errorf("run env sweep: list users: %w", err)
	}
	s.runEnvSweepMu.Lock()
	if len(users) < runEnvSweepMaxUsers {
		s.runEnvSweepCursor = uuid.Nil // reached the end: start over next tick
	} else {
		s.runEnvSweepCursor = users[len(users)-1]
	}
	s.runEnvSweepMu.Unlock()
	var deleted int64
	var errs []error
	for _, userID := range users {
		if ctx.Err() != nil {
			break // the rest waits for the next tick
		}
		n, err := s.sweepRunEnvUser(ctx, userID)
		deleted += n
		if err != nil {
			slog.Error("run env sweep: user failed", "user_id", userID, "error", err) // carries no value
			errs = append(errs, err)
		}
	}
	return deleted, errors.Join(errs...)
}

// sweepRunEnvUser deletes one user's releasable rows in one transaction. A busy owner row (55P03) is a
// skip, not an error.
func (s *Service) sweepRunEnvUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	q := store.New(tx)
	if _, err := q.LockUserVaultStateForShareNowait(ctx, userID); err != nil {
		if isLockNotAvailable(err) {
			return 0, nil
		}
		return 0, err
	}
	versions, err := q.DeleteReleasedRunEnvVars(ctx, store.DeleteReleasedRunEnvVarsParams{UserID: userID, MaxRows: runEnvSweepBatch})
	if err != nil {
		return 0, err
	}
	identities, err := q.DeleteUnreferencedRunEnvVarIDs(ctx, store.DeleteUnreferencedRunEnvVarIDsParams{UserID: userID, MaxRows: runEnvSweepBatch})
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	committed = true
	return versions + identities, nil
}
