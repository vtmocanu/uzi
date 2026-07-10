// Package vault implements the per-user secret vault (PRD #32): a Bitwarden-style
// key hierarchy that keeps a user's personal secrets (their Anthropic token)
// decryptable only while that user's login password has been supplied to this
// API process — never from anything an operator can read at rest.
//
// Key hierarchy:
//
//   - DEK: 32 random bytes per user, generated on first unlock. User secrets are
//     sealed with the DEK (the same secretbox AES-256-GCM construction used for
//     the master box, different key).
//   - KEK = Argon2id(login password, per-user kek_salt). The DEK is stored only
//     as wrapped_dek = secretbox(KEK, DEK). The KEK is derived at unlock, used to
//     unwrap the DEK, and discarded. Neither the KEK nor the plaintext DEK is ever
//     written anywhere.
//   - The unwrapped DEK lives in an in-memory cache keyed by user id ("unlocked").
//     It is evicted on Lock and lost on process restart ("locked").
//
// Scope boundary: the master box (UZI_SECRET_KEY) is NOT retired. It still seals
// connection-level secrets (the forge bot PAT, synced 24/7 with no user present)
// and opens legacy user_secrets rows that predate the vault (sealed_with='master')
// until they are lazily rewrapped under the DEK. The vault reuses the master box
// only for those two read paths; it never seals new user-secret data with it.
package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// KEK derivation cost. Deliberately a private copy of the auth package's
// Argon2id params (t=2, 19 MiB, p=1) rather than an import: the KEK and the login
// hash are different security domains, and a vault-protected deployment may want a
// higher KEK cost than login latency tolerates (PRD #32 residual #1) without
// coupling the two. Keep these >= the login-hash cost. The 16-byte kek_salt is
// generated fresh and independent of the password_hash salt — same salt+params
// would make the KEK equal the stored password hash, i.e. put the KEK in the DB.
const (
	kekMemoryKiB = 19 * 1024
	kekTime      = 2
	kekThreads   = 1
	kekLength    = secretbox.KeySize // 32-byte KEK feeds secretbox.New
	kekSaltLen   = 16
	dekLen       = 32 // 256-bit data key
)

var (
	// ErrLocked is returned by Seal and by Open on a dek-sealed secret when the
	// user's vault is not unlocked in this process (before first login, after
	// Lock, or after a restart).
	ErrLocked = errors.New("vault: locked")
	// ErrWrongPassword is returned by Unlock/UnlockExisting when the supplied
	// password does not unwrap the stored DEK (a GCM authentication failure). It is
	// no more of an oracle than login already is; the unlock endpoint sits behind
	// the per-user rate limiter.
	ErrWrongPassword = errors.New("vault: wrong password")
	// ErrNoVault is returned by UnlockExisting when the user has no vault row yet.
	// The interactive unlock endpoint must NOT create a vault (only login/register,
	// holding a freshly verified password, may) — otherwise any password would mint
	// a fresh vault and lock the user out of their real (differently-keyed) secrets.
	ErrNoVault = errors.New("vault: no vault for user")
)

// Store is the narrow set of generated queries the vault needs. *store.Queries
// satisfies it; tests supply an in-memory fake.
type Store interface {
	GetUserVault(ctx context.Context, userID uuid.UUID) (store.UserVault, error)
	CreateUserVaultIfAbsent(ctx context.Context, arg store.CreateUserVaultIfAbsentParams) (store.UserVault, error)
	ListMasterSealedSecrets(ctx context.Context, userID uuid.UUID) ([]store.ListMasterSealedSecretsRow, error)
	RewrapUserSecret(ctx context.Context, arg store.RewrapUserSecretParams) (int64, error)
}

// Vault owns the per-user DEK cache and the wrap/unwrap crypto. It is safe for
// concurrent use.
type Vault struct {
	// master opens legacy sealed_with='master' rows (and, in M2, rewraps them). It
	// never seals new DEK-scoped data.
	master *secretbox.Box
	q      Store

	mu    sync.RWMutex
	cache map[uuid.UUID][]byte // userID → plaintext DEK; presence == unlocked
}

// New constructs a Vault over the process master box and the store.
func New(master *secretbox.Box, q Store) *Vault {
	return &Vault{master: master, q: q, cache: make(map[uuid.UUID][]byte)}
}

// Unlock caches the user's DEK for this process, creating the vault on the
// first-ever unlock. Use it only on the login/register paths, which hold a
// freshly verified password. A wrong password surfaces as ErrWrongPassword.
// Idempotent: unlocking an already-unlocked vault refreshes the cached DEK.
func (v *Vault) Unlock(ctx context.Context, userID uuid.UUID, password string) error {
	return v.unlock(ctx, userID, password, true)
}

