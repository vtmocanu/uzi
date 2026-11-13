package kube

import (
	"context"
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// The operator's Deployment metadata (chart workloadLabels / workloadAnnotations) lands
// on the worker Deployment's own metadata, never on the pod template (so it moves no
// spec hash), and the controller's provenance labels always win.
func TestDeploymentCarriesOperatorMetadata(t *testing.T) {
	spec := testSpec(t, "base", "m")
	plain := RenderDeployment(testConfig(), desired("abc"), spec)

	cfg := testConfig()
	cfg.DeploymentLabels = map[string]string{"team": "platform", LabelManagedBy: "someone-else"}
	cfg.DeploymentAnnotations = map[string]string{"policy.example.com/skip-probes": "true"}
	dep := RenderDeployment(cfg, desired("abc"), spec)

	if dep.Labels["team"] != "platform" {
		t.Errorf("labels = %v, want the operator's team label", dep.Labels)
	}
	if id, ours := IsOurs(dep.Labels); !ours || id != "abc" {
		t.Errorf("provenance lost: labels = %v", dep.Labels)
	}
	if dep.Annotations["policy.example.com/skip-probes"] != "true" {
		t.Errorf("annotations = %v, want the operator's annotation", dep.Annotations)
	}
	if dep.Spec.Template.Annotations[AnnotationSpecHash] != plain.Spec.Template.Annotations[AnnotationSpecHash] {
		t.Error("Deployment metadata moved the pod spec hash: it would roll the fleet")
	}
	if _, ok := dep.Spec.Template.Labels["team"]; ok {
		t.Error("operator labels leaked into the pod template")
	}
	if len(plain.Annotations) != 0 || !maps.Equal(plain.Labels, objectLabels("abc")) {
		t.Errorf("no operator metadata configured: labels=%v annotations=%v", plain.Labels, plain.Annotations)
	}
}

// A drift roll's merge patch carries the operator metadata, so an existing worker picks
// it up on its next roll.
func TestPatchForCarriesOperatorMetadata(t *testing.T) {
	spec := testSpec(t, "base", "m")
	w := desired("abc")
	live := RenderDeployment(testConfig(), w, spec)
	client := fake.NewSimpleClientset(live)
	ctx := context.Background()
	got, err := client.AppsV1().Deployments(live.Namespace).Get(ctx, live.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.DeploymentLabels = map[string]string{"team": "platform"}
	cfg.DeploymentAnnotations = map[string]string{"policy.example.com/skip-probes": "true"}
	dep := RenderDeployment(cfg, w, spec)
	dep.UID, dep.ResourceVersion = got.UID, got.ResourceVersion
	body, err := patchFor(dep, got.Spec.Template.Spec.NodeSelector)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := client.AppsV1().Deployments(live.Namespace).Patch(ctx, live.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if patched.Labels["team"] != "platform" || patched.Annotations["policy.example.com/skip-probes"] != "true" {
		t.Errorf("patched labels=%v annotations=%v, want the operator metadata", patched.Labels, patched.Annotations)
	}
	if _, ours := IsOurs(patched.Labels); !ours {
		t.Errorf("patch dropped provenance: %v", patched.Labels)
	}
}

// Every update the controller sends carries the operator metadata (an admission policy
// that validates updates would otherwise refuse a reservation on an older worker), and a
// key the patch sets itself, the roll marker, keeps its value.
func TestWithOperatorMetadataKeepsThePatchesOwnKeys(t *testing.T) {
	m := &Materializer{cfg: RenderConfig{
		DeploymentLabels:      map[string]string{"team": "platform"},
		DeploymentAnnotations: map[string]string{"policy.example.com/skip-probes": "true", annotationRoll: "operator"},
	}}
	meta := m.withOperatorMetadata(map[string]any{"uid": "u", "annotations": map[string]any{annotationRoll: "marker"}})
	ann := meta["annotations"].(map[string]any)
	if ann[annotationRoll] != "marker" || ann["policy.example.com/skip-probes"] != "true" {
		t.Fatalf("annotations = %v, want the roll marker kept and the operator annotation added", ann)
	}
	if meta["labels"].(map[string]string)["team"] != "platform" || meta["uid"] != "u" {
		t.Fatalf("meta = %v", meta)
	}
	if got := (&Materializer{}).withOperatorMetadata(map[string]any{"uid": "u"}); len(got) != 1 {
		t.Fatalf("no operator metadata configured: meta = %v, want it unchanged", got)
	}
}
