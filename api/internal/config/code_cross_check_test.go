package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadCodeCrossCheckTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("RUN_TIMEOUT", "20m")
	t.Setenv("PLAN_CROSS_CHECK_TIMEOUT", "")
	if err := os.Unsetenv("PLAN_CROSS_CHECK_TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODE_CROSS_CHECK_TIMEOUT", "")
	if err := os.Unsetenv("CODE_CROSS_CHECK_TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.CodeCrossCheckTimeout != 30*time.Minute {
		t.Fatalf("independent default: %v %v", cfg.CodeCrossCheckTimeout, err)
	}
	for _, value := range []string{"", "bad", "0", "-1s", "2h1ns"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CODE_CROSS_CHECK_TIMEOUT", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CODE_CROSS_CHECK_TIMEOUT") {
				t.Fatalf("accepted %q: %v", value, err)
			}
		})
	}
	t.Setenv("CODE_CROSS_CHECK_TIMEOUT", "2h")
	cfg, err = Load()
	if err != nil || cfg.CodeCrossCheckTimeout != 2*time.Hour {
		t.Fatalf("upper bound independent of run wall: %v", err)
	}
}
