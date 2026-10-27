package workersvc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// memVaultStore is an in-memory vault.Store so the claim gate + lock-race paths
// can run against a real *vault.Vault without a database.
type memVaultStore struct {
	// defaultMode is opt-in for populations of legacy master-sealed fixtures.
	// The zero value keeps protected admission for vault gate tests.
	defaultMode string
	mu          sync.Mutex
	txMu        sync.Mutex
	vaults      map[uuid.UUID]store.UserVault
	states      map[uuid.UUID]store.LockVaultAuthStateRow
}

func newMemVaultStore() *memVaultStore {
	return &memVaultStore{vaults: map[uuid.UUID]store.UserVault{}, states: map[uuid.UUID]store.LockVaultAuthStateRow{}}
}

func (m *memVaultStore) GetUserVault(_ context.Context, id uuid.UUID) (store.UserVault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vaults[id]
	if !ok {
		return store.UserVault{}, pgx.ErrNoRows
	}
	return v, nil
}

func (m *memVaultStore) CreateUserVaultIfAbsent(_ context.Context, arg store.CreateUserVaultIfAbsentParams) (store.UserVault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vaults[arg.UserID]; ok {
		return store.UserVault{}, pgx.ErrNoRows
	}
	v := store.UserVault{UserID: arg.UserID, KekSalt: arg.KekSalt, WrappedDek: arg.WrappedDek}
	m.vaults[arg.UserID] = v
	return v, nil
}

// transaction serializes crypto-only fixture callbacks; it does not model
// rollback. Atomicity and database lock ordering require LiveDB tests.
func (m *memVaultStore) transaction(ctx context.Context, fn func(context.Context, vault.Store) error) error {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(vault.WithStateReader(ctx, m), m)
}

func newMemoryVault(box *secretbox.Box, m *memVaultStore) *vault.Vault {
	return vault.New(box, m, m.transaction)
}

// newOffModeService gives master-sealed claim fixtures a configured real vault
// with explicit off-mode state at epoch zero for every fixture owner.
func newOffModeService(q Store, box *secretbox.Box, p Params) *Service {
	m := newMemVaultStore()
	m.defaultMode = store.VaultModeUnprotected
	s := New(q, box, p)
	s.SetVault(newMemoryVault(box, m))
	return s
}

func (m *memVaultStore) state(id uuid.UUID) store.LockVaultAuthStateRow {
	s, ok := m.states[id]
	if !ok {
		s.VaultMode = m.defaultMode
		if s.VaultMode == "" {
			s.VaultMode = store.VaultModeProtected
		}
	}
	return s
}
func (m *memVaultStore) GetUserVaultState(_ context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state(id)
	return store.GetUserVaultStateRow{VaultMode: s.VaultMode, VaultEpoch: s.VaultEpoch}, nil
}
func (m *memVaultStore) LockVaultAuthState(_ context.Context, id uuid.UUID) (store.LockVaultAuthStateRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state(id), nil
}
func (m *memVaultStore) LockSettingsAdmission(ctx context.Context) error           { return ctx.Err() }
func (m *memVaultStore) LockSecretMutation(ctx context.Context, _ uuid.UUID) error { return ctx.Err() }
func (m *memVaultStore) GetVaultPolicy(ctx context.Context) (string, error) {
	return "optional", ctx.Err()
}
func (m *memVaultStore) LockUserVault(ctx context.Context, id uuid.UUID) (store.UserVault, error) {
	return m.GetUserVault(ctx, id)
}
func (m *memVaultStore) DeleteUserVault(_ context.Context, id uuid.UUID) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vaults[id]; !ok {
		return 0, nil
	}
	delete(m.vaults, id)
	return 1, nil
}
func (m *memVaultStore) AdvanceUserVaultMode(_ context.Context, arg store.AdvanceUserVaultModeParams) (store.AdvanceUserVaultModeRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.state(arg.ID)
	s.VaultMode = arg.VaultMode
	s.VaultEpoch++
	m.states[arg.ID] = s
	return store.AdvanceUserVaultModeRow{VaultMode: s.VaultMode, VaultEpoch: s.VaultEpoch}, nil
}
func (m *memVaultStore) HasMasterVaultMaterial(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}
func (m *memVaultStore) GetVaultMaterialTotals(context.Context, uuid.UUID) (store.GetVaultMaterialTotalsRow, error) {
	return store.GetVaultMaterialTotalsRow{}, nil
}
func (m *memVaultStore) LockVaultAccountMaterialBatch(context.Context, store.LockVaultAccountMaterialBatchParams) ([]store.LockVaultAccountMaterialBatchRow, error) {
	return nil, nil
}
func (m *memVaultStore) LockVaultSecretMaterialBatch(context.Context, store.LockVaultSecretMaterialBatchParams) ([]store.LockVaultSecretMaterialBatchRow, error) {
	return nil, nil
}
func (m *memVaultStore) ResealVaultAccountMaterial(context.Context, store.ResealVaultAccountMaterialParams) (int64, error) {
	return 0, errors.New("memory vault: no account material")
}
func (m *memVaultStore) ResealVaultSecretMaterial(context.Context, store.ResealVaultSecretMaterialParams) (int64, error) {
	return 0, errors.New("memory vault: no secret material")
}
func (m *memVaultStore) ClaimVaultLockNotice(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	return id, nil
}

