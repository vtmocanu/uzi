package vault

import (
	"context"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// authMutation serializes one bounded transaction with unlock/restore/lock.
// Callback failure rolls back; uncertain commit evicts through finishLifecycle.
func (v *Vault) authMutation(ctx context.Context, id uuid.UUID, fn func(context.Context, BrowserStore, store.LockVaultAuthStateRow) error) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, LifecycleTimeout)
	defer cancel()
	release := v.acquireLifecycle(id)
	defer release()
	err := v.transact(ctx, func(ctx context.Context, q Store) error {
		state, _, _, _, err := lockLifecycleState(ctx, q, id, false)
		if err != nil {
			return err
		}
		bq, ok := q.(BrowserStore)
		if !ok {
			return ErrNotConfigured
		}
		return fn(ctx, bq, state)
	})
	return v.finishLifecycle(id, err)
}

// Logout preserves the cached DEK on acknowledged success, matching existing
// logout semantics. The version bump and slot deletion are one storage statement.
func (v *Vault) Logout(ctx context.Context, id uuid.UUID) (int32, error) {
	var version int32
	err := v.authMutation(ctx, id, func(ctx context.Context, q BrowserStore, _ store.LockVaultAuthStateRow) error {
		var err error
		version, err = q.BumpTokenVersion(ctx, id)
		return err
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// SetUserActive retains the existing storage query return contract. Deactivation
// evicts under the lifecycle mutex before it can admit another restore.
func (v *Vault) SetUserActive(ctx context.Context, arg store.SetUserActiveParams) (store.User, error) {
	var user store.User
	err := v.authMutation(ctx, arg.ID, func(ctx context.Context, q BrowserStore, _ store.LockVaultAuthStateRow) error {
		var err error
		user, err = q.SetUserActive(ctx, arg)
		if err == nil && !arg.IsActive {
			v.evict(arg.ID)
		}
		return err
	})
	if err != nil {
		return store.User{}, err
	}
	return user, nil
}

// UpdatePassword changes login authentication only; it does not rewrap a vault.
// The storage query itself revokes slots, including for non-lifecycle callers.
// M2 must hash and validate the new password before invoking this seam.
func (v *Vault) UpdatePassword(ctx context.Context, arg store.UpdatePasswordParams) error {
	return v.authMutation(ctx, arg.ID, func(ctx context.Context, q BrowserStore, _ store.LockVaultAuthStateRow) error {
		return q.UpdatePassword(ctx, arg)
	})
}

// SetAutoReunlock changes only preference and durable slots. Turning it off
// advances generation and revokes all slots; turning it on never mints.
func (v *Vault) SetAutoReunlock(ctx context.Context, auth AuthSnapshot, enabled bool) error {
	return v.authMutation(ctx, auth.owner, func(ctx context.Context, q BrowserStore, state store.LockVaultAuthStateRow) error {
		if !auth.matches(auth.owner, state) {
			return ErrStaleAuth
		}
		if err := q.SetVaultAutoReunlock(ctx, store.SetVaultAutoReunlockParams{ID: auth.owner, Enabled: enabled}); err != nil {
			return err
		}
		if !enabled && !state.VaultAutoReunlock {
			_, err := q.RevokeVaultBrowserSlots(ctx, auth.owner)
			return err
		}
		return nil
	})
}
