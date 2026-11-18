package kube

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// The maintenance read's RV must not let a cancelled or changed reservation reuse
// planning-time permission. A marker-preserving status update is harmless.
func TestDinDRecyclingFreshReadPreservesReservationFence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		change bool
		force  bool
	}{
		{"cancelled", true, false, false},
		{"changed", false, true, false},
		{"status-only", false, false, false},
		{"force-without-live-marker", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDriftHarness(t, driftNewDeployment, driftNewPVC)
			h.pinChange()
			h.m.drain.ForceRoll = tc.force
			ctx := context.Background()
			seed := h.deployment()
			marker, err := decodeRoll(seed.Annotations[annotationRoll])
			if err != nil || marker == nil {
				t.Fatalf("fixture marker: %v %v", marker, err)
			}
			superseded := *marker
			spec, err := h.m.resolver.Resolve(h.w.Template, h.w.Size)
			if err != nil {
				t.Fatal(err)
			}
			target := RenderDeployment(h.m.cfg, h.w, spec)
			want := target.Spec.Template.Annotations[AnnotationSpecHash]
			marker.Hash, marker.Image, marker.TargetVersion = want, workerImage(target), driftPinVersion
			raw, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			seed.Annotations[annotationRoll] = string(raw)
			if _, err := h.client.AppsV1().Deployments(h.cfg.DockerNamespace).Update(ctx, seed, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			observed, err := h.m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			resource := appsv1.SchemeGroupVersion.WithResource("deployments")
			lists, changed := 0, false
			h.client.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.GetNamespace() != h.cfg.DockerNamespace {
					return false, nil, nil
				}
				lists++
				if lists != 2 { // planning read, then fresh maintenance read
					return false, nil, nil
				}
				obj, err := h.client.Tracker().Get(resource, h.cfg.DockerNamespace, seed.Name)
				if err != nil {
					t.Fatal(err)
				}
				fresh := obj.(*appsv1.Deployment).DeepCopy()
				planned, err := decodeRoll(fresh.Annotations[annotationRoll])
				if !tc.force && (err != nil || planned == nil || planned.Hash != want || planned.Image != workerImage(target) || planned.TargetVersion != driftPinVersion) {
					t.Fatal("planning did not confirm the desired reservation")
				}
				if tc.cancel {
					delete(fresh.Annotations, annotationRoll)
				} else if tc.change {
					value, err := json.Marshal(superseded)
					if err != nil {
						t.Fatal(err)
					}
					fresh.Annotations[annotationRoll] = string(value)
				} else {
					fresh.Status.ObservedGeneration++
				}
				fresh.ResourceVersion = "900"
				if err := h.client.Tracker().Update(resource, fresh, h.cfg.DockerNamespace); err != nil {
					t.Fatal(err)
				}
				changed = true
				return false, nil, nil
			})
			if err := h.m.Reconcile(ctx, []protocol.DesiredWorker{h.w}, observed); err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("fixture did not reach the maintenance read")
			}
			hold := !tc.force && (tc.cancel || tc.change)
			if hold {
				if got := h.templateHash(); got != h.oldRev {
					t.Fatalf("patched after reservation cancellation/change: hash=%s want held %s", got, h.oldRev)
				}
			} else if got := h.templateHash(); got != want {
				t.Fatalf("status update/ForceRoll blocked: hash=%s want %s", got, want)
			}
			if h.completed() {
				t.Fatal("completed without a ready replacement")
			}
			if hold {
				for i := 0; i < 6 && h.templateHash() != want; i++ {
					h.tick()
				}
				if got := h.templateHash(); got != want {
					t.Fatalf("fresh planning failed to recover: hash=%s want %s", got, want)
				}
			}
		})
	}
}
