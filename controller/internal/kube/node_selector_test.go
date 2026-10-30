package kube

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// The worker nodeSelector (workers.nodeSelector) lands on every tier's pod, next to the
// docker tier's soft anti-affinity rather than instead of it.
func TestRenderedNodeSelectorOnEveryTier(t *testing.T) {
	spec := testSpec(t, "base", "m")
	want := map[string]string{"kubernetes.io/arch": "amd64"}
	cfg := laneTestConfig()
	cfg.NodeSelector = want

	for name, w := range map[string]struct {
		plain, docker, isolated bool
	}{"plain": {plain: true}, "docker": {docker: true}, "isolated": {isolated: true}} {
		d := desired("abc")
		switch {
		case w.docker:
			d = desiredDocker("abc")
		case w.isolated:
			d = desiredIsolated("abc")
		}
		pod := RenderDeployment(cfg, d, spec).Spec.Template.Spec
		if !maps.Equal(pod.NodeSelector, want) {
			t.Errorf("%s nodeSelector = %v, want %v", name, pod.NodeSelector, want)
		}
		if w.docker && (pod.Affinity == nil || pod.Affinity.PodAntiAffinity == nil) {
			t.Errorf("docker worker lost its soft anti-affinity next to the nodeSelector")
		}
	}
}

// Changing the selector re-hashes (so it rolls), and no selector marshals no key, so an
// install that clears it hashes exactly as before the field existed.
func TestNodeSelectorHashAndOmitempty(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	none := testConfig()
	amd := testConfig()
	amd.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
	arm := testConfig()
	arm.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}

	if SpecHashOf(none, w, spec) == SpecHashOf(amd, w, spec) || SpecHashOf(amd, w, spec) == SpecHashOf(arm, w, spec) {
		t.Error("spec hash did not change with the nodeSelector; a changed selector must roll the workers")
	}
	raw, err := json.Marshal(RenderDeployment(none, w, spec).Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if _, present := generic["spec"]["nodeSelector"]; present {
		t.Error("an unset nodeSelector rendered a nodeSelector key; it must be omitted so the hash is unchanged")
	}
}

// A roll's merge patch must REMOVE live selector keys the desired template dropped.
// A merge patch leaves absent keys alone, so without explicit nulls a cleared or
// narrowed workers.nodeSelector would never reach an existing worker. Applied through
// the fake clientset, which implements real JSON merge-patch semantics.
func TestPatchForConvergesTheNodeSelector(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	for name, tc := range map[string]struct {
		live, want map[string]string
	}{
		"key dropped":      {live: map[string]string{"kubernetes.io/arch": "amd64", "pool": "a"}, want: map[string]string{"kubernetes.io/arch": "amd64"}},
		"key replaced":     {live: map[string]string{"kubernetes.io/arch": "amd64"}, want: map[string]string{"pool": "arm"}},
		"selector cleared": {live: map[string]string{"kubernetes.io/arch": "amd64"}, want: nil},
		"value changed":    {live: map[string]string{"kubernetes.io/arch": "amd64"}, want: map[string]string{"kubernetes.io/arch": "arm64"}},
		"unchanged":        {live: map[string]string{"kubernetes.io/arch": "amd64"}, want: map[string]string{"kubernetes.io/arch": "amd64"}},
	} {
		t.Run(name, func(t *testing.T) {
			liveCfg := testConfig()
			liveCfg.NodeSelector = tc.live
			live := RenderDeployment(liveCfg, w, spec)
			client := fake.NewSimpleClientset(live)
			ctx := context.Background()

			wantCfg := testConfig()
			wantCfg.NodeSelector = tc.want
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
			if sel := patched.Spec.Template.Spec.NodeSelector; !maps.Equal(sel, tc.want) {
				t.Errorf("patched nodeSelector = %v, want %v", sel, tc.want)
			}
			if patched.Spec.Template.Annotations[AnnotationSpecHash] != dep.Spec.Template.Annotations[AnnotationSpecHash] {
				t.Error("patched spec-hash annotation does not match the desired template")
			}
		})
	}
}
