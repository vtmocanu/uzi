package kube

import (
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestGoConcurrencyRenderAndSpecHash(t *testing.T) {
	spec := testSpec(t, "base", "m")
	const (
		buildKey = "WORKER_GO_BUILD_PARALLELISM"
		procsKey = "WORKER_GO_MAXPROCS"
	)
	workers := map[string]protocol.DesiredWorker{
		"plain":    desired("abc"),
		"isolated": desiredIsolated("iso"),
	}
	cfgFor := func(name string) RenderConfig {
		if name == "isolated" {
			return laneTestConfig()
		}
		return testConfig()
	}

	for name, w := range workers {
		t.Run(name, func(t *testing.T) {
			envOf := func(cfg RenderConfig) map[string]string {
				return envMap(laneWorkerContainer(t, cfg, w))
			}
			base := cfgFor(name)

			env := envOf(base)
			for _, k := range []string{buildKey, procsKey} {
				if _, ok := env[k]; ok {
					t.Errorf("unset config rendered %s", k)
				}
			}

			build := base
			build.GoBuildParallelism = 3
			env = envOf(build)
			if env[buildKey] != "3" {
				t.Errorf("%s = %q, want 3", buildKey, env[buildKey])
			}
			if _, ok := env[procsKey]; ok {
				t.Errorf("%s rendered while unset", procsKey)
			}

			procs := base
			procs.GoMaxProcs = 5
			env = envOf(procs)
			if env[procsKey] != "5" {
				t.Errorf("%s = %q, want 5", procsKey, env[procsKey])
			}
			if _, ok := env[buildKey]; ok {
				t.Errorf("%s rendered while unset", buildKey)
			}

			both := build
			both.GoMaxProcs = 5
			env = envOf(both)
			if env[buildKey] != "3" || env[procsKey] != "5" {
				t.Errorf("both set: %s=%q %s=%q", buildKey, env[buildKey], procsKey, env[procsKey])
			}

			h := SpecHashOf(base, w, spec)
			for label, c := range map[string]RenderConfig{"build": build, "procs": procs, "both": both} {
				if SpecHashOf(c, w, spec) == h {
					t.Errorf("%s: setting the env must roll the pod", label)
				}
			}
		})
	}
}
