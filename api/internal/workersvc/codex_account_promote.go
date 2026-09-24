package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_promote.go is PRD #1590 M3 (D3): the promote_codex_account_available sweeper
// pass. A run held in recovery_wait with cause codex_account_unavailable has no timer (both
// timer promoters skip the cause); it goes back to queued only when the unchanged
// evalCodexReleasePredicate passes on its alias and account, read under lock.
//
// Lock order is run -> alias -> account, one transaction per run. The re-login writer
// (CodexReconciler.reconcileTuple: RefreshCodexAccountLogin, then LinkCodexCredentialState)
// takes the account before the alias, so both alias and account FOR SHARE locks are NOWAIT: a
// held row is SQLSTATE 55P03, the transaction rolls back with the run still held, and the next
// tick retries. The promoter never waits on either row, so it cannot close a deadlock cycle.
//
// Out of scope here (M4, D5): same-identity re-admission, and failing a held run whose alias
// was deleted. Until then a held run that fails the predicate, has no alias binding, or whose
// alias has no linked account simply stays held.

// codexAccountPromoteStore is the pass's page read. *store.Queries satisfies it; a fake Store
// that does not is reported as errCodexStoreUnavailable, like the park pass.
type codexAccountPromoteStore interface {
	ListCodexAccountWaitRunsPage(ctx context.Context, arg store.ListCodexAccountWaitRunsPageParams) ([]uuid.UUID, error)
}

// codexPromoteQueries is the statement surface of one per-run promotion transaction.
type codexPromoteQueries interface {
	LockCodexAccountWaitRunForUpdate(ctx context.Context, id uuid.UUID) (store.Run, error)
	LockCodexAliasForShareNowait(ctx context.Context, arg store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error)
	LockCodexAccountForShareNowait(ctx context.Context, arg store.LockCodexAccountForShareNowaitParams) (uuid.UUID, error)
	GetRunCodexAuthContext(ctx context.Context, id uuid.UUID) (store.GetRunCodexAuthContextRow, error)
	PromoteCodexAccountWaitRun(ctx context.Context, id uuid.UUID) (int64, error)
}

// codexPromoteTestHooks are LiveDB race seams (nil in production).
type codexPromoteTestHooks struct {
	// afterList runs for each listed run before its transaction opens.
	afterList func(ctx context.Context, runID uuid.UUID)
	// afterAliasLock runs once the alias FOR SHARE lock is held, before the account lock.
	afterAliasLock func(ctx context.Context, runID uuid.UUID)
}

// promoteCodexAccountAvailable runs one page of the promote_codex_account_available pass and
// returns how many held runs it promoted to queued. Each listed run is decided in its own
// transaction (promoteCodexAccountRun); a per-run error is logged, the page continues, and the
// errors are returned joined so Sweep logs the tick as failed while keeping the count. On a
// page-read error the cursor is left in place.
func (s *Service) promoteCodexAccountAvailable(ctx context.Context) (int64, error) {
	q, ok := s.q.(codexAccountPromoteStore)
	if !ok {
		return 0, errCodexStoreUnavailable
	}
	s.codexPromote.mu.Lock()
	defer s.codexPromote.mu.Unlock()
	ids, err := q.ListCodexAccountWaitRunsPage(ctx, store.ListCodexAccountWaitRunsPageParams{
		AfterID: s.codexPromote.after,
		PageCap: s.codexPromote.pageCap(),
	})
	if err != nil {
		return 0, fmt.Errorf("list codex account wait runs: %w", err)
	}
	last := uuid.Nil
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	s.codexPromote.advance(int64(len(ids)), last)

	var promoted int64
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		if h := s.codexPromoteHooks; h != nil && h.afterList != nil {
			h.afterList(ctx, id)
		}
		ok, err := s.promoteCodexAccountRun(ctx, id)
		if err != nil {
			slog.Warn("sweeper: promote codex account wait run failed", "run", id, "error", err)
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
			continue
		}
		if ok {
			promoted++
			s.publishSwept(id, "queued")
		}
	}
	return promoted, errors.Join(errs...)
}

// promoteCodexAccountRun decides one held run in one transaction: lock the run (status and
// cause re-checked), then the alias and the linked account FOR SHARE NOWAIT, re-read the
// authority context under those locks, and promote only if evalCodexReleasePredicate passes.
// It reports whether the run was promoted. A run that moved, is locked by another
// transaction, has no alias or linked account, meets a 55P03 on either NOWAIT lock, or fails
// the predicate is left held and is not an error.
func (s *Service) promoteCodexAccountRun(ctx context.Context, runID uuid.UUID) (bool, error) {
	if s.txBeginner == nil {
		return false, errClaimRecoveryNoTx
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	promoted, err := s.promoteCodexAccountRunTx(ctx, store.New(tx), runID)
	if err != nil || !promoted {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// promoteCodexAccountRunTx is promoteCodexAccountRun's body over the open transaction. It
// writes only through PromoteCodexAccountWaitRun, so returning false without committing (the
// caller rolls back) leaves every row untouched.
func (s *Service) promoteCodexAccountRunTx(ctx context.Context, q codexPromoteQueries, runID uuid.UUID) (bool, error) {
	run, err := q.LockCodexAccountWaitRunForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // cancelled or promoted since the list, or locked by another tx
	}
	if err != nil {
		return false, err
	}
	if !run.CodexSecretID.Valid {
		return false, nil // no alias binding: stays held (M4 decides terminal cases)
	}
	alias, err := q.LockCodexAliasForShareNowait(ctx, store.LockCodexAliasForShareNowaitParams{
		UserSecretID: uuid.UUID(run.CodexSecretID.Bytes), UserID: run.UserID,
	})
	if held, err := codexPromoteLockOutcome(err); held || err != nil {
		return false, err
	}
	if h := s.codexPromoteHooks; h != nil && h.afterAliasLock != nil {
		h.afterAliasLock(ctx, runID)
	}
	if !alias.ProviderAccountID.Valid {
		return false, nil // re-login staging or unlinked: stays held
	}
	_, err = q.LockCodexAccountForShareNowait(ctx, store.LockCodexAccountForShareNowaitParams{
		ID: uuid.UUID(alias.ProviderAccountID.Bytes), UserID: run.UserID,
	})
	if held, err := codexPromoteLockOutcome(err); held || err != nil {
		return false, err
	}
	row, err := q.GetRunCodexAuthContext(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if evalCodexReleasePredicate(codexReleaseInputsFromAuthRow(row)) != nil {
		return false, nil
	}
	n, err := q.PromoteCodexAccountWaitRun(ctx, runID)
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// codexPromoteLockOutcome maps a NOWAIT lock result: a missing row or a 55P03 leaves the run
// held for a later tick (held=true, no error); any other error is returned.
func codexPromoteLockOutcome(err error) (held bool, _ error) {
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, pgx.ErrNoRows), isLockNotAvailable(err):
		return true, nil
	default:
		return false, err
	}
}
