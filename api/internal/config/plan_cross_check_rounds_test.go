package config

import (
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestLoadPlanCrossCheckMaxRevisions(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://uzi:pw@db:5432/uzi?sslmode=disable")
	t.Setenv("JWT_SECRET", "unit-test-jwt-signing-key-not-a-real-secret")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t.Setenv("UZI_SECRET_KEY", base64.StdEncoding.EncodeToString(key))
	t.Setenv("PLAN_CROSS_CHECK_MAX_REVISIONS", "")
	if err := os.Unsetenv("PLAN_CROSS_CHECK_MAX_REVISIONS"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.PlanCrossCheckMaxRevisions != DefaultPlanCrossCheckMaxRevisions {
		t.Fatalf("unset: %d %v", cfg.PlanCrossCheckMaxRevisions, err)
	}
	for n := int32(0); n <= MaxPlanCrossCheckMaxRevisions; n++ {
		t.Run(strconv.Itoa(int(n)), func(t *testing.T) {
			t.Setenv("PLAN_CROSS_CHECK_MAX_REVISIONS", strconv.Itoa(int(n)))
			cfg, err := Load()
			if err != nil || cfg.PlanCrossCheckMaxRevisions != n {
				t.Fatalf("explicit budget: %d %v", cfg.PlanCrossCheckMaxRevisions, err)
			}
		})
	}
	for _, raw := range []string{"", "bad", "-1", "5", "1.5", " 2", "2 ", "999999999999999"} {
		t.Run("invalid_"+raw, func(t *testing.T) {
			t.Setenv("PLAN_CROSS_CHECK_MAX_REVISIONS", raw)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PLAN_CROSS_CHECK_MAX_REVISIONS") {
				t.Fatalf("accepted %q: %v", raw, err)
			}
		})
	}
}
