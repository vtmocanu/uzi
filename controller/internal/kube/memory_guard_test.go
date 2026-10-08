package kube

import (
	"strings"
	"testing"
)

func TestMemoryGuardRenderAndSpecHash(t *testing.T) {
	w, spec := desired("abc"), testSpec(t, "base", "m")
	envOf := func(cfg RenderConfig) map[string]string {
		t.Helper()
		dep := RenderDeployment(cfg, w, spec)
		worker := containerByName(t, dep.Spec.Template.Spec.Containers, workerContainerName)
		env := map[string]string{}
		for _, e := range worker.Env {
			if strings.HasPrefix(e.Name, "WORKER_MEMORY_GUARD_") {
				env[e.Name] = e.Value
			}
		}
		if dep.Spec.Template.Annotations[AnnotationSpecHash] != SpecHashOf(cfg, w, spec) {
			t.Fatal("deployment hash differs from SpecHashOf")
		}
		return env
	}
	for _, cfg := range []RenderConfig{{}, testConfig()} {
		env := envOf(cfg)
		if len(env) != 1 || env["WORKER_MEMORY_GUARD_ENABLED"] != "false" {
			t.Fatalf("disabled env: %v", env)
		}
	}
	cfg := testConfig()
	cfg.MemoryGuardEnabled = true
	cfg.MemoryGuardReserveBytes = 11
	cfg.MemoryGuardSampleMs = 12
	cfg.MemoryGuardHysteresisBytes = 13
	cfg.MemoryGuardRearmMs = 14
	cfg.MemoryGuardResponseBudgetMs = 15
	cfg.MemoryGuardMaxInterventions = 16
	env := envOf(cfg)
	want := map[string]string{
		"WORKER_MEMORY_GUARD_ENABLED":            "true",
		"WORKER_MEMORY_GUARD_RESERVE_BYTES":      "11",
		"WORKER_MEMORY_GUARD_SAMPLE_MS":          "12",
		"WORKER_MEMORY_GUARD_HYSTERESIS_BYTES":   "13",
		"WORKER_MEMORY_GUARD_REARM_MS":           "14",
		"WORKER_MEMORY_GUARD_RESPONSE_BUDGET_MS": "15",
		"WORKER_MEMORY_GUARD_MAX_INTERVENTIONS":  "16",
	}
	if len(env) != len(want) {
		t.Fatalf("env: %v", env)
	}
	for name, value := range want {
		if env[name] != value {
			t.Errorf("%s = %q, want %q", name, env[name], value)
		}
	}
	base := SpecHashOf(cfg, w, spec)
	off := cfg
	off.MemoryGuardEnabled = false
	if SpecHashOf(off, w, spec) == base {
		t.Fatal("enable toggle must change hash")
	}
	if SpecHashOf(off, w, spec) != SpecHashOf(testConfig(), w, spec) {
		t.Fatal("disabled thresholds must not affect hash")
	}
	for name, change := range map[string]func(*RenderConfig){
		"ReserveBytes":     func(c *RenderConfig) { c.MemoryGuardReserveBytes++ },
		"SampleMs":         func(c *RenderConfig) { c.MemoryGuardSampleMs++ },
		"HysteresisBytes":  func(c *RenderConfig) { c.MemoryGuardHysteresisBytes++ },
		"RearmMs":          func(c *RenderConfig) { c.MemoryGuardRearmMs++ },
		"ResponseBudgetMs": func(c *RenderConfig) { c.MemoryGuardResponseBudgetMs++ },
		"MaxInterventions": func(c *RenderConfig) { c.MemoryGuardMaxInterventions++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := cfg
			change(&changed)
			if SpecHashOf(changed, w, spec) == base {
				t.Fatal("configuration change must roll pod")
			}
		})
	}
}
