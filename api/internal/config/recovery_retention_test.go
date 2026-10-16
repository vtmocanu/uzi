package config

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/secretbox"
)

func TestLoadRecoveryReadyRetentionDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	varied := make([]byte, secretbox.KeySize)
	for i := range varied {
		varied[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(varied))
	t.Setenv("UZI_RECOVERY_READY_RETENTION", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RecoveryReadyRetention != 168*time.Hour {
		t.Fatalf("default retention=%s, want 168h", cfg.RecoveryReadyRetention)
	}
}