// UnlockExisting is Unlock without the create-on-first path: a missing vault row
// returns ErrNoVault instead of minting one. This is the interactive
// /api/vault/unlock endpoint's entry point — see ErrNoVault for why unlock must
// never create.
func (v *Vault) UnlockExisting(ctx context.Context, userID uuid.UUID, password string) error {
	return v.unlock(ctx, userID, password, false)
}

func (v *Vault) unlock(ctx context.Context, userID uuid.UUID, password string, allowCreate bool) error {
	row, err := v.q.GetUserVault(ctx, userID)
	switch {
	case err == nil:
		if uerr := v.cacheFromRow(userID, password, row); uerr != nil {
			return uerr
		}
	case errors.Is(err, pgx.ErrNoRows):
		if !allowCreate {
			// Burn one KEK-cost Argon2 so a no-vault result is not timing-
			// distinguishable from a wrong password (both cases the endpoint answers
			// with an identical 403): the wrong-password path derives one KEK in
			// cacheFromRow, so this path must too. The derived key is discarded.
			wipe(deriveKEK(password, dummyKEKSalt))
			return ErrNoVault
		}
		if cerr := v.create(ctx, userID, password); cerr != nil {
			return cerr
		}
	default:
		return fmt.Errorf("vault: load: %w", err)
	}
	// Lazy migration: reseal any still-master-sealed secrets under the now-cached
	// DEK. Best-effort — a rewrap fault must never fail an otherwise-valid unlock;
	// the rows stay 'master' and are retried on the next unlock.
	v.rewrapMasterSecrets(ctx, userID)
	return nil
}

// cacheFromRow derives the KEK from the password + the row's salt, unwraps the
// DEK, and caches it. A GCM auth failure means the password is wrong.
func (v *Vault) cacheFromRow(userID uuid.UUID, password string, row store.UserVault) error {
	kek := deriveKEK(password, row.KekSalt)
	defer wipe(kek)
	kekBox, err := secretbox.New(kek)
	if err != nil {
		return fmt.Errorf("vault: kek box: %w", err)
	}
	dek, err := kekBox.OpenWithAAD(row.WrappedDek, wrapAAD(userID))
	if err != nil {
		return ErrWrongPassword
	}
	v.put(userID, dek)
	return nil
}

// create generates a fresh DEK + kek_salt, wraps the DEK under the password KEK,
// and persists it race-safely: CreateUserVaultIfAbsent lets exactly one of N
// concurrent first-unlocks win. If we lose (pgx.ErrNoRows), we re-read the
// winner's row and cache THAT DEK (unwrapped with our identical password) so the
// cached DEK always matches what is persisted — never our discarded local one.
func (v *Vault) create(ctx context.Context, userID uuid.UUID, password string) error {
	dek := make([]byte, dekLen)
	if _, err := rand.Read(dek); err != nil {
		return fmt.Errorf("vault: generate dek: %w", err)
	}
	salt := make([]byte, kekSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("vault: generate salt: %w", err)
	}
	kek := deriveKEK(password, salt)
	defer wipe(kek)
	kekBox, err := secretbox.New(kek)
	if err != nil {
		return fmt.Errorf("vault: kek box: %w", err)
	}
	wrapped, err := kekBox.SealWithAAD(dek, wrapAAD(userID))
	if err != nil {
		return fmt.Errorf("vault: wrap dek: %w", err)
	}
	_, err = v.q.CreateUserVaultIfAbsent(ctx, store.CreateUserVaultIfAbsentParams{
		UserID:     userID,
		KekSalt:    salt,
		WrappedDek: wrapped,
	})
	switch {
	case err == nil:
		// We won the insert; the DEK we generated is the persisted one.
		v.put(userID, dek)
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		// A concurrent first-unlock won. Discard our DEK and adopt the persisted
		// row's, keyed by our (identical) password, so cache == DB.
		wipe(dek)
		row, gerr := v.q.GetUserVault(ctx, userID)
		if gerr != nil {
			return fmt.Errorf("vault: reload after conflict: %w", gerr)
		}
		return v.cacheFromRow(userID, password, row)
	default:
		return fmt.Errorf("vault: persist: %w", err)
	}
}

