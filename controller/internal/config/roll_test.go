package config

import (
	"strings"
	"testing"
)

func TestLoadRollAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, concurrency, version string
		want                       int
		invalid                    bool
	}{
		{"default", "", "", 1, false},
		{"bounded override", "3", "1.2.3+g1234567", 3, false},
		{"zero", "0", "", 0, true},
		{"negative", "-1", "", 0, true},
		{"fraction", "1.5", "", 0, true},
		{"text", "many", "", 0, true},
		{"long expected version", "1", strings.Repeat("a", 129), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "controller-fixture"))
			t.Setenv("UZI_WORKER_ROLL_CONCURRENCY", tc.concurrency)
			t.Setenv("UZI_WORKER_ROLL_TARGET_VERSION", tc.version)
			cfg, err := Load()
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid admission configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RollConcurrency != tc.want || cfg.RollTargetVersion != tc.version {
				t.Fatalf("roll configuration = %d %q", cfg.RollConcurrency, cfg.RollTargetVersion)
			}
		})
	}
}
