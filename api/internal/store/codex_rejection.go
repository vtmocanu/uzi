package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CodexRejectionTxBeginner opens the transaction QuarantineRejectedCodexRefresh runs in.
// *pgxpool.Pool satisfies it.
type CodexRejectionTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// QuarantineRejectedCodexRefreshParams identifies the refresh operation the provider
// rejected (issue #1594): the owner, the account, the operation that held the lease, and
// the generation the rotation started from.
type QuarantineRejectedCodexRefreshParams struct {
	UserID         uuid.UUID
	AccountID      uuid.UUID
	OperationID    uuid.UUID
	FromGeneration int64
}

// CodexRejectionOutcome reports whether QuarantineRejectedCodexRefresh wrote anything.
type CodexRejectionOutcome int

const (
	// CodexRejectionNotApplied (the zero value): a fence did not hold, or an error
	// occurred, and the transaction was rolled back, so nothing changed.
	CodexRejectionNotApplied CodexRejectionOutcome = iota
	// CodexRejectionApplied: the account was quarantined with reauth_required and
	// reauth_reason='provider_rejected', and the intent marked 'unrecoverable', in one
	// committed transaction.
	CodexRejectionApplied
)

// codexRejectionAfterAccountLock is a test seam: nil in production, set only by the
// store's own tests to act between the account lock and the intent lock.
var codexRejectionAfterAccountLock func(ctx context.Context)

// QuarantineRejectedCodexRefresh records a provider rejection of an operation's refresh
// material (issue #1594) in ONE transaction: the account is quarantined and flagged
// re-login-required with reason 'provider_rejected', and the operation's rotating intent
// is marked 'unrecoverable'. Either both writes commit or neither does.
//
// The account row is locked first, then the intent. No lock cycle forms with the other
// statements that touch both tables: ClearMismatchedCodexRecoveryAndMarkIntents (one
// statement) updates only intents already 'reconciled', never a 'rotating' intent this
// primitive locks, and SetCodexRefreshIntentStateFenced only reads the account in an EXISTS
// subquery (no row lock on it) while it updates the intent. (InsertCodexRefreshIntent's
// foreign-key check takes a KEY SHARE lock on the account, but its only intent row is the
// one it is inserting, which no other transaction can hold.) Each lock re-checks its fence
// (see the query comments); a fence that does not hold rolls back and returns
// CodexRejectionNotApplied with a nil error. Any database error rolls back and returns
// CodexRejectionNotApplied with that error. No recovery column is written.
func QuarantineRejectedCodexRefresh(ctx context.Context, b CodexRejectionTxBeginner, arg QuarantineRejectedCodexRefreshParams) (CodexRejectionOutcome, error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return CodexRejectionNotApplied, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := New(tx)

	if _, err := qtx.LockCodexAccountForRejection(ctx, LockCodexAccountForRejectionParams{
		ID: arg.AccountID, UserID: arg.UserID, Op: arg.OperationID, FromGeneration: arg.FromGeneration,
	}); err != nil {
		return notAppliedUnlessErr(err)
	}

	if codexRejectionAfterAccountLock != nil {
		codexRejectionAfterAccountLock(ctx)
	}

	if _, err := qtx.LockRotatingCodexRefreshIntent(ctx, LockRotatingCodexRefreshIntentParams{
		Op: arg.OperationID, UserID: arg.UserID, ProviderAccountID: arg.AccountID, FromGeneration: arg.FromGeneration,
	}); err != nil {
		return notAppliedUnlessErr(err)
	}

	accountRows, err := qtx.ApplyCodexRejectionQuarantine(ctx, ApplyCodexRejectionQuarantineParams{
		ID: arg.AccountID, UserID: arg.UserID, Op: arg.OperationID, FromGeneration: arg.FromGeneration,
	})
	if err != nil {
		return CodexRejectionNotApplied, err
	}
	intentRows, err := qtx.MarkCodexRejectionIntentUnrecoverable(ctx, MarkCodexRejectionIntentUnrecoverableParams{
		Op: arg.OperationID, UserID: arg.UserID, ProviderAccountID: arg.AccountID, FromGeneration: arg.FromGeneration,
	})
	if err != nil {
		return CodexRejectionNotApplied, err
	}
	// Defensive invariant, not a live path: both rows are locked under fences each UPDATE's
	// WHERE repeats verbatim, so each touches exactly one row. Should a future edit let the
	// two diverge, roll back rather than commit a half-applied rejection.
	if accountRows != 1 || intentRows != 1 {
		return CodexRejectionNotApplied, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return CodexRejectionNotApplied, err
	}
	return CodexRejectionApplied, nil
}

// notAppliedUnlessErr maps a lock query's pgx.ErrNoRows (the fence did not hold) to a
// clean NotApplied and passes every other error through.
func notAppliedUnlessErr(err error) (CodexRejectionOutcome, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return CodexRejectionNotApplied, nil
	}
	return CodexRejectionNotApplied, err
}
