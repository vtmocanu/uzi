// Package secretopen opens a user's decrypted secret via the per-user vault path,
// the one place that logic lives (factored out of workersvc for PRD #53 so the
// run lane and the rate-limit poller open tokens the same way). It reproduces the
// vault dispatch exactly: a 'dek'-sealed row needs the owner's vault unlocked (a
// lock surfaces as ErrVaultLocked — transient, retry), a legacy 'master'-sealed
// row opens under the process master box regardless of lock state, and a
// 'master_aad' row authenticates its owner and kind under that box. Without a
// vault, only the legacy OpenSealed helper can open master formats. Snapshot
// readers require a vault to validate the durable epoch. Unknown discriminators
// are refused. Callers map these sentinels to their own domain
// errors (workersvc) or skip semantics (the poller).
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
	// ErrReaderFailure marks lookup, configuration, or policy infrastructure
	// failures so callers preserve them instead of blaming the credential.
	// The underlying error remains available through errors.Is/errors.As.
	ErrReaderFailure = errors.New("secretopen: reader failure")
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
	return openByID(ctx, q, vlt, box, userID, secretID, "", nil)
}

// OpenByIDOfKind additionally refuses a row of any other kind without disclosing it.
func OpenByIDOfKind(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID, secretID uuid.UUID, expectedKind string) ([]byte, error) {
	return openByID(ctx, q, vlt, box, userID, secretID, expectedKind, nil)
}

// OpenByIDOfKindAtRevision fences an alias revision captured before this lookup.
// It checks that revision before the first crypto operation as well as the retry.
func OpenByIDOfKindAtRevision(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID, secretID uuid.UUID, expectedKind string, revision int64) ([]byte, error) {
	return openByID(ctx, q, vlt, box, userID, secretID, expectedKind, &revision)
}

func openByID(ctx context.Context, q Store, vlt *vault.Vault, box *secretbox.Box, userID, secretID uuid.UUID, expectedKind string, revision *int64) ([]byte, error) {
	arg := store.GetUserSecretCiphertextByIDParams{ID: secretID, UserID: userID}
	row, err := q.GetUserSecretCiphertextByID(ctx, arg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoSecret
		}
		return nil, fmt.Errorf("%w: lookup by id: %w", ErrReaderFailure, err)
	}
	if row.ID != secretID || row.UserID != userID || (expectedKind != "" && row.Kind != expectedKind) {
		return nil, ErrNoSecret
	}
	if revision != nil && (!row.MaterialRevision.Valid || row.MaterialRevision.Int64 != *revision) {
		return nil, store.ErrVaultSealStale
	}
	plain, err := OpenSealedAtEpoch(ctx, vlt, box, userID, row.Kind, row.SealedWith, row.Ciphertext, row.VaultEpoch)
	if err != store.ErrVaultSealStale {
		return plain, err
	}
	// Exactly one same-ID refetch, only after the crypto layer observed an epoch
	// mismatch. Disabled rows are permitted here for already-started refreshes;
	// callers retain their admission/finalization checks.
	fresh, err := q.GetUserSecretCiphertextByID(ctx, arg)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrVaultSealStale
	}
	if err != nil {
		return nil, fmt.Errorf("%w: refetch by id: %w", ErrReaderFailure, err)
	}
	if fresh.ID != row.ID || fresh.UserID != row.UserID || fresh.Kind != row.Kind ||
		fresh.Disabled != row.Disabled || fresh.EnablementRev != row.EnablementRev ||
		fresh.MaterialRevision != row.MaterialRevision {
		return nil, store.ErrVaultSealStale
	}
	return OpenSealedAtEpoch(ctx, vlt, box, userID, fresh.Kind, fresh.SealedWith, fresh.Ciphertext, fresh.VaultEpoch)
}

// OpenSealedAtEpoch opens only a material/epoch pair captured in one SQL snapshot.
// It preserves policy and infrastructure failures; only invalid material is
// classified as an unusable credential. A Vault is required even in off mode.
func OpenSealedAtEpoch(ctx context.Context, vlt *vault.Vault, _ *secretbox.Box, userID uuid.UUID, kind, sealedWith string, ciphertext []byte, epoch int64) ([]byte, error) {
	plain, err := vlt.OpenAtEpoch(ctx, userID, kind, sealedWith, ciphertext, epoch)
	if err == store.ErrVaultSealStale {
		return nil, store.ErrVaultSealStale
	}
	if errors.Is(err, vault.ErrLocked) {
		return nil, ErrVaultLocked
	}
	if errors.Is(err, vault.ErrInvalidMaterial) {
		return nil, ErrUndecryptable
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReaderFailure, err)
	}
	return plain, nil
}

// OpenSealed is the compatibility opener for material without a captured epoch.
// Production snapshot readers use OpenSealedAtEpoch and retain their read fences.
// It shares the same discriminator dispatch: a 'dek' row
// needs the owner unlocked (ErrVaultLocked otherwise), a legacy 'master' row opens
// under the master box regardless of lock state, and master_aad authenticates
// the owner and kind with that box. Without a vault, DEK rows remain locked and
// master discriminators keep the same dispatch. The caller already has the row,
// so ErrNoSecret cannot occur here. Errors never carry plaintext; a decryption
// failure collapses to ErrUndecryptable.
func OpenSealed(vlt *vault.Vault, box *secretbox.Box, userID uuid.UUID, kind, sealedWith string, ciphertext []byte) ([]byte, error) {
	var plain []byte
	var err error
	if vlt != nil {
		plain, err = vlt.Open(userID, kind, sealedWith, ciphertext)
	} else {
		plain, err = vault.OpenMasterMaterial(box, userID, kind, sealedWith, ciphertext)
	}
	if errors.Is(err, vault.ErrLocked) {
		return nil, ErrVaultLocked
	}
	if err != nil {
		return nil, ErrUndecryptable
	}
	return plain, nil
}

// Opener binds OpenSealedAtEpoch's collaborators so it satisfies a caller's one-method
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

// OpenSealedAtEpoch opens an atomic SQL material/epoch snapshot.
func (o *Opener) OpenSealedAtEpoch(ctx context.Context, userID uuid.UUID, kind, sealedWith string, ciphertext []byte, epoch int64) ([]byte, error) {
	return OpenSealedAtEpoch(ctx, o.vlt, o.box, userID, kind, sealedWith, ciphertext, epoch)
}
