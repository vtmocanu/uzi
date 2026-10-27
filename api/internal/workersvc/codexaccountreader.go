package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type codexAccountSnapshotStore interface {
	GetCodexProviderAccountSnapshot(context.Context, store.GetCodexProviderAccountSnapshotParams) (store.GetCodexProviderAccountSnapshotRow, error)
}

type codexAccountSnapshot struct {
	store.CodexProviderAccount
	VaultEpoch int64
}

func readCodexAccountSnapshot(ctx context.Context, q codexAccountSnapshotStore, arg store.GetCodexProviderAccountSnapshotParams) (codexAccountSnapshot, error) {
	result, err := q.GetCodexProviderAccountSnapshot(ctx, arg)
	row := codexAccountSnapshot{CodexProviderAccount: result.CodexProviderAccount, VaultEpoch: result.VaultEpoch}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return row, err
		}
		return row, fmt.Errorf("%w: account snapshot: %w", secretopen.ErrReaderFailure, err)
	}
	if row.UserID != arg.UserID || row.ID != arg.ID {
		return codexAccountSnapshot{}, store.ErrVaultSealStale
	}
	return row, nil
}

func codexAccountMarkerValid(marker string) bool {
	switch marker {
	case store.SealedWithMaster, store.SealedWithMasterAAD, store.SealedWithDEK:
		return true
	default:
		return false
	}
}

// codexSameAccountAuthority excludes only the four crypto conversion fields.
// Slot presence and marker validity remain constraints on conversion; all other
// metadata, including UpdatedAt, must equal the original captured authority.
func codexSameAccountAuthority(a, b store.CodexProviderAccount) bool {
	if (len(a.SealedLogin) == 0) != (len(b.SealedLogin) == 0) ||
		(len(a.RecoverySealed) == 0) != (len(b.RecoverySealed) == 0) ||
		!codexAccountMarkerValid(a.SealedWith) || !codexAccountMarkerValid(b.SealedWith) {
		return false
	}
	if a.RecoverySealedWith.Valid != b.RecoverySealedWith.Valid ||
		(len(a.RecoverySealed) > 0) != a.RecoverySealedWith.Valid ||
		(len(b.RecoverySealed) > 0) != b.RecoverySealedWith.Valid {
		return false
	}
	if len(a.RecoverySealed) > 0 && (!a.RecoverySealedWith.Valid || !b.RecoverySealedWith.Valid ||
		!codexAccountMarkerValid(a.RecoverySealedWith.String) || !codexAccountMarkerValid(b.RecoverySealedWith.String)) {
		return false
	}
	a.SealedLogin, b.SealedLogin = nil, nil
	a.SealedWith, b.SealedWith = "", ""
	a.RecoverySealed, b.RecoverySealed = nil, nil
	a.RecoverySealedWith, b.RecoverySealedWith = store.CodexProviderAccount{}.RecoverySealedWith, store.CodexProviderAccount{}.RecoverySealedWith
	return reflect.DeepEqual(a, b)
}

func (s *Service) openCodexAccountLogin(ctx context.Context, owner uuid.UUID, snapshot codexAccountSnapshot) (codexLoginBlob, error) {
	return s.openCodexAccountSlot(ctx, owner, snapshot, false)
}

// openCodexAccountSlot opens at most twice. Only the exact stale observation
// permits one same-ID refetch, and conversion cannot replace account authority.
func (s *Service) openCodexAccountSlot(ctx context.Context, owner uuid.UUID, snapshot codexAccountSnapshot, recovery bool) (codexLoginBlob, error) {
	if snapshot.UserID != owner {
		return codexLoginBlob{}, store.ErrVaultSealStale
	}
	open := func(row codexAccountSnapshot) ([]byte, error) {
		marker, ct := row.SealedWith, row.SealedLogin
		if recovery {
			if !row.RecoverySealedWith.Valid {
				return nil, secretopen.ErrUndecryptable
			}
			marker, ct = row.RecoverySealedWith.String, row.RecoverySealed
		}
		if !codexAccountMarkerValid(marker) || len(ct) == 0 {
			return nil, secretopen.ErrUndecryptable
		}
		return secretopen.OpenSealedAtEpoch(ctx, s.vlt, s.box, owner, store.KindCodexAuth, marker, ct, row.VaultEpoch)
	}
	plain, err := open(snapshot)
	if err == store.ErrVaultSealStale {
		q, ok := s.q.(codexAccountSnapshotStore)
		if !ok {
			return codexLoginBlob{}, fmt.Errorf("%w: account snapshot accessor missing", secretopen.ErrReaderFailure)
		}
		fresh, ferr := readCodexAccountSnapshot(ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: owner, ID: snapshot.ID})
		if ferr != nil {
			if errors.Is(ferr, pgx.ErrNoRows) {
				return codexLoginBlob{}, store.ErrVaultSealStale
			}
			return codexLoginBlob{}, ferr
		}
		if !codexSameAccountAuthority(snapshot.CodexProviderAccount, fresh.CodexProviderAccount) {
			return codexLoginBlob{}, store.ErrVaultSealStale
		}
		plain, err = open(fresh)
	}
	switch {
	case errors.Is(err, secretopen.ErrVaultLocked):
		return codexLoginBlob{}, errVaultLocked
	case errors.Is(err, secretopen.ErrUndecryptable):
		return codexLoginBlob{}, fmt.Errorf("%w: codex account login could not be decrypted", errCredentialUnavailable)
	case err != nil:
		return codexLoginBlob{}, err
	}
	var blob codexLoginBlob
	if err := json.Unmarshal(plain, &blob); err != nil {
		return codexLoginBlob{}, fmt.Errorf("%w: %v", ErrCodexLoginBlob, err)
	}
	if blob.AccessToken == "" {
		return codexLoginBlob{}, fmt.Errorf("%w: missing access token", ErrCodexLoginBlob)
	}
	return blob, nil
}
