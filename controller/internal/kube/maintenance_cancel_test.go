package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestDinDPendingCancellationRequiresServerLowEvidence(t *testing.T) {
	for _, low := range []bool{false, true} {
		ctx := context.Background()
		cfg := dockerTestConfig()
		w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, Busy: true}
		spec, err := testResolver(t).Resolve(w.Template, w.Size)
		if err != nil {
			t.Fatal(err)
		}
		dep := RenderDeployment(cfg, w, spec)
		dep.UID = "old-deployment"
		objs := []runtime.Object{dep}
		for _, p := range RenderPVCs(cfg, w, spec) {
			p.UID = types.UID(p.Name)
			objs = append(objs, p)
		}
		m, client := newMat(t, objs...)
		m.cfg = cfg
		f := &maintenanceFake{below: low, op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "registration", DeploymentUID: string(dep.UID), PVCUID: dindDataPVCName(w.ID), Phase: "requested"}}
		m.cordoner = f
		w.DindMaintenance = f.op
		obs, err := m.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
			t.Fatal(err)
		}
		if low && f.op.Phase != "cancelled" || !low && f.op.Phase != "requested" {
			t.Fatalf("low=%v phase=%s", low, f.op.Phase)
		}
		for _, a := range client.Actions() {
			if a.GetVerb() == "delete" {
				t.Fatal("cancel/unknown pressure destroyed objects")
			}
		}
	}
}

func TestDinDUnlabelledPVCReferenceBlocksStop(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled", Namespace: cfg.DockerNamespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "docker", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dindDataPVCName(w.ID)}}}}}}
	m, client := newMat(t, pod)
	m.cfg = cfg
	f := &maintenanceFake{op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "registration", DeploymentUID: "old-deployment", PVCUID: "old-pvc", Phase: "stopping", Fenced: true, ReadyACK: false}}
	m.cordoner = f
	w.DindMaintenance = f.op
	obs, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatal(err)
	}
	if f.op.Phase != "stopping" {
		t.Fatal("referenced data root counted as pod gone")
	}
	if err = client.CoreV1().Pods(cfg.DockerNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	obs, err = m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatal(err)
	}
	if f.op.Phase != "recycling" {
		t.Fatal("ACK reset stranded accepted stop after pods disappeared")
	}
}
