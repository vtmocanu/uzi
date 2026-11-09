package kube

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

var uziTaint = []corev1.Toleration{{Key: "uzi", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}

// workers.tolerations lands on every tier's pod, next to the nodeSelector and the
// docker tier's soft anti-affinity.
func TestRenderedTolerationsOnEveryTier(t *testing.T) {
	spec := testSpec(t, "base", "m")
	cfg := laneTestConfig()
	cfg.Tolerations = uziTaint
	cfg.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}

	for _, name := range []string{"plain", "docker", "isolated"} {
		w := desired("abc")
		switch name {
		case "docker":
			w = desiredDocker("abc")
		case "isolated":
			w = desiredIsolated("abc")
		}
		pod := RenderDeployment(cfg, w, spec).Spec.Template.Spec
		if !reflect.DeepEqual(pod.Tolerations, uziTaint) {
			t.Errorf("%s tolerations = %v, want %v", name, pod.Tolerations, uziTaint)
		}
		if len(pod.NodeSelector) == 0 {
			t.Errorf("%s lost its nodeSelector next to the tolerations", name)
		}
		if name == "docker" && (pod.Affinity == nil || pod.Affinity.PodAntiAffinity == nil) {
			t.Errorf("docker worker lost its soft anti-affinity next to the tolerations")
		}
	}
}

// Setting tolerations re-hashes (so the workers roll), and none marshals no key, so an
// install without them hashes exactly as before the field existed.
func TestTolerationsHashAndOmitempty(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	none := testConfig()
	with := testConfig()
	with.Tolerations = uziTaint
	if SpecHashOf(none, w, spec) == SpecHashOf(with, w, spec) {
		t.Error("spec hash did not change with the tolerations; a change must roll the workers")
	}
	raw, err := json.Marshal(RenderDeployment(none, w, spec).Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, present := generic["spec"]["tolerations"]; present {
		t.Error("unset tolerations rendered a tolerations key; it must be omitted so the hash is unchanged")
	}
}

// A roll's merge patch must converge the live tolerations onto the desired ones,
// including clearing them: an omitted list is left alone by a merge patch. Applied
// through the fake clientset, which implements real JSON merge-patch semantics.
func TestPatchForConvergesTheTolerations(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	other := []corev1.Toleration{{Key: "pool", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	for name, tc := range map[string]struct {
		live, want []corev1.Toleration
	}{
		"added":     {live: nil, want: uziTaint},
		"replaced":  {live: other, want: uziTaint},
		"cleared":   {live: uziTaint, want: nil},
		"unchanged": {live: uziTaint, want: uziTaint},
		"none":      {live: nil, want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			liveCfg := testConfig()
			liveCfg.Tolerations = tc.live
			live := RenderDeployment(liveCfg, w, spec)
			client := fake.NewSimpleClientset(live)
			ctx := context.Background()

			wantCfg := testConfig()
			wantCfg.Tolerations = tc.want
			dep := RenderDeployment(wantCfg, w, spec)
			got, err := client.AppsV1().Deployments(live.Namespace).Get(ctx, live.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			dep.UID, dep.ResourceVersion = got.UID, got.ResourceVersion
			body, err := patchFor(dep, got.Spec.Template.Spec.NodeSelector)
			if err != nil {
				t.Fatal(err)
			}
			patched, err := client.AppsV1().Deployments(live.Namespace).Patch(ctx, live.Name, types.MergePatchType, body, metav1.PatchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := patched.Spec.Template.Spec.Tolerations; len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("patched tolerations = %v, want %v", got, tc.want)
			}
			if patched.Spec.Template.Annotations[AnnotationSpecHash] != dep.Spec.Template.Annotations[AnnotationSpecHash] {
				t.Error("patched spec-hash annotation does not match the desired template")
			}
		})
	}
}
