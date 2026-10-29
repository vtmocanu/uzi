package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// LifecycleTimeout bounds a conversion transaction. Result-preserving writers
// must allow their background lock wait to outlast this conversion boundary.
const LifecycleTimeout = 2 * time.Minute

// ErrUncertainOutcome requires callers to refresh durable status before retrying.
// No key is retained, even if fresh reconciliation observes a committed change.
var ErrUncertainOutcome = errors.New("vault: lifecycle outcome uncertain")

type UncertainOutcomeError struct {
	Cause          error
	Reconciliation error
}

func (e *UncertainOutcomeError) Error() string {
	if e.Reconciliation != nil {
		return fmt.Sprintf("%v; reconciliation failed: %v; commit: %v", ErrUncertainOutcome, e.Reconciliation, e.Cause)
	}
	return fmt.Sprintf("%v; durable state reconciled, unlock again: %v", ErrUncertainOutcome, e.Cause)
}

func (e *UncertainOutcomeError) Unwrap() error { return e.Cause }
func (e *UncertainOutcomeError) Is(target error) bool {
	return target == ErrUncertainOutcome
}

// lockLifecycleState uses separate statements for admission and the authoritative
// policy read. The advisory guard precedes users, wrapper, accounts and secrets.
func lockLifecycleState(ctx context.Context, q Store, id uuid.UUID, admission bool) (store.LockVaultAuthStateRow, store.UserVault, bool, string, error) {
	var state store.LockVaultAuthStateRow
	var wrapper store.UserVault
	var policy string
	if admission {
		if err := q.LockSettingsAdmission(ctx); err != nil {
			return state, wrapper, false, policy, err
		}
		var err error
		policy, err = q.GetVaultPolicy(ctx)
		if err != nil {
			return state, wrapper, false, policy, err
		}
		if policy != "optional" && policy != "required" {
			return state, wrapper, false, policy, ErrInvalidState
		}
	}
	if err := q.LockSecretMutation(ctx, id); err != nil {
		return state, wrapper, false, policy, err
	}
	var err error
	state, err = q.LockVaultAuthState(ctx, id)
	if err != nil {
		return state, wrapper, false, policy, err
	}
	if !validState(state.VaultMode, state.VaultEpoch) {
		return state, wrapper, false, policy, ErrInvalidState
	}
	wrapper, err = q.LockUserVault(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, wrapper, false, policy, nil
	}
	return state, wrapper, err == nil, policy, err
}

// finishLifecycle is called while the per-user mutex is held. Reconciliation
// makes one attempt with a fresh 15-second deadline, independent of cancellation
// of the original request. Its locks wait for the original transaction to resolve;
// a timeout is explicitly reported and never treated as proof of rollback.
func (v *Vault) finishLifecycle(id uuid.UUID, err error) error {
	var commit *CommitError
	if !errors.As(err, &commit) {
		return err
	}
	v.evict(id)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reconcileErr := v.transact(ctx, func(ctx context.Context, q Store) error {
		_, _, _, _, err := lockLifecycleState(ctx, q, id, true)
		return err
	})
	return &UncertainOutcomeError{Cause: err, Reconciliation: reconcileErr}
}

func (v *Vault) Unlock(ctx context.Context, id uuid.UUID, password string) error {
	return v.unlock(ctx, id, password, true, false)
}

func (v *Vault) UnlockExisting(ctx context.Context, id uuid.UUID, password string) error {
	return v.unlock(ctx, id, password, false, false)
}

// Enable is create-only for the wrapper. Existing protected wrappers must be
// unlocked, never replaced, even when the cache is empty.
func (v *Vault) Enable(ctx context.Context, id uuid.UUID, password string) error {
	return v.unlock(ctx, id, password, true, true)
}

