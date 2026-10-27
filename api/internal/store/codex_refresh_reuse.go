package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CodexRefreshReuseParams fences admission to the locally opened account snapshot.
type CodexRefreshReuseParams struct {
	UserID         uuid.UUID
	AccountID      uuid.UUID
	OperationID    uuid.UUID
	FromGeneration int64
}

// AdmitCodexRefreshReuse consumes at most one retry, with no provider work in the
// transaction. LockSecretMutation serializes alias enable/disable and owner secret
// mutation; the account lock then serializes restore and operation/generation changes.
// A false result means no admission, including an ambiguous commit error.
func AdmitCodexRefreshReuse(ctx context.Context, b CodexRejectionTxBeginner, arg CodexRefreshReuseParams) (bool, error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockSecretMutation(ctx, tx, arg.UserID); err != nil {
		return false, err
	}
	q := New(tx)
	if _, err := q.LockCodexRefreshReuseAccount(ctx, LockCodexRefreshReuseAccountParams{
		AccountID: arg.AccountID, UserID: arg.UserID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	// Match the rejection/resolution account-before-intent order. The clock must
	// follow every potentially blocking lock, including this final row lock.
	if _, err := q.LockRotatingCodexRefreshIntent(ctx, LockRotatingCodexRefreshIntentParams{
		Op: arg.OperationID, UserID: arg.UserID, ProviderAccountID: arg.AccountID, FromGeneration: arg.FromGeneration,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	admittedAt, err := q.LeaseClockNow(ctx)
	if err != nil || !admittedAt.Valid {
		return false, err
	}
	n, err := q.ConsumeCodexRefreshReuse(ctx, ConsumeCodexRefreshReuseParams{
		UserID: arg.UserID, AccountID: arg.AccountID, OperationID: arg.OperationID, FromGeneration: arg.FromGeneration,
		AdmittedAt: admittedAt,
	})
	if err != nil || n != 1 {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
