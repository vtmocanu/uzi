package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// This file can be copied alone into the pinned pre-capacity kube package. It
// uses the existing fake-client Tick seam and asserts JSON literals, not new types.
func TestSchedulerCapacityPortable(t *testing.T) {
	for _, tc := range []struct{ name, diagnostic, fixed string }{
		{"cpu", "2 Insufficient cpu", "scheduler capacity: insufficient cpu"},
		{"memory", "2 Insufficient memory", "scheduler capacity: insufficient memory"},
		{"both", "1 Insufficient cpu, 1 Insufficient memory", "scheduler capacity: insufficient cpu and memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep, pod := capacityPortableObjects()
			pod.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Minute))
			pod.Status.Conditions[0].Message = "0/2 nodes are available: " + tc.diagnostic + "."
			claim := pvcFor(capacityPortableID, "nix")
			claim.Status.Phase = corev1.ClaimPending
			claim.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Hour))
			pod.Spec.Volumes = []corev1.Volume{{Name: "nix", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name},
			}}}
			wire := capacityPortableTick(t, []runtime.Object{dep, pod, claim}, false)
			var row map[string]any
			if err := json.Unmarshal(wire, &row); err != nil {
				t.Fatal(err)
			}
			if row["phase"] != "stuck" {
				t.Fatalf("phase: %s", wire)
			}
			if row["blocking_kind"] != "scheduler_capacity" {
				t.Fatalf("blocking_kind missing scheduler_capacity: %s", wire)
			}
			if row["blocking_reason"] != tc.fixed {
				t.Fatalf("reason: %s", wire)
			}
		})
	}
}

const capacityPortableID = "11111111-1111-1111-1111-111111111111"

func capacityPortableObjects() (*appsv1.Deployment, *corev1.Pod) {
	dep := deployedWorker(capacityPortableID, 0, "current")
	dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "worker", Image: "example.com/agent:1.2.3"}}
	pod := workerPod(capacityPortableID, "current", time.Hour)
	pod.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Hour))
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/2 nodes are available: 2 Insufficient cpu."}}
	return dep, pod
}

func capacityPortableTick(t *testing.T, objects []runtime.Object, failPods bool, reversePVCList ...bool) json.RawMessage {
	t.Helper()
	reverse := len(reversePVCList) != 0 && reversePVCList[0]
	return capacityPortableTickConfigured(t, objects, failPods, reverse, nil)
}

func capacityPortableTickConfigured(t *testing.T, objects []runtime.Object, failPods, reversePVCList bool, configure func(*Materializer)) json.RawMessage {
	t.Helper()
	m, client := newMatWithRecycle(t, nil, DrainPolicy{}, RecyclePolicy{}, io.Discard, objects...)
	if configure != nil {
		configure(m)
	}
	if reversePVCList {
		list, err := client.CoreV1().PersistentVolumeClaims(testConfig().Namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i, j := 0, len(list.Items)-1; i < j; i, j = i+1, j-1 {
			list.Items[i], list.Items[j] = list.Items[j], list.Items[i]
		}
		client.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) { return true, list.DeepCopy(), nil })
	}
	if failPods {
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("observation unavailable")
		})
	}
	reporter := &strandReporter{}
	loop := reconcile.New(strandPoller{resp: protocol.PollResponse{Workers: []protocol.DesiredWorker{{ID: capacityPortableID, Template: "base", Size: "m"}}}}, m, reporter, 10*time.Second, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reporter.got) != 1 {
		t.Fatalf("reports: %+v", reporter.got)
	}
	for _, row := range reporter.got[0].Workers {
		if row.ID == capacityPortableID {
			wire, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			return wire
		}
	}
	return json.RawMessage("null")
}
