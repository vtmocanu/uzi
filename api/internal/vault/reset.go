package vault

import (
	"context"
	"errors"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// sqlStateDeadlockDetected is PostgreSQL's deadlock_detected SQLSTATE.
const sqlStateDeadlockDetected = "40P01"

// resetStore is the transaction-bound storage a destructive reset needs beyond
// BrowserStore. Reset type-asserts the transaction's Store to it, like
// authMutation does for BrowserStore; fixtures that do not implement it get
// ErrNotConfigured.
type resetStore interface {
	LockOwnerWorkersForReset(context.Context, uuid.UUID) ([]uuid.UUID, error)
	LockOwnerRunsForReset(context.Context, uuid.UUID) ([]uuid.UUID, error)
	LockOwnerSecretsForReset(context.Context, uuid.UUID) ([]uuid.UUID, error)
	DeleteAllUserSecretsForOwner(context.Context, uuid.UUID) (int64, error)
	DeleteAllCodexProviderAccountsForOwner(context.Context, uuid.UUID) (int64, error)
	ClearSidebarCodexAccounts(context.Context, uuid.UUID) error
}

var _ resetStore = (*store.Queries)(nil)

// Reset destroys the owner's vault material and leaves the account protected with
// no wrapper: every user_secrets row and codex_provider_account row (whatever its
// sealing marker), the wrapper, and the sidebar account ids go, and the vault
// epoch advances. It never reads or reuses a cached DEK; the owner's cached key is
// evicted on every outcome.
//
// fn runs inside the same transaction after the lifecycle locks are held and
// before anything is deleted, so the caller's own writes (link consumption, auth
// changes, CLI-request denial) commit or roll back together with the deletion. fn
// may run twice: a deadlock-detected failure (SQLSTATE 40P01) rolls the whole
// transaction back and Reset retries it exactly once, so fn must be a pure
// function of the transaction it is given. The single retry exists because a
// binding acquired after the up-front row locks can still meet the cascade.
//
// Atomicity: one PostgreSQL transaction. A failure or crash before commit rolls
// everything back. An ambiguous commit acknowledgment is never retried and is
// reported as UncertainOutcomeError (errors.Is ErrUncertainOutcome) after one
// reconciliation attempt in finishLifecycle; the caller must refresh durable
// state. Durability is whatever commit guarantee the PostgreSQL server is
// configured with.
//
// Lock order: the per-owner lifecycle mutex, the owner's advisory secret-mutation
// guard, the owner's worker rows, the owner's run rows, the owner's user_secrets
// rows, then the users row and the wrapper. Worker, run, then users matches
// setRecoveryWaitLocked in workersvc, so the credential-binding SET NULL cascades
// of the deletes cannot invert that order against a parking run. Secrets before
// users matches run creation with a pinned override, which share-locks the secret
// and then needs KEY SHARE on users for the runs INSERT. Codex provider accounts
// are not locked: their share locks are NOWAIT, taken under a run row lock Reset
// already holds, and are not followed by a users-referencing insert.
//
// Reset owns its transaction and must never be called from inside a
// caller-opened one (PRD #3989 D14): every lifecycle operation takes the in-process
// mutex before its transaction, and a caller transaction would invert that order.
func (v *Vault) Reset(ctx context.Context, owner uuid.UUID, fn func(context.Context, BrowserStore) error) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, LifecycleTimeout)
	defer cancel()
	release := v.acquireLifecycle(owner)
	defer release()
	// The key must not survive any attempt, successful or not.
	defer v.evict(owner)
	err := v.transact(ctx, func(ctx context.Context, q Store) error { return resetTx(ctx, q, owner, fn) })
	if isDeadlock(err) {
		err = v.transact(ctx, func(ctx context.Context, q Store) error { return resetTx(ctx, q, owner, fn) })
	}
	return v.finishLifecycle(owner, err)
}

func resetTx(ctx context.Context, q Store, owner uuid.UUID, fn func(context.Context, BrowserStore) error) error {
	bq, ok := q.(BrowserStore)
	rq, rok := q.(resetStore)
	if !ok || !rok {
		return ErrNotConfigured
	}
	if err := q.LockSecretMutation(ctx, owner); err != nil {
		return err
	}
	if _, err := rq.LockOwnerWorkersForReset(ctx, owner); err != nil {
		return err
	}
	if _, err := rq.LockOwnerRunsForReset(ctx, owner); err != nil {
		return err
	}
	if _, err := rq.LockOwnerSecretsForReset(ctx, owner); err != nil {
		return err
	}
	state, _, _, err := lockVaultRows(ctx, q, owner)
	if err != nil {
		return err
	}
	if state.VaultEpoch == math.MaxInt64 {
		return ErrEpochExhausted
	}
	if err := fn(ctx, bq); err != nil {
		return err
	}
	if _, err := rq.DeleteAllUserSecretsForOwner(ctx, owner); err != nil {
		return err
	}
	if _, err := rq.DeleteAllCodexProviderAccountsForOwner(ctx, owner); err != nil {
		return err
	}
	if _, err := q.DeleteUserVault(ctx, owner); err != nil {
		return err
	}
	if err := rq.ClearSidebarCodexAccounts(ctx, owner); err != nil {
		return err
	}
	// The users-row trigger bumps the browser-slot generation with the epoch.
	return advanceMode(ctx, q, owner, store.VaultModeProtected, state.VaultEpoch+1)
}

// isDeadlock reports a pre-commit PostgreSQL deadlock abort. A CommitError is
// never a deadlock retry: its outcome is uncertain, and a replay could repeat
// an already committed reset.
func isDeadlock(err error) bool {
	var commit *CommitError
	if err == nil || errors.As(err, &commit) {
		return false
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlStateDeadlockDetected
}
