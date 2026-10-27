package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/auth"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func toggleRequest(h *Handler, id uuid.UUID, action, password string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"password": password})
	rec := httptest.NewRecorder()
	req := authedAs(http.MethodPost, "/api/vault/"+action, body, id)
	switch action {
	case "enable":
		h.VaultEnable(rec, req)
	case "disable":
		h.VaultDisable(rec, req)
	default:
		panic("unknown toggle action")
	}
	return rec
}

func TestVaultTogglePasswordLifecycle(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	const password = "toggle-login-password" //nolint:gosec // G101: synthetic local vault lifecycle fixture, not a provider credential.
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	m := newMemVaultQ()
	m.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, PasswordHash: pgtype.Text{String: hash, Valid: true}}
	h := &Handler{vault: newHandlerMemoryVault(newHandlerTestBox(t), m)}
	poker := &fakeCodexPoker{}
	anthropicPoker := &recordingUsagePoker{}
	h.SetCodexUsagePoker(poker)
	h.SetUsagePoker(anthropicPoker)

	rec := toggleRequest(h, id, "enable", "incorrect-password")
	if rec.Code != http.StatusForbidden || len(m.vaults) != 0 || m.states[id].VaultEpoch != 0 || m.states[id].VaultMode != store.VaultModeUnprotected || poker.count(id) != 0 {
		t.Fatalf("wrong password: status=%d state=%+v pokes=%d", rec.Code, m.states[id], poker.count(id))
	}
	rec = toggleRequest(h, id, "enable", password)
	var response vaultToggleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.Mode != store.VaultModeProtected || !strings.Contains(response.RotationAdvice, "backups") || !strings.Contains(response.RotationAdvice, "Rotate") {
		t.Fatalf("enable: status=%d response=%+v", rec.Code, response)
	}
	if strings.Contains(rec.Body.String(), password) || strings.Contains(rec.Body.String(), hash) {
		t.Fatal("enable leaked authentication material")
	}
	if rec := toggleRequest(h, id, "enable", password); rec.Code != http.StatusConflict {
		t.Fatalf("repeat enable=%d", rec.Code)
	}
	h.vault.Lock(id)
	if rec := toggleRequest(h, id, "disable", ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "unlock") {
		t.Fatalf("locked disable=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := vaultRecoveryUnlock(h, ctx, id, password); rec.Code != http.StatusNoContent {
		t.Fatalf("unlock=%d", rec.Code)
	}
	if rec := toggleRequest(h, id, "disable", ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "rotation_advice") {
		t.Fatalf("disable=%d body=%s", rec.Code, rec.Body.String())
	}
	status := h.vaultStatus(ctx, store.User{ID: id})
	if status != (vault.Status{Mode: store.VaultModeUnprotected, Policy: "optional"}) || h.vault.RequiresUnlock(ctx, id) || h.vault.Unlocked(id) || m.states[id].VaultEpoch != 2 || poker.count(id) != 3 {
		t.Fatalf("disabled status=%+v epoch=%d pokes=%d", status, m.states[id].VaultEpoch, poker.count(id))
	}
	users, secrets := anthropicPoker.snapshot()
	if len(users) != 3 || len(secrets) != 0 {
		t.Fatalf("Anthropic usage pokes=%d secret pokes=%d want=3/0", len(users), len(secrets))
	}
	for _, owner := range users {
		if owner != id {
			t.Fatal("Anthropic usage wake targeted a different owner")
		}
	}
	// Unlock while Off is harmless and cannot implicitly reenroll the account.
	if rec := vaultRecoveryUnlock(h, ctx, id, password); rec.Code != http.StatusNoContent || len(m.vaults) != 0 || m.states[id].VaultEpoch != 2 {
		t.Fatalf("off unlock=%d", rec.Code)
	}
}

