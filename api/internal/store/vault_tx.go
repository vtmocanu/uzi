package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	// ErrVaultSealStale refuses material sealed before a mode or key-epoch change.
	ErrVaultSealStale = errors.New("vault: seal mode or key epoch changed")
	// ErrVaultTransactionRequired prevents an autocommit query from pretending to
	// hold a fence through a later ciphertext write.
	ErrVaultTransactionRequired = errors.New("vault: transaction-bound queries required")
)

func (q *Queries) requireVaultTransaction() error {
	if _, ok := q.db.(pgx.Tx); !ok {
		return ErrVaultTransactionRequired
	}
	return nil
}

// LockSettingsAdmission takes shared enrollment/disable admission on this
// transaction. Read GetVaultPolicy in a separate READ COMMITTED statement after it.
func (q *Queries) LockSettingsAdmission(ctx context.Context) error {
	if err := q.requireVaultTransaction(); err != nil {
		return err
	}
	return LockSettingsAdmission(ctx, q.db)
}

// LockSecretMutation takes the owner's existing advisory guard on this transaction.
func (q *Queries) LockSecretMutation(ctx context.Context, userID uuid.UUID) error {
	if err := q.requireVaultTransaction(); err != nil {
		return err
	}
	return LockSecretMutation(ctx, q.db, userID)
}

// LockUserVaultStateForMutation keeps mode-based state transitions inside the
// caller's existing transaction. An autocommit SELECT FOR UPDATE would release
// its lock before the subsequent transition and falsely claim serialization.
func (q *Queries) LockUserVaultStateForMutation(ctx context.Context, userID uuid.UUID) (LockUserVaultStateRow, error) {
	if err := q.requireVaultTransaction(); err != nil {
		return LockUserVaultStateRow{}, err
	}
	return q.LockUserVaultState(ctx, userID)
}

// FenceVaultWrite locks the owner row and checks BOTH the sealing mode and epoch.
// Call after the applicable advisory/worker/run prefix and before material rows.
// The returned current state permits a provider-result writer to reseal that same
// result on a stale snapshot, without repeating an external provider exchange.
func (q *Queries) FenceVaultWrite(ctx context.Context, userID uuid.UUID, mode string, epoch int64) (LockUserVaultStateRow, error) {
	if err := q.requireVaultTransaction(); err != nil {
		return LockUserVaultStateRow{}, err
	}
	state, err := q.LockUserVaultState(ctx, userID)
	if err != nil {
		return state, err
	}
	if (mode != VaultModeProtected && mode != VaultModeUnprotected) || epoch < 0 ||
		state.VaultMode != mode || state.VaultEpoch != epoch {
		return state, ErrVaultSealStale
	}
	return state, nil
}
