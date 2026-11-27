package vault

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
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
func (f *fakeVaultStore) LockSettingsAdmission(ctx context.Context) error { return ctx.Err() }
func (f *fakeVaultStore) LockSecretMutation(ctx context.Context, _ uuid.UUID) error {
	f.record("LockSecretMutation")
	return ctx.Err()
}

// record appends to the ordered call log reset_test.go asserts on.
func (f *fakeVaultStore) record(name string) {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeVaultStore) resetCalls() {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.calls = nil
}

func (f *fakeVaultStore) callLog() []string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeVaultStore) GetVaultPolicy(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}
func (f *fakeVaultStore) LockVaultAuthState(_ context.Context, id uuid.UUID) (store.LockVaultAuthStateRow, error) {
	f.record("LockVaultAuthState")
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
	f.record("DeleteUserVault")
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.vaults[id]; !ok {
		return 0, nil
	}
	delete(f.vaults, id)
	return 1, nil
}
func (f *fakeVaultStore) AdvanceUserVaultMode(_ context.Context, arg store.AdvanceUserVaultModeParams) (store.AdvanceUserVaultModeRow, error) {
	f.record("AdvanceUserVaultMode")
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
	for _, e := range f.envs {
		if e.userID == id && (e.sealedWith == store.SealedWithMaster || e.sealedWith == store.SealedWithMasterAAD) {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeVaultStore) GetVaultMaterialTotals(_ context.Context, id uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	f.record("GetVaultMaterialTotals")
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
	for _, e := range f.envs {
		if e.userID != id {
			continue
		}
		n := int64(len(e.ciphertext))
		t.MaterialRows++
		t.CiphertextBytes += n
		if n > t.MaxRowBytes {
			t.MaxRowBytes = n
		}
	}
	return t, nil
}

// totalsHookStore wraps the fixture so a test can act between the conversion's first
// measurement of the owner's material and the end of its env batches, which is where a
// repo delete (cascading env rows without the secret-mutation lock) can land. after is
// called once, right after the first GetVaultMaterialTotals; skewFresh, when set,
// is added to the rows of every later measurement.
type totalsHookStore struct {
	*fakeVaultStore
	after     func()
	skewFresh int64
	calls     int
}

func (s *totalsHookStore) GetVaultMaterialTotals(ctx context.Context, id uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	t, err := s.fakeVaultStore.GetVaultMaterialTotals(ctx, id)
	s.calls++
	if s.calls == 1 && s.after != nil {
		s.after()
	}
	if s.calls > 1 {
		t.MaterialRows += s.skewFresh
	}
	return t, err
}

// No account rows are seeded by this crypto fixture.
func (f *fakeVaultStore) LockVaultAccountMaterialBatch(context.Context, store.LockVaultAccountMaterialBatchParams) ([]store.LockVaultAccountMaterialBatchRow, error) {
	f.record("LockVaultAccountMaterialBatch")
	return nil, nil
}
func (f *fakeVaultStore) ResealVaultAccountMaterial(context.Context, store.ResealVaultAccountMaterialParams) (int64, error) {
	return 0, errors.New("fake: account reseal unsupported")
}
func (f *fakeVaultStore) LockVaultSecretMaterialBatch(_ context.Context, arg store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error) {
	f.record("LockVaultSecretMaterialBatch")
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
func (f *fakeVaultStore) LockVaultEnvConnectionsKeyShare(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	f.record("LockVaultEnvConnectionsKeyShare")
	return nil, nil
}
func (f *fakeVaultStore) LockVaultEnvReposKeyShare(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	f.record("LockVaultEnvReposKeyShare")
	return nil, nil
}
func (f *fakeVaultStore) ListVaultEnvMaterialCandidates(_ context.Context, arg store.ListVaultEnvMaterialCandidatesParams) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []*fakeEnv
	for _, e := range f.envs {
		if e.userID == arg.UserID && (!arg.AfterID.Valid || bytes.Compare(e.id[:], arg.AfterID.Bytes[:]) > 0) {
			matching = append(matching, e)
		}
	}
	sort.Slice(matching, func(i, j int) bool { return bytes.Compare(matching[i].id[:], matching[j].id[:]) < 0 })
	var out []uuid.UUID
	var size int64
	for _, e := range matching {
		size += int64(len(e.ciphertext))
		if int64(len(out)) == maxBatchRows || size > maxBatchBytes {
			break
		}
		out = append(out, e.id)
	}
	return out, nil
}
func (f *fakeVaultStore) LockVaultEnvMaterialBatch(_ context.Context, arg store.LockVaultEnvMaterialBatchParams) ([]store.LockVaultEnvMaterialBatchRow, error) {
	f.record("LockVaultEnvMaterialBatch")
	f.mu.Lock()
	defer f.mu.Unlock()
	var matching []*fakeEnv
	for _, e := range f.envs {
		if e.userID != arg.UserID {
			continue
		}
		for _, id := range arg.Ids {
			if id == e.id {
				matching = append(matching, e)
				break
			}
		}
	}
	sort.Slice(matching, func(i, j int) bool { return bytes.Compare(matching[i].id[:], matching[j].id[:]) < 0 })
	var out []store.LockVaultEnvMaterialBatchRow
	for _, e := range matching {
		out = append(out, store.LockVaultEnvMaterialBatchRow{ID: e.id, RepoID: e.repoID, Name: e.name, Secret: e.secret,
			Ciphertext: append([]byte(nil), e.ciphertext...), SealedWith: e.sealedWith,
			VarID: e.varID, Rev: e.rev, AadVersion: e.aadVersion})
	}
	return out, nil
}

// candidateHookStore lets a test act between the env candidate listing and the row
// lock, which is where a cascade delete can remove a whole page. after runs once, right
// after the first non-empty candidate list is returned.
type candidateHookStore struct {
	*fakeVaultStore
	after func(candidates []uuid.UUID)
	fired bool
	// afterIDs records the AfterID cursor of every listing call, in call order.
	afterIDs []pgtype.UUID
}

func (s *candidateHookStore) ListVaultEnvMaterialCandidates(ctx context.Context, arg store.ListVaultEnvMaterialCandidatesParams) ([]uuid.UUID, error) {
	s.afterIDs = append(s.afterIDs, arg.AfterID)
	ids, err := s.fakeVaultStore.ListVaultEnvMaterialCandidates(ctx, arg)
	if err == nil && len(ids) > 0 && !s.fired && s.after != nil {
		s.fired = true
		s.after(ids)
	}
	return ids, err
}
func (f *fakeVaultStore) ResealVaultEnvMaterial(_ context.Context, arg store.ResealVaultEnvMaterialParams) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.envs {
		if e.id == arg.ID && e.userID == arg.UserID {
			e.ciphertext = append([]byte(nil), arg.Ciphertext...)
			e.sealedWith = arg.SealedWith
			return 1, nil
		}
	}
	return 0, nil
}
func (f *fakeVaultStore) DeleteAllRunEnvVarsForOwner(_ context.Context, id uuid.UUID) (int64, error) {
	f.record("DeleteAllRunEnvVarsForOwner")
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []*fakeEnv
	var n int64
	for _, e := range f.envs {
		if e.userID == id {
			n++
			continue
		}
		kept = append(kept, e)
	}
	f.envs = kept
	return n, nil
}
func (f *fakeVaultStore) ClaimVaultLockNotice(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	return id, nil
}

func (f *fakeVaultStore) RevokeVaultBrowserSlots(ctx context.Context, id uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state(id)
	s.VaultSlotGeneration++
	f.states[id] = s
	return s.VaultSlotGeneration, ctx.Err()
}

// resetStore surface. The fixture has no rollback model, so tests assert that a
// failing callback never reaches the Delete* methods rather than undoing them.
func (f *fakeVaultStore) LockOwnerWorkersForReset(ctx context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	f.record("LockOwnerWorkersForReset")
	return nil, ctx.Err()
}
func (f *fakeVaultStore) LockOwnerRunsForReset(ctx context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	f.record("LockOwnerRunsForReset")
	return nil, ctx.Err()
}
func (f *fakeVaultStore) LockOwnerSecretsForReset(ctx context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	f.record("LockOwnerSecretsForReset")
	return nil, ctx.Err()
}
func (f *fakeVaultStore) DeleteAllUserSecretsForOwner(_ context.Context, id uuid.UUID) (int64, error) {
	f.record("DeleteAllUserSecretsForOwner")
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []*fakeSecret
	var n int64
	for _, s := range f.secrets {
		if s.userID == id {
			n++
			continue
		}
		kept = append(kept, s)
	}
	f.secrets = kept
	return n, nil
}
func (f *fakeVaultStore) DeleteAllCodexProviderAccountsForOwner(_ context.Context, id uuid.UUID) (int64, error) {
	f.record("DeleteAllCodexProviderAccountsForOwner")
	f.mu.Lock()
	defer f.mu.Unlock()
	n := int64(f.codexAccounts[id])
	delete(f.codexAccounts, id)
	return n, nil
}
func (f *fakeVaultStore) ClearSidebarCodexAccounts(context.Context, uuid.UUID) error {
	f.record("ClearSidebarCodexAccounts")
	return nil
}

// BrowserStore surface: reset hands its callback a BrowserStore. The reset tests
// never exercise these; they exist so the fixture satisfies the interface.
var errFakeBrowser = errors.New("fake: browser store method unsupported")

func (f *fakeVaultStore) GetVaultReunlockDays(context.Context) (string, error) {
	return "", errFakeBrowser
}
func (f *fakeVaultStore) GetVaultBrowserSlot(context.Context, store.GetVaultBrowserSlotParams) (store.VaultBrowserSlot, error) {
	return store.VaultBrowserSlot{}, errFakeBrowser
}
func (f *fakeVaultStore) PrepareVaultBrowserSlot(context.Context, store.PrepareVaultBrowserSlotParams) error {
	return errFakeBrowser
}
func (f *fakeVaultStore) TrimVaultBrowserSlots(context.Context, uuid.UUID) error {
	return errFakeBrowser
}
func (f *fakeVaultStore) InsertVaultBrowserSlot(context.Context, store.InsertVaultBrowserSlotParams) error {
	return errFakeBrowser
}
func (f *fakeVaultStore) SetVaultAutoReunlock(context.Context, store.SetVaultAutoReunlockParams) error {
	return errFakeBrowser
}
func (f *fakeVaultStore) BumpTokenVersion(context.Context, uuid.UUID) (int32, error) {
	return 0, errFakeBrowser
}
func (f *fakeVaultStore) UpdatePassword(context.Context, store.UpdatePasswordParams) error {
	return errFakeBrowser
}
func (f *fakeVaultStore) SetUserActive(context.Context, store.SetUserActiveParams) (store.User, error) {
	return store.User{}, errFakeBrowser
}
func (f *fakeVaultStore) SupersedeLiveResetLinksForUser(context.Context, store.SupersedeLiveResetLinksForUserParams) (int64, error) {
	return 0, errFakeBrowser
}

var (
	_ Store        = (*fakeVaultStore)(nil)
	_ BrowserStore = (*fakeVaultStore)(nil)
	_ resetStore   = (*fakeVaultStore)(nil)
)
