package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Unlike SimpleClientset, these reactors enforce the optimistic mutation seam.
// This is simulated UID/RV evidence, not real-apiserver verification.
func enforcingClient(t *testing.T, objects ...runtime.Object) *fake.Clientset {
	t.Helper()
	for _, object := range objects {
		m, err := meta.Accessor(object)
		if err != nil {
			t.Fatal(err)
		}
		if m.GetUID() == "" {
			m.SetUID(types.UID("uid-" + m.GetName()))
		}
		if m.GetResourceVersion() == "" {
			m.SetResourceVersion("1")
		}
	}
	c := fake.NewSimpleClientset(objects...)
	serial := 100
	c.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create := a.(k8stesting.CreateAction)
		obj := create.GetObject().DeepCopyObject()
		metadata, _ := meta.Accessor(obj)
		serial++
		if metadata.GetUID() == "" {
			metadata.SetUID(types.UID(fmt.Sprintf("created-%d", serial)))
		}
		metadata.SetResourceVersion(strconv.Itoa(serial))
		if err := c.Tracker().Create(a.GetResource(), obj, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	c.PrependReactor("patch", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		action := a.(k8stesting.PatchAction)
		old, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), action.GetName())
		if err != nil {
			return true, nil, err
		}
		metadata, _ := meta.Accessor(old)
		var body struct {
			Metadata struct {
				UID             types.UID
				ResourceVersion string
			}
		}
		if err := json.Unmarshal(action.GetPatch(), &body); err != nil {
			return true, nil, err
		}
		if body.Metadata.UID != metadata.GetUID() || body.Metadata.ResourceVersion != metadata.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: a.GetResource().Group, Resource: a.GetResource().Resource}, action.GetName(), fmt.Errorf("UID/RV precondition"))
		}
		raw, _ := json.Marshal(old)
		patched, err := jsonpatch.MergePatch(raw, action.GetPatch())
		if err != nil {
			return true, nil, err
		}
		obj := old.DeepCopyObject()
		// Unmarshal into an empty annotations map: a removed merge-patch key
		// must not survive from the previous object in the fake response.
		fresh, _ := meta.Accessor(obj)
		fresh.SetAnnotations(nil)
		if err := json.Unmarshal(patched, obj); err != nil {
			return true, nil, err
		}
		next, _ := meta.Accessor(obj)
		serial++
		next.SetResourceVersion(strconv.Itoa(serial))
		if err := c.Tracker().Update(a.GetResource(), obj, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	c.PrependReactor("delete", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		action := a.(k8stesting.DeleteAction)
		obj, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), action.GetName())
		if err != nil {
			return true, nil, err
		}
		metadata, _ := meta.Accessor(obj)
		pre := action.GetDeleteOptions().Preconditions
		if pre == nil {
			return false, nil, nil
		}
		if pre.UID == nil || *pre.UID != metadata.GetUID() || (pre.ResourceVersion != nil && *pre.ResourceVersion != metadata.GetResourceVersion()) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, action.GetName(), fmt.Errorf("delete UID/RV precondition"))
		}
		return true, nil, c.Tracker().Delete(a.GetResource(), a.GetNamespace(), action.GetName())
	})
	return c
}
func onlineEvidence(at time.Time) *protocol.RollEvidence {
	version, nonce := "0.1.0", "prior-registration"
	until := at.Add(time.Minute)
	return &protocol.RollEvidence{Version: 1, OwnerID: "owner-A", Registered: true, SchedulingCapabilities: []string{}, ProtocolCapabilities: []string{}, OrdinaryCoverage: true, RegisteredVersion: &version, RegisterNonce: &nonce, EvaluatedAt: at, ValidUntil: until, LivenessValidUntil: &until}
}
func evidence(w protocol.DesiredWorker, at time.Time) protocol.DesiredWorker {
	w.RollEvidence = onlineEvidence(at)
	return w
}
func (f *fakeCordoner) RequestDrainWithReason(ctx context.Context, id string, reason ...string) error {
	return f.RequestDrain(ctx, id)
}
func (f *fakeCordoner) ClearDrainWithReason(ctx context.Context, id string, reason ...string) error {
	return f.ClearDrain(ctx, id)
}
func (f *fakeCordoner) ConfirmImageRollDrain(ctx context.Context, id string) (protocol.DesiredWorker, error) {
	if err := f.RequestDrainWithReason(ctx, id, "image_roll"); err != nil {
		return protocol.DesiredWorker{}, err
	}
	w, ok := f.confirmWorkers[id]
	if !ok {
		return protocol.DesiredWorker{}, fmt.Errorf("fresh API state not configured")
	}
	return w, nil
}
func templatePatches(c *fake.Clientset) []string {
	ids := []string{}
	for _, a := range c.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "deployments" {
			var body map[string]json.RawMessage
			_ = json.Unmarshal(p.GetPatch(), &body)
			if _, ok := body["spec"]; ok {
				ids = append(ids, p.GetName())
			}
		}
	}
	return ids
}
func currentProof(t *testing.T, c *fake.Clientset, w *protocol.DesiredWorker, at time.Time) {
	t.Helper()
	ns := testConfig().Namespace
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs-" + w.ID, Namespace: ns, UID: types.UID("rs-uid-" + w.ID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &yes}}}}
	if err := c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, ns); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + w.ID, Namespace: ns, UID: types.UID("pod-uid-" + w.ID), Labels: objectLabels(w.ID), Annotations: map[string]string{AnnotationSpecHash: d.Spec.Template.Annotations[AnnotationSpecHash]}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: workerContainerName, Image: workerImage(d), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err := c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, ns); err != nil {
		t.Fatal(err)
	}
	w.RollEvidence = onlineEvidence(at)
	version, nonce, uid := "1.0.0", "new-"+w.ID, string(pod.UID)
	w.RollEvidence.RegisteredVersion = &version
	w.RollEvidence.RegisterNonce = &nonce
	w.RollEvidence.PodUID = &uid
	w.RollEvidence.HeartbeatAt = &at
}
