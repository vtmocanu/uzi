package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/apiclient"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Exercise all safety decisions through Reconcile rather than observation helpers.
func TestDinDAuthoritativeSafety(t *testing.T) {
	for _, mode := range []string{"foreign-deployment", "foreign-pvc", "incomplete-pods", "stale-ready", "foreign-ready", "wrong-owner", "replacement-pvc", "missing-objects", "dangling-replicaset", "old-replicaset", "wrong-replicaset-uid", "terminating-replicaset", "replicaset-list-denied"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cfg := dockerTestConfig()
			w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true}
			spec, err := testResolver(t).Resolve(w.Template, w.Size)
			if err != nil {
				t.Fatal(err)
			}
			dep := RenderDeployment(cfg, w, spec)
			dep.UID = "new-deployment"
			var pvc *corev1.PersistentVolumeClaim
			for _, p := range RenderPVCs(cfg, w, spec) {
				if p.Name == dindDataPVCName(w.ID) {
					pvc = p
				}
			}
			pvc.UID = "new-pvc"
			pvc.Status.Phase = corev1.ClaimBound
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: cfg.DockerNamespace, UID: "new-pod", Labels: map[string]string{}, Annotations: dep.Spec.Template.Annotations}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			for k, v := range dep.Spec.Template.Labels {
				pod.Labels[k] = v
			}
			stampMaintenancePodOwner(pod, dep.Name)
			op := &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "register", DeploymentUID: "old-deployment", PVCUID: "old-pvc", Phase: "recycling", Fenced: true}
			switch mode {
			case "foreign-deployment":
				dep.Labels = nil
				dep.UID = "old-deployment"
				op.Phase = "stopping"
			case "foreign-pvc":
				pvc.Labels = nil
				pvc.UID = "old-pvc"
			case "foreign-ready":
				delete(pod.Labels, LabelManagedBy)
			case "wrong-owner":
				pod.OwnerReferences[0].Name = "other-hash"
			}
			rs := maintenanceReplicaSet(pod, dep)
			switch mode {
			case "wrong-owner":
				rs.Name = dep.Name + "-hash"
			case "old-replicaset":
				rs.OwnerReferences[0].UID = "old-deployment"
			case "wrong-replicaset-uid":
				rs.UID = "other-replicaset"
			case "terminating-replicaset":
				at := metav1.Now()
				rs.DeletionTimestamp = &at
			}
			objs := []runtime.Object{dep, pvc, pod, rs}
			if mode == "dangling-replicaset" {
				objs = []runtime.Object{dep, pvc, pod}
			}
			if mode == "missing-objects" {
				objs = nil
				op.Phase = "stopping"
			}
			m, client := newMat(t, objs...)
			m.cfg = cfg
			f := &maintenanceFake{op: op}
			m.cordoner = f
			w.DindMaintenance = op
			obs, err := m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stale-ready" {
				pod.Status.Conditions = nil
				if _, err = client.CoreV1().Pods(cfg.DockerNamespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replicaset-list-denied" {
				client.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("denied")
				})
			}
			if mode == "incomplete-pods" {
				client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
				})
			}
			client.ClearActions()
			err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs)
			wantErr := mode == "foreign-deployment" || mode == "foreign-pvc" || mode == "incomplete-pods"
			if (err != nil) != wantErr {
				t.Fatalf("error=%v wantError=%v", err, wantErr)
			}
			if mode == "missing-objects" {
				if f.op.Phase != "recycling" {
					t.Fatal("authoritative absence did not advance")
				}
			} else if mode == "replacement-pvc" {
				if f.op.Phase != "complete" {
					t.Fatal("safe external replacement did not converge")
				}
			} else if f.op.Phase != op.Phase {
				t.Fatal("unqualified replacement completed")
			}
			for _, a := range client.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatal("deleted foreign or replacement identity")
				}
			}
		})
	}
}

func TestDinDSharedRecycleHelperFailsClosed(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, Busy: true}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	dep := RenderDeployment(cfg, w, spec)
	objs := []runtime.Object{dep}
	for _, p := range RenderPVCs(cfg, w, spec) {
		objs = append(objs, p)
	}
	m, client := newMat(t, objs...)
	m.cfg = cfg
	m.drain.ForceRoll = true
	obs, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, vols := range []recycleVolumes{{DinD: true}, {Nix: true, Data: true, DinD: true}} {
		client.ClearActions()
		if err = m.recycleWorkerVolumes(ctx, w, obs[0], cfg.DockerNamespace, vols); err != nil {
			t.Fatal(err)
		}
		if len(client.Actions()) != 0 {
			t.Fatal("DinD helper bypassed fenced lifecycle")
		}
	}
}

func TestDinDActualClientRefusalPreservesLegacy(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(status) }))
			defer server.Close()
			cfg := dockerTestConfig()
			w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DiskPressure: true, DiskPressureVolumes: []string{"dind"}}
			spec, err := testResolver(t).Resolve(w.Template, w.Size)
			if err != nil {
				t.Fatal(err)
			}
			dep := RenderDeployment(cfg, w, spec)
			dep.UID = "old-deployment"
			objs := []runtime.Object{dep}
			for _, p := range RenderPVCs(cfg, w, spec) {
				p.UID = "old-pvc"
				objs = append(objs, p)
			}
			m, client := newMat(t, objs...)
			m.cfg = cfg
			m.cordoner = apiclient.New(server.URL, "token", time.Second, nil, nil)
			obs, err := m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
				t.Fatal(err)
			}
			deleted := 0
			for _, a := range client.Actions() {
				if a.GetVerb() == "delete" {
					deleted++
					if a.GetResource().Resource == "persistentvolumeclaims" && a.(k8stesting.DeleteAction).GetName() == dindDataPVCName(w.ID) {
						t.Fatal("legacy destroyed DinD")
					}
				}
			}
			if deleted != 3 {
				t.Fatalf("legacy boolean changed: %d deletions", deleted)
			}
		})
	}
}

func TestDinDActualClientReadiness404RetainsFence(t *testing.T) {
	ctx := context.Background()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", Docker: true, DindMaintenance: &protocol.DindMaintenance{ID: "operation", Nonce: "nonce", RegisterNonce: "register", DeploymentUID: "old", PVCUID: "old", Phase: "recycling", Fenced: true}}
	spec, err := testResolver(t).Resolve(w.Template, w.Size)
	if err != nil {
		t.Fatal(err)
	}
	dep := RenderDeployment(cfg, w, spec)
	dep.UID = "replacement"
	var pvc *corev1.PersistentVolumeClaim
	for _, p := range RenderPVCs(cfg, w, spec) {
		if p.Name == dindDataPVCName(w.ID) {
			pvc = p
		}
	}
	pvc.UID = "replacement-pvc"
	pvc.Status.Phase = corev1.ClaimBound
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: cfg.DockerNamespace, UID: "replacement-pod", Labels: dep.Spec.Template.Labels, Annotations: dep.Spec.Template.Annotations}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	stampMaintenancePodOwner(pod, dep.Name)
	transitions := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/controller/status" {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		transitions++
		_ = json.NewEncoder(rw).Encode(w.DindMaintenance)
	}))
	defer server.Close()
	m, _ := newMat(t, dep, pvc, pod, maintenanceReplicaSet(pod, dep))
	m.cfg = cfg
	m.cordoner = apiclient.New(server.URL, "token", time.Second, nil, nil)
	obs, err := m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(ctx, []protocol.DesiredWorker{w}, obs); err == nil {
		t.Fatal("missing readiness publication accepted")
	}
	if transitions != 0 {
		t.Fatal("completion attempted after 404")
	}
}
