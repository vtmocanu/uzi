package kube

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// A fresh maintenance list can find drift that the planner did not reserve, or a
// same-name replacement with a different identity. Neither inherits roll permission.
func TestDinDRecyclingFreshDriftNeedsMatchingPlannedRoll(t *testing.T) {
	for _, tc := range []struct {
		name           string
		plannedCurrent bool
		replaceUID     bool
		force          bool
	}{
		{"unreserved-same-uid", true, false, false},
		{"unreserved-new-uid", true, true, false},
		{"reserved-drift-new-uid", false, true, false},
		{"force-unreserved", true, false, true},
		{"force-new-uid", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDriftHarness(t, driftNewDeployment, driftNewPVC)
			h.pinChange()
			h.m.drain.ForceRoll = tc.force
			ctx := context.Background()
			seed := h.deployment()
			staleTemplate := *seed.Spec.Template.DeepCopy()
			spec, err := h.m.resolver.Resolve(h.w.Template, h.w.Size)
			if err != nil {
				t.Fatal(err)
			}
			target := RenderDeployment(h.m.cfg, h.w, spec)
			want := target.Spec.Template.Annotations[AnnotationSpecHash]
			if tc.plannedCurrent {
				seed.Spec.Template = *target.Spec.Template.DeepCopy()
				seed.Annotations = nil // no drift means no reservation is required
			} else {
				marker, err := decodeRoll(seed.Annotations[annotationRoll])
				if err != nil || marker == nil {
					t.Fatalf("seed marker: %v %v", marker, err)
				}
				marker.Hash, marker.Image, marker.TargetVersion = want, workerImage(target), driftPinVersion
				raw, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				seed.Annotations[annotationRoll] = string(raw)
			}
			if _, err := h.client.AppsV1().Deployments(h.cfg.DockerNamespace).Update(ctx, seed, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			observed, err := h.m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			lists, changed := 0, false
			resource := appsv1.SchemeGroupVersion.WithResource("deployments")
			h.client.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetNamespace() != h.cfg.DockerNamespace {
					return false, nil, nil
				}
				lists++
				if lists != 2 { // planner read first, maintenance read second
					return false, nil, nil
				}
				obj, err := h.client.Tracker().Get(resource, h.cfg.DockerNamespace, seed.Name)
				if err != nil {
					t.Fatal(err)
				}
				fresh := obj.(*appsv1.Deployment).DeepCopy()
				if tc.plannedCurrent && (fresh.Spec.Template.Annotations[AnnotationSpecHash] != want || fresh.Annotations[annotationRoll] != "") {
					t.Fatal("planner did not see the unchanged unreserved template")
				}
				fresh.Spec.Template = *staleTemplate.DeepCopy()
				fresh.ResourceVersion = "900"
				if tc.replaceUID {
					fresh.UID = types.UID("different-replacement")
					if err := h.client.Tracker().Delete(resource, h.cfg.DockerNamespace, seed.Name); err != nil {
						t.Fatal(err)
					}
					if err := h.client.Tracker().Add(fresh); err != nil {
						t.Fatal(err)
					}
				} else if err := h.client.Tracker().Update(resource, fresh, h.cfg.DockerNamespace); err != nil {
					t.Fatal(err)
				}
				changed = true
				return false, nil, nil
			})
			if err := h.m.Reconcile(ctx, []protocol.DesiredWorker{h.w}, observed); err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("fixture never reached the maintenance read")
			}
			got := h.templateHash()
			if tc.force {
				if got != want {
					t.Fatalf("ForceRoll did not preserve its bypass: hash=%s want %s", got, want)
				}
			} else if got != h.oldRev {
				t.Fatalf("fresh unplanned/unattributed drift was patched: hash=%s want held %s", got, h.oldRev)
			}
			if h.completed() {
				t.Fatal("completed during the fresh-drift tick")
			}
			if !tc.force {
				for i := 0; i < 6 && h.templateHash() != want; i++ {
					h.tick()
				}
				if got := h.templateHash(); got != want {
					t.Fatalf("fresh planning did not admit the replacement: hash=%s want %s", got, want)
				}
				if h.completed() {
					t.Fatal("completed before a ready registered replacement")
				}
			}
		})
	}
}
