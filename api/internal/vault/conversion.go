package vault

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	maxMaterialRows  int64 = 10000
	maxMaterialBytes int64 = 64 * 1024 * 1024
	maxBatchBytes    int64 = 1024 * 1024
	maxBatchRows     int64 = 64
)

// ConversionLimitError is returned before commit. Its transaction is aborted, so
// the mode and all material remain unchanged; it does not promise that deleting
// visible aliases removes orphan canonical accounts.
type ConversionLimitError struct {
	Rows, Bytes, MaxRowBytes int64
}

func (e *ConversionLimitError) Error() string {
	return fmt.Sprintf("vault conversion measured %d material rows and %d ciphertext bytes (limits: %d rows and %d bytes; largest record %d bytes, batch limit %d bytes). Your mode and credentials were not changed. Remove credentials using the existing management UI before retrying.",
		e.Rows, e.Bytes, maxMaterialRows, maxMaterialBytes, e.MaxRowBytes, maxBatchBytes)
}

func checkMaterialBudget(totals store.GetVaultMaterialTotalsRow) error {
	if totals.MaterialRows < 0 || totals.CiphertextBytes < 0 || totals.MaxRowBytes < 0 {
		return ErrInvalidState
	}
	if totals.MaterialRows > maxMaterialRows || totals.CiphertextBytes > maxMaterialBytes || totals.MaxRowBytes > maxBatchBytes {
		return &ConversionLimitError{Rows: totals.MaterialRows, Bytes: totals.CiphertextBytes, MaxRowBytes: totals.MaxRowBytes}
	}
	return nil
}

// CheckMaterialHeadroom reports whether adding addRows rows and addBytes ciphertext
// bytes to the owner's measured vault material (totals) stays inside the conversion
// budget, so a writer can refuse a save that would make a later vault enable or
// disable fail. It returns a *ConversionLimitError on overflow. Run environment
// variables count live, retired and pending-deletion rows alike (PRD #4416). addBytes is
// the change in total ciphertext (negative when a row shrinks); rowBytes is the size of
// the largest row being written, which the per-row batch limit applies to.
func CheckMaterialHeadroom(totals store.GetVaultMaterialTotalsRow, addRows, addBytes, rowBytes int64) error {
	next := totals
	next.MaterialRows += addRows
	next.CiphertextBytes += addBytes
	if rowBytes > next.MaxRowBytes {
		next.MaxRowBytes = rowBytes
	}
	return checkMaterialBudget(next)
}

type conversionProgress struct {
	rows, bytes int64
	totals      store.GetVaultMaterialTotalsRow
}

func (p *conversionProgress) add(rows, size int64) error {
	nextRows, nextBytes := p.rows+rows, p.bytes+size
	if rows < 1 || size < 0 || size > maxBatchBytes {
		return ErrInvalidState
	}
	if nextRows > maxMaterialRows || nextBytes > maxMaterialBytes {
		return &ConversionLimitError{Rows: nextRows, Bytes: nextBytes, MaxRowBytes: size}
	}
	if nextRows > p.totals.MaterialRows || nextBytes > p.totals.CiphertextBytes {
		return fmt.Errorf("vault: material changed during conversion: %w", ErrInvalidState)
	}
	p.rows, p.bytes = nextRows, nextBytes
	return nil
}

