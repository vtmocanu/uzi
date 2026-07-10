package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
)

// memVaultQ is an in-memory vault.Store for the handler tests: enough to exercise
// create-on-first-unlock and the endpoints without a database.
type memVaultQ struct {
	mu     sync.Mutex
	vaults map[uuid.UUID]store.UserVault
}

func newMemVaultQ() *memVaultQ { return &memVaultQ{vaults: map[uuid.UUID]store.UserVault{}} }

func (m *memVaultQ) GetUserVault(_ context.Context, id uuid.UUID) (store.UserVault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vaults[id]
	if !ok {
		return store.UserVault{}, pgx.ErrNoRows
	}
	return v, nil
}

func (m *memVaultQ) CreateUserVaultIfAbsent(_ context.Context, arg store.CreateUserVaultIfAbsentParams) (store.UserVault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vaults[arg.UserID]; ok {
		return store.UserVault{}, pgx.ErrNoRows
	}
	v := store.UserVault{UserID: arg.UserID, KekSalt: arg.KekSalt, WrappedDek: arg.WrappedDek}
	m.vaults[arg.UserID] = v
	return v, nil
}

func (m *memVaultQ) ListMasterSealedSecrets(context.Context, uuid.UUID) ([]store.ListMasterSealedSecretsRow, error) {
	return nil, nil
}
func (m *memVaultQ) RewrapUserSecret(context.Context, store.RewrapUserSecretParams) (int64, error) {
	return 0, nil
}

func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i) + 3
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return vault.New(box, newMemVaultQ())
}

func authedAs(method, target string, body []byte, userID uuid.UUID) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	return r.WithContext(mw.ContextWithUser(r.Context(), store.User{ID: userID}))
}

func statusUnlocked(t *testing.T, h *Handler, userID uuid.UUID) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.VaultStatus(rec, authedAs(http.MethodGet, "/api/vault/status", nil, userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("VaultStatus code = %d", rec.Code)
	}
	var out struct {
		Unlocked bool `json:"unlocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out.Unlocked
}

// TestVaultUnlockLockStatusLifecycle: unlock (via the login path) → status
// unlocked; POST /vault/lock → locked; POST /vault/unlock with the right password
// → unlocked again.
func TestVaultUnlockLockStatusLifecycle(t *testing.T) {
	v := testVault(t)
	h := &Handler{vault: v}
	uid := uuid.New()
	const pw = "correct-horse-battery"

	// Login-path unlock creates the vault.
	if err := v.Unlock(context.Background(), uid, pw); err != nil {
		t.Fatalf("seed unlock: %v", err)
	}
	if !statusUnlocked(t, h, uid) {
		t.Fatal("status should be unlocked after login unlock")
	}

	// Lock.
	rec := httptest.NewRecorder()
	h.VaultLock(rec, authedAs(http.MethodPost, "/api/vault/lock", nil, uid))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("VaultLock code = %d", rec.Code)
	}
	if statusUnlocked(t, h, uid) {
		t.Fatal("status should be locked after VaultLock")
	}

	// Unlock with the correct password.
	body, _ := json.Marshal(map[string]string{"password": pw})
	rec = httptest.NewRecorder()
	h.VaultUnlock(rec, authedAs(http.MethodPost, "/api/vault/unlock", body, uid))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("VaultUnlock code = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !statusUnlocked(t, h, uid) {
		t.Fatal("status should be unlocked after VaultUnlock")
	}
}

// TestVaultUnlockWrongPassword: a wrong password on an existing vault → 403, and
// the vault stays locked.
func TestVaultUnlockWrongPassword(t *testing.T) {
	v := testVault(t)
	h := &Handler{vault: v}
	uid := uuid.New()
	if err := v.Unlock(context.Background(), uid, "the-real-one"); err != nil {
		t.Fatalf("seed unlock: %v", err)
	}
	v.Lock(uid)

	body, _ := json.Marshal(map[string]string{"password": "wrong"})
	rec := httptest.NewRecorder()
	h.VaultUnlock(rec, authedAs(http.MethodPost, "/api/vault/unlock", body, uid))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("VaultUnlock wrong password code = %d, want 403", rec.Code)
	}
	if statusUnlocked(t, h, uid) {
		t.Fatal("wrong password must not unlock")
	}
}

// TestVaultUnlockNoVaultDoesNotCreate: unlocking a user with no vault → 403, and
// no vault is minted (the endpoint must never create — see ErrNoVault).
func TestVaultUnlockNoVaultDoesNotCreate(t *testing.T) {
	mq := newMemVaultQ()
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i) + 3
	}
	box, _ := secretbox.New(key)
	h := &Handler{vault: vault.New(box, mq)}
	uid := uuid.New()

	body, _ := json.Marshal(map[string]string{"password": "anything"})
	rec := httptest.NewRecorder()
	h.VaultUnlock(rec, authedAs(http.MethodPost, "/api/vault/unlock", body, uid))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if _, ok := mq.vaults[uid]; ok {
		t.Fatal("unlock minted a vault row — it must never create")
	}
}

// TestPutAnthropicTokenLockedReturns409: with the vault locked, saving a token
// returns 409 + code vault_locked (the SPA turns this into an unlock prompt).
func TestPutAnthropicTokenLockedReturns409(t *testing.T) {
	v := testVault(t)
	db := &fakeUpsertDB{}
	h := &Handler{q: store.New(db), vault: v}
	uid := uuid.New()
	// Vault exists (created once) but is currently locked.
	if err := v.Unlock(context.Background(), uid, "pw"); err != nil {
		t.Fatalf("seed unlock: %v", err)
	}
	v.Lock(uid)

	body, _ := json.Marshal(map[string]string{"token": "sk-ant-oat01-LOCKEDTEST-abcdef1234567890"})
	rec := httptest.NewRecorder()
	h.PutAnthropicToken(rec, authedAs(http.MethodPut, "/api/me/secrets/anthropic_token", body, uid))

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if out.Code != "vault_locked" {
		t.Fatalf("error code = %q, want vault_locked", out.Code)
	}
	if db.lastArgs != nil {
		t.Fatal("nothing should have been written to the store while locked")
	}
}

// TestPutAnthropicTokenDEKSealedWhenUnlocked: with the vault unlocked, the token
// is DEK-sealed (sealed_with='dek' handed to the store) and the plaintext never
// reaches the store args.
func TestPutAnthropicTokenDEKSealedWhenUnlocked(t *testing.T) {
	const fixture = "sk-ant-oat01-DEKSAVETEST-abcdef1234567890"
	v := testVault(t)
	db := &fakeUpsertDB{}
	h := &Handler{q: store.New(db), vault: v}
	uid := uuid.New()
	if err := v.Unlock(context.Background(), uid, "pw"); err != nil {
		t.Fatalf("seed unlock: %v", err)
	}

	body, _ := json.Marshal(map[string]string{"token": fixture})
	rec := httptest.NewRecorder()
	h.PutAnthropicToken(rec, authedAs(http.MethodPut, "/api/me/secrets/anthropic_token", body, uid))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// UpsertUserSecret args: (user_id, kind, ciphertext, sealed_with).
	if len(db.lastArgs) != 4 {
		t.Fatalf("store received %d args, want 4", len(db.lastArgs))
	}
	if sw, _ := db.lastArgs[3].(string); sw != store.SealedWithDEK {
		t.Fatalf("sealed_with = %v, want %q", db.lastArgs[3], store.SealedWithDEK)
	}
	if ct, ok := db.lastArgs[2].([]byte); ok && bytes.Contains(ct, []byte(fixture)) {
		t.Fatal("stored ciphertext contains the plaintext token")
	}
}