func (v *Vault) unlock(ctx context.Context, id uuid.UUID, password string, create, enable bool) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, LifecycleTimeout)
	defer cancel()
	release := v.acquireLifecycle(id)
	defer release()
	var dek []byte
	defer func() { wipe(dek) }()
	var epoch int64
	var publish, lazy bool
	err := v.transact(ctx, func(ctx context.Context, q Store) error {
		state, wrapper, exists, _, err := lockLifecycleState(ctx, q, id, create)
		if err != nil {
			return err
		}
		// Login and interactive unlock are harmless while protection is off.
		if !enable && state.VaultMode == store.VaultModeUnprotected {
			return nil
		}
		if exists {
			if state.VaultMode != store.VaultModeProtected {
				return ErrInvalidState
			}
			if enable {
				return ErrAlreadyProtected
			}
			dek, err = unwrapKey(id, password, wrapper)
			if err != nil {
				return err
			}
			epoch = state.VaultEpoch
			lazy = true
		} else {
			if !create {
				wipe(deriveKEK(password, dummyKEKSalt))
				return ErrNoVault
			}
			if err := verifyCreationPassword(password, state); err != nil {
				return err
			}
			if state.VaultEpoch == math.MaxInt64 {
				return ErrEpochExhausted
			}
			dek, err = createWrappedKey(ctx, q, id, password)
			if err != nil {
				return err
			}
			box, err := secretbox.New(dek)
			if err != nil {
				return err
			}
			epoch = state.VaultEpoch + 1
			source := store.GetUserVaultStateRow{VaultMode: state.VaultMode, VaultEpoch: state.VaultEpoch}
			if err := v.convertMaterial(ctx, q, id, source, store.VaultModeProtected, epoch, box, false); err != nil {
				return err
			}
			if err := advanceMode(ctx, q, id, store.VaultModeProtected, epoch); err != nil {
				return err
			}
		}
		publish = true
		return nil
	})
	if err != nil {
		// Discard the unpublished key before any ambiguous-commit reconciliation.
		wipe(dek)
		dek = nil
	}
	if err := v.finishLifecycle(id, err); err != nil {
		return err
	}
	if !publish {
		v.evict(id)
	}
	if publish {
		v.put(id, dek, epoch)
		// Best effort, outside the committed transaction: a notice failure must
		// neither abort crypto changes nor turn a successful unlock into failure.
		if err := v.q.ClearVaultLockNotice(ctx, id); err != nil {
			slog.Error("vault: clear lock-notice on unlock", "user", id, "error", err)
		}
	}
	if lazy {
		var stale bool
		err := v.transact(ctx, func(ctx context.Context, q Store) error {
			state, _, exists, _, err := lockLifecycleState(ctx, q, id, false)
			if err != nil {
				return err
			}
			if !exists || state.VaultMode != store.VaultModeProtected || state.VaultEpoch != epoch {
				stale = true
				v.evict(id)
				return ErrInvalidState
			}
			hasMaster, err := q.HasMasterVaultMaterial(ctx, id)
			if err != nil || !hasMaster {
				return err
			}
			box, err := secretbox.New(dek)
			if err != nil {
				return err
			}
			source := store.GetUserVaultStateRow{VaultMode: state.VaultMode, VaultEpoch: epoch}
			return v.convertMaterial(ctx, q, id, source, source.VaultMode, epoch, box, true)
		})
		if err != nil {
			// Database errors can contain material; log only the phase and owner.
			slog.Error("vault: lazy conversion failed", "user", id)
			var commit *CommitError
			if errors.As(err, &commit) {
				wipe(dek)
				dek = nil
				return v.finishLifecycle(id, err)
			}
			if stale {
				return err
			}
		}
	}
	return nil
}

func verifyCreationPassword(password string, state store.LockVaultAuthStateRow) error {
	if len(password) > auth.MaxPasswordLen {
		return ErrPassphraseLength
	}
	if state.PasswordHash.Valid {
		ok, err := auth.VerifyPassword(password, state.PasswordHash.String)
		if err != nil {
			return fmt.Errorf("vault: verify current password: %w", err)
		}
		if !ok {
			return ErrWrongPassword
		}
		return nil
	}
	if len(password) < auth.MinPasswordLen {
		return ErrPassphraseLength
	}
	return nil
}

