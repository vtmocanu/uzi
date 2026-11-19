package kube

import (
	"maps"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

var newPlacementTaint = []corev1.Toleration{{Key: "pool", Operator: corev1.TolerationOpEqual, Value: "new", Effect: corev1.TaintEffectNoSchedule}}

func placementConfig() RenderConfig {
	cfg := testConfig()
	cfg.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	cfg.Tolerations = uziTaint
	cfg.NewPlacementNodeSelector = map[string]string{"pool": "new"}
	cfg.NewPlacementTolerations = newPlacementTaint
	return cfg
}

func TestPlacementStampValue(t *testing.T) {
	if AnnotationPlacement != "uzi.dev/placement" || placementNew != "new" {
		t.Errorf("placement stamp = %q=%q", AnnotationPlacement, placementNew)
	}
}

// A stamped worker renders the new placement and none of the global one (replace, not merge).
func TestStampedWorkerRendersTheNewPlacement(t *testing.T) {
	spec := testSpec(t, "base", "m")
	cfg := placementConfig().withPlacement(true)
	pod := RenderDeployment(cfg, desired("abc"), spec).Spec.Template.Spec
	if !maps.Equal(pod.NodeSelector, map[string]string{"pool": "new"}) {
		t.Errorf("nodeSelector = %v, want only pool=new", pod.NodeSelector)
	}
	if !reflect.DeepEqual(pod.Tolerations, newPlacementTaint) {
		t.Errorf("tolerations = %v, want %v", pod.Tolerations, newPlacementTaint)
	}
}

// Tolerations-only new placement REPLACES the global selector with nothing.
func TestTolerationsOnlyNewPlacementRendersNoNodeSelector(t *testing.T) {
	spec := testSpec(t, "base", "m")
	cfg := placementConfig()
	cfg.NewPlacementNodeSelector = nil
	pod := RenderDeployment(cfg.withPlacement(true), desired("abc"), spec).Spec.Template.Spec
	if len(pod.NodeSelector) != 0 {
		t.Errorf("nodeSelector = %v, want none", pod.NodeSelector)
	}
	if !reflect.DeepEqual(pod.Tolerations, newPlacementTaint) {
		t.Errorf("tolerations = %v, want %v", pod.Tolerations, newPlacementTaint)
	}
}

// Legacy hashes stay byte-identical: unstamped, or stamped with no new placement set.
func TestLegacyHashUnchangedByNewPlacement(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	legacy := placementConfig()
	legacy.NewPlacementNodeSelector, legacy.NewPlacementTolerations = nil, nil
	want := SpecHashOf(legacy, w, spec)

	if got := SpecHashOf(legacy.withPlacement(true), w, spec); got != want {
		t.Error("stamped with no new placement configured changed the hash")
	}
	if got := SpecHashOf(legacy.withPlacement(false), w, spec); got != want {
		t.Error("unstamped changed the hash")
	}
	if got := SpecHashOf(placementConfig().withPlacement(false), w, spec); got != want {
		t.Error("new placement configured but worker unstamped changed the hash")
	}
	if got := SpecHashOf(placementConfig().withPlacement(true), w, spec); got == want {
		t.Error("stamped worker with new placement configured did not re-hash")
	}
}

func TestNewPlacementSet(t *testing.T) {
	var c RenderConfig
	if c.NewPlacementSet() {
		t.Error("empty config reports set")
	}
	c.NewPlacementTolerations = newPlacementTaint
	if !c.NewPlacementSet() {
		t.Error("tolerations alone not reported set")
	}
	c = RenderConfig{NewPlacementNodeSelector: map[string]string{"a": "b"}}
	if !c.NewPlacementSet() {
		t.Error("selector alone not reported set")
	}
}

// SpecHashOf and the rendered annotation agree for the placement-adjusted config.
func TestSpecHashMatchesRenderedAnnotationPerPlacement(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	for _, stamped := range []bool{false, true} {
		cfg := placementConfig().withPlacement(stamped)
		got := RenderDeployment(cfg, w, spec).Spec.Template.Annotations[AnnotationSpecHash]
		if want := SpecHashOf(cfg, w, spec); got != want {
			t.Errorf("stamped=%v: annotation %s != SpecHashOf %s", stamped, got, want)
		}
	}
}