// rewrapMasterSecrets reseals every still-master-sealed secret of the user under
// their now-cached DEK, flipping sealed_with 'master' → 'dek'. It runs on every
// unlock but does nothing once a user is fully migrated. Every step is
// best-effort and logged (never with secret bytes); a failure leaves the row
// 'master' for the next unlock to retry. It does NOT un-leak a token that ever
// existed master-sealed — rotation is the real fix (PRD #32).
func (v *Vault) rewrapMasterSecrets(ctx context.Context, userID uuid.UUID) {
	rows, err := v.q.ListMasterSealedSecrets(ctx, userID)
	if err != nil {
		slog.Error("vault: list master-sealed for rewrap", "user", userID, "error", err)
		return
	}
	for _, row := range rows {
		plain, err := v.master.Open(row.Ciphertext)
		if err != nil {
			slog.Error("vault: rewrap open (master)", "user", userID, "kind", row.Kind, "error", err)
			continue
		}
		sealed, serr := v.Seal(userID, row.Kind, plain)
		wipe(plain)
		if serr != nil {
			slog.Error("vault: rewrap seal (dek)", "user", userID, "kind", row.Kind, "error", serr)
			continue
		}
		if _, rerr := v.q.RewrapUserSecret(ctx, store.RewrapUserSecretParams{
			UserID:     userID,
			Kind:       row.Kind,
			Ciphertext: sealed,
		}); rerr != nil {
			slog.Error("vault: rewrap persist", "user", userID, "kind", row.Kind, "error", rerr)
		}
	}
}

// Lock evicts and best-effort zeroizes the user's cached DEK. Go gives no
// guarantee the bytes were not already copied elsewhere by the runtime, so this
// reduces — does not eliminate — DEK-in-RAM exposure. No-op if already locked.
func (v *Vault) Lock(userID uuid.UUID) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if dek, ok := v.cache[userID]; ok {
		wipe(dek)
		delete(v.cache, userID)
	}
}

// Unlocked reports whether the user's DEK is cached — the gate the claim path
// checks before ClaimRun.
func (v *Vault) Unlocked(userID uuid.UUID) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.cache[userID]
	return ok
}

// Seal encrypts a user secret under the user's DEK, binding user_id||kind as AAD
// so the ciphertext cannot be authenticated onto a different owner or kind. The
// caller records sealed_with='dek'. ErrLocked if the vault is not unlocked.
func (v *Vault) Seal(userID uuid.UUID, kind string, plaintext []byte) ([]byte, error) {
	box, ok, err := v.dekBox(userID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	return box.SealWithAAD(plaintext, secretAAD(userID, kind))
}

// Open decrypts a user secret according to how it was sealed. A legacy
// sealed_with='master' row opens under the process master key and needs no unlock
// (while locked, the claim gate — not crypto — is what withholds it; PRD #32
// lazy-migration note). A sealed_with='dek' row requires the vault unlocked
// (ErrLocked otherwise) and is bound to user_id||kind.
func (v *Vault) Open(userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	switch sealedWith {
	case store.SealedWithMaster:
		return v.master.Open(ciphertext)
	case store.SealedWithDEK:
		box, ok, err := v.dekBox(userID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrLocked
		}
		return box.OpenWithAAD(ciphertext, secretAAD(userID, kind))
	default:
		return nil, fmt.Errorf("vault: unknown sealed_with %q", sealedWith)
	}
}

// dekBox builds a secretbox over the user's cached DEK. secretbox.New copies the
// key into the AES key schedule, so the returned box is independent of the cache
// slice — a concurrent Lock wipe cannot corrupt an in-flight Seal/Open. The box
// is constructed under the read lock so the wipe cannot race the copy either.
func (v *Vault) dekBox(userID uuid.UUID) (*secretbox.Box, bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	dek, ok := v.cache[userID]
	if !ok {
		return nil, false, nil
	}
	box, err := secretbox.New(dek)
	if err != nil {
		return nil, false, fmt.Errorf("vault: dek box: %w", err)
	}
	return box, true, nil
}

// put caches (or refreshes) the user's DEK, wiping any prior copy.
func (v *Vault) put(userID uuid.UUID, dek []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if old, ok := v.cache[userID]; ok {
		wipe(old)
	}
	v.cache[userID] = dek
}

// dummyKEKSalt is a fixed salt used only by the no-vault timing-equalization burn
// in unlock(); the derived key is discarded, so the salt value is irrelevant — it
// exists solely to give deriveKEK an argument.
var dummyKEKSalt = make([]byte, kekSaltLen)

func deriveKEK(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, kekTime, kekMemoryKiB, kekThreads, kekLength)
}

// wrapAAD binds a wrapped DEK to its owner: a DB-write operator who swaps one
// user's user_vaults row onto another gets a GCM auth failure, not a working DEK.
func wrapAAD(userID uuid.UUID) []byte {
	aad := make([]byte, 0, len("uzi-vault-dek\x00")+16)
	aad = append(aad, "uzi-vault-dek\x00"...)
	return append(aad, userID[:]...)
}

// secretAAD binds a DEK-sealed user secret to user_id||kind.
func secretAAD(userID uuid.UUID, kind string) []byte {
	aad := make([]byte, 0, 16+1+len(kind))
	aad = append(aad, userID[:]...)
	aad = append(aad, 0) // separator so (id,"ab")||"" ≠ (id,"a")||"b"
	return append(aad, kind...)
}

// wipe best-effort zeroizes key material in place. Go provides no guarantee the
// bytes were not already copied by the GC; documented, not oversold.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
