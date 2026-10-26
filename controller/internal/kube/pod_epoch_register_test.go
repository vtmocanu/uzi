package kube

import (
	"context"
	"errors"
	"fmt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
	"strings"
	"testing"
)

func TestPodEpochRegister(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("future-worker-foreign-", foreign), func(t *testing.T) {
			h := newEpochHarness(t)
			// Begin with no registered workers; identity objects still exist live.
			h.session = NewPodEpochSession(h.client, PodEpochKubeResolver{Client: h.client}, nil)
			x := epochPod(epochA)
			pods := []corev1.Pod{*x}
			if foreign {
				f := epochPod(epochC)
				f.Labels = nil
				pods = append(pods, *f)
			}
			g, err := h.session.Reset(context.Background(), pods)
			if err != nil {
				t.Fatal(err)
			}
			h.generation = g
			if foreign {
				f := epochPod(epochC)
				f.Labels = nil
				f.Spec.Volumes = nil
				h.event(watch.Modified, f)
				h.event(watch.Deleted, f)
			}
			if err := h.session.RegisterWorker(h.worker); err != nil {
				t.Fatal(err)
			}
			if h.session.CurrentGeneration() != g {
				t.Fatal("generation changed on registration")
			}
			h.event(watch.Modified, epochTerminal(x))
			h.assertDecision(h.event(watch.Added, epochPod(epochB)), !foreign)
		})
	}
	t.Run("new-registration-then-A-B", func(t *testing.T) {
		h := newEpochHarness(t)
		h.session = NewPodEpochSession(h.client, PodEpochKubeResolver{Client: h.client}, nil)
		g, err := h.session.Reset(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		h.generation = g
		if err := h.session.RegisterWorker(h.worker); err != nil {
			t.Fatal(err)
		}
		h.assertDecision(h.event(watch.Added, epochPod(epochA)), false)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("ambiguous-baseline", func(t *testing.T) {
		h := newEpochHarness(t)
		h.session = NewPodEpochSession(h.client, PodEpochKubeResolver{Client: h.client}, nil)
		g, err := h.session.Reset(context.Background(), []corev1.Pod{*epochPod(epochA), *epochPod(epochC)})
		if err != nil {
			t.Fatal(err)
		}
		h.generation = g
		if err := h.session.RegisterWorker(h.worker); err != nil {
			t.Fatal(err)
		}
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("bounded-declarations", func(t *testing.T) {
		s := NewPodEpochSession(nil, nil, nil)
		for _, w := range []PodEpochWorker{{ID: ""}, {ID: strings.Repeat("x", 254)}, {ID: "w", Runtime: "other"}} {
			if s.RegisterWorker(w) == nil {
				t.Fatal("invalid worker accepted")
			}
		}
		for i := 0; i < PodEpochMaxIdentities; i++ {
			if err := s.RegisterWorker(PodEpochWorker{ID: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		if s.RegisterWorker(PodEpochWorker{ID: "overflow"}) == nil {
			t.Fatal("unbounded registration")
		}
	})
}

func TestPodEpochResolverLists(t *testing.T) {
	t.Run("no-get", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.client.ClearActions()
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
		lists := 0
		for _, a := range h.client.Actions() {
			if a.GetVerb() == "get" {
				t.Fatalf("resolver used GET: %v", a)
			}
			if a.GetVerb() == "list" {
				lists++
			}
		}
		if lists != 4 {
			t.Fatalf("identity list calls=%d", lists)
		}
	})
	for _, mode := range []string{"duplicate-claim", "wrong-local-name", "list-error", "rs-uid", "terminating-rs", "terminating-deployment"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			switch mode {
			case "duplicate-claim", "wrong-local-name", "list-error":
				h.client.PrependReactor("list", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
					if mode == "list-error" {
						return true, nil, errors.New("unavailable")
					}
					p := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataPVCName("w"), Namespace: "docker", UID: epochData}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pv"}}
					if mode == "wrong-local-name" {
						p.Name = "unrelated"
					}
					items := []corev1.PersistentVolumeClaim{p}
					if mode == "duplicate-claim" {
						items = append(items, p)
					}
					return true, &corev1.PersistentVolumeClaimList{Items: items}, nil
				})
			case "rs-uid", "terminating-rs":
				h.client.PrependReactor("list", "replicasets", func(ktesting.Action) (bool, runtime.Object, error) {
					rs := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "docker", UID: "wrong"}}
					if mode == "terminating-rs" {
						rs.UID = "rs-uid"
						now := metav1.Now()
						rs.DeletionTimestamp = &now
					}
					return true, &appsv1.ReplicaSetList{Items: []appsv1.ReplicaSet{rs}}, nil
				})
			case "terminating-deployment":
				h.client.PrependReactor("list", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
					now := metav1.Now()
					return true, &appsv1.DeploymentList{Items: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: deploymentName("w"), Namespace: "docker", UID: "dep-uid", DeletionTimestamp: &now}}}}, nil
				})
			}
			h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
		})
	}
}
