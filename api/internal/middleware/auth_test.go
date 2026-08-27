package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
)

func TestShouldRefresh(t *testing.T) {
	const ttl = 168 * time.Hour
	now := time.Now()

	tests := []struct {
		name      string
		expiresAt *jwt.NumericDate
		want      bool
	}{
		{"missing expiry refreshes", nil, true},
		{"fresh token does not refresh", jwt.NewNumericDate(now.Add(ttl)), false},
		{"just over half elapsed refreshes", jwt.NewNumericDate(now.Add(ttl/2 - time.Minute)), true},
		{"just under half elapsed does not refresh", jwt.NewNumericDate(now.Add(ttl/2 + time.Minute)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &auth.Claims{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: tt.expiresAt}}
			if got := shouldRefresh(claims, ttl); got != tt.want {
				t.Errorf("shouldRefresh = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRollingRefreshPassiveExemption drives rollingRefresh directly (no DB) and
// asserts that a passive request (X-Uzi-Passive: 1) is exempt from the rolling
// refresh, so an idle backgrounded favicon poll (#331) cannot slide the session
// forward, while a normal request past half-TTL still refreshes.
func TestRollingRefreshPassiveExemption(t *testing.T) {
	const ttl = 168 * time.Hour
	cfg := config.Config{
		JWTSecret:    []byte("test-secret"),
		AuthTokenTTL: ttl,
		CookieSecure: false,
	}
	now := time.Now()

	tests := []struct {
		name        string
		passive     bool
		expiresAt   *jwt.NumericDate
		wantRefresh bool
	}{
		{"non-passive past half-TTL refreshes", false, jwt.NewNumericDate(now.Add(ttl/2 - time.Minute)), true},
		{"passive past half-TTL does not refresh", true, jwt.NewNumericDate(now.Add(ttl/2 - time.Minute)), false},
		{"non-passive fresh token does not refresh", false, jwt.NewNumericDate(now.Add(ttl)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &auth.Claims{
				UserID:           uuid.NewString(),
				RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: tt.expiresAt},
			}
			r := httptest.NewRequest(http.MethodGet, "/runs", nil)
			if tt.passive {
				r.Header.Set(passiveHeader, "1")
			}
			rec := httptest.NewRecorder()

			rollingRefresh(rec, r, claims, 0, cfg)

			gotRefresh := false
			for _, c := range rec.Result().Cookies() {
				if c.Name == auth.AuthCookieName {
					gotRefresh = true
					break
				}
			}
			if gotRefresh != tt.wantRefresh {
				t.Errorf("auth cookie set = %v, want %v", gotRefresh, tt.wantRefresh)
			}
		})
	}
}
