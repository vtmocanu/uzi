package vault

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// StateReader may be transaction-bound. Writers already holding the owner fence
// pass their current transaction to Seal instead of reading through the pool.
type StateReader interface {
	GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error)
}

// Store is the query seam shared by real transaction-bound queries and explicit
// crypto-only test fixtures. Tests do not obtain a nontransactional default.
type Store interface {
	StateReader
	LockSettingsAdmission(context.Context) error
	LockSecretMutation(context.Context, uuid.UUID) error
	LockVaultAuthState(context.Context, uuid.UUID) (store.LockVaultAuthStateRow, error)
	GetVaultPolicy(context.Context) (string, error)
	GetUserVault(context.Context, uuid.UUID) (store.UserVault, error)
	LockUserVault(context.Context, uuid.UUID) (store.UserVault, error)
	CreateUserVaultIfAbsent(context.Context, store.CreateUserVaultIfAbsentParams) (store.UserVault, error)
	DeleteUserVault(context.Context, uuid.UUID) (int64, error)
	AdvanceUserVaultMode(context.Context, store.AdvanceUserVaultModeParams) (store.AdvanceUserVaultModeRow, error)
	HasMasterVaultMaterial(context.Context, uuid.UUID) (bool, error)
	GetVaultMaterialTotals(context.Context, uuid.UUID) (store.GetVaultMaterialTotalsRow, error)
	LockVaultAccountMaterialBatch(context.Context, store.LockVaultAccountMaterialBatchParams) ([]store.LockVaultAccountMaterialBatchRow, error)
	LockVaultSecretMaterialBatch(context.Context, store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error)
	ResealVaultAccountMaterial(context.Context, store.ResealVaultAccountMaterialParams) (int64, error)
	ResealVaultSecretMaterial(context.Context, store.ResealVaultSecretMaterialParams) (int64, error)
	ClaimVaultLockNotice(context.Context, uuid.UUID) (uuid.UUID, error)
	ClearVaultLockNotice(context.Context, uuid.UUID) error
}

// Transact invokes fn synchronously exactly once, commits on nil, and rolls back
// on callback failure/panic. Callback queries must not escape. CommitError means
// the acknowledgment was uncertain and requires fresh locked reconciliation.
type Transact func(ctx context.Context, fn func(context.Context, Store) error) error

// Option configures a Vault at construction time.
type Option struct {
	onLifecycleWaitObserver func(uuid.UUID)
}

// WithLifecycleWaitObserverForTest reports a failed lifecycle TryLock. The
// observer must return promptly, be concurrency-safe, and never reenter Vault.
// It receives only the owner ID; it does not report mutex queue insertion.
func WithLifecycleWaitObserverForTest(observer func(uuid.UUID)) Option {
	return Option{onLifecycleWaitObserver: observer}
}

// New requires an explicit transaction implementation. Production uses
// NewPostgres; a nil callback is a programming error, never a fallback to fn(q).
func New(master *secretbox.Box, q Store, transact Transact, opts ...Option) *Vault {
	if transact == nil {
		panic("vault: transaction callback required")
	}
	v := &Vault{master: master, q: q, transact: transact, cache: make(map[uuid.UUID]cachedKey)}
	for _, opt := range opts {
		v.onLifecycleWaitObserver = opt.onLifecycleWaitObserver
	}
	return v
}
