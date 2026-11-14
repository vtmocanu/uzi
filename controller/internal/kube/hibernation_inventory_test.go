package kube

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func hibGateW(state, op, gate string) protocol.DesiredWorker {
	w := hibW(state, op)
	w.Hibernation.InventoryGate = gate
	return w
}

func hibStorageClass(name string, policy corev1.PersistentVolumeReclaimPolicy) *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: "x", ReclaimPolicy: &policy}
}

func hibObservedCalls(rec *hibRec) []protocol.HibernationTransition {
	var out []protocol.HibernationTransition
	for _, c := range rec.calls {
		if c.Transition == protocol.TransitionObserved {
			out = append(out, c)
		}
	}
	return out
}

func TestObservedPrecedesEveryScaleDown(t *testing.T) {
	for _, gate := range []string{protocol.InventoryGateObserve, protocol.InventoryGateAwait} {
		m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
		w := hibGateW(protocol.HibernationPreparing, "op-1", gate)
		for i := 0; i < 2; i++ {
			if err := hibTick(t, m, w); err != nil {
				t.Fatalf("%s: %v", gate, err)
			}
		}
		if replicaPatches(client) != 0 {
			t.Fatalf("%s: a replicas patch was issued before the api permitted", gate)
		}
		if got := rec.transitions(); len(got) != 2 || got[0] != protocol.TransitionObserved || got[1] != protocol.TransitionObserved {
			t.Fatalf("%s: transitions = %v, want observed re-sent each tick", gate, got)
		}
		assertNothingDeleted(t, client)
	}
}

func TestPermitAndEmptyGateScaleDownCarriesTheFenceGeneration(t *testing.T) {
	for _, gate := range []string{protocol.InventoryGatePermit, ""} {
		m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false), hibPod("pod-uid-2", "x", false, false))
		// The fake does not bump metadata.generation; stamp what a real write would return.
		client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj, err := client.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), testConfig().Namespace, "uzi-hw-w1")
			if err != nil {
				return true, nil, err
			}
			d := obj.(*appsv1.Deployment).DeepCopy()
			d.Generation = 7
			_ = client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, testConfig().Namespace)
			return false, nil, nil
		})
		if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", gate)); err != nil {
			t.Fatalf("gate %q: %v", gate, err)
		}
		if replicaPatches(client) != 1 {
			t.Fatalf("gate %q: replica patches = %d, want 1", gate, replicaPatches(client))
		}
		if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScalingDown {
			t.Fatalf("gate %q: transitions = %v", gate, got)
		}
		c := rec.calls[0]
		if c.FenceGeneration == nil || *c.FenceGeneration != 7 || c.DeploymentGeneration == nil || *c.DeploymentGeneration != 7 {
			t.Fatalf("gate %q: fence/generation = %v/%v, want 7/7", gate, c.FenceGeneration, c.DeploymentGeneration)
		}
		if c.DeploymentReplicas == nil || *c.DeploymentReplicas != 0 {
			t.Fatalf("gate %q: replicas = %v, want 0", gate, c.DeploymentReplicas)
		}
		if c.PodUIDs == nil || len(*c.PodUIDs) != 2 || c.PVCUserPods == nil {
			t.Fatalf("gate %q: pod sets = %v / %v", gate, c.PodUIDs, c.PVCUserPods)
		}
		// The re-send carries no fence and the observed generation.
		if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", gate)); err != nil {
			t.Fatal(err)
		}
		re := rec.calls[len(rec.calls)-1]
		if re.Transition != protocol.TransitionScalingDown || re.FenceGeneration != nil || re.DeploymentGeneration == nil || *re.DeploymentGeneration != 7 {
			t.Fatalf("gate %q: re-send = %+v", gate, re)
		}
		assertNothingDeleted(t, client)
	}
}

func TestRefusalStillComesFirstUnderAnInventoryGate(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	w := hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGateObserve)
	w.Busy = true
	if err := hibTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionRefused {
		t.Fatalf("transitions = %v, want [refused] and no observed", got)
	}
	if replicaPatches(client) != 0 {
		t.Fatal("a refused worker was scaled")
	}
	assertNothingDeleted(t, client)
}

