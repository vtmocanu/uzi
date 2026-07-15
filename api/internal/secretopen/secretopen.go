// Package secretopen opens a user's decrypted secret via the per-user vault path,
// the one place that logic lives (factored out of workersvc for PRD #53 so the
// run lane and the rate-limit poller open tokens the same way). It reproduces the
// vault dispatch exactly: a 'dek'-sealed row needs the owner's vault unlocked (a
// lock surfaces as ErrVaultLocked — transient, retry), a legacy 'master'-sealed
// row opens under the process master box regardless of lock state, and a nil vault
// (tests) opens under the master box directly. Callers map these sentinels to
// their own domain errors (workersvc) or skip semantics (the poller).
package secretopen

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// Sentinel errors. ErrNoSecret and ErrUndecryptable are both "credential
// unavailable" cases the caller may collapse; keeping them distinct lets
// workersvc preserve its original two failure-reason messages.
var (
	// ErrNoSecret: the user has no secret of the requested kind.
	ErrNoSecret = errors.New("secretopen: secret not configured")
	// ErrUndecryptable: the ciphertext exists but could not be decrypted.
	ErrUndecryptable = errors.New("secretopen: secret could not be decrypted")
	// ErrVaultLocked: a 'dek'-sealed secret whose owner's vault is not unlocked in
	// this process. Transient — the caller retries after the next unlock, never
	// fails terminally on it.
	ErrVaultLocked = errors.New("secretopen: vault locked")
)

// Store is the narrow query surface Open needs. *store.Queries satisfies it, as
// does workersvc's own Store interface (both carry GetUserSecretCiphertext).
type Store interface {
	GetUserSecretCiphertext(ctx context.Context, arg store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error)
}

// Open returns the decrypted plaintext of the user's secret of the given kind.
// vlt is the concrete *vault.Vault (nil in tests → open under box directly, the
// pre-vault behavior); passing the concrete type keeps the nil check honest (a
// typed-nil interface would not be nil). box opens legacy master-sealed rows and
// the nil-vault path.
func Open(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID uuid.UUID, kind string) ([]byte, error) {
	secret, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{
		UserID: userID,
		Kind:   kind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSecret
		}
		return nil, fmt.Errorf("secretopen: lookup: %w", err)
	}
	var plain []byte
	if vlt != nil {
		plain, err = vlt.Open(userID, kind, secret.SealedWith, secret.Ciphertext)
		if errors.Is(err, vault.ErrLocked) {
			return nil, ErrVaultLocked
		}
	} else {
		plain, err = box.Open(secret.Ciphertext)
	}
	if err != nil {
		// box/vault Open errors never carry plaintext; collapse to the sentinel.
		return nil, ErrUndecryptable
	}
	return plain, nil
}

// Opener binds Open's collaborators so it satisfies a caller's one-method seam
// (the usage poller's TokenOpener). One instance is shared across polls.
type Opener struct {
	q   Store
	vlt *vault.Vault
	box *secretbox.Box
}

// NewOpener builds an Opener over the store, the per-user vault (may be nil), and
// the master box.
func NewOpener(q Store, vlt *vault.Vault, box *secretbox.Box) *Opener {
	return &Opener{q: q, vlt: vlt, box: box}
}

// Open opens the user's secret of the given kind, returning the same sentinels as
// the package-level Open.
func (o *Opener) Open(ctx context.Context, userID uuid.UUID, kind string) ([]byte, error) {
	return Open(ctx, o.q, o.vlt, o.box, userID, kind)
}
