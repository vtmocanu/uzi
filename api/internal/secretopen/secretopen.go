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

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Sentinel errors. ErrNoSecret and ErrUndecryptable are both "credential
// unavailable" cases the caller may collapse; keeping them distinct lets
// workersvc preserve its original two failure-reason messages.
var (
	// ErrNoSecret: the requested secret does not exist for this user (or is of
	// another kind, for OpenByIDOfKind).
	ErrNoSecret = errors.New("secretopen: secret not configured")
	// ErrUndecryptable: the ciphertext exists but could not be decrypted.
	ErrUndecryptable = errors.New("secretopen: secret could not be decrypted")
	// ErrVaultLocked: a 'dek'-sealed secret whose owner's vault is not unlocked in
	// this process. Transient — the caller retries after the next unlock, never
	// fails terminally on it.
	ErrVaultLocked = errors.New("secretopen: vault locked")
)

// Store is the narrow query surface OpenByID and OpenByIDOfKind need.
// *store.Queries satisfies it, as does workersvc's own Store interface.
type Store interface {
	GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error)
}

// OpenByID returns the decrypted plaintext of ONE specific secret of the user's,
// named by identity rather than by kind — the primitive a bound worker (M3) or a
// bound judge lane (M4) resolves through. Same sentinels and the same vault
// dispatch as OpenSealed, so callers map one set of errors either way.
//
// A secret id that does not belong to userID is ErrNoSecret, never that other
// user's credential: the query is owner-scoped AND the returned owner is
// re-checked here. Both are deliberate (PRD #104 D11) and neither is the primary
// defense — M3's composite FK is what makes a cross-user binding unrepresentable.
// This is the layer that holds if a handler check is ever bypassed, and the check
// below is the layer that holds if the query is ever edited to drop its scope.
//
// The row's OWN kind feeds the vault dispatch: the DEK AAD is user_id||kind, so a
// caller-supplied kind could only ever be a guess that fails GCM authentication.
func OpenByID(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID, secretID uuid.UUID) ([]byte, error) {
	row, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
		ID:     secretID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSecret
		}
		return nil, fmt.Errorf("secretopen: lookup by id: %w", err)
	}
	if row.UserID != userID {
		return nil, ErrNoSecret
	}
	return OpenSealed(vlt, box, userID, row.Kind, row.SealedWith, row.Ciphertext)
}

// OpenByIDOfKind is OpenByID with an additional KIND GUARD: it opens ONE specific
// secret of the user's only when the row's own user_secrets.kind equals expectedKind,
// and otherwise returns the SAME not-found sentinel (ErrNoSecret) a missing or foreign
// secret returns — never the plaintext, and never a distinct error that would confirm
// the row exists under a different kind.
//
// This is defense-in-depth for the bound credential paths (PRD #1147 audit #6): an
// api_key open path must never decrypt and return a codex_auth login blob (which carries
// a refresh_token), even if an upstream kind check were ever bypassed or a binding were
// somehow mis-kinded. The caller states the kind it EXPECTS to open; a row of any other
// kind is treated as if it did not exist. The kind check is non-disclosing on purpose —
// it collapses to ErrNoSecret rather than a "wrong kind" error so it leaks nothing about
// the row's real kind.
//
// The row's OWN kind still feeds the vault dispatch (the DEK AAD is user_id||kind); the
// expectedKind argument only gates access, it never changes how the ciphertext is opened.
func OpenByIDOfKind(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID, secretID uuid.UUID, expectedKind string) ([]byte, error) {
	row, err := q.GetUserSecretCiphertextByID(ctx, store.GetUserSecretCiphertextByIDParams{
		ID:     secretID,
		UserID: userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSecret
		}
		return nil, fmt.Errorf("secretopen: lookup by id: %w", err)
	}
	if row.UserID != userID {
		return nil, ErrNoSecret
	}
	if row.Kind != expectedKind {
		// Non-disclosing: a row of the wrong kind is the not-found sentinel, so this path
		// can never decrypt+return a credential of a kind the caller did not ask for.
		return nil, ErrNoSecret
	}
	return OpenSealed(vlt, box, userID, row.Kind, row.SealedWith, row.Ciphertext)
}

// OpenSealed decrypts an already-fetched sealed row, without a DB lookup — the
// path the rate-limit poller takes when it lists every token in one query (D1).
// It is the crypto half of OpenByID and shares the exact vault dispatch: a 'dek' row
// needs the owner unlocked (ErrVaultLocked otherwise), a legacy 'master' row opens
// under the master box regardless of lock state, and a nil vault opens under box
// directly. There is no ErrNoSecret here — the caller already has the row. Open
// errors never carry plaintext, so a decrypt failure collapses to ErrUndecryptable.
func OpenSealed(vlt *vault.Vault, box *secretbox.Box, userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	var plain []byte
	var err error
	if vlt != nil {
		plain, err = vlt.Open(userID, kind, sealedWith, ciphertext)
		if errors.Is(err, vault.ErrLocked) {
			return nil, ErrVaultLocked
		}
	} else {
		plain, err = box.Open(ciphertext)
	}
	if err != nil {
		return nil, ErrUndecryptable
	}
	return plain, nil
}

// Opener binds OpenSealed's collaborators so it satisfies a caller's one-method
// seam (the usage poller's TokenOpener). One instance is shared across polls.
type Opener struct {
	vlt *vault.Vault
	box *secretbox.Box
}

// NewOpener builds an Opener over the per-user vault (may be nil) and the
// master box.
func NewOpener(vlt *vault.Vault, box *secretbox.Box) *Opener {
	return &Opener{vlt: vlt, box: box}
}

// OpenSealed opens an already-fetched sealed row without a lookup. Both of the
// usage poller's paths take it: the tick opens the rows of its bulk listing, and
// the poke opens the one row it resolved.
func (o *Opener) OpenSealed(userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	return OpenSealed(o.vlt, o.box, userID, kind, sealedWith, ciphertext)
}