var _ vault.Store = (*memVaultStore)(nil)

func (m *memVaultStore) ClearVaultLockNotice(context.Context, uuid.UUID) error { return nil }

// unlockedVault returns a vault with uid unlocked (its DEK cached).
func unlockedVault(t *testing.T, uid uuid.UUID, box *secretbox.Box) *vault.Vault {
	t.Helper()
	v := newMemoryVault(box, newMemVaultStore())
	if err := v.Unlock(context.Background(), uid, "vault-password-fixture"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	return v
}

// TestClaimGateSkipsLockedOwner: while the run owner's vault is locked, Claim
// reports idle and never calls ClaimRun — the run stays queued, not failed
// (PRD #32 M3, directive 9; mode-aware admission runs before the claim transaction).
func TestClaimGateSkipsLockedOwner(t *testing.T) {
	box := newBox(t)
	uid := uuid.New()
	v := newMemoryVault(box, newMemVaultStore()) // uid never unlocked → locked

	fs := &fakeStore{}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid}, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle (nil payload) while the owner's vault is locked")
	}
	if fs.claimParams != nil {
		t.Fatal("ClaimRun must NOT be called when the owner's vault is locked")
	}
}

// TestClaimUnlockedOwnerOpensDEKToken: an unlocked owner's DEK-sealed token is
// opened via the vault and delivered in the payload.
func TestClaimUnlockedOwnerOpensDEKToken(t *testing.T) {
	const pat = "bot-pat-DEKGATE-abcdef1234567890"
	const token = "anthropic-oauth-DEKGATE-abcdef1234567890" //nolint:gosec // synthetic fake token for a test fixture, not a real credential

	box := newBox(t)
	uid := uuid.New()
	v := unlockedVault(t, uid, box)

	sealedPAT, _ := box.Seal([]byte(pat))
	dekTok, err := v.Seal(context.Background(), uid, store.KindAnthropicToken, []byte(token), vault.SealForUse)
	if err != nil {
		t.Fatalf("vault Seal: %v", err)
	}

	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), UserID: uid, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:           dekTok.Ciphertext,
		anthropicSealedWith: dekTok.SealedWith,
		anthropicVaultEpoch: dekTok.Epoch,
		templates:           []store.AgentTemplate{{Name: "coder", PromptBody: "code"}},
	}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid}, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil {
		t.Fatal("expected a payload for an unlocked owner")
	}
	if payload.Secrets.AnthropicOAuthToken != token {
		t.Fatal("DEK-sealed anthropic token not opened into the payload")
	}
	if payload.Secrets.ForgePAT != pat {
		t.Fatal("bot PAT (master box) not opened into the payload")
	}
}

// TestClaimLockRaceRequeuesNeverFails: when the owner locks between the claim
// gate and the token open, the just-claimed run is reset to queued (never failed)
// so it reclaims after the next unlock (PRD #32 M3, directive 10 / success
// criteria 3 & 5).
func TestClaimLockRaceRequeuesNeverFails(t *testing.T) {
	box := newBox(t)
	uid := uuid.New()
	v := unlockedVault(t, uid, box) // passes the gate...
	sealed, err := v.Seal(context.Background(), uid, store.KindAnthropicToken, []byte("lock-race-token"), vault.SealForUse)
	if err != nil {
		t.Fatalf("vault Seal: %v", err)
	}

	sealedPAT, _ := box.Seal([]byte("bot-pat-LOCKRACE-abcdef1234567890"))
	runID := uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{ID: runID, UserID: uid, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgconv.TextOrNull("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:           sealed.Ciphertext,
		anthropicSealedWith: sealed.SealedWith,
		anthropicVaultEpoch: sealed.Epoch,
		// ...then the owner locks after ClaimRun, before the token open.
		onClaimRun: func() { v.Lock(uid) },
	}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid}, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle after a lock race, got a payload")
	}
	if fs.claimRequeued == nil || fs.claimRequeued.PoolWait || fs.claimRequeued.ID != runID {
		t.Fatalf("run not requeued to queued: %v", fs.claimRequeued)
	}
	if fs.claimFailed != nil {
		t.Fatal("a lock race must NEVER fail the run (it is transient)")
	}
}
