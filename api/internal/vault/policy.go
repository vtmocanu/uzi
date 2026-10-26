package vault

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type stateReaderKey struct{}

// WithStateReader binds lookups to an existing transaction, avoiding a nested
// pool acquisition at claim/refresh gates. The context must not escape that
// transaction. Lifecycle operations must never enter through this bound context.
func WithStateReader(ctx context.Context, reader StateReader) context.Context {
	if reader == nil {
		panic("vault: state reader required")
	}
	return context.WithValue(ctx, stateReaderKey{}, reader)
}

func (v *Vault) stateReader(ctx context.Context) (StateReader, error) {
	if reader, ok := ctx.Value(stateReaderKey{}).(StateReader); ok {
		return reader, nil
	}
	if v == nil || v.q == nil {
		return nil, ErrNotConfigured
	}
	return v.q, nil
}

func validState(mode string, epoch int64) bool {
	return (mode == store.VaultModeProtected || mode == store.VaultModeUnprotected) && epoch >= 0
}

// State reads durable mode and epoch through the applicable transaction reader.
func (v *Vault) State(ctx context.Context, userID uuid.UUID) (store.GetUserVaultStateRow, error) {
	reader, err := v.stateReader(ctx)
	if err != nil {
		return store.GetUserVaultStateRow{}, err
	}
	state, err := reader.GetUserVaultState(ctx, userID)
	if err != nil {
		return state, err
	}
	if !validState(state.VaultMode, state.VaultEpoch) {
		return state, ErrInvalidState
	}
	return state, nil
}

// RequiresUnlock is true only for protected material without its current DEK.
// Missing/failed state lookups fail closed. A bounded refetch avoids treating a
// cache eviction after a committed disable as evidence that an off user is locked.
func (v *Vault) RequiresUnlock(ctx context.Context, userID uuid.UUID) bool {
	state, err := v.State(ctx, userID)
	if err != nil {
		return true
	}
	if state.VaultMode == store.VaultModeUnprotected {
		return false
	}
	if v != nil && v.hasEpoch(userID, state.VaultEpoch) {
		return false
	}
	fresh, err := v.State(ctx, userID)
	if err != nil {
		return true
	}
	return fresh.VaultMode == store.VaultModeProtected && (v == nil || !v.hasEpoch(userID, fresh.VaultEpoch))
}

type SealPurpose uint8

const (
	SealForUse SealPurpose = iota
	// SealForRecovery permits only Codex's protected, currently locked recovery
	// exception. It prefers the DEK whenever the current key is cached.
	SealForRecovery
)

// SealedMaterial carries the fence a ciphertext writer must verify in its write
// transaction. It is internal material, never an API response.
type SealedMaterial struct {
	Ciphertext     []byte `json:"-"`
	SealedWith     string `json:"-"`
	Mode           string `json:"-"`
	Epoch          int64  `json:"-"`
	RecoveryMaster bool   `json:"-"`
}

// Seal is the policy-aware user-material seam for handlers, workers and seeding.
// It never takes the lifecycle mutex. Callers seal before their transaction, then
// fence mode AND epoch, or bind the current locked transaction with WithStateReader.
func (v *Vault) Seal(ctx context.Context, userID uuid.UUID, kind string, plaintext []byte, purpose SealPurpose) (SealedMaterial, error) {
	if v == nil {
		return SealedMaterial{}, ErrNotConfigured
	}
	state, err := v.State(ctx, userID)
	if err != nil {
		return SealedMaterial{}, err
	}
	sealed, err := v.sealState(userID, kind, plaintext, state.VaultMode, state.VaultEpoch, nil, purpose)
	if !errors.Is(err, ErrLocked) {
		return sealed, err
	}
	fresh, ferr := v.State(ctx, userID)
	if ferr != nil {
		return SealedMaterial{}, ferr
	}
	if fresh.VaultMode == state.VaultMode && fresh.VaultEpoch == state.VaultEpoch {
		return SealedMaterial{}, ErrLocked
	}
	// Exactly one reseal attempt on an observed mode/epoch change.
	return v.sealState(userID, kind, plaintext, fresh.VaultMode, fresh.VaultEpoch, nil, purpose)
}

