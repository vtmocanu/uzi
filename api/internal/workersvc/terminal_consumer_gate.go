package workersvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4488 (ADR-4007 T3): every consumer admission of a terminal-preserved ref takes the
// repository's shared consumer gate as the first lock of its create transaction, so a retirement
// (which try-takes the same gate exclusively) and an admission can never both decide on stale facts.

var (
	// ErrTerminalConsumerGateBusy is the store's busy sentinel: the shared gate could not be acquired
	// within the wait (a retirement holds it) or the context ended meanwhile. Nothing was created;
	// the caller may retry.
	ErrTerminalConsumerGateBusy = store.ErrTerminalConsumerGateBusy
	// ErrTerminalConsumerGateIdentity: the run a gated create targets vanished, or belongs to a
	// repository other than the one whose gate was taken. Not retryable; nothing was created.
	ErrTerminalConsumerGateIdentity = errors.New("workersvc: terminal consumer gate target run changed repository or vanished")
)

// sqlStateActiveSQLTransaction is the SQLSTATE of SET TRANSACTION ISOLATION LEVEL after a query.
const sqlStateActiveSQLTransaction = "25001"

// consumerGateWaitFor is the shared gate wait: the production constant unless a test overrode it.
func (s *Service) consumerGateWaitFor() time.Duration {
	if s.consumerGateWait > 0 {
		return s.consumerGateWait
	}
	return terminalConsumerGateWait
}

// takeConsumerGate is the ONLY place the production gate wait is passed. It runs as the first
// statements of the create transaction: READ COMMITTED (the helper refuses anything else), then the
// shared gate. A busy gate returns ErrTerminalConsumerGateBusy.
func (s *Service) takeConsumerGate(ctx context.Context, tx pgx.Tx, repo uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
		if ctx.Err() != nil {
			return errors.Join(ErrTerminalConsumerGateBusy, err)
		}
		// 25001: a statement already ran and the transaction is at another level, so it cannot be
		// forced to READ COMMITTED. Fail closed as the helper does. This check does NOT prove the
		// gate is first: on a READ COMMITTED transaction (or a savepoint) that already ran
		// statements the SET succeeds silently. Gate-first holds because every caller passes a
		// transaction it has just begun (createRunAtomic, insertGatedNoBeginner,
		// reconcileRunMRGated).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlStateActiveSQLTransaction {
			return errors.Join(store.ErrTerminalConsumerGateIsolation, err)
		}
		return fmt.Errorf("terminal consumer gate: set isolation: %w", err)
	}
	return store.New(tx).LockTerminalConsumerGateShared(ctx, repo, s.consumerGateWaitFor())
}

// insertGatedNoBeginner is createRunAtomic's test-only fallback for a service with no transaction
// beginner and a gated origin: the insert runs in a transaction of its own (isolation, gate, insert,
// commit). A handle that cannot begin a transaction fails closed with the BeginTx error.
func (s *Service) insertGatedNoBeginner(ctx context.Context, q *store.Queries, repo uuid.UUID, res resolvedHarness, insert func(q *store.Queries, resolved resolvedHarness) (store.Run, error)) (store.Run, error) {
	tx, err := q.BeginTx(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.takeConsumerGate(ctx, tx, repo); err != nil {
		return store.Run{}, err
	}
	run, err := insert(store.New(tx), res)
	if err != nil {
		return store.Run{}, err
	}
	if err := s.beforeCommit(run); err != nil {
		return store.Run{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, err
	}
	return run, nil
}

// reconcileRunMRGated runs ReconcileRunMR in a transaction of its own under the repository's shared
// consumer gate (ADR-4007 T3): the write is status-agnostic and can give a nonterminal ci_fix the
// mr_iid of another source, which is a consumer of that source. It runs detached from the request's
// cancellation. A repo-less run and a store that is not *store.Queries (the in-memory unit fake)
// use the plain write. With no transaction beginner a pool-bound *store.Queries opens its own
// transaction; a tx-bound one (the claim-locked facade, whose states return before the MR
// reconcile) fails closed with store.ErrTerminalConsumerGateNestedTx.
func (s *Service) reconcileRunMRGated(ctx context.Context, repo pgtype.UUID, arg store.ReconcileRunMRParams) error {
	ctx = context.WithoutCancel(ctx) // also detached by the caller (setStateWithTx postCtx); no deadline
	var tx pgx.Tx
	var err error
	switch q, isQueries := s.q.(*store.Queries); {
	case !repo.Valid:
		_, err = s.q.ReconcileRunMR(ctx, arg)
		return err
	case s.txBeginner != nil:
		tx, err = s.txBeginner.Begin(ctx)
	case isQueries:
		tx, err = q.BeginTx(ctx)
	default:
		_, err = s.q.ReconcileRunMR(ctx, arg)
		return err
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.takeConsumerGate(ctx, tx, repo.Bytes); err != nil {
		return err
	}
	qtx := store.New(tx)
	// After the gate: the run must still be in the repository whose gate was taken.
	if err := requireGateTargetRepo(ctx, qtx, arg.ID, repo); err != nil {
		return err
	}
	if _, err := qtx.ReconcileRunMR(ctx, arg); err != nil {
		return err
	}
	if s.reconcileBeforeCommitHook != nil {
		s.reconcileBeforeCommitHook(arg.ID)
	}
	return tx.Commit(ctx)
}

// gateRepoOf is the origin value for a gate on repo.
func gateRepoOf(repo uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: repo, Valid: true} }

// requireGateTargetRepo re-reads the target run's repository after the gate was taken and refuses
// (ErrTerminalConsumerGateIdentity) when the run vanished or lives in another repository than the
// gate key. An invalid want (no gate) passes, and so does a non-transactional in-memory store
// (the unit-test fake), which takes no gate either.
func requireGateTargetRepo(ctx context.Context, st Store, targetID uuid.UUID, want pgtype.UUID) error {
	q, ok := st.(*store.Queries)
	if !ok || !want.Valid {
		return nil
	}
	got, err := q.RunRepoForConsumerGate(ctx, targetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTerminalConsumerGateIdentity
	}
	if err != nil {
		return err
	}
	if !got.Valid || got.Bytes != want.Bytes {
		return ErrTerminalConsumerGateIdentity
	}
	return nil
}