func unwrapKey(id uuid.UUID, password string, wrapper store.UserVault) ([]byte, error) {
	if len(wrapper.KekSalt) != kekSaltLen {
		return nil, ErrInvalidState
	}
	kek := deriveKEK(password, wrapper.KekSalt)
	defer wipe(kek)
	box, err := secretbox.New(kek)
	if err != nil {
		return nil, err
	}
	dek, err := box.OpenWithAAD(wrapper.WrappedDek, wrapAAD(id))
	if err != nil {
		return nil, ErrWrongPassword
	}
	if len(dek) != dekLen {
		wipe(dek)
		return nil, ErrInvalidState
	}
	return dek, nil
}

func createWrappedKey(ctx context.Context, q Store, id uuid.UUID, password string) ([]byte, error) {
	dek := make([]byte, dekLen)
	defer wipe(dek)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	salt := make([]byte, kekSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	kek := deriveKEK(password, salt)
	defer wipe(kek)
	box, err := secretbox.New(kek)
	if err != nil {
		return nil, err
	}
	wrapped, err := box.SealWithAAD(dek, wrapAAD(id))
	if err != nil {
		return nil, err
	}
	if _, err := q.CreateUserVaultIfAbsent(ctx, store.CreateUserVaultIfAbsentParams{
		UserID: id, KekSalt: salt, WrappedDek: wrapped,
	}); err != nil {
		return nil, err
	}
	return append([]byte(nil), dek...), nil
}

func advanceMode(ctx context.Context, q Store, id uuid.UUID, mode string, epoch int64) error {
	state, err := q.AdvanceUserVaultMode(ctx, store.AdvanceUserVaultModeParams{ID: id, VaultMode: mode})
	if err != nil {
		return err
	}
	if state.VaultMode != mode || state.VaultEpoch != epoch {
		return ErrInvalidState
	}
	return nil
}

func (v *Vault) Disable(ctx context.Context, id uuid.UUID) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, LifecycleTimeout)
	defer cancel()
	release := v.acquireLifecycle(id)
	defer release()
	err := v.transact(ctx, func(ctx context.Context, q Store) error {
		state, _, exists, policy, err := lockLifecycleState(ctx, q, id, true)
		if err != nil {
			return err
		}
		if policy != "optional" {
			return ErrRequiredPolicy
		}
		if state.VaultMode == store.VaultModeUnprotected {
			if exists {
				return ErrInvalidState
			}
			return nil
		}
		if !exists {
			return ErrNoVault
		}
		if !v.hasEpoch(id, state.VaultEpoch) {
			return ErrLocked
		}
		if state.VaultEpoch == math.MaxInt64 {
			return ErrEpochExhausted
		}
		source := store.GetUserVaultStateRow{VaultMode: state.VaultMode, VaultEpoch: state.VaultEpoch}
		if err := v.convertMaterial(ctx, q, id, source, store.VaultModeUnprotected, state.VaultEpoch+1, nil, false); err != nil {
			return err
		}
		if err := advanceMode(ctx, q, id, store.VaultModeUnprotected, state.VaultEpoch+1); err != nil {
			return err
		}
		n, err := q.DeleteUserVault(ctx, id)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrInvalidState
		}
		return nil
	})
	if err := v.finishLifecycle(id, err); err != nil {
		return err
	}
	v.evict(id)
	return nil
}

// Lock serializes eviction with lifecycle commits, publication and notice clears.
func (v *Vault) Lock(id uuid.UUID) {
	if v == nil {
		return
	}
	release := v.acquireLifecycle(id)
	defer release()
	v.evict(id)
}

// LockAndAcknowledge keeps a deliberate lock's notice claim inside the same
// serialization boundary as unlock's notice clear. Notice failures are logged;
// eviction succeeds regardless, matching the manual lock handler's behavior.
func (v *Vault) LockAndAcknowledge(ctx context.Context, id uuid.UUID) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	// A notice is best effort: a stalled claim must not retain a usable DEK
	// indefinitely. Preserve a shorter request deadline and evict even on failure.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	release := v.acquireLifecycle(id)
	defer release()
	defer v.evict(id)
	if _, err := v.q.ClaimVaultLockNotice(ctx, id); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("vault lock: pre-ack lock-notice", "user", id, "error", err)
	}
	return nil
}
