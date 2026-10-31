package kube

import (
	"context"
	"encoding/json"
	"maps"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
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

// liveWorkerWithSelector is a deployed worker whose pod template carries sel, as an
// earlier controller config would have rendered it.
func liveWorkerWithSelector(t *testing.T, w protocol.DesiredWorker, sel map[string]string) *appsv1.Deployment {
	t.Helper()
	cfg := testConfig()
	cfg.NodeSelector = sel
	dep := RenderDeployment(cfg, w, testSpec(t, w.Template, w.Size))
	dep.UID, dep.ResourceVersion = types.UID("uid-"+dep.Name), "1"
	return dep
}

// Every observer that feeds the roll must record the live selector, or patchFor gets
// nil and a cleared selector never leaves the worker. This drives the real path: the
// default observation (observeNamespace) through Reconcile against a fake cluster, then
// the maintenance and paced-roll observers directly.
func TestObserversFeedTheLiveNodeSelectorToTheRoll(t *testing.T) {
	ctx := context.Background()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}
	live := map[string]string{"kubernetes.io/arch": "amd64", "pool": "a"}

	t.Run("observeNamespace through Reconcile clears the selector", func(t *testing.T) {
		m, client := newMat(t, liveWorkerWithSelector(t, w, live))
		obs, err := m.Observe(ctx)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if len(obs) != 1 || !maps.Equal(obs[0].DeploymentNodeSelector, live) {
			t.Fatalf("observed selector = %+v, want %v", obs, live)
		}
		// testConfig() renders no selector, so the desired template drops both keys.
		if err := m.Reconcile(ctx, []protocol.DesiredWorker{evidence(w, m.now())}, obs); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got, err := client.AppsV1().Deployments(testConfig().Namespace).Get(ctx, deploymentName(w.ID), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Spec.Template.Spec.NodeSelector) != 0 {
			t.Errorf("after the roll nodeSelector = %v, want none", got.Spec.Template.Spec.NodeSelector)
		}
	})

	t.Run("maintenance observer", func(t *testing.T) {
		m, _ := newMat(t, liveWorkerWithSelector(t, w, live))
		o, err := m.maintenanceObservation(ctx, w.ID, testConfig().Namespace, reconcile.ObservedWorker{ID: w.ID})
		if err != nil {
			t.Fatalf("maintenanceObservation: %v", err)
		}
		if !maps.Equal(o.DeploymentNodeSelector, live) {
			t.Errorf("maintenance observed selector = %v, want %v", o.DeploymentNodeSelector, live)
		}
	})

	t.Run("paced-roll snapshot", func(t *testing.T) {
		m, _ := newMat(t, liveWorkerWithSelector(t, w, live))
		// The desired Deployment, as the real caller (roll_pacing.go) sets it.
		target := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
		entries := []*rollEntry{{w: w, target: target, ns: testConfig().Namespace}}
		if err := m.rollSnapshot(ctx, testConfig().Namespace, entries); err != nil {
			t.Fatalf("rollSnapshot: %v", err)
		}
		if !maps.Equal(entries[0].o.DeploymentNodeSelector, live) {
			t.Errorf("roll snapshot observed selector = %v, want %v", entries[0].o.DeploymentNodeSelector, live)
		}
	})
}
