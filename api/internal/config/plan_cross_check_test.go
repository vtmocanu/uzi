package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadPlanCrossCheckTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("RUN_TIMEOUT", "3h")
	t.Setenv("PLAN_CROSS_CHECK_TIMEOUT", "")
	if err := os.Unsetenv("PLAN_CROSS_CHECK_TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.PlanCrossCheckTimeout != 30*time.Minute {
		t.Fatalf("default: timeout=%v err=%v", cfg.PlanCrossCheckTimeout, err)
	}
	for _, value := range []string{"", "bad", "0", "-1s", "2h1ns"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PLAN_CROSS_CHECK_TIMEOUT", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PLAN_CROSS_CHECK_TIMEOUT") {
				t.Fatalf("accepted %q: %v", value, err)
			}
		})
	}
	t.Setenv("PLAN_CROSS_CHECK_TIMEOUT", "2h")
	cfg, err = Load()
	if err != nil || cfg.PlanCrossCheckTimeout != 2*time.Hour {
		t.Fatalf("upper bound: %v", err)
	}
	t.Setenv("RUN_TIMEOUT", "2h")
	if _, err := Load(); err == nil {
		t.Fatal("accepted equal run deadline")
	}
}

// An existing install with a short RUN_TIMEOUT and no checker timeout must still
// boot after upgrade; only an explicit out-of-range value is refused.
func TestLoadPlanCrossCheckTimeoutShortRunTimeoutDefault(t *testing.T) {
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
	cfg, err := Load()
	if err != nil || cfg.PlanCrossCheckTimeout != 10*time.Minute {
		t.Fatalf("short run default: timeout=%v err=%v", cfg.PlanCrossCheckTimeout, err)
	}
	t.Setenv("PLAN_CROSS_CHECK_TIMEOUT", "30m")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PLAN_CROSS_CHECK_TIMEOUT") {
		t.Fatalf("accepted explicit timeout at or above RUN_TIMEOUT: %v", err)
	}
}
