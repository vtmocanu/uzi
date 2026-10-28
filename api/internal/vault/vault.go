// Package vault implements optional per-user password-wrapped model credentials.
// Protected users use a random DEK wrapped by an Argon2id-derived KEK. Unprotected
// users use the process master key with the same owner/kind AAD binding. Durable
// mode and key epochs, not wrapper existence, decide which key a writer uses.
package vault

import (
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep KEK derivation at least as expensive as login hashing. The independent
// random salt prevents users.password_hash from supplying the wrapping key.
const (
	kekMemoryKiB = 19 * 1024
	kekTime      = 2
	kekThreads   = 1
	kekLength    = secretbox.KeySize
	kekSaltLen   = 16
	dekLen       = 32
)

var (
	ErrLocked           = errors.New("vault: locked")
	ErrWrongPassword    = errors.New("vault: wrong password")
	ErrNoVault          = errors.New("vault: no vault for user")
	ErrNotConfigured    = errors.New("vault: not configured")
	ErrInvalidState     = errors.New("vault: invalid mode or key state")
	ErrAlreadyProtected = errors.New("vault: a vault already exists")
	ErrRequiredPolicy   = errors.New("vault: the instance requires credential protection")
	ErrPassphraseLength = errors.New("vault: passphrase length is outside the password policy")
	ErrEpochExhausted   = errors.New("vault: key epoch exhausted")
)

type cachedKey struct {
	dek   []byte
	epoch int64
}

// Vault owns lifecycle serialization and the epoch-tagged in-memory DEK cache.
// The per-user lifecycle mutex is acquired before any transaction; cache mu never
// spans database or network I/O. Normal ciphertext writers use epoch fences and
// never acquire a lifecycle mutex from an already-open transaction.
type Vault struct {
	master                  *secretbox.Box
	q                       Store
	transact                Transact
	onLifecycleWaitObserver func(uuid.UUID)

	mu        sync.RWMutex
	cache     map[uuid.UUID]cachedKey
	lifecycle sync.Map // uuid.UUID -> *sync.Mutex; entries are never deleted
}

func (v *Vault) lifecycleMutex(userID uuid.UUID) *sync.Mutex {
	m, _ := v.lifecycle.LoadOrStore(userID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// acquireLifecycle returns a release function only after obtaining ownership.
// The nil observer path preserves ordinary mutex admission in production.
func (v *Vault) acquireLifecycle(userID uuid.UUID) func() {
	m := v.lifecycleMutex(userID)
	if v.onLifecycleWaitObserver == nil {
		m.Lock()
	} else if !m.TryLock() {
		v.onLifecycleWaitObserver(userID)
		m.Lock()
	}
	return m.Unlock
}

// Unlocked means exactly that a DEK is cached. RequiresUnlock is the mode-aware
// gate; an unprotected user is runnable without a cache entry.
func (v *Vault) Unlocked(userID uuid.UUID) bool {
	if v == nil {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.cache[userID]
	return ok
}

func (v *Vault) evict(userID uuid.UUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if entry, ok := v.cache[userID]; ok {
		wipe(entry.dek)
		delete(v.cache, userID)
	}
}

// put copies the caller's private key; the caller must still wipe its own copy.
func (v *Vault) put(userID uuid.UUID, dek []byte, epoch int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if old, ok := v.cache[userID]; ok {
		wipe(old.dek)
	}
	v.cache[userID] = cachedKey{dek: append([]byte(nil), dek...), epoch: epoch}
}

func (v *Vault) hasEpoch(userID uuid.UUID, epoch int64) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	entry, ok := v.cache[userID]
	return ok && entry.epoch == epoch
}

func (v *Vault) dekBox(userID uuid.UUID) (*secretbox.Box, bool, error) {
	return v.dekBoxAtEpoch(userID, nil)
}

// dekBoxAtEpoch copies the DEK into an AES key schedule while holding the read lock, so a
// concurrent eviction cannot race that copy. No cache mutex spans crypto I/O.
func (v *Vault) dekBoxAtEpoch(userID uuid.UUID, epoch *int64) (*secretbox.Box, bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	entry, ok := v.cache[userID]
	if !ok || (epoch != nil && entry.epoch != *epoch) {
		return nil, false, nil
	}
	box, err := secretbox.New(entry.dek)
	if err != nil {
		return nil, false, fmt.Errorf("vault: dek box: %w", err)
	}
	return box, true, nil
}

// Open decrypts according to the discriminator and never retries a failed tag
// under another key/AAD scheme. Fetched DEK snapshots that cross a crypto epoch
// must be refetched by the owner-scoped caller before another open attempt.
func (v *Vault) Open(userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	if sealedWith != store.SealedWithDEK {
		return OpenMasterMaterial(v.master, userID, kind, sealedWith, ciphertext)
	}
	box, ok, err := v.dekBox(userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	return box.OpenWithAAD(ciphertext, secretAAD(userID, kind))
}

// OpenMasterMaterial also serves readers without a Vault. Legacy master retains
// nil AAD, master_aad authenticates owner/kind, and DEK/unknown markers fail closed.
func OpenMasterMaterial(master *secretbox.Box, userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	var aad []byte
	switch sealedWith {
	case store.SealedWithMaster:
	case store.SealedWithMasterAAD:
		aad = secretAAD(userID, kind)
	case store.SealedWithDEK:
		return nil, ErrLocked
	default:
		return nil, fmt.Errorf("vault: unknown sealed_with %q", sealedWith)
	}
	if master == nil {
		return nil, ErrNotConfigured
	}
	return master.OpenWithAAD(ciphertext, aad)
}

var dummyKEKSalt = make([]byte, kekSaltLen)

func deriveKEK(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, kekTime, kekMemoryKiB, kekThreads, kekLength)
}

func wrapAAD(userID uuid.UUID) []byte {
	aad := make([]byte, 0, len("uzi-vault-dek\x00")+16)
	aad = append(aad, "uzi-vault-dek\x00"...)
	return append(aad, userID[:]...)
}

// secretAAD binds to the user and kind, not to an individual row. Same-kind,
// same-owner row swaps remain the accepted PRD #104 residual; cross-owner/kind
// swaps fail for both DEK and master_aad material.
func secretAAD(userID uuid.UUID, kind string) []byte {
	aad := make([]byte, 0, 16+1+len(kind))
	aad = append(aad, userID[:]...)
	aad = append(aad, 0)
	return append(aad, kind...)
}

// wipe is best effort: Go cannot guarantee that the GC made no prior copies.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
