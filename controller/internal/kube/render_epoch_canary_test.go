package kube

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const (
	canaryUUID      = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	otherCanaryUUID = "9f8e7d6c-5b4a-4a39-8281-706f5e4d3c2b"
	canaryEnvName   = "UZI_EPOCH_RECLAIM_DELETION"
)

func canaryWorkerEnv(t *testing.T, tpl corev1.PodTemplateSpec) []corev1.EnvVar {
	t.Helper()
	for _, c := range tpl.Spec.Containers {
		if c.Name == workerContainerName {
			return c.Env
		}
	}
	t.Fatal("no worker container in template")
	return nil
}

func canaryImages(tpl corev1.PodTemplateSpec) []string {
	var out []string
	for _, list := range [][]corev1.Container{tpl.Spec.InitContainers, tpl.Spec.Containers} {
		for _, c := range list {
			out = append(out, c.Name+"="+c.Image)
		}
	}
	return out
}

// Only a persistent, non-isolated Docker worker selected by its own id changes; every
// other worker's template and spec hash stay identical to the baseline render.
func TestEpochReclaimDeletionCanaryLeavesOtherTemplatesUnchanged(t *testing.T) {
	cases := []struct {
		name string
		w    protocol.DesiredWorker
		sel  string // id the operator selects
	}{
		{"plain", protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m"}, canaryUUID},
		{"isolated plain", protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m", Isolated: true}, canaryUUID},
		{"docker isolated", protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m", Docker: true, Isolated: true}, canaryUUID},
		{"ephemeral docker", protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m", Docker: true, Ephemeral: true}, canaryUUID},
		{"docker other uuid", protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m", Docker: true}, otherCanaryUUID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := dockerTestConfig()
			base.IsolatedNamespace = "isolated"
			sel := base
			sel.EpochReclaimDeletionCanaryID = tc.sel
			spec := testSpec(t, "base", "m")
			if !reflect.DeepEqual(podTemplate(sel, tc.w, spec), podTemplate(base, tc.w, spec)) {
				t.Fatal("selector changed an unselected worker's template")
			}
			if SpecHashOf(sel, tc.w, spec) != SpecHashOf(base, tc.w, spec) {
				t.Fatal("selector changed an unselected worker's spec hash")
			}
		})
	}
}

func TestEpochReclaimDeletionCanarySelectedDockerWorker(t *testing.T) {
	base := dockerTestConfig()
	sel := base
	sel.EpochReclaimDeletionCanaryID = canaryUUID
	w := protocol.DesiredWorker{ID: canaryUUID, Template: "base", Size: "m", Docker: true}
	spec := testSpec(t, "base", "m")

	baseTpl, selTpl := podTemplate(base, w, spec), podTemplate(sel, w, spec)
	n := 0
	for _, e := range canaryWorkerEnv(t, selTpl) {
		if e.Name == canaryEnvName {
			n++
			if e.Value != "1" || e.ValueFrom != nil {
				t.Fatalf("env = %+v, want value 1", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%s rendered %d times, want exactly 1", canaryEnvName, n)
	}
	for _, e := range canaryWorkerEnv(t, baseTpl) {
		if e.Name == canaryEnvName {
			t.Fatal("baseline render carries the deletion env")
		}
	}

	stripped := selTpl.DeepCopy()
	for i := range stripped.Spec.Containers {
		c := &stripped.Spec.Containers[i]
		kept := c.Env[:0:0]
		for _, e := range c.Env {
			if e.Name != canaryEnvName {
				kept = append(kept, e)
			}
		}
		c.Env = kept
	}
	if !reflect.DeepEqual(*stripped, baseTpl) {
		t.Fatal("selected template differs from baseline by more than the deletion env")
	}
	if SpecHashOf(sel, w, spec) == SpecHashOf(base, w, spec) {
		t.Fatal("spec hash did not change, so the selected worker would never roll")
	}
	if !reflect.DeepEqual(canaryImages(selTpl), canaryImages(baseTpl)) {
		t.Fatalf("images changed: %v vs %v", canaryImages(selTpl), canaryImages(baseTpl))
	}
}
