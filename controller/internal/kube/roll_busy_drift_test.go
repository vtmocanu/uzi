package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Metadata reservations may advance while a busy worker keeps its exact workload.
func assertBusyDriftUnchanged(t *testing.T, c *fake.Clientset, before *appsv1.Deployment) {
	t.Helper()
	got, err := c.AppsV1().Deployments(before.Namespace).Get(context.Background(), before.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != before.UID || !reflect.DeepEqual(got.Spec, before.Spec) {
		t.Fatal("busy drift changed deployment identity, template, or replicas")
	}
	for _, a := range c.Actions() {
		resource := a.GetResource().Resource
		if resource != "deployments" && resource != "pods" && resource != "persistentvolumeclaims" {
			continue
		}
		if a.GetVerb() == "delete" || a.GetVerb() == "delete-collection" {
			t.Fatalf("busy drift destroyed %s: %s", resource, a.GetVerb())
		}
		if p, ok := a.(k8stesting.PatchAction); ok {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["spec"]; ok {
				t.Fatalf("busy drift patched %s spec: %s", resource, p.GetPatch())
			}
		}
	}
}

// The persistent candidate has fresh live coverage and acquires a durable admission.
// Ephemeral workers deliberately have no pacing entry, exercising the fallback caller.
func TestBusyDriftNeverExpires(t *testing.T) {
	for _, ephemeral := range []bool{false, true} {
		for _, drift := range []string{"image", "hash", "generation"} {
			for _, deadline := range []time.Duration{time.Hour, 24 * time.Hour} {
				for _, drain := range []string{"image_roll", "plain", "none"} {
					for _, exceeded := range []bool{false, true} {
						t.Run(fmt.Sprintf("ephemeral=%t/%s/%s/%s/exceeded=%t", ephemeral, drift, deadline, drain, exceeded), func(t *testing.T) {
							w := evidence(desired("w1"), m5Now)
							w.Busy, w.Ephemeral = true, ephemeral
							d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
							d.ResourceVersion = "1"
							switch drift {
							case "image":
								d.Spec.Template.Spec.Containers[0].Image = "worker:old"
								d.Spec.Template.Annotations[AnnotationSpecHash] = "old-image-hash"
							case "hash":
								d.Spec.Template.Annotations[AnnotationSpecHash] = "old-config-hash"
							case "generation":
								w.Generation++
							}
							if drain != "none" {
								since := m5Now.Add(-deadline)
								if exceeded {
									since = since.Add(-time.Second)
								}
								w.DrainingSince = &since
								if drain == "image_roll" {
									w.DrainingReason = token(drain)
								}
							}
							objects := physicalOld(t, d)
							for _, p := range RenderPVCs(testConfig(), w, testSpec(t, w.Template, w.Size)) {
								objects = append(objects, p)
							}
							peer := evidence(desired("w2"), m5Now)
							peer.Busy = true
							objects = append(objects, physicalOld(t, deployedWorker(peer.ID, 0, "old"))...)
							f := &fakeCordoner{}
							m, c := newMatWithDrain(t, f, DrainPolicy{Deadline: deadline}, objects...)
							workers := []protocol.DesiredWorker{w, peer}
							rollTickTest(t, m, workers)
							assertBusyDriftUnchanged(t, c, d)
							later := m5Now.Add(7 * 24 * time.Hour)
							m.now = func() time.Time { return later }
							for i := range workers {
								workers[i] = evidence(workers[i], later)
							}

							m = matrixRestart(m)
							rollTickTest(t, m, workers)
							assertBusyDriftUnchanged(t, c, d)
							if !ephemeral {
								if outcome := m.PacingOutcomes()[w.ID]; outcome == nil || outcome.State != "active" {
									t.Fatalf("busy drift was held before its mutation guard: %+v", outcome)
								}
								got, err := c.AppsV1().Deployments(d.Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
								if err != nil {
									t.Fatal(err)
								}
								marker, err := decodeRoll(got.Annotations[annotationRoll])
								if err != nil || marker == nil || marker.Completed {
									t.Fatalf("busy candidate lacked unresolved admission: marker=%+v err=%v", marker, err)
								}
							}
							workers[0].Busy = false
							rollTickTest(t, matrixRestart(m), workers)
							if patches := templatePatches(c); len(patches) != 1 || patches[0] != d.Name {
								t.Fatalf("idle admitted worker did not roll alone: %v", patches)
							}
							rollTickTest(t, matrixRestart(m), workers)
							matrixNoSibling(t, c)
						})
					}
				}
			}
		}
	}
}

// Direct maintenance callers have no pacing entry to classify the actual cause.
func TestBusyDriftNeverExpiresDirectRecycle(t *testing.T) {
	for _, drift := range []string{"hash", "generation", "image-drain"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			w := desired("w1")
			w.Busy = true
			since := m5Now.Add(-48 * time.Hour)
			w.DrainingSince = &since
			d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
			d.UID, d.ResourceVersion = "direct-deployment", "1"
			switch drift {
			case "hash":
				d.Spec.Template.Annotations[AnnotationSpecHash] = "old-config"
			case "generation":
				w.Generation++
			case "image-drain":
				w.DrainingReason = token("image_roll")
			}
			m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, d, pvcNix(w.ID, "20Gi"), pvcFor(w.ID, "data"))
			obs, err := m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.recycleWorkerVolumes(ctx, w, obs[0], m.cfg.Namespace, recycleVolumes{Nix: true, Data: true}); err != nil {
				t.Fatal(err)
			}
			assertBusyDriftUnchanged(t, c, d)
		})
	}
}
