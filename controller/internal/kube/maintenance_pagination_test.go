package kube

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

func TestMaintenancePaginationBounds(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		count, pages            int
		repeat, fail, cancelled bool
		wantErr                 bool
	}{
		{"exact 4096", 256, 16, false, false, false, false},
		{"past 16 pages", 256, 17, false, false, false, true},
		{"oversized page", 10000, 1, false, false, false, true},
		{"257-item page", 257, 1, false, false, false, true},
		{"repeated token", 256, 3, true, false, false, true},
		{"expired page", 256, 3, false, true, false, true},
		{"cancelled", 256, 3, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := maintenancePages(ctx, "pods", func(options metav1.ListOptions) (int, string, error) {
				calls++
				if options.Limit != 256 {
					t.Fatalf("limit: %d", options.Limit)
				}
				if tc.fail && calls == 2 {
					return 0, "", fmt.Errorf("expired")
				}
				if tc.cancelled {
					cancel()
				}
				next := ""
				if calls < tc.pages {
					next = fmt.Sprint(calls)
				}
				if tc.repeat {
					next = "same"
				}
				return tc.count, next, nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if calls > 16 {
				t.Fatalf("unbounded calls: %d", calls)
			}
		})
	}
}

func TestMaintenanceObservationReplacementAncestry(t *testing.T) {
	for _, mode := range []string{"current", "old-deployment", "wrong-rs-uid", "terminating-rs", "denied", "stopping", "ready", "initial", "old-pvc", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			control := true
			labels := map[string]string{LabelManagedBy: ValueManagedBy, LabelWorkerID: "w1"}
			dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName("w1"), Namespace: "workers", UID: "new-dep", Labels: labels}}
			dep.Spec.Template.Annotations = map[string]string{AnnotationSpecHash: "hash"}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dindDataPVCName("w1"), Namespace: "workers", UID: "new-pvc", Labels: labels}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: dep.Name + "-hash", Namespace: "workers", UID: "rs", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &control}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "workers", UID: "pod", Labels: labels, Annotations: dep.Spec.Template.Annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &control}}}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			op := &protocol.DindMaintenance{Phase: "recycling", DeploymentUID: "old-dep", PVCUID: "old-pvc"}
			switch mode {
			case "old-deployment":
				rs.OwnerReferences[0].UID = "old-dep"
			case "wrong-rs-uid":
				pod.OwnerReferences[0].UID = types.UID("different-rs")
			case "terminating-rs":
				now := metav1.Now()
				rs.DeletionTimestamp = &now
			case "stopping", "ready":
				op.Phase = mode
			case "initial":
				op = nil
			case "old-pvc":
				pvc.UID = "old-pvc"
			case "unbound":
				pvc.Status.Phase = corev1.ClaimPending
			}
			m, client := newMat(t, dep, pvc, rs, pod)
			rsCalls := 0
			client.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				rsCalls++
				if mode == "denied" {
					return true, nil, fmt.Errorf("forbidden")
				}
				return false, nil, nil
			})
			o, err := m.maintenanceObservation(context.Background(), "w1", "workers", reconcile.ObservedWorker{}, op)
			if err != nil {
				t.Fatal(err)
			}
			if (o.ReadyPodUID != "") != (mode == "current") {
				t.Fatalf("readiness proof: %q", o.ReadyPodUID)
			}
			if o.WorkerPodCount != 1 || !o.PodsKnown {
				t.Fatalf("broad count lost: %+v", o)
			}
			noRS := mode == "stopping" || mode == "ready" || mode == "initial" || mode == "old-pvc" || mode == "unbound"
			if (rsCalls == 0) != noRS {
				t.Fatalf("ReplicaSet reads: %d", rsCalls)
			}
		})
	}
}

func TestMaintenanceObservationRejectsOversizedForeignPods(t *testing.T) {
	m, client := newMat(t)
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: make([]corev1.Pod, 10000)}, nil
	})
	_, err := m.maintenanceObservation(context.Background(), "w1", "workers", reconcile.ObservedWorker{})
	if err == nil {
		t.Fatal("10000 foreign pods accepted")
	}
}
