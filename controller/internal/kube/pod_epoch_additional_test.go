package kube

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
)

func TestPodEpochDeletionAndHistoricalMembership(t *testing.T) {
	t.Run("terminal-only-on-delete", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Deleted, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("deleted-payload-loses-status", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		x := epochTerminal(epochPod(epochA))
		h.event(watch.Modified, x)
		x.Status = corev1.PodStatus{}
		h.event(watch.Deleted, x)
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("foreign-label-user-loses-membership", func(t *testing.T) {
		// No claim mounts: only the original label makes this pod count.
		w := epochPod(epochC)
		w.Spec.Volumes = nil
		h := newEpochHarness(t, epochPod(epochA), w)
		w.Labels = nil
		h.event(watch.Modified, w)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("foreign-claim-user-loses-membership", func(t *testing.T) {
		w := epochPod(epochC)
		w.Labels = nil
		h := newEpochHarness(t, epochPod(epochA), w)
		w.Spec.Volumes = nil
		h.event(watch.Modified, w)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("unseen-terminal-B", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.event(watch.Modified, epochTerminal(epochPod(epochB)))
		h.assertDecision(h.event(watch.Added, epochPod(epochC)), false)
	})
	t.Run("failed-handoff-B-can-prove-C", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.event(watch.Modified, epochTerminal(epochPod(epochB)))
		e := h.assertDecision(h.event(watch.Added, epochPod(epochC)), true)
		if e.Predecessor != epochB {
			t.Fatal("skipped immediate B")
		}
	})
	t.Run("older-unproven-writer-outside-window", func(t *testing.T) {
		w := epochPod(epochC)
		w.Labels = nil
		h := newEpochHarness(t, w)
		h.event(watch.Modified, epochTerminal(w))
		h.assertDecision(h.event(watch.Added, epochPod(epochA)), false)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("terminal-regression", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.event(watch.Modified, epochPod(epochA))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
}
func TestPodEpochReadFailureAndCancellation(t *testing.T) {
	t.Run("live-read-error", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.client.PrependReactor("list", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("unavailable") })
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("event-context-cancel-before-deliver", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		ctx, cancel := context.WithCancel(context.Background())
		d, err := h.session.Handle(ctx, h.generation, watch.Added, epochPod(epochB))
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := h.session.Deliver(context.Background(), d); err == nil || len(h.patches) != 0 {
			t.Fatal("canceled event delivered")
		}
	})
	t.Run("submitted-patch-keeps-original-binding", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		d := h.event(watch.Added, epochPod(epochB))
		entered, release := make(chan struct{}), make(chan struct{})
		h.client.PrependReactor("patch", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
			close(entered)
			<-release
			var body map[string]any
			if err := json.Unmarshal(a.(ktesting.PatchAction).GetPatch(), &body); err != nil {
				t.Error(err)
			}
			m := body["metadata"].(map[string]any)
			if m["uid"] != epochB || m["resourceVersion"] != epochPod(epochB).ResourceVersion {
				t.Error("retargeted submitted patch")
			}
			return true, &corev1.Pod{}, nil
		})
		done := make(chan error, 1)
		go func() { done <- h.session.Deliver(context.Background(), d) }()
		<-entered
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		g, err := h.session.Reset(context.Background(), []corev1.Pod{*epochPod(epochB)})
		if err != nil {
			t.Fatal(err)
		}
		h.generation = g
		if err := h.session.Deliver(context.Background(), d); err == nil {
			t.Fatal("submitted decision survived restart")
		}
	})
}
func TestPodEpochAdditionalContractBoundaries(t *testing.T) {
	b, err := os.ReadFile("../../../fixtures/pod-epoch/positive.json")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := DecodePodEpochDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	base, err := EncodePodEpochEvidence(*e)
	if err != nil {
		t.Fatal(err)
	}
	e.Containers[0].ContainerID += strings.Repeat("x", PodEpochMaxDocument-len(base))
	at, err := EncodePodEpochEvidence(*e)
	if err != nil || len(at) != PodEpochMaxDocument {
		t.Fatal("8KiB boundary", len(at), err)
	}
	e.Containers[0].ContainerID += "x"
	if _, err := EncodePodEpochEvidence(*e); err == nil {
		t.Fatal("8193-byte positive accepted")
	}
	// Invalid UTF-8 must not be silently normalized before the worker sees it.
	if _, _, err := DecodePodEpochDocument([]byte{'{', '"', 0xff, '"', ':', '1', '}'}); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}
func TestPodEpochRemintBinding(t *testing.T) {
	for _, mode := range []string{"wrong-old", "wrong-new", "new-pv", "same-uid-new-pv", "data-change-despite-receipt"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			receipt := PodEpochRemint{OperationID: "op", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}
			if mode == "wrong-old" {
				receipt.OldClaimUID = epochData
			}
			if mode == "wrong-new" {
				receipt.NewClaimUID = epochC
			}
			if err := h.session.RecordMaintenanceRemint(h.generation, "w", receipt); err != nil {
				t.Fatal(err)
			}
			c, err := h.client.CoreV1().PersistentVolumeClaims("docker").Get(context.Background(), dindDataPVCName("w"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "same-uid-new-pv" {
				c.UID = epochNewDinD
			}
			if mode == "new-pv" || mode == "same-uid-new-pv" {
				c.Spec.VolumeName = "new-dind-pv"
			}
			if _, err := h.client.CoreV1().PersistentVolumeClaims("docker").Update(context.Background(), c, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if mode == "data-change-despite-receipt" {
				data, err := h.client.CoreV1().PersistentVolumeClaims("docker").Get(context.Background(), dataPVCName("w"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				data.Spec.VolumeName = "new-data-pv"
				if _, err := h.client.CoreV1().PersistentVolumeClaims("docker").Update(context.Background(), data, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			h.assertDecision(h.event(watch.Added, epochPod(epochB)), mode == "new-pv")
		})
	}
}
