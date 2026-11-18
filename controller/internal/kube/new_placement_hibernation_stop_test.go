package kube

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// A claim-name collision blocks provisioning, not stopping an existing owned
// Deployment. The exception must not wake a worker or create a Deployment.
func TestNewPlacementClaimDeferralAllowsOnlyExistingHibernationStop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		existing bool
		enabled  bool
		want     int32
	}{
		{"preparing-existing", protocol.HibernationPreparing, true, true, 0},
		{"feature-off-control", protocol.HibernationPreparing, true, false, 0},
		{"waking-stays-deferred", protocol.HibernationWaking, true, true, 0},
		{"preparing-missing-stays-deferred", protocol.HibernationPreparing, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := placementConfig()
			if !tc.enabled {
				cfg = globalOnly(cfg)
			}
			w := evidence(desired("w1"), m5Now)
			w.Hibernation = &protocol.Hibernation{State: tc.state, OpID: "sleep-op"}
			objects := livePVCs(t, cfg, w, false)
			for _, obj := range objects {
				p := obj.(*corev1.PersistentVolumeClaim)
				if p.Name == dataPVCName(w.ID) {
					p.Labels = nil
					p.Annotations = map[string]string{"foreign.example/keep": "unchanged"}
				}
			}
			if tc.existing {
				dep := liveDeployment(t, cfg, w, false)
				if tc.state == protocol.HibernationWaking {
					zero := int32(0)
					dep.Spec.Replicas = &zero
				}
				objects = append(objects, dep)
			}
			m, client := npMat(t, cfg, objects...)
			beforeClaim := npPVC(t, client, cfg.Namespace, dataPVCName(w.ID)).DeepCopy()
			var beforeTemplate corev1.PodTemplateSpec
			var deploymentUID, deploymentRV string
			if tc.existing {
				before := npDep(t, client, cfg.Namespace, w.ID)
				beforeTemplate = *before.Spec.Template.DeepCopy()
				deploymentUID, deploymentRV = string(before.UID), before.ResourceVersion
			}
			client.ClearActions()
			if err := npTick(t, m, w); err != nil {
				t.Fatal(err)
			}
			if after := npPVC(t, client, cfg.Namespace, dataPVCName(w.ID)); !reflect.DeepEqual(beforeClaim, after) {
				t.Fatal("hibernation changed the colliding foreign claim")
			}
			if !tc.existing {
				deps, err := client.AppsV1().Deployments(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(deps.Items) != 0 {
					t.Fatalf("deferred preparation created %d Deployment(s)", len(deps.Items))
				}
				return
			}
			after := npDep(t, client, cfg.Namespace, w.ID)
			if after.Spec.Replicas == nil {
				t.Fatal("replicas is unset")
			}
			if *after.Spec.Replicas != tc.want {
				t.Fatalf("replicas=%d, want %d", *after.Spec.Replicas, tc.want)
			}
			if !reflect.DeepEqual(beforeTemplate, after.Spec.Template) {
				t.Fatal("claim deferral must not change the pod template")
			}
			if tc.state == protocol.HibernationWaking {
				if got := deploymentPatchBodies(client); len(got) != 0 {
					t.Fatalf("wake patched a Deployment over the unverified claim: %v", got)
				}
				return
			}
			patches := deploymentPatchBodies(client)
			if replicaPatches(client) != 1 {
				t.Fatalf("replica patches=%d, want one conditioned stop", replicaPatches(client))
			}
			for _, patch := range patches {
				meta, _ := patch["metadata"].(map[string]any)
				if meta["uid"] != deploymentUID || meta["resourceVersion"] != deploymentRV {
					t.Fatalf("stop lost UID/resourceVersion preconditions: %v", meta)
				}
				spec, _ := patch["spec"].(map[string]any)
				if _, present := spec["template"]; present {
					t.Fatal("stop patch includes a pod template")
				}
			}
		})
	}
}
