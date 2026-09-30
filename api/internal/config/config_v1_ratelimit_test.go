package config

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// TestLoadV1RateLimit pins the PRD #1908 D-B budget of the /api/v1 subtree: 120 per minute per
// user by default, overridable by V1_RATE_LIMIT_MAX / V1_RATE_LIMIT_WINDOW, and independent of
// the authLimiter budget (RATE_LIMIT_MAX / RATE_LIMIT_WINDOW), which keeps guarding the create.
func TestLoadV1RateLimit(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.V1RateLimitMax != 120 || cfg.V1RateLimitWindow != time.Minute {
		t.Errorf("defaults = %d per %s, want 120 per 1m0s", cfg.V1RateLimitMax, cfg.V1RateLimitWindow)
	}
	if cfg.RateLimitMax != 10 {
		t.Errorf("RateLimitMax = %d, want the unchanged default 10 (the v1 budget must not move it)", cfg.RateLimitMax)
	}

	t.Setenv("V1_RATE_LIMIT_MAX", "300")
	t.Setenv("V1_RATE_LIMIT_WINDOW", "30s")
	t.Setenv("RATE_LIMIT_MAX", "7")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with overrides: %v", err)
	}
	if cfg.V1RateLimitMax != 300 || cfg.V1RateLimitWindow != 30*time.Second {
		t.Errorf("overrides = %d per %s, want 300 per 30s", cfg.V1RateLimitMax, cfg.V1RateLimitWindow)
	}
	if cfg.RateLimitMax != 7 {
		t.Errorf("RateLimitMax = %d, want 7 (independent of V1_RATE_LIMIT_MAX)", cfg.RateLimitMax)
	}

	// A non-positive or malformed value falls back to the default like every parseInt setting.
	t.Setenv("V1_RATE_LIMIT_MAX", "0")
	t.Setenv("V1_RATE_LIMIT_WINDOW", "soon")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with bad values: %v", err)
	}
	if cfg.V1RateLimitMax != 120 || cfg.V1RateLimitWindow != time.Minute {
		t.Errorf("bad values = %d per %s, want the defaults 120 per 1m0s", cfg.V1RateLimitMax, cfg.V1RateLimitWindow)
	}
}
