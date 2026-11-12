package config

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

// TestLoadAdminRunDebug pins UZI_ADMIN_RUN_DEBUG: default off, "true" enables, and an
// unrecognised value refuses to start.
func TestLoadAdminRunDebug(t *testing.T) {
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
	if cfg.AdminRunDebug {
		t.Fatal("AdminRunDebug default = true, want off")
	}

	t.Setenv("UZI_ADMIN_RUN_DEBUG", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with true: %v", err)
	}
	if !cfg.AdminRunDebug {
		t.Fatal("AdminRunDebug with UZI_ADMIN_RUN_DEBUG=true = false, want on")
	}

	t.Setenv("UZI_ADMIN_RUN_DEBUG", "garbage")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_ADMIN_RUN_DEBUG") {
		t.Fatalf("garbage: Load() err = %v, want a UZI_ADMIN_RUN_DEBUG error", err)
	}
}
