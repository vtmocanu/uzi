package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrStaleAuth = errors.New("vault: stale authentication snapshot")

// AuthSnapshot is an immutable copy of the authenticated user, captured BEFORE
// password verification or any lifecycle wait. M2 must use the user already read
// by authentication, not refresh a stale user to make an operation succeed.
type AuthSnapshot struct {
	owner uuid.UUID
	state store.LockVaultAuthStateRow
}

func Snapshot(user store.User) AuthSnapshot {
	return AuthSnapshot{owner: user.ID, state: store.LockVaultAuthStateRow{
		VaultMode: user.VaultMode, VaultEpoch: user.VaultEpoch,
		PasswordHash: user.PasswordHash, TokenVersion: user.TokenVersion,
		IsActive: user.IsActive, VaultAutoReunlock: user.VaultAutoReunlock,
		VaultSlotGeneration: user.VaultSlotGeneration,
	}}
}

func (s AuthSnapshot) matches(id uuid.UUID, state store.LockVaultAuthStateRow) bool {
	return s.owner != uuid.Nil && s.owner == id && state.IsActive && s.state == state
}

// InteractiveOperation selects the existing unlock, create-if-absent, or
// create-only enable flow. No noninteractive caller should use UnlockAndMint.
type InteractiveOperation uint8

const (
	InteractiveExisting InteractiveOperation = iota
	InteractiveCreate
	InteractiveEnable
)

// BrowserMintRequest is optional. M2 supplies this only for an HTTPS browser,
// with a random browser UUID and the expiry of the ALREADY CONSTRUCTED session
// JWT. Omit it for CLI, seed and non-browser authentication.
type BrowserMintRequest struct {
	HTTPS            bool
	BrowserID        uuid.UUID
	SessionExpiresAt time.Time
}

// BrowserKey contains only private secret fields. It cannot be serialized as
// JSON and all formatting is redacted. M2's cookie setter may call CookieValue;
// it must never log that value. Destroy after setting the cookie.
type BrowserKey struct {
	slot    uuid.UUID
	key     [32]byte
	expires time.Time
}

func (k BrowserKey) String() string             { return "[vault browser key]" }
func (k BrowserKey) GoString() string           { return k.String() }
func (k BrowserKey) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }
func (k BrowserKey) MarshalJSON() ([]byte, error) {
	return nil, errors.New("vault: browser key is not JSON")
}
func (k *BrowserKey) Destroy() {
	if k != nil {
		wipe(k.key[:])
		k.slot = uuid.Nil
		k.expires = time.Time{}
	}
}
func (k *BrowserKey) ExpiresAt() time.Time {
	if k == nil {
		return time.Time{}
	}
	return k.expires
}

type interactiveUnlock struct {
	auth    AuthSnapshot
	request *BrowserMintRequest
	key     *BrowserKey
}

// UnlockAndMint authenticates, unwraps/creates, optionally mints, commits, and
// publishes under one lifecycle mutex. A nil key means successful unlock
// without a slot (Off, opt-out, or no request). Errors never return key material.
// Upcoming HTTP callers remain responsible for verified authentication, CSRF,
// HTTPS detection, cookie attributes, and checking JWT version against the
// authenticated user before constructing Snapshot.
func (v *Vault) UnlockAndMint(ctx context.Context, auth AuthSnapshot, password string, operation InteractiveOperation, request *BrowserMintRequest) (*BrowserKey, error) {
	if operation > InteractiveEnable {
		return nil, ErrInvalidState
	}
	if request != nil && (!request.HTTPS || request.BrowserID == uuid.Nil || request.SessionExpiresAt.IsZero()) {
		return nil, ErrInvalidState
	}
	interactive := &interactiveUnlock{auth: auth, request: request}
	err := v.unlockInteractive(ctx, auth.owner, password, operation != InteractiveExisting, operation == InteractiveEnable, interactive)
	if err != nil {
		interactive.key.Destroy()
		return nil, err
	}
	return interactive.key, nil
}

func browserAAD(owner, slot uuid.UUID) []byte {
	aad := append([]byte("uzi-vault-browser-dek-v1\x00"), owner[:]...)
	return append(aad, slot[:]...)
}

