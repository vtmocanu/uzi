package kube

import (
	"context"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
	"testing"
)

func TestPodEpochMaintenanceOwnReceipt(t *testing.T) {
	for _, mode := range []string{"own", "restart", "generation", "operation", "nonce", "old-uid", "already-exists", "not-found"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			h := newEpochHarness(t, epochPod(epochA))
			cfg := dockerTestConfig()
			w := desired("w")
			w.Docker = true
			op := &protocol.DindMaintenance{ID: "op", Nonce: "nonce", PVCUID: epochDinD, DeploymentUID: "old-dep", Phase: "recycling", Fenced: true}
			w.DindMaintenance = op
			var objects []runtime.Object
			for _, p := range RenderPVCs(cfg, w, testSpec(t, "base", "m")) {
				p.UID = epochData
				if p.Name == dindDataPVCName("w") {
					p.UID = epochDinD
				}
				p.Status.Phase = corev1.ClaimBound
				objects = append(objects, p)
			}
			m, client := newMat(t, objects...)
			m.cfg = cfg
			m.drain.ForceRoll = true
			m.cordoner = &maintenanceFake{}
			m.SetPodEpochSession(h.session)
			if mode == "not-found" {
				client.PrependReactor("delete", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
					name := a.(ktesting.DeleteAction).GetName()
					_ = client.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), cfg.DockerNamespace, name)
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, name)
				})
			}
			tick := func() {
				t.Helper()
				observed, err := m.Observe(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, observed); err != nil {
					t.Fatal(err)
				}
			}
			tick() // successful own UID-preconditioned delete, except explicit NotFound
			if mode == "restart" {
				m = New(client, cfg, testResolver(t), &maintenanceFake{}, DrainPolicy{ForceRoll: true}, RecyclePolicy{Enabled: true}, m.log)
				m.SetPodEpochSession(h.session)
			}
			switch mode {
			case "generation":
				var err error
				h.generation, err = h.session.Reset(ctx, []corev1.Pod{*epochPod(epochA)})
				if err != nil {
					t.Fatal(err)
				}
			case "operation":
				op.ID = "other"
			case "nonce":
				op.Nonce = "other"
			case "old-uid":
				op.PVCUID = epochData
			}
			client.PrependReactor("create", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
				p := a.(ktesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
				if p.Name == dindDataPVCName("w") {
					if mode == "already-exists" {
						return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, p.Name)
					}
					p.UID = epochNewDinD
				}
				return false, nil, nil
			})
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			minted, _ := h.client.CoreV1().PersistentVolumeClaims("docker").Get(ctx, dindDataPVCName("w"), metav1.GetOptions{})
			minted.UID = epochNewDinD
			minted.Spec.VolumeName = "new-dind-pv"
			_, _ = h.client.CoreV1().PersistentVolumeClaims("docker").Update(ctx, minted, metav1.UpdateOptions{})
			var decision *PodEpochDecision
			client.PrependReactor("create", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
				// The successor may arrive before the next maintenance poll.
				decision = h.event(watch.Added, epochPod(epochB))
				return false, nil, nil
			})
			tick()
			if decision == nil {
				t.Fatal("replacement did not create deployment")
			}
			evidence, _, err := DecodePodEpochDocument([]byte(decision.Document()))
			if err != nil {
				t.Fatal(err)
			}
			if (evidence != nil) != (mode == "own") {
				t.Fatalf("mode=%s doc=%s", mode, decision.Document())
			}
			assertNoSecretReads(t, client)
		})
	}
}
