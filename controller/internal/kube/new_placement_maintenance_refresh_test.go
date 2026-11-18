package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Maintenance has a fresher DinD list than the tick's placement snapshot. Do not
// apply that snapshot to an appeared/replaced claim; the next tick must decide
// from its stamp. Matching identity and stamp still allow same-tick rebuilding.
func TestNewPlacementMaintenanceDefersChangedDinDClaim(t *testing.T) {
	for _, tc := range []struct {
		name           string
		initialPresent bool
		initialStamped bool
		freshStamped   bool
		replaced       bool
		enabled        bool
	}{
		{"appeared-unstamped", false, false, false, false, true},
		{"replaced-with-unstamped", true, true, false, true, true},
		{"replaced-with-stamped", true, false, true, true, true},
		{"replaced-same-stamp", true, true, true, true, true},
		{"unchanged", true, true, true, false, true},
		{"feature-off-appeared", false, false, false, false, false},
		{"feature-off-replaced", true, true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := placementConfig()
			if !tc.enabled {
				cfg = globalOnly(cfg)
			}
			cfg.DockerNamespace = "uzi-workers-docker"
			cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
			w := desiredDocker("w1")
			w.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "recycling", Fenced: true, DeploymentUID: "original-deployment", PVCUID: "original-dind"}
			var dind *corev1.PersistentVolumeClaim
			objects := livePVCs(t, cfg, w, true)
			for i, obj := range objects {
				p := obj.(*corev1.PersistentVolumeClaim)
				if p.Name != dindDataPVCName(w.ID) {
					continue
				}
				dind = p.DeepCopy()
				p.UID = "snapshot-dind"
				p.Status.Phase = corev1.ClaimBound
				if !tc.initialStamped {
					delete(p.Annotations, AnnotationPlacement)
				}
				if !tc.initialPresent {
					objects = append(objects[:i], objects[i+1:]...)
				}
				break
			}
			if dind == nil {
				t.Fatal("fixture has no DinD claim")
			}
			m, client := npMat(t, cfg, objects...)
			m.cordoner = &maintenanceFake{op: w.DindMaintenance}
			ctx := context.Background()
			observed, err := m.Observe(ctx)
			if err != nil || len(observed) != 1 {
				t.Fatalf("Observe: %v %v", observed, err)
			}
			initial := observed[0]
			tickCtx := context.WithValue(ctx, placementTickKey{}, m.placementDecisions([]protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{w.ID: initial}))
			changed := !tc.initialPresent || tc.replaced
			if changed {
				if tc.initialPresent {
					if err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Delete(ctx, dind.Name, metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				dind.UID = "fresh-dind"
				dind.Status.Phase = corev1.ClaimBound
				if !tc.freshStamped {
					delete(dind.Annotations, AnnotationPlacement)
				}
				if _, err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Create(ctx, dind, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			client.ClearActions()
			handled, err := m.reconcileDinD(tickCtx, w, initial, cfg.DockerNamespace, testSpec(t, w.Template, w.Size))
			if err != nil || !handled {
				t.Fatalf("maintenance handled=%v err=%v", handled, err)
			}
			if changed && cfg.NewPlacementSet() {
				if got := writes(client); len(got) != 0 {
					t.Fatalf("changed DinD snapshot must defer all writes, got %v", got)
				}
				observed, err = m.Observe(ctx)
				if err != nil || len(observed) != 1 {
					t.Fatalf("next Observe: %v %v", observed, err)
				}
				fresh := observed[0]
				tickCtx = context.WithValue(ctx, placementTickKey{}, m.placementDecisions([]protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{w.ID: fresh}))
				handled, err = m.reconcileDinD(tickCtx, w, fresh, cfg.DockerNamespace, testSpec(t, w.Template, w.Size))
				if err != nil || !handled {
					t.Fatalf("next maintenance handled=%v err=%v", handled, err)
				}
			}
			want := "global"
			if cfg.NewPlacementSet() && tc.freshStamped {
				want = "new"
			}
			if got := podPlacement(npDep(t, client, cfg.DockerNamespace, w.ID)); got != want {
				t.Fatalf("replacement placement=%s want %s", got, want)
			}
			if got := isStamped(npPVC(t, client, cfg.DockerNamespace, dind.Name)); got != tc.freshStamped {
				t.Fatal("existing DinD claim stamp was changed")
			}
		})
	}
}
