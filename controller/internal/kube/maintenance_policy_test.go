package kube

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func stampMaintenancePodOwner(p *corev1.Pod, deployment string) {
	controller := true
	p.Labels["pod-template-hash"] = "hash"
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: deployment + "-hash", UID: "replicaset", Controller: &controller}}
}

func maintenanceReplicaSet(p *corev1.Pod, d *appsv1.Deployment) *appsv1.ReplicaSet {
	controller := true
	owner := p.OwnerReferences[0]
	return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: owner.Name, UID: owner.UID, Namespace: p.Namespace,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &controller}},
	}}
}

func TestDinDCancelOnlyBeforeStopping(t *testing.T) {
	for _, phase := range []string{"requested", "ready", "stopping", "recycling"} {
		t.Run(phase, func(t *testing.T) {
			cfg := dockerTestConfig()
			w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, Busy: true}
			f := &maintenanceFake{op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "registration", DeploymentUID: "old", PVCUID: "old-pvc", Phase: phase, Fenced: phase != "requested", ReadyACK: true}}
			w.DindMaintenance = f.op
			m, _ := newMat(t)
			m.cfg = cfg
			m.cordoner = f
			m.recycle.Enabled = false
			observed, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed); err != nil {
				t.Fatal(err)
			}
			if phase == "requested" || phase == "ready" {
				if f.op.Phase != "cancelled" || f.op.Reason != "recycle_disabled" {
					t.Fatal("did not cancel before stop")
				}
			} else if slices.Contains(f.calls, "cancelled") {
				t.Fatal("cancelled after stop started")
			}
		})
	}
}

func TestDinDStrandedPendingReport(t *testing.T) {
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	var pvc *corev1.PersistentVolumeClaim
	for _, p := range RenderPVCs(cfg, w, spec) {
		if p.Name == dindDataPVCName(w.ID) {
			pvc = p
		}
	}
	pvc.CreationTimestamp = metav1.NewTime(time.Now().Add(-pvcBindTimeout - time.Minute))
	pvc.Status.Phase = corev1.ClaimPending
	m, client := newMat(t, pvc)
	m.cfg = cfg
	m.recycle.Enabled = false
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Roll.Phase != protocol.PhaseStuck || obs[0].Roll.BlockingContainer != pvc.Name {
		t.Fatalf("stranded DinD not surfaced: %+v", obs)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "list" {
			t.Fatal("strand observation actuated")
		}
	}
}

func TestDinDSimultaneousLegacyFirstThenRefreshAndDinDPriority(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DiskPressure: true, DiskPressureVolumes: []string{"data", "nix", "dind"}}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	dep := RenderDeployment(cfg, w, spec)
	dep.UID = "old-deployment"
	objs := []runtime.Object{dep}
	for _, p := range RenderPVCs(cfg, w, spec) {
		p.UID = types.UID(p.Name)
		p.Status.Phase = corev1.ClaimBound
		objs = append(objs, p)
	}
	m, client := newMat(t, objs...)
	m.cfg = cfg
	f := &maintenanceFake{}
	m.cordoner = f
	tick := func() {
		t.Helper()
		w.DindMaintenance = f.op
		obs, err := m.Observe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if f.op == nil || f.op.Phase != "requested" {
		t.Fatal("no initial server binding")
	}
	deleted := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			deleted++
		}
	}
	if deleted != 3 {
		t.Fatalf("initial persisted request must service legacy same tick: deletes=%d", deleted)
	}
	client.ClearActions()
	tick() // rebuilding after same-tick legacy deletion, request survives
	if f.op.Phase != "requested" {
		t.Fatal("lost request to legacy")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" && a.(k8stesting.DeleteAction).GetName() == dindDataPVCName(w.ID) {
			t.Fatal("combined volume recycle")
		}
	}
	tick() // rebuilding, no second legacy
	replacement, err := client.AppsV1().Deployments(cfg.DockerNamespace).Get(ctx, dep.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.UID = "replacement-deployment"
	if _, err = client.AppsV1().Deployments(cfg.DockerNamespace).Update(ctx, replacement, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: cfg.DockerNamespace, UID: "replacement-pod", Labels: replacement.Spec.Template.Labels, Annotations: replacement.Spec.Template.Annotations}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	stampMaintenancePodOwner(pod, replacement.Name)
	if _, err = client.AppsV1().ReplicaSets(cfg.DockerNamespace).Create(ctx, maintenanceReplicaSet(pod, replacement), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = client.CoreV1().Pods(cfg.DockerNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.refuse = "ready"
	tick()
	if f.op.DeploymentUID != "old-deployment" {
		t.Fatal("rejected refresh changed binding")
	}
	f.refuse = ""
	tick()
	if f.op.DeploymentUID != "replacement-deployment" || f.op.Phase != "ready" {
		t.Fatal("replacement binding not refreshed atomically with its fence")
	}
	client.ClearActions()
	tick()
	if f.op.Phase != "ready" {
		t.Fatal("DinD did not get priority after legacy refresh")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatal("second legacy recycle outran DinD")
		}
	}
}

func TestDinDUIDConflictDoesNotDeletePVC(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true}
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
	f := &maintenanceFake{op: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "register", Phase: "stopping", DeploymentUID: string(dep.UID), PVCUID: dindDataPVCName(w.ID), Fenced: true, ReadyACK: true}}
	m.cordoner = f
	w.DindMaintenance = f.op
	client.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("UID conflict") })
	obs, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err == nil {
		t.Fatal("UID conflict swallowed")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "delete" && a.GetResource().Resource == "persistentvolumeclaims" {
			t.Fatal("PVC delete followed conflict")
		}
	}
}
