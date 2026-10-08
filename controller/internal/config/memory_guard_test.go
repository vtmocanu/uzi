package config

import (
	"strconv"
	"strings"
	"testing"
)

func TestLoadMemoryGuard(t *testing.T) {
	settings := []struct {
		name    string
		ceiling int64
	}{
		{"RESERVE_BYTES", 9007199254740991},
		{"SAMPLE_MS", 2147483647},
		{"HYSTERESIS_BYTES", 9007199254740991},
		{"REARM_MS", 2147483647},
		{"RESPONSE_BUDGET_MS", 2147483647},
		{"MAX_INTERVENTIONS", 9999},
	}
	setup := func(t *testing.T, flag string) {
		t.Helper()
		setWorkerEnv(t)
		t.Setenv("UZI_API_URL", "https://uzi.example.com")
		t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "controller-token"))
		t.Setenv("UZI_WORKER_MEMORY_GUARD_ENABLED", flag)
		for _, s := range settings {
			t.Setenv("UZI_WORKER_MEMORY_GUARD_"+s.name, "7")
		}
	}
	for _, flag := range []string{"", "0", "false", "no", "off", " FALSE "} {
		t.Run("disabled/"+flag, func(t *testing.T) {
			setup(t, flag)
			for _, s := range settings {
				t.Setenv("UZI_WORKER_MEMORY_GUARD_"+s.name, "garbage")
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerMemoryGuardEnabled || cfg.WorkerMemoryGuardReserveBytes != 0 {
				t.Fatal("disabled config consumed thresholds")
			}
		})
	}
	t.Run("default", func(t *testing.T) {
		setup(t, "")
		for _, s := range settings {
			t.Setenv("UZI_WORKER_MEMORY_GUARD_"+s.name, "")
		}
		cfg, err := Load()
		if err != nil || cfg.WorkerMemoryGuardEnabled {
			t.Fatalf("default: %+v, %v", cfg, err)
		}
	})
	for _, flag := range []string{"1", "true", "yes", "on", " ON "} {
		t.Run("enabled/"+flag, func(t *testing.T) {
			setup(t, flag)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.WorkerMemoryGuardEnabled {
				t.Fatal("not enabled")
			}
			got := []int64{cfg.WorkerMemoryGuardReserveBytes, cfg.WorkerMemoryGuardSampleMs, cfg.WorkerMemoryGuardHysteresisBytes, cfg.WorkerMemoryGuardRearmMs, cfg.WorkerMemoryGuardResponseBudgetMs, cfg.WorkerMemoryGuardMaxInterventions}
			for i, value := range got {
				if value != 7 {
					t.Errorf("%s = %d, want 7", settings[i].name, value)
				}
			}
		})
	}
	for _, flag := range []string{"garbage", "2", "-1"} {
		t.Run("flag/"+flag, func(t *testing.T) {
			setup(t, flag)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_MEMORY_GUARD_ENABLED") {
				t.Fatalf("invalid flag accepted: %v", err)
			}
		})
	}
	for _, s := range settings {
		t.Run(s.name, func(t *testing.T) {
			for _, raw := range []string{"", "0", "-1", "+1", "1.5", "1e2", "garbage", strconv.FormatInt(s.ceiling+1, 10), "9223372036854775808"} {
				t.Run("invalid/"+raw, func(t *testing.T) {
					setup(t, "true")
					name := "UZI_WORKER_MEMORY_GUARD_" + s.name
					t.Setenv(name, raw)
					if _, err := Load(); err == nil || !strings.Contains(err.Error(), name) {
						t.Fatalf("%s=%q: %v", name, raw, err)
					}
				})
			}
			t.Run("ceiling", func(t *testing.T) {
				setup(t, "true")
				t.Setenv("UZI_WORKER_MEMORY_GUARD_"+s.name, strconv.FormatInt(s.ceiling, 10))
				if _, err := Load(); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("decimal", func(t *testing.T) {
				setup(t, "true")
				t.Setenv("UZI_WORKER_MEMORY_GUARD_"+s.name, " 0007 ")
				if _, err := Load(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
