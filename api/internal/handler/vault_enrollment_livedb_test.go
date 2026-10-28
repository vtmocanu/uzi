package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Policy changes are local to each serial test; preserve the incoming setting.
func enrollmentPolicy(t *testing.T, pool *pgxpool.Pool, policy string) {
	t.Helper()
	ctx := context.Background()
	var previous string
	err := pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key='vault_policy'").Scan(&previous)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	existed := err == nil
	t.Cleanup(func() {
		if existed {
			mustExecT(ctx, t, pool, "INSERT INTO app_settings (key,value) VALUES ('vault_policy',$1) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", previous)
		} else {
			mustExecT(ctx, t, pool, "DELETE FROM app_settings WHERE key='vault_policy'")
		}
	})
	// TRUNCATE users CASCADE also clears app_settings through its updated_by FK.
	// Capture the old policy first, then install this case's policy after reset.
	resetUsers(t, pool)
	if policy == "" {
		mustExecT(ctx, t, pool, "DELETE FROM app_settings WHERE key='vault_policy'")
	} else {
		mustExecT(ctx, t, pool, "INSERT INTO app_settings (key,value) VALUES ('vault_policy',$1) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", policy)
	}
}

func TestVaultEnrollmentPasswordPolicyLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	for _, tc := range []struct {
		name, policy, mode string
	}{
		{"optional", "optional", store.VaultModeUnprotected},
		{"required", "required", store.VaultModeProtected},
		{"missing", "", store.VaultModeProtected},
		{"invalid", "invalid", store.VaultModeProtected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enrollmentPolicy(t, pool, tc.policy)
			q := store.New(pool)
			h := &Handler{pool: pool, q: q, cfg: oidcLiveCfg(), settings: settings.New(q, time.Minute)}
			h.vault = vault.NewPostgres(oidcLiveBox(t), pool)
			email := uuid.NewString() + "@example.com"
			body := `{"email":"` + email + `","password":"enrollment-password"}`
			rec := httptest.NewRecorder()
			h.Register(rec, httptest.NewRequest(http.MethodPost, "/api/auth/register", strings.NewReader(body)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("register status = %d", rec.Code)
			}
			user, err := q.GetUserByEmail(context.Background(), email)
			if err != nil {
				t.Fatal(err)
			}
			var epoch int64
			if tc.mode == store.VaultModeProtected {
				epoch = 1 // First password enrollment creates the wrapped DEK.
			}
			if user.VaultMode != tc.mode || user.VaultEpoch != epoch || !user.IsAdmin {
				t.Fatalf("new user mode=%s epoch=%d admin=%v", user.VaultMode, user.VaultEpoch, user.IsAdmin)
			}
			var wrapper bool
			if err := pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM user_vaults WHERE user_id=$1)", user.ID).Scan(&wrapper); err != nil {
				t.Fatal(err)
			}
			if wrapper != (tc.mode == store.VaultModeProtected) {
				t.Fatalf("wrapper present = %v for mode %s", wrapper, tc.mode)
			}

			// Later policy changes must not convert this existing account on login.
			next := "optional"
			if tc.mode == store.VaultModeUnprotected {
				next = "required"
			}
			mustExecT(context.Background(), t, pool, "INSERT INTO app_settings (key,value) VALUES ('vault_policy',$1) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", next)
			h.vault.Lock(user.ID)
			rec = httptest.NewRecorder()
			h.Login(rec, httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("login status = %d", rec.Code)
			}
			after := reloadUser(t, pool, user)
			if after.VaultMode != tc.mode || after.VaultEpoch != epoch {
				t.Fatalf("login converted user: mode=%s epoch=%d", after.VaultMode, after.VaultEpoch)
			}
			if err := pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM user_vaults WHERE user_id=$1)", user.ID).Scan(&wrapper); err != nil {
				t.Fatal(err)
			}
			if wrapper != (tc.mode == store.VaultModeProtected) {
				t.Fatalf("login wrapper present = %v for mode %s", wrapper, tc.mode)
			}
		})
	}
}

func TestVaultEnrollmentOIDCPolicyLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	for _, tc := range []struct {
		name, policy, mode               string
		groupMapping, groupMember, admin bool
	}{
		{"optional-first-admin", "optional", store.VaultModeUnprotected, false, false, true},
		{"required-group-grant", "required", store.VaultModeProtected, true, true, true},
		{"missing-group-denial", "", store.VaultModeProtected, true, false, false},
		{"invalid-first-admin", "invalid", store.VaultModeProtected, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enrollmentPolicy(t, pool, tc.policy)
			f := newSigningIDP(t)
			defer f.srv.Close()
			cfg := oidcLiveCfg()
			if tc.groupMapping {
				cfg.OIDCAdminGroups = []string{"admins"}
			}
			h := oidcLiveHandlerWith(t, pool, f, cfg)
			h.settings = settings.New(h.q, time.Minute)
			f.email, f.sub = uuid.NewString()+"@example.com", uuid.NewString()
			f.groupsSet = true
			f.groups = []any{"staff"}
			if tc.groupMember {
				f.groups = []any{"admins"}
			}
			assertLoggedInRedirect(t, callbackFor(t, h, f, true))
			user, err := h.q.GetUserByEmail(context.Background(), f.email)
			if err != nil {
				t.Fatal(err)
			}
			if user.VaultMode != tc.mode || user.VaultEpoch != 0 || user.IsAdmin != tc.admin || user.PasswordHash.Valid {
				t.Fatalf("OIDC user mode=%s epoch=%d admin=%v password=%v", user.VaultMode, user.VaultEpoch, user.IsAdmin, user.PasswordHash.Valid)
			}
			next := "optional"
			if tc.mode == store.VaultModeUnprotected {
				next = "required"
			}
			mustExecT(context.Background(), t, pool, "INSERT INTO app_settings (key,value) VALUES ('vault_policy',$1) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value", next)
			assertLoggedInRedirect(t, callbackFor(t, h, f, true))
			after := reloadUser(t, pool, user)
			if after.VaultMode != tc.mode || after.VaultEpoch != 0 {
				t.Fatalf("OIDC login converted user: mode=%s epoch=%d", after.VaultMode, after.VaultEpoch)
			}
			var wrappers int
			if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM user_vaults WHERE user_id=$1", user.ID).Scan(&wrappers); err != nil {
				t.Fatal(err)
			}
			if wrappers != 0 {
				t.Fatalf("OIDC created %d password wrappers", wrappers)
			}
		})
	}
}
