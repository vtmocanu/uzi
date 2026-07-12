package config

import (
	"encoding/base64"
	"reflect"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
)

// oidcBaseEnv sets a syntactically valid non-OIDC environment so each subtest can
// layer the OIDC vars on top. FrontendOrigin drives the derived redirect URL.
func oidcBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	t.Setenv("FRONTEND_ORIGIN", "https://uzi.example.com")
}

// TestOIDCDisabledByDefault: with no UZI_OIDC_* set, OIDC is off, password login
// stays on, and nothing OIDC-derived is populated.
func TestOIDCDisabledByDefault(t *testing.T) {
	oidcBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.OIDCEnabled() {
		t.Error("OIDCEnabled() = true with no OIDC env set")
	}
	if !cfg.PasswordLoginEnabled {
		t.Error("PasswordLoginEnabled defaulted to false")
	}
	if cfg.OIDCRedirectURL != "" {
		t.Errorf("OIDCRedirectURL = %q, want empty when disabled", cfg.OIDCRedirectURL)
	}
}

// TestOIDCFullConfigLoads: a complete config enables the feature, derives the
// redirect URL from FRONTEND_ORIGIN, and defaults scopes/provider name.
func TestOIDCFullConfigLoads(t *testing.T) {
	oidcBaseEnv(t)
	t.Setenv("UZI_OIDC_ISSUER_URL", "https://idp.example.com/realms/uzi")
	t.Setenv("UZI_OIDC_CLIENT_ID", "uzi-client")
	t.Setenv("UZI_OIDC_CLIENT_SECRET", "s3cr3t")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.OIDCEnabled() {
		t.Fatal("OIDCEnabled() = false with a full config")
	}
	if cfg.OIDCIssuerURL != "https://idp.example.com/realms/uzi" {
		t.Errorf("OIDCIssuerURL = %q", cfg.OIDCIssuerURL)
	}
	if cfg.OIDCClientID != "uzi-client" || cfg.OIDCClientSecret != "s3cr3t" {
		t.Errorf("client id/secret not carried: %q / %q", cfg.OIDCClientID, cfg.OIDCClientSecret)
	}
	if want := "https://uzi.example.com/api/auth/oidc/callback"; cfg.OIDCRedirectURL != want {
		t.Errorf("OIDCRedirectURL = %q, want %q", cfg.OIDCRedirectURL, want)
	}
	if want := []string{"openid", "profile", "email"}; !reflect.DeepEqual(cfg.OIDCScopes, want) {
		t.Errorf("OIDCScopes = %v, want %v", cfg.OIDCScopes, want)
	}
	if cfg.OIDCProviderName != "SSO" {
		t.Errorf("OIDCProviderName = %q, want SSO", cfg.OIDCProviderName)
	}
	if cfg.OIDCHTTPTimeout != 15*time.Second {
		t.Errorf("OIDCHTTPTimeout = %v, want 15s", cfg.OIDCHTTPTimeout)
	}
}

// TestOIDCAllOrNothing: any subset of issuer/client-id/client-secret set without
// the others refuses to start (Decision 8).
func TestOIDCAllOrNothing(t *testing.T) {
	partials := map[string]map[string]string{
		"issuer only":        {"UZI_OIDC_ISSUER_URL": "https://idp.example.com"},
		"client id only":     {"UZI_OIDC_CLIENT_ID": "c"},
		"client secret only": {"UZI_OIDC_CLIENT_SECRET": "s"},
		"issuer + id":        {"UZI_OIDC_ISSUER_URL": "https://idp.example.com", "UZI_OIDC_CLIENT_ID": "c"},
		"issuer + secret":    {"UZI_OIDC_ISSUER_URL": "https://idp.example.com", "UZI_OIDC_CLIENT_SECRET": "s"},
		"id + secret":        {"UZI_OIDC_CLIENT_ID": "c", "UZI_OIDC_CLIENT_SECRET": "s"},
	}
	for name, env := range partials {
		t.Run(name, func(t *testing.T) {
			oidcBaseEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Errorf("Load() = nil error for partial OIDC config %v, want refuse-to-start", env)
			}
		})
	}
}