func (v *Vault) sealState(userID uuid.UUID, kind string, plaintext []byte, mode string, epoch int64, private *secretbox.Box, purpose SealPurpose) (SealedMaterial, error) {
	if !validState(mode, epoch) || (purpose != SealForUse && purpose != SealForRecovery) ||
		(purpose == SealForRecovery && kind != store.KindCodexAuth) {
		return SealedMaterial{}, ErrInvalidState
	}
	out := SealedMaterial{Mode: mode, Epoch: epoch}
	var box *secretbox.Box
	if mode == store.VaultModeUnprotected {
		box, out.SealedWith = v.master, store.SealedWithMasterAAD
	} else {
		box = private
		if box == nil {
			var ok bool
			var err error
			box, ok, err = v.dekBoxAtEpoch(userID, &epoch)
			if err != nil {
				return SealedMaterial{}, err
			}
			if !ok {
				if purpose != SealForRecovery {
					return SealedMaterial{}, ErrLocked
				}
				box, out.SealedWith, out.RecoveryMaster = v.master, store.SealedWithMasterAAD, true
			}
		}
		if out.SealedWith == "" {
			out.SealedWith = store.SealedWithDEK
		}
	}
	if box == nil {
		return SealedMaterial{}, ErrNotConfigured
	}
	ciphertext, err := box.SealWithAAD(plaintext, secretAAD(userID, kind))
	if err != nil {
		return SealedMaterial{}, err
	}
	out.Ciphertext = ciphertext
	return out, nil
}

// OpenAtEpoch authenticates a fetched snapshot only while its user epoch is
// current. An owner-scoped caller can refetch once on ErrVaultSealStale; it must
// never reinterpret a discriminator or fall back to master after a DEK failure.
func (v *Vault) OpenAtEpoch(ctx context.Context, userID uuid.UUID, kind, sealedWith string, ciphertext []byte, epoch int64) ([]byte, error) {
	if v == nil {
		return nil, ErrNotConfigured
	}
	state, err := v.State(ctx, userID)
	if err != nil {
		return nil, err
	}
	if state.VaultEpoch != epoch {
		return nil, store.ErrVaultSealStale
	}
	if sealedWith == store.SealedWithDEK {
		if state.VaultMode != store.VaultModeProtected {
			return nil, ErrInvalidState
		}
		box, ok, err := v.dekBoxAtEpoch(userID, &epoch)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrLocked
		}
		return box.OpenWithAAD(ciphertext, secretAAD(userID, kind))
	}
	return OpenMasterMaterial(v.master, userID, kind, sealedWith, ciphertext)
}

type Status struct {
	Mode     string `json:"mode"`
	Policy   string `json:"policy"`
	Unlocked bool   `json:"unlocked"`
	Exists   bool   `json:"exists"`
}

// Status observes lifecycle/cache publication under the same per-user boundary.
func (v *Vault) Status(ctx context.Context, userID uuid.UUID) (Status, error) {
	if err := v.lifecycleContext(ctx); err != nil {
		return Status{}, err
	}
	// Status holds the lifecycle boundary too; bound its database reads so it
	// cannot indefinitely prevent an explicit lock or conversion from entering.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	m := v.lifecycleMutex(userID)
	m.Lock()
	defer m.Unlock()
	state, err := v.State(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	policy, err := v.q.GetVaultPolicy(ctx)
	if err != nil {
		return Status{}, err
	}
	exists, err := v.exists(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	return Status{Mode: state.VaultMode, Policy: policy, Exists: exists,
		Unlocked: state.VaultMode == store.VaultModeProtected && v.hasEpoch(userID, state.VaultEpoch)}, nil
}

// Exists reports actual wrapper presence, never the user's protection mode.
func (v *Vault) Exists(ctx context.Context, userID uuid.UUID) (bool, error) {
	if v == nil || v.q == nil {
		return false, ErrNotConfigured
	}
	return v.exists(ctx, userID)
}

func (v *Vault) exists(ctx context.Context, userID uuid.UUID) (bool, error) {
	_, err := v.q.GetUserVault(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vault: wrapper presence: %w", err)
	}
	return true, nil
}

func (v *Vault) lifecycleContext(ctx context.Context) error {
	if v == nil || v.q == nil || v.transact == nil {
		return ErrNotConfigured
	}
	if _, bound := ctx.Value(stateReaderKey{}).(StateReader); bound {
		return errors.New("vault: lifecycle operation inside a transaction")
	}
	return ctx.Err()
}