func mintBrowserKey(ctx context.Context, q BrowserStore, owner uuid.UUID, state store.LockVaultAuthStateRow, dek []byte, request BrowserMintRequest) (*BrowserKey, error) {
	value, err := q.GetVaultReunlockDays(ctx)
	if err != nil {
		return nil, err
	}
	days, err := settings.ParseVaultReunlockDays(value)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	expires := now.Add(time.Duration(days) * 24 * time.Hour)
	if request.SessionExpiresAt.Before(expires) {
		expires = request.SessionExpiresAt
	}
	if !expires.After(now) {
		return nil, ErrStaleAuth
	}
	key := &BrowserKey{slot: uuid.New(), expires: expires}
	success := false
	defer func() {
		if !success {
			key.Destroy()
		}
	}()
	if _, err := rand.Read(key.key[:]); err != nil {
		return nil, err
	}
	box, err := secretbox.New(key.key[:])
	if err != nil {
		return nil, err
	}
	wrapped, err := box.SealWithAAD(dek, browserAAD(owner, key.slot))
	if err != nil {
		return nil, err
	}
	if err := q.PrepareVaultBrowserSlot(ctx, store.PrepareVaultBrowserSlotParams{
		UserID: owner, BrowserID: request.BrowserID, ObservedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return nil, err
	}
	if err := q.TrimVaultBrowserSlots(ctx, owner); err != nil {
		return nil, err
	}
	err = q.InsertVaultBrowserSlot(ctx, store.InsertVaultBrowserSlotParams{
		ID: key.slot, UserID: owner, BrowserID: request.BrowserID, WrappedDek: wrapped,
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
		TokenVersion: state.TokenVersion, VaultEpoch: state.VaultEpoch, Generation: state.VaultSlotGeneration,
	})
	if err != nil {
		return nil, err
	}
	success = true
	return key, nil
}

// VerifiedSession is an explicit domain assertion by M2: authentication and CSRF
// already passed, TokenVersion is from the verified JWT, and BrowserID is the
// validated browser identity. It must not be synthesized from the slot row.
type VerifiedSession struct {
	Auth         AuthSnapshot
	TokenVersion int32
	BrowserID    uuid.UUID
}

// Restore never mints or extends a slot. It checks actual time AFTER lock wait,
// requires the exact original session/auth snapshot, and publishes only after
// an acknowledged transaction. The caller must Destroy the decoded key.
func (v *Vault) Restore(ctx context.Context, session VerifiedSession, key *BrowserKey) error {
	if err := v.lifecycleContext(ctx); err != nil {
		return err
	}
	if key == nil || key.slot == uuid.Nil || session.BrowserID == uuid.Nil {
		return ErrStaleAuth
	}
	ctx, cancel := context.WithTimeout(ctx, LifecycleTimeout)
	defer cancel()
	id := session.Auth.owner
	release := v.acquireLifecycle(id)
	defer release()
	var dek []byte
	defer func() { wipe(dek) }()
	var epoch int64
	err := v.transact(ctx, func(ctx context.Context, q Store) error {
		state, _, exists, _, err := lockLifecycleState(ctx, q, id, false)
		if err != nil {
			return err
		}
		if !session.Auth.matches(id, state) || !exists || !state.VaultAutoReunlock ||
			state.VaultMode != store.VaultModeProtected || session.TokenVersion != state.TokenVersion {
			return ErrStaleAuth
		}
		bq, ok := q.(BrowserStore)
		if !ok {
			return ErrNotConfigured
		}
		slot, err := bq.GetVaultBrowserSlot(ctx, store.GetVaultBrowserSlotParams{ID: key.slot, UserID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleAuth
		}
		if err != nil {
			// A read failure is transient: callers keep the cookie and may retry.
			return err
		}
		if slot.UserID != id || slot.BrowserID != session.BrowserID ||
			slot.TokenVersion != state.TokenVersion || slot.Generation != state.VaultSlotGeneration ||
			slot.VaultEpoch != state.VaultEpoch || !slot.ExpiresAt.Valid ||
			!slot.ExpiresAt.Time.After(time.Now()) {
			return ErrStaleAuth
		}
		box, err := secretbox.New(key.key[:])
		if err != nil {
			return ErrStaleAuth
		}
		dek, err = box.OpenWithAAD(slot.WrappedDek, browserAAD(id, slot.ID))
		if err != nil || len(dek) != dekLen {
			return ErrStaleAuth
		}
		epoch = state.VaultEpoch
		return nil
	})
	if err != nil {
		wipe(dek)
		dek = nil
	}
	if err := v.finishLifecycle(id, err); err != nil {
		return err
	}
	v.put(id, dek, epoch)
	if err := v.q.ClearVaultLockNotice(ctx, id); err != nil {
		// Notice failures must not disclose driver diagnostics or key material.
		return nil
	}
	return nil
}