// TestOIDCIssuerScheme: https accepted; non-loopback http rejected; loopback http
// accepted for local dev.
func TestOIDCIssuerScheme(t *testing.T) {
	cases := map[string]struct {
		issuer  string
		wantErr bool
	}{
		"https ok":              {"https://idp.example.com", false},
		"http non-loopback bad": {"http://idp.example.com", true},
		"http localhost ok":     {"http://localhost:8080/realms/uzi", false},
		"http localhost bare":   {"http://localhost", false}, // no port
		"http 127.0.0.1 ok":     {"http://127.0.0.1:8080", false},
		"http ::1 ok":           {"http://[::1]:8080", false},
		"http ::1 bare":         {"http://[::1]", false}, // no port
		"ftp bad":               {"ftp://idp.example.com", true},
		"no host bad":           {"https://", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			oidcBaseEnv(t)
			t.Setenv("UZI_OIDC_ISSUER_URL", tc.issuer)
			t.Setenv("UZI_OIDC_CLIENT_ID", "c")
			t.Setenv("UZI_OIDC_CLIENT_SECRET", "s")
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Errorf("Load() = nil error for issuer %q, want rejection", tc.issuer)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Load() rejected valid issuer %q: %v", tc.issuer, err)
			}
		})
	}
}

// TestPasswordLoginLockoutGuard: disabling password login with OIDC unconfigured
// is a total lockout and must refuse to start; with OIDC configured it is allowed.
func TestPasswordLoginLockoutGuard(t *testing.T) {
	t.Run("false + no OIDC refuses to boot", func(t *testing.T) {
		oidcBaseEnv(t)
		t.Setenv("UZI_PASSWORD_LOGIN_ENABLED", "false")
		if _, err := Load(); err == nil {
			t.Error("Load() = nil error for password login off + OIDC unset, want lockout guard")
		}
	})
	t.Run("false + OIDC configured is allowed", func(t *testing.T) {
		oidcBaseEnv(t)
		t.Setenv("UZI_PASSWORD_LOGIN_ENABLED", "false")
		t.Setenv("UZI_OIDC_ISSUER_URL", "https://idp.example.com")
		t.Setenv("UZI_OIDC_CLIENT_ID", "c")
		t.Setenv("UZI_OIDC_CLIENT_SECRET", "s")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() rejected password-off + OIDC-on: %v", err)
		}
		if cfg.PasswordLoginEnabled {
			t.Error("PasswordLoginEnabled = true, want false")
		}
	})
	t.Run("malformed refuses to boot", func(t *testing.T) {
		oidcBaseEnv(t)
		t.Setenv("UZI_PASSWORD_LOGIN_ENABLED", "notabool")
		if _, err := Load(); err == nil {
			t.Error("Load() = nil error for malformed UZI_PASSWORD_LOGIN_ENABLED")
		}
	})
}

// TestOIDCScopesForceOpenid: a custom scope list without "openid" gets it
// prepended; a custom list keeps its order and de-dupes.
func TestOIDCScopesForceOpenid(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want []string
	}{
		"custom without openid": {"profile email groups", []string{"openid", "profile", "email", "groups"}},
		"custom with openid":     {"openid email", []string{"openid", "email"}},
		"dedup":                  {"openid openid profile", []string{"openid", "profile"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			oidcBaseEnv(t)
			t.Setenv("UZI_OIDC_ISSUER_URL", "https://idp.example.com")
			t.Setenv("UZI_OIDC_CLIENT_ID", "c")
			t.Setenv("UZI_OIDC_CLIENT_SECRET", "s")
			t.Setenv("UZI_OIDC_SCOPES", tc.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if !reflect.DeepEqual(cfg.OIDCScopes, tc.want) {
				t.Errorf("OIDCScopes = %v, want %v", cfg.OIDCScopes, tc.want)
			}
		})
	}
}
