package workersvc

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// memVaultStore is an in-memory vault.Store so the claim gate + lock-race paths
// can run against a real *vault.Vault without a database.
type memVaultStore struct {
	mu     sync.Mutex
	vaults map[uuid.UUID]store.UserVault
}

func newMemVaultStore() *memVaultStore {
	return &memVaultStore{vaults: map[uuid.UUID]store.UserVault{}}
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

func (m *memVaultStore) ListMasterSealedSecrets(context.Context, uuid.UUID) ([]store.ListMasterSealedSecretsRow, error) {
	return nil, nil
}
func (m *memVaultStore) RewrapUserSecret(context.Context, store.RewrapUserSecretParams) (int64, error) {
	return 0, nil
}
func (m *memVaultStore) ClearVaultLockNotice(context.Context, uuid.UUID) error { return nil }

// unlockedVault returns a vault with uid unlocked (its DEK cached).
func unlockedVault(t *testing.T, uid uuid.UUID, box *secretbox.Box) *vault.Vault {
	t.Helper()
	v := vault.New(box, newMemVaultStore())
	if err := v.Unlock(context.Background(), uid, "vault-pw"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	return v
}

// TestClaimGateSkipsLockedOwner: while the run owner's vault is locked, Claim
// reports idle and never calls ClaimRun — the run stays queued, not failed
// (PRD #32 M3, directive 9: a pure in-memory gate, no SQL filter).
func TestClaimGateSkipsLockedOwner(t *testing.T) {
	box := newBox(t)
	uid := uuid.New()
	v := vault.New(box, newMemVaultStore()) // uid never unlocked → locked

	fs := &fakeStore{}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
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
	dekTok, err := v.Seal(uid, store.KindAnthropicToken, []byte(token))
	if err != nil {
		t.Fatalf("vault Seal: %v", err)
	}

	fs := &fakeStore{
		claimRun: store.Run{ID: uuid.New(), UserID: uid, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:           dekTok,
		anthropicSealedWith: store.SealedWithDEK,
		templates:           []store.AgentTemplate{{Name: "coder", PromptBody: "code"}},
	}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
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

	sealedPAT, _ := box.Seal([]byte("bot-pat-LOCKRACE-abcdef1234567890"))
	runID := uuid.New()
	fs := &fakeStore{
		claimRun: store.Run{ID: runID, UserID: uid, Status: "claimed"},
		claimCtx: store.GetRunClaimContextRow{
			RepoWebUrl: "https://gitlab.example.com/grp/proj", RepoPath: "grp/proj",
			DefaultBranch: pgText("main"), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com",
			BotUsername: "uzi-bot", TokenCiphertext: sealedPAT,
		},
		anthropic:           []byte("locked-so-never-decrypted"),
		anthropicSealedWith: store.SealedWithDEK,
		// ...then the owner locks after ClaimRun, before the token open.
		onClaimRun: func() { v.Lock(uid) },
	}
	svc := New(fs, box, testParams())
	svc.SetVault(v)

	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload != nil {
		t.Fatal("expected idle after a lock race, got a payload")
	}
	if fs.requeuedRun == nil || *fs.requeuedRun != runID {
		t.Fatalf("run not requeued to queued: %v", fs.requeuedRun)
	}
	if fs.markedFailed != nil {
		t.Fatal("a lock race must NEVER fail the run (it is transient)")
	}
}
