package config

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/kube"
	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestLoadCrossCheckSlots(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", 1},
		{"empty", "", 1},
		{"whitespace", "  ", 1},
		{"zero", "0", 0},
		{"default", "1", 1},
		{"maximum", "16", 16},
		{"trimmed", " 2 ", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_MAX_CONCURRENT_RUNS", "3")
			t.Setenv("UZI_WORKER_CROSS_CHECK_SLOTS", tc.raw)
			if tc.name == "unset" {
				if err := os.Unsetenv("UZI_WORKER_CROSS_CHECK_SLOTS"); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkerCrossCheckSlots != tc.want {
				t.Errorf("cross-check slots = %d, want %d", cfg.WorkerCrossCheckSlots, tc.want)
			}
			resolver, err := preset.NewResolver(cfg.WorkerImageRepo, cfg.WorkerImageTag)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := resolver.Resolve("base", "m")
			if err != nil {
				t.Fatal(err)
			}
			dep := kube.RenderDeployment(kube.RenderConfig{
				CrossCheckSlots:   cfg.WorkerCrossCheckSlots,
				MaxConcurrentRuns: cfg.WorkerMaxConcurrentRuns,
			}, protocol.DesiredWorker{ID: "abc"}, spec)
			found := false
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "WORKER_CROSS_CHECK_SLOTS" {
					found = true
					if e.Value != fmt.Sprint(tc.want) {
						t.Errorf("loaded slots rendered %q, want %d", e.Value, tc.want)
					}
				}
			}
			if !found {
				t.Error("loaded slots missing from pod env")
			}
			if cfg.WorkerMaxConcurrentRuns != 3 {
				t.Errorf("run cap = %d, want unchanged 3", cfg.WorkerMaxConcurrentRuns)
			}
		})
	}
	for _, raw := range []string{"-1", "17", "100000", "abc", "1.5", "1x", "999999999999999999999999999999"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			setWorkerEnv(t)
			t.Setenv("UZI_API_URL", "https://uzi.example.com")
			t.Setenv("UZI_CONTROLLER_TOKEN_FILE", writeToken(t, "tok"))
			t.Setenv("UZI_WORKER_CROSS_CHECK_SLOTS", raw)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "UZI_WORKER_CROSS_CHECK_SLOTS") {
				t.Fatalf("Load error = %v, want error naming the setting", err)
			}
		})
	}
}