// convertMaterial is called only with lifecycle/advisory/users/wrapper exclusion.
// Accounts precede secret rows; all material is read locked and in ascending ID
// order; run environment variables follow the secret rows. Each returned collection is capped by SQL at 64 slots and 1 MiB.
func (v *Vault) convertMaterial(ctx context.Context, q Store, userID uuid.UUID, source store.GetUserVaultStateRow, targetMode string, targetEpoch int64, private *secretbox.Box, lazy bool) error {
	// Lock-order fence. A repo delete (DeleteRepoForUser) and a forge-connection delete
	// (DeleteForgeConnectionForUser) cascade run_env_vars rows without the
	// secret-mutation lock, so they can run against this conversion. Taking KEY SHARE on
	// the owner's env-bearing connections and then repos (each in id order) before any env
	// row lock makes both orders one-directional: a delete either finishes first, or waits
	// for this transaction at its connection/repo row before it reaches an env row. The
	// connection fence is needed because a connection delete cascades its repos in scan
	// order, which would otherwise lock a repo after this transaction holds env rows.
	if _, err := q.LockVaultEnvConnectionsKeyShare(ctx, userID); err != nil {
		return err
	}
	if _, err := q.LockVaultEnvReposKeyShare(ctx, userID); err != nil {
		return err
	}
	totals, err := q.GetVaultMaterialTotals(ctx, userID)
	if err != nil {
		return err
	}
	if err := checkMaterialBudget(totals); err != nil {
		return err
	}
	progress := conversionProgress{totals: totals}
	var sourceBox *secretbox.Box
	if source.VaultMode == store.VaultModeProtected {
		var ok bool
		sourceBox, ok, err = v.dekBoxAtEpoch(userID, &source.VaultEpoch)
		if err != nil {
			return err
		}
		if !ok {
			sourceBox = nil
		}
	}
	// Unlock supplies the privately unwrapped key without publishing it before
	// commit. A lazy conversion keeps the same protected epoch and key.
	if lazy && private != nil {
		if source.VaultMode != store.VaultModeProtected || targetMode != source.VaultMode || targetEpoch != source.VaultEpoch {
			return ErrInvalidState
		}
		sourceBox = private
	}
	if lazy && sourceBox == nil {
		return ErrLocked
	}
	if targetMode == store.VaultModeProtected && private == nil {
		private = sourceBox
	}
	cursor := pgtype.UUID{}
	for {
		rows, err := q.LockVaultAccountMaterialBatch(ctx, store.LockVaultAccountMaterialBatchParams{UserID: userID, AfterID: cursor})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		var batchRows, batchBytes int64
		for _, row := range rows {
			slots, size := int64(1), int64(len(row.SealedLogin))
			if row.RecoverySealedWith.Valid {
				slots++
				size += int64(len(row.RecoverySealed))
			} else if row.RecoverySealed != nil {
				return ErrInvalidState
			}
			batchRows, batchBytes = batchRows+slots, batchBytes+size
			if batchRows > maxBatchRows || batchBytes > maxBatchBytes {
				return ErrInvalidState
			}
			if err := progress.add(slots, size); err != nil {
				return err
			}
			live, liveMarker, err := v.convertCipher(userID, store.KindCodexAuth, row.SealedLogin, row.SealedWith, source, sourceBox, targetMode, targetEpoch, private, lazy)
			if err != nil {
				return err
			}
			var recovery []byte
			var recoveryMarker pgtype.Text
			if row.RecoverySealedWith.Valid {
				recovery, recoveryMarker.String, err = v.convertCipher(userID, store.KindCodexAuth, row.RecoverySealed, row.RecoverySealedWith.String, source, sourceBox, targetMode, targetEpoch, private, lazy)
				if err != nil {
					return err
				}
				recoveryMarker.Valid = true
			}
			if !lazy || row.SealedWith != store.SealedWithDEK || (row.RecoverySealedWith.Valid && row.RecoverySealedWith.String != store.SealedWithDEK) {
				n, err := q.ResealVaultAccountMaterial(ctx, store.ResealVaultAccountMaterialParams{
					ID: row.ID, UserID: userID, SealedLogin: live, SealedWith: liveMarker,
					RecoverySealed: recovery, RecoverySealedWith: recoveryMarker,
				})
				if err != nil {
					return err
				}
				if n != 1 {
					return ErrInvalidState
				}
			}
			cursor = pgtype.UUID{Bytes: row.ID, Valid: true}
		}
	}
	cursor = pgtype.UUID{}
	for {
		rows, err := q.LockVaultSecretMaterialBatch(ctx, store.LockVaultSecretMaterialBatchParams{UserID: userID, AfterID: cursor})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			break
		}
		if int64(len(rows)) > maxBatchRows {
			return ErrInvalidState
		}
		var batchBytes int64
		for _, row := range rows {
			size := int64(len(row.Ciphertext))
			batchBytes += size
			if batchBytes > maxBatchBytes {
				return ErrInvalidState
			}
			if err := progress.add(1, size); err != nil {
				return err
			}
			if !lazy || row.SealedWith != store.SealedWithDEK {
				ciphertext, marker, err := v.convertCipher(userID, row.Kind, row.Ciphertext, row.SealedWith, source, sourceBox, targetMode, targetEpoch, private, lazy)
				if err != nil {
					return err
				}
				n, err := q.ResealVaultSecretMaterial(ctx, store.ResealVaultSecretMaterialParams{
					ID: row.ID, UserID: userID, Ciphertext: ciphertext, SealedWith: marker,
				})
				if err != nil {
					return err
				}
				if n != 1 {
					return ErrInvalidState
				}
			}
			cursor = pgtype.UUID{Bytes: row.ID, Valid: true}
		}
	}
	// Run environment variables (PRD #4416): every version row, whatever its state.
	// The sealing context is rebuilt from the row's own columns.
	cursor = pgtype.UUID{}
	for {
		// Candidates are listed unlocked, then locked by id. Rows can vanish in between,
		// so the scan ends only on an empty candidate list. The cursor follows the last
		// candidate, so a listed id is never listed again (defensive: a vanished id
		// would also drop out of the next listing on its own).
		candidates, err := q.ListVaultEnvMaterialCandidates(ctx, store.ListVaultEnvMaterialCandidatesParams{UserID: userID, AfterID: cursor})
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			break
		}
		if int64(len(candidates)) > maxBatchRows {
			return ErrInvalidState
		}
		lastCandidate := candidates[len(candidates)-1]
		rows, err := q.LockVaultEnvMaterialBatch(ctx, store.LockVaultEnvMaterialBatchParams{UserID: userID, Ids: candidates})
		if err != nil {
			return err
		}
		if int64(len(rows)) > maxBatchRows {
			return ErrInvalidState
		}
		var batchBytes int64
		for _, row := range rows {
			size := int64(len(row.Ciphertext))
			batchBytes += size
			if batchBytes > maxBatchBytes {
				return ErrInvalidState
			}
			if err := progress.add(1, size); err != nil {
				return err
			}
			if row.SealedWith == store.SealedWithMaster {
				return ErrInvalidState // env rows are never sealed without AAD (table CHECK)
			}
			if !lazy || row.SealedWith != store.SealedWithDEK {
				scope, repoID := runenv.ScopeUser, (*uuid.UUID)(nil)
				if row.RepoID.Valid {
					id := uuid.UUID(row.RepoID.Bytes)
					scope, repoID = runenv.ScopeRepo, &id
				}
				kind, err := runenv.AADFor(row.AadVersion, userID, scope, repoID, row.Secret, row.Name, row.VarID, row.Rev)
				if err != nil {
					return err
				}
				ciphertext, marker, err := v.convertCipher(userID, kind, row.Ciphertext, row.SealedWith, source, sourceBox, targetMode, targetEpoch, private, lazy)
				if err != nil {
					return err
				}
				n, err := q.ResealVaultEnvMaterial(ctx, store.ResealVaultEnvMaterialParams{
					ID: row.ID, UserID: userID, Ciphertext: ciphertext, SealedWith: marker,
				})
				if err != nil {
					return err
				}
				if n != 1 {
					return ErrInvalidState
				}
			}
		}
		cursor = pgtype.UUID{Bytes: lastCandidate, Valid: true}
	}
	// progress counts the pre-reseal sizes of the rows this pass locked and compares them
	// with totals, which was measured before any row was resealed. Only the fresh
	// fallback below reads post-reseal sizes. Both comparisons, and the totals model as a
	// whole, rely on resealing preserving ciphertext length (every mode is AES-GCM:
	// nonce plus a constant tag).
	if progress.rows != totals.MaterialRows || progress.bytes != totals.CiphertextBytes {
		// The fence above is taken before the first measurement, so no env row can vanish
		// to a repo or forge-connection delete (which cascades env rows without the
		// secret-mutation lock) during this conversion, and every env-row writer takes the
		// secret-mutation advisory lock this conversion holds (withSecretLock and
		// mutateLiveRunEnv in handler/run_env_vars.go); a new writer must too. This branch
		// stays defensive: one fresh read (READ COMMITTED, postgres.go) is the settled
		// total, accepted only on an exact match.
		fresh, err := q.GetVaultMaterialTotals(ctx, userID)
		if err != nil {
			return err
		}
		if progress.rows != fresh.MaterialRows || progress.bytes != fresh.CiphertextBytes {
			return fmt.Errorf("vault: material totals changed: %w", ErrInvalidState)
		}
	}
	return nil
}

func (v *Vault) convertCipher(userID uuid.UUID, kind string, ciphertext []byte, marker string, source store.GetUserVaultStateRow, sourceBox *secretbox.Box, targetMode string, targetEpoch int64, private *secretbox.Box, lazy bool) ([]byte, string, error) {
	if lazy && marker == store.SealedWithDEK {
		return ciphertext, marker, nil
	}
	var plain []byte
	var err error
	if marker == store.SealedWithDEK {
		if source.VaultMode != store.VaultModeProtected {
			return nil, "", ErrInvalidState
		}
		if sourceBox == nil {
			return nil, "", ErrLocked
		}
		plain, err = sourceBox.OpenWithAAD(ciphertext, secretAAD(userID, kind))
	} else {
		plain, err = OpenMasterMaterial(v.master, userID, kind, marker, ciphertext)
	}
	if err != nil {
		return nil, "", err
	}
	// This defer is per helper invocation, not per collection, so plaintext does
	// not accumulate across pages or rows.
	defer wipe(plain)
	sealed, err := v.sealState(userID, kind, plain, targetMode, targetEpoch, private, SealForUse)
	if err != nil {
		return nil, "", err
	}
	return sealed.Ciphertext, sealed.SealedWith, nil
}
