package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const (
	runCacheCapMaxKey  = "UZI_RUN_CACHE_CAP_MAX_BYTES"
	runCacheCapFracKey = "UZI_RUN_CACHE_CAP_FRACTION"
)

type runCacheCapShape struct {
	name string
	cfg  func() RenderConfig
	w    protocol.DesiredWorker
}

func runCacheCapShapes() []runCacheCapShape {
	return []runCacheCapShape{
		{"plain", testConfig, desired("abc")},
		{"isolated", laneTestConfig, desiredIsolated("iso")},
		{"docker", dockerTestConfig, desiredDocker("dkr")},
	}
}

// envLookup scans container.Env by Name, so an absent key is distinguishable from an
// empty value.
func envLookup(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func TestRunCacheCapEnvVerbatim(t *testing.T) {
	for _, s := range runCacheCapShapes() {
		t.Run(s.name, func(t *testing.T) {
			cases := []struct {
				field, key string
				set        func(*RenderConfig, string)
				values     []string
				other      string
			}{
				{"max", runCacheCapMaxKey, func(c *RenderConfig, v string) { c.RunCacheCapMaxBytes = v },
					[]string{"21474836480", " OFF ", "0", "not-a-number"}, runCacheCapFracKey},
				{"fraction", runCacheCapFracKey, func(c *RenderConfig, v string) { c.RunCacheCapFraction = v },
					[]string{"0.25", " OFF "}, runCacheCapMaxKey},
			}
			for _, tc := range cases {
				for _, v := range tc.values {
					cfg := s.cfg()
					tc.set(&cfg, v)
					pod := RenderDeployment(cfg, s.w, testSpec(t, "base", "m")).Spec.Template.Spec
					worker := containerByName(t, pod.Containers, workerContainerName)
					if got, ok := envLookup(worker, tc.key); !ok || got != v {
						t.Errorf("%s=%q: worker env %s = %q (present=%v), want verbatim", tc.field, v, tc.key, got, ok)
					}
					if got, ok := envLookup(worker, tc.other); ok {
						t.Errorf("%s=%q: worker rendered unset sibling %s=%q", tc.field, v, tc.other, got)
					}
					for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
						if c.Name == workerContainerName {
							continue
						}
						for _, k := range []string{runCacheCapMaxKey, runCacheCapFracKey} {
							if _, ok := envLookup(c, k); ok {
								t.Errorf("container %q carries %s; it belongs on the worker container only", c.Name, k)
							}
						}
					}
				}
			}
		})
	}
}

func TestRunCacheCapEnvAbsentWhenEmpty(t *testing.T) {
	for _, s := range runCacheCapShapes() {
		t.Run(s.name, func(t *testing.T) {
			cfg := s.cfg()
			cfg.RunCacheCapMaxBytes = ""
			cfg.RunCacheCapFraction = ""
			pod := RenderDeployment(cfg, s.w, testSpec(t, "base", "m")).Spec.Template.Spec
			for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
				for _, k := range []string{runCacheCapMaxKey, runCacheCapFracKey} {
					if v, ok := envLookup(c, k); ok {
						t.Errorf("container %q rendered %s=%q with empty config", c.Name, k, v)
					}
				}
			}
		})
	}
}

func TestRunCacheCapChangesSpecHash(t *testing.T) {
	for _, s := range runCacheCapShapes() {
		t.Run(s.name, func(t *testing.T) {
			spec := testSpec(t, "base", "m")
			h := SpecHashOf(s.cfg(), s.w, spec)

			maxOnly := s.cfg()
			maxOnly.RunCacheCapMaxBytes = "21474836480"
			if SpecHashOf(maxOnly, s.w, spec) == h {
				t.Error("setting RunCacheCapMaxBytes must change the spec hash")
			}

			fracOnly := s.cfg()
			fracOnly.RunCacheCapFraction = "0.25"
			if SpecHashOf(fracOnly, s.w, spec) == h {
				t.Error("setting RunCacheCapFraction must change the spec hash")
			}
		})
	}
}