func TestVaultToggleOIDCCreateOnlyAndLength(t *testing.T) {
	for _, password := range []string{strings.Repeat("x", minPasswordLen-1), strings.Repeat("x", maxPasswordLen+1)} {
		h := &Handler{vault: testVault(t)}
		id := uuid.New()
		if rec := toggleRequest(h, id, "enable", password); rec.Code != http.StatusBadRequest {
			t.Fatalf("length %d: status=%d", len(password), rec.Code)
		}
		if status := h.vaultStatus(context.Background(), store.User{ID: id}); status.Exists || status.Unlocked {
			t.Fatalf("invalid passphrase created vault: %+v", status)
		}
	}
	h := &Handler{vault: testVault(t)}
	id := uuid.New()
	if rec := toggleRequest(h, id, "enable", "oidc-toggle-passphrase"); rec.Code != http.StatusOK {
		t.Fatalf("OIDC enable=%d", rec.Code)
	}
	h.vault.Lock(id)
	if rec := postPassphrase(h, id, "replacement-passphrase"); rec.Code != http.StatusConflict {
		t.Fatalf("passphrase cannot replace enabled wrapper: %d", rec.Code)
	}
	if rec := vaultRecoveryUnlock(h, context.Background(), id, "oidc-toggle-passphrase"); rec.Code != http.StatusNoContent {
		t.Fatalf("original passphrase no longer unlocks: %d", rec.Code)
	}
}

func TestVaultStatusFallbackTruth(t *testing.T) {
	for _, mode := range []string{store.VaultModeUnprotected, store.VaultModeProtected, "unknown"} {
		for _, broken := range []bool{false, true} {
			h := newSettingsHandler(store.AppSetting{Key: settings.KeyVaultPolicy, Value: "optional"})
			if broken {
				m := newMemVaultQ()
				h.vault = vault.New(newHandlerTestBox(t), &rateLimitFailedVaultState{memVaultQ: m}, m.transaction)
			}
			user := store.User{ID: uuid.New(), VaultMode: mode}
			wantMode := mode
			if mode == "unknown" {
				wantMode = store.VaultModeProtected
			}
			want := vault.Status{Mode: wantMode, Policy: "optional", Exists: wantMode == store.VaultModeProtected}
			if got := h.sessionPayload(context.Background(), user)["vault"]; got != want {
				t.Fatalf("mode=%s broken=%v session=%+v want=%+v", mode, broken, got, want)
			}
			rec := httptest.NewRecorder()
			req := authedAs(http.MethodGet, "/api/vault/status", nil, user.ID)
			req = req.WithContext(mw.ContextWithUser(req.Context(), user))
			h.VaultStatus(rec, req)
			var got vault.Status
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got != want {
				t.Fatalf("status=%d got=%+v err=%v", rec.Code, got, err)
			}
		}
	}
	h := &Handler{}
	if got := h.vaultStatus(context.Background(), store.User{}); got != (vault.Status{Mode: store.VaultModeProtected, Policy: "required", Exists: true}) {
		t.Fatalf("unknown fallback=%+v", got)
	}
}

func TestVaultToggleErrorResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		code     int
		contains string
	}{
		{"required", vault.ErrRequiredPolicy, http.StatusForbidden, "requires"},
		{"locked", vault.ErrLocked, http.StatusConflict, "unlock"},
		{"not-set-up", vault.ErrNoVault, http.StatusConflict, "enable"},
		{"budget", &vault.ConversionLimitError{Rows: 10001, Bytes: 123, MaxRowBytes: 12}, http.StatusRequestEntityTooLarge, "10001"},
		{"uncertain", &vault.UncertainOutcomeError{Cause: errors.New("private-store-detail")}, http.StatusServiceUnavailable, "refresh vault status"},
		{"generic", errors.New("private-store-detail"), http.StatusInternalServerError, "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemVaultQ()
			h := &Handler{vault: vault.New(newHandlerTestBox(t), m, func(context.Context, func(context.Context, vault.Store) error) error { return tc.err })}
			rec := toggleRequest(h, uuid.New(), "disable", "")
			if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.contains) || strings.Contains(rec.Body.String(), "private-store-detail") {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.name == "uncertain" && strings.Contains(rec.Body.String(), "not changed") {
				t.Fatal("uncertain response claimed rollback")
			}
			if tc.name == "budget" {
				for _, text := range []string{"123", "10000", "67108864", "1048576", "not changed", "management UI"} {
					if !strings.Contains(rec.Body.String(), text) {
						t.Errorf("budget response missing %q", text)
					}
				}
			}
		})
	}
}