func TestObservedPayload(t *testing.T) {
	ns := testConfig().Namespace
	nix := pvcNix("w1", "20Gi")
	nix.UID = "nix-uid"
	cls := "retain-class"
	nix.Spec.StorageClassName = &cls
	data := pvcFor("w1", "data")
	data.UID = "data-uid"
	delCls := "delete-class"
	data.Spec.StorageClassName = &delCls
	dind := pvcFor("w1", "dind-data")
	dind.UID = "dind-uid"
	forbidden := "forbidden-class"
	dind.Spec.StorageClassName = &forbidden

	rec := &hibRec{fakeCordoner: &fakeCordoner{}}
	unlabelled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: ns, UID: "stray-uid"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVCName("w1")}}}}}}
	m, client := newMatWithCordoner(t, rec, hibDeployment(1, ""), nix, data, dind, hibPod("pod-uid-1", "x", true, false), unlabelled,
		hibStorageClass(cls, corev1.PersistentVolumeReclaimRetain), hibStorageClass(delCls, corev1.PersistentVolumeReclaimDelete))
	client.PrependReactor("get", "storageclasses", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == forbidden {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, forbidden, nil)
		}
		return false, nil, nil
	})
	if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGateObserve)); err != nil {
		t.Fatal(err)
	}
	obs := hibObservedCalls(rec)
	if len(obs) != 1 {
		t.Fatalf("observed reports = %d (%v)", len(obs), rec.transitions())
	}
	c := obs[0]
	if c.PodUID != "pod-uid-1" || c.DeploymentUID == "" || c.ResourceVersion == "" {
		t.Fatalf("identity = %+v", c)
	}
	want := map[string][2]string{
		protocol.PVCRoleData:     {"data-uid", protocol.ReclaimPolicyDelete},
		protocol.PVCRoleNix:      {"nix-uid", protocol.ReclaimPolicyRetain},
		protocol.PVCRoleDindData: {"dind-uid", protocol.ReclaimPolicyUnknown},
	}
	if len(c.PVCs) != 3 {
		t.Fatalf("pvcs = %+v", c.PVCs)
	}
	for _, p := range c.PVCs {
		if w := want[p.Role]; p.UID != w[0] || p.ClassReclaimPolicy != w[1] {
			t.Fatalf("pvc %+v, want uid/policy %v", p, w)
		}
	}
	if c.PodUIDs == nil || len(*c.PodUIDs) != 1 || (*c.PodUIDs)[0] != "pod-uid-1" {
		t.Fatalf("pod_uids = %v", c.PodUIDs)
	}
	if c.PVCUserPods == nil || len(*c.PVCUserPods) != 1 || (*c.PVCUserPods)[0] != "stray-uid" {
		t.Fatalf("pvc_user_pods = %v, want the unlabelled mounting pod", c.PVCUserPods)
	}
	if c.DeploymentReplicas == nil || *c.DeploymentReplicas != 1 || c.DeploymentGeneration == nil {
		t.Fatalf("deployment facts = %v/%v", c.DeploymentReplicas, c.DeploymentGeneration)
	}
	assertNothingDeleted(t, client)
}

func TestObservedUnknownPolicyWhenClaimHasNoClass(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGateAwait)); err != nil {
		t.Fatal(err)
	}
	obs := hibObservedCalls(rec)
	if len(obs) != 1 || len(obs[0].PVCs) != 2 {
		t.Fatalf("observed = %+v", obs)
	}
	for _, p := range obs[0].PVCs {
		if p.ClassReclaimPolicy != protocol.ReclaimPolicyUnknown {
			t.Fatalf("pvc %+v, want unknown without a class", p)
		}
	}
	if obs[0].PVCUserPods == nil || len(*obs[0].PVCUserPods) != 0 {
		t.Fatalf("pvc_user_pods = %v, want known-empty", obs[0].PVCUserPods)
	}
	assertNothingDeleted(t, client)
}

func TestInventoryGateHoldsWhenPodsOrPodUIDUnknown(t *testing.T) {
	// No pod at all: the bound pod uid is unknown.
	m, client, rec := hibEnv(t, hibDeployment(1, ""))
	if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGateObserve)); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 || replicaPatches(client) != 0 {
		t.Fatalf("no-pod tick reported %v / patched", rec.transitions())
	}
	// Pod list failing: pods not known.
	m2, client2, rec2 := hibEnv(t, hibDeployment(1, ""), hibPod("pod-uid-1", "x", true, false))
	client2.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	if err := hibTick(t, m2, hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGateObserve)); err != nil {
		t.Fatal(err)
	}
	// A failed pod list also removes the roll evidence, so the existing fail-closed
	// refusal may fire; what must never happen is an observed report or a scale-down.
	if hasTransition(rec2, protocol.TransitionObserved) || replicaPatches(client2) != 0 {
		t.Fatalf("pods-unknown tick reported %v / patched", rec2.transitions())
	}
	assertNothingDeleted(t, client)
	assertNothingDeleted(t, client2)
}

func TestScaledDownEncodesAnEmptyPodSet(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(0, "op-1"))
	if err := hibTick(t, m, hibGateW(protocol.HibernationPreparing, "op-1", protocol.InventoryGatePermit)); err != nil {
		t.Fatal(err)
	}
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScaledDown {
		t.Fatalf("transitions = %v", got)
	}
	raw, err := json.Marshal(rec.calls[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pod_uids":[]`) || !strings.Contains(string(raw), `"pvc_user_pods":[]`) {
		t.Fatalf("scaled_down body = %s, want empty (not omitted) pod sets", raw)
	}
	assertNothingDeleted(t, client)
}

func TestHeldIsSentEveryTickWithGenerationAndReplicas(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(0, "op-1"))
	w := hibW(protocol.HibernationHibernated, "op-1")
	for i := 0; i < 2; i++ {
		if err := hibTick(t, m, w); err != nil {
			t.Fatal(err)
		}
	}
	if got := rec.transitions(); len(got) != 2 || got[0] != protocol.TransitionHeld || got[1] != protocol.TransitionHeld {
		t.Fatalf("transitions = %v, want held each tick", got)
	}
	c := rec.calls[0]
	if c.DeploymentUID == "" || c.ResourceVersion == "" || c.DeploymentGeneration == nil || c.DeploymentReplicas == nil || *c.DeploymentReplicas != 0 ||
		c.PodUIDs == nil || c.PVCUserPods == nil {
		t.Fatalf("held = %+v", c)
	}
	if n := len(deploymentPatchBodies(client)); n != 0 {
		t.Fatalf("held tick patched %d times", n)
	}
	assertNothingDeleted(t, client)
}

func TestHeldNotSentWhenPodsUnknown(t *testing.T) {
	m, client, rec := hibEnv(t, hibDeployment(0, "op-1"))
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	if err := hibTick(t, m, hibW(protocol.HibernationHibernated, "op-1")); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("held sent with unknown pods: %v", rec.transitions())
	}
	assertNothingDeleted(t, client)
}
