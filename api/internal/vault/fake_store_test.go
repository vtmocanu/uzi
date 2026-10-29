package vault

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// transaction serializes fixture callbacks only. It deliberately supplies no
// rollback model; database atomicity must be tested with real PostgreSQL.
func (f *fakeVaultStore) transaction(q Store) Transact {
	return func(ctx context.Context, fn func(context.Context, Store) error) error {
		f.txMu.Lock()
		defer f.txMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(WithStateReader(ctx, q), q)
	}
}

func (f *fakeVaultStore) state(id uuid.UUID) store.LockVaultAuthStateRow {
	s, ok := f.states[id]
	if !ok {
		s.VaultMode = store.VaultModeProtected
	}
	return s
}
func (f *fakeVaultStore) LockSettingsAdmission(ctx context.Context) error           { return ctx.Err() }
func (f *fakeVaultStore) LockSecretMutation(ctx context.Context, _ uuid.UUID) error { return ctx.Err() }
func (f *fakeVaultStore) GetVaultPolicy(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}
func (f *fakeVaultStore) LockVaultAuthState(_ context.Context, id uuid.UUID) (store.LockVaultAuthStateRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state(id), nil
}
func (f *fakeVaultStore) GetUserVaultState(_ context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state(id)
	return store.GetUserVaultStateRow{VaultMode: s.VaultMode, VaultEpoch: s.VaultEpoch}, nil
}
func (f *fakeVaultStore) LockUserVault(ctx context.Context, id uuid.UUID) (store.UserVault, error) {
	return f.GetUserVault(ctx, id)
}
func (f *fakeVaultStore) DeleteUserVault(_ context.Context, id uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vaults[id]; !ok {
		return 0, nil
	}
	delete(f.vaults, id)
	return 1, nil
}
func (f *fakeVaultStore) AdvanceUserVaultMode(_ context.Context, arg store.AdvanceUserVaultModeParams) (store.AdvanceUserVaultModeRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state(arg.ID)
	s.VaultMode = arg.VaultMode
	s.VaultEpoch++
	f.states[arg.ID] = s
	return store.AdvanceUserVaultModeRow{VaultMode: s.VaultMode, VaultEpoch: s.VaultEpoch}, nil
}
func (f *fakeVaultStore) HasMasterVaultMaterial(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		if s.userID == id && (s.sealedWith == store.SealedWithMaster || s.sealedWith == store.SealedWithMasterAAD) {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeVaultStore) GetVaultMaterialTotals(_ context.Context, id uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var t store.GetVaultMaterialTotalsRow
	for _, s := range f.secrets {
		if s.userID != id {
			continue
		}
		n := int64(len(s.ciphertext))
		t.MaterialRows++
		t.CiphertextBytes += n
		if n > t.MaxRowBytes {
			t.MaxRowBytes = n
		}
	}
	return t, nil
}

// No account rows are seeded by this crypto fixture.
func (f *fakeVaultStore) LockVaultAccountMaterialBatch(context.Context, store.LockVaultAccountMaterialBatchParams) ([]store.LockVaultAccountMaterialBatchRow, error) {
	return nil, nil
}
func (f *fakeVaultStore) ResealVaultAccountMaterial(context.Context, store.ResealVaultAccountMaterialParams) (int64, error) {
	return 0, errors.New("fake: account reseal unsupported")
}
func (f *fakeVaultStore) LockVaultSecretMaterialBatch(_ context.Context, arg store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []*fakeSecret
	for _, s := range f.secrets {
		if s.userID == arg.UserID && (!arg.AfterID.Valid || bytes.Compare(s.id[:], arg.AfterID.Bytes[:]) > 0) {
			matching = append(matching, s)
		}
	}
	sort.Slice(matching, func(i, j int) bool { return bytes.Compare(matching[i].id[:], matching[j].id[:]) < 0 })
	var out []store.LockVaultSecretMaterialBatchRow
	var size int64
	for _, s := range matching {
		size += int64(len(s.ciphertext))
		if int64(len(out)) == maxBatchRows || size > maxBatchBytes {
			break
		}
		out = append(out, store.LockVaultSecretMaterialBatchRow{ID: s.id, Kind: s.kind, Ciphertext: append([]byte(nil), s.ciphertext...), SealedWith: s.sealedWith})
	}
	return out, nil
}
func (f *fakeVaultStore) ResealVaultSecretMaterial(_ context.Context, arg store.ResealVaultSecretMaterialParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.secrets {
		if s.id == arg.ID && s.userID == arg.UserID {
			s.ciphertext = append([]byte(nil), arg.Ciphertext...)
			s.sealedWith = arg.SealedWith
			return 1, nil
		}
	}
	return 0, nil
}
func (f *fakeVaultStore) ClaimVaultLockNotice(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	return id, nil
}

var _ Store = (*fakeVaultStore)(nil)
