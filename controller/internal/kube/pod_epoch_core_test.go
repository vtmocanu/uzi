package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const epochA = "00000000-0000-4000-8000-000000000001"
const epochB = "00000000-0000-4000-8000-000000000002"
const epochC = "00000000-0000-4000-8000-000000000003"
const epochData = "10000000-0000-4000-8000-000000000001"
const epochDinD = "10000000-0000-4000-8000-000000000002"
const epochNewDinD = "10000000-0000-4000-8000-000000000003"

type epochHarness struct {
	t          *testing.T
	client     *fake.Clientset
	session    *PodEpochSession
	generation PodEpochGeneration
	worker     PodEpochWorker
	patches    []map[string]any
}

func epochPod(uid string) *corev1.Pod {
	yes := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "docker", Name: "pod-" + uid, UID: types.UID(uid), ResourceVersion: "opaque/" + uid, Labels: map[string]string{LabelWorkerID: "w"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", UID: "rs-uid", Controller: &yes}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Env: []corev1.EnvVar{{Name: PodEpochRuntimeEnv, Value: "containerd-runc"}}}}, InitContainers: []corev1.Container{{Name: "seed"}}, Volumes: []corev1.Volume{
			{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVCName("w")}}},
			{Name: "dind", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dindDataPVCName("w")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}
func epochTerminal(p *corev1.Pod) *corev1.Pod {
	p = p.DeepCopy()
	p.ResourceVersion += "/terminal"
	p.Status.Phase = corev1.PodSucceeded
	status := func(name string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, ContainerID: "containerd://" + name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}}
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{status("worker")}
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{status("seed")}
	return p
}
func newEpochHarness(t *testing.T, baseline ...*corev1.Pod) *epochHarness {
	t.Helper()
	p := epochPod(epochA)
	yes := true
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName("w"), Namespace: "docker", UID: "dep-uid"}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelWorkerID: "w"}}, Spec: *p.Spec.DeepCopy()}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "docker", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &yes}}}, Spec: appsv1.ReplicaSetSpec{Template: *dep.Spec.Template.DeepCopy()}}
	rs.Spec.Template.Labels["pod-template-hash"] = "generated"
	objects := []runtime.Object{dep, rs,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataPVCName("w"), Namespace: "docker", UID: epochData}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "data-pv"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dindDataPVCName("w"), Namespace: "docker", UID: epochDinD}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "dind-pv"}}}
	client := fake.NewClientset(objects...)
	h := &epochHarness{t: t, client: client, worker: PodEpochWorker{ID: "w", Runtime: "containerd-runc"}}
	client.PrependReactor("patch", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		patch := a.(ktesting.PatchAction)
		if patch.GetPatchType() != types.MergePatchType {
			t.Error("not MergePatch")
		}
		var body map[string]any
		if err := json.Unmarshal(patch.GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 {
			t.Fatalf("nonmetadata patch: %s", patch.GetPatch())
		}
		m := body["metadata"].(map[string]any)
		if len(m) != 3 || m["uid"] == nil || m["resourceVersion"] == nil {
			t.Fatalf("metadata fields: %s", patch.GetPatch())
		}
		annotations := m["annotations"].(map[string]any)
		if len(annotations) != 1 || annotations[PodEpochAnnotation] == nil {
			t.Fatalf("annotations: %s", patch.GetPatch())
		}
		if _, _, err := DecodePodEpochDocument([]byte(annotations[PodEpochAnnotation].(string))); err != nil {
			t.Fatal(err)
		}
		h.patches = append(h.patches, body)
		return true, &corev1.Pod{}, nil
	})
	h.session = NewPodEpochSession(client, PodEpochKubeResolver{Client: client}, []PodEpochWorker{h.worker})
	pods := make([]corev1.Pod, len(baseline))
	for i, p := range baseline {
		pods[i] = *p
	}
	g, err := h.session.Reset(context.Background(), pods)
	if err != nil {
		t.Fatal(err)
	}
	h.generation = g
	return h
}
func (h *epochHarness) event(event watch.EventType, p *corev1.Pod) *PodEpochDecision {
	h.t.Helper()
	d, err := h.session.Handle(context.Background(), h.generation, event, p)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}
func (h *epochHarness) assertDecision(d *PodEpochDecision, positive bool) *PodEpochEvidence {
	h.t.Helper()
	if d == nil {
		h.t.Fatal("missing decision")
	}
	e, n, err := DecodePodEpochDocument([]byte(d.Document()))
	if err != nil {
		h.t.Fatal(err)
	}
	if (e != nil) != positive {
		h.t.Fatalf("positive=%v document=%s", positive, d.Document())
	}
	if !positive && n == nil {
		h.t.Fatal("no negative")
	}
	if err := h.session.Deliver(context.Background(), d); err != nil {
		h.t.Fatal(err)
	}
	patch := h.patches[len(h.patches)-1]["metadata"].(map[string]any)
	if patch["uid"] != d.uid || patch["resourceVersion"] != d.rv {
		h.t.Fatal("decision retargeted")
	}
	for _, a := range h.client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource != "pods" {
			h.t.Fatal("PVC evidence writer")
		}
	}
	return e
}
func TestPodEpochGracefulAndSharedFixture(t *testing.T) {
	h := newEpochHarness(t, epochPod(epochA))
	x := epochTerminal(epochPod(epochA))
	h.event(watch.Modified, x)
	h.event(watch.Deleted, x)
	d := h.event(watch.Added, epochPod(epochB))
	e := h.assertDecision(d, true)
	if e.Predecessor != epochA || e.Successor != epochB || e.Runtime != "containerd-runc" {
		t.Fatal(e)
	}
	// Normalize only nondeterministic session/time fields; every identity and
	// terminal entry is produced by the handler through the live fake client.
	e.Session = "20000000-0000-4000-8000-000000000001"
	e.SessionStarted = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	e.TerminalObserved = e.SessionStarted.Add(time.Second)
	e.SuccessorAdded = e.SessionStarted.Add(2 * time.Second)
	b, err := EncodePodEpochEvidence(*e)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../../fixtures/pod-epoch/positive.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != strings.TrimSpace(string(fixture)) {
		t.Fatalf("fixture mismatch; produced:\n%s", b)
	}
	if err := h.session.Deliver(context.Background(), d); err == nil || len(h.patches) != 1 {
		t.Fatal("retried delivery")
	}
	if h.event(watch.Added, epochPod(epochB)) != nil {
		t.Fatal("duplicate successor")
	}
}
func TestPodEpochRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{"eviction", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "Evicted" }},
		{"podgc", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "NodeLost" }},
		{"podgc-status", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown"
		}},
		{"waiting", func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}
		}},
		{"missing", func(p *corev1.Pod) { p.Status.ContainerStatuses = nil }},
		{"wrong-name", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "other" }},
		{"duplicate-status", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		}},
		{"duplicate-spec", func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, p.Spec.Containers[0]) }},
		{"missing-init", func(p *corev1.Pod) { p.Status.InitContainerStatuses = nil }},
		{"extra-init", func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, p.Status.InitContainerStatuses[0])
		}},
		{"ephemeral-spec", func(p *corev1.Pod) { p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} }},
		{"ephemeral-status", func(p *corev1.Pod) { p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug"}} }},
		{"runtimeclass", func(p *corev1.Pod) { v := ""; p.Spec.RuntimeClassName = &v }},
		{"host-pid", func(p *corev1.Pod) { p.Spec.HostPID = true }},
		{"shared-pid", func(p *corev1.Pod) { v := true; p.Spec.ShareProcessNamespace = &v }},
		{"no-id", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" }},
		{"negative-restarts", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = -1 }},
		{"minus-one", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = -1 }},
		{"137", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 137 }},
		{"255", func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 255 }},
		{"unknown", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown" }},
		{"sidecar", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar"})
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: "sidecar", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{}}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			x := epochTerminal(epochPod(epochA))
			tt.mutate(x)
			h.event(watch.Modified, x)
			h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
		})
	}
	t.Run("force-delete", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Deleted, epochPod(epochA))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	for _, mode := range []string{"host-pid", "shared-pid"} {
		t.Run("successor-"+mode, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			b := epochPod(epochB)
			if mode == "host-pid" {
				b.Spec.HostPID = true
			} else {
				v := true
				b.Spec.ShareProcessNamespace = &v
			}
			h.assertDecision(h.event(watch.Added, b), false)
		})
		t.Run("foreign-before-"+mode, func(t *testing.T) {
			w := epochPod(epochC)
			w.Labels = map[string]string{}
			if mode == "host-pid" {
				w.Spec.HostPID = true
			} else {
				v := true
				w.Spec.ShareProcessNamespace = &v
			}
			h := newEpochHarness(t, epochPod(epochA), w)
			h.event(watch.Modified, epochTerminal(w))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
		})
	}
	t.Run("private-pid-explicit-false", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		x := epochTerminal(epochPod(epochA))
		v := false
		x.Spec.ShareProcessNamespace = &v
		h.event(watch.Modified, x)
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("signal-after-add", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		d := h.event(watch.Added, epochPod(epochB))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(d, false)
	})
}
func TestPodEpochHistory(t *testing.T) {
	t.Run("baseline-not-successor", func(t *testing.T) {
		h := newEpochHarness(t, epochTerminal(epochPod(epochA)), epochPod(epochB))
		if h.event(watch.Added, epochPod(epochB)) != nil {
			t.Fatal("baseline successor")
		}
	})
	t.Run("duplicate-terminal", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		x := epochTerminal(epochPod(epochA))
		h.event(watch.Modified, x)
		h.event(watch.Modified, x)
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("label-drift", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		x := epochPod(epochA)
		x.Labels[LabelWorkerID] = "other"
		h.event(watch.Modified, x)
		x = epochTerminal(epochPod(epochA))
		h.event(watch.Modified, x)
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
	})
	t.Run("unseen-B", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.event(watch.Modified, epochPod(epochB))
		h.assertDecision(h.event(watch.Added, epochPod(epochC)), false)
	})
	t.Run("unevidenced-B", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), false)
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochC)), false)
	})
	t.Run("B-terminal-C-does-not-skip", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
		h.event(watch.Modified, epochTerminal(epochPod(epochB)))
		e := h.assertDecision(h.event(watch.Added, epochPod(epochC)), true)
		if e.Predecessor != epochB {
			t.Fatal("skipped B")
		}
	})
	for _, foreign := range []bool{false, true} {
		for _, order := range []string{"running", "before", "after", "deleted"} {
			t.Run(fmt.Sprintf("concurrent-foreign=%v-%s", foreign, order), func(t *testing.T) {
				w := epochPod(epochC)
				if foreign {
					w.Labels = map[string]string{}
				}
				h := newEpochHarness(t, epochPod(epochA), w)
				if order == "before" {
					h.event(watch.Modified, epochTerminal(w))
				}
				if order == "deleted" {
					h.event(watch.Deleted, w)
				}
				h.event(watch.Modified, epochTerminal(epochPod(epochA)))
				if order == "after" {
					h.event(watch.Modified, epochTerminal(w))
				}
				// A modified after the ambiguous baseline establishes no new ordering;
				// labeled baseline overlap remains ambiguous, hence always refuses.
				h.assertDecision(h.event(watch.Added, epochPod(epochB)), foreign && order == "before")
			})
		}
	}
}
func TestPodEpochLiveIdentity(t *testing.T) {
	for _, name := range []string{"rs-owner", "pod-owner", "template", "pvc-name", "pvc-uid", "data-pv", "runtime-env", "runtime-valueFrom", "runtime-duplicate", "runtimeclass", "annotation-forgery"} {
		t.Run(name, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			p := epochPod(epochB)
			ctx := context.Background()
			switch name {
			case "rs-owner":
				rs, _ := h.client.AppsV1().ReplicaSets("docker").Get(ctx, "rs", metav1.GetOptions{})
				rs.OwnerReferences[0].UID = "different"
				_, _ = h.client.AppsV1().ReplicaSets("docker").Update(ctx, rs, metav1.UpdateOptions{})
			case "pod-owner":
				p.OwnerReferences[0].UID = "different"
			case "template":
				dep, _ := h.client.AppsV1().Deployments("docker").Get(ctx, deploymentName("w"), metav1.GetOptions{})
				dep.Spec.Template.Spec.Containers[0].Image = "new"
				_, _ = h.client.AppsV1().Deployments("docker").Update(ctx, dep, metav1.UpdateOptions{})
			case "pvc-name":
				p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "wrong"
			case "pvc-uid", "data-pv":
				c, _ := h.client.CoreV1().PersistentVolumeClaims("docker").Get(ctx, dataPVCName("w"), metav1.GetOptions{})
				if name == "pvc-uid" {
					c.UID = epochNewDinD
				} else {
					c.Spec.VolumeName = "new-pv"
				}
				_, _ = h.client.CoreV1().PersistentVolumeClaims("docker").Update(ctx, c, metav1.UpdateOptions{})
			case "runtime-env":
				p.Spec.Containers[0].Env[0].Value = "crio-crun"
			case "runtime-valueFrom":
				p.Spec.Containers[0].Env[0].ValueFrom = &corev1.EnvVarSource{}
			case "runtime-duplicate":
				p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, p.Spec.Containers[0].Env[0])
			case "runtimeclass":
				v := "other"
				p.Spec.RuntimeClassName = &v
			case "annotation-forgery":
				p.OwnerReferences = nil
				p.Annotations = map[string]string{PodEpochAnnotation: "copied proof"}
			}
			h.assertDecision(h.event(watch.Added, p), false)
		})
	}
	t.Run("old-template-predecessor", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		x := epochTerminal(epochPod(epochA))
		x.OwnerReferences[0].Name = "old-rs"
		h.event(watch.Modified, x)
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
}
func TestPodEpochMaintenanceRemint(t *testing.T) {
	for _, receipt := range []bool{false, true} {
		t.Run(fmt.Sprint(receipt), func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			c, _ := h.client.CoreV1().PersistentVolumeClaims("docker").Get(context.Background(), dindDataPVCName("w"), metav1.GetOptions{})
			c.UID = epochNewDinD
			_, _ = h.client.CoreV1().PersistentVolumeClaims("docker").Update(context.Background(), c, metav1.UpdateOptions{})
			if receipt {
				if err := h.session.RecordMaintenanceRemint(h.generation, "w", PodEpochRemint{OperationID: "maintenance-1", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}); err != nil {
					t.Fatal(err)
				}
			}
			e := h.assertDecision(h.event(watch.Added, epochPod(epochB)), receipt)
			if receipt && (e.Maintenance == nil || e.Maintenance.OperationID != "maintenance-1") {
				t.Fatal("receipt lost")
			}
		})
	}
}

type epochBlockingResolver struct {
	delegate PodEpochIdentityResolver
	entered  chan struct{}
	release  chan struct{}
}

func (r epochBlockingResolver) ResolvePodEpoch(ctx context.Context, w PodEpochWorker, p *corev1.Pod, successor bool) (PodEpochIdentity, error) {
	if successor {
		close(r.entered)
		<-r.release
	}
	return r.delegate.ResolvePodEpoch(ctx, w, p, successor)
}
func TestPodEpochGenerationAndDelivery(t *testing.T) {
	t.Run("reset-during-identity", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		r := epochBlockingResolver{delegate: PodEpochKubeResolver{Client: h.client}, entered: make(chan struct{}), release: make(chan struct{})}
		h.session.resolver = r
		result := make(chan error, 1)
		go func() {
			d, err := h.session.Handle(context.Background(), h.generation, watch.Added, epochPod(epochB))
			if d != nil {
				err = errors.New("stale decision returned")
			}
			result <- err
		}()
		<-r.entered
		// Empty baseline does not need the event lock held by the pending resolution.
		g, err := h.session.Reset(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		close(r.release)
		if err := <-result; err == nil {
			t.Fatal("reset did not invalidate resolution")
		}
		if _, err := h.session.Handle(context.Background(), h.generation, watch.Added, epochPod(epochC)); err == nil {
			t.Fatal("stale event replay")
		}
		h.generation = g
	})
	for _, mode := range []string{"reset", "cancel", "conflict", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			h := newEpochHarness(t, epochPod(epochA))
			h.event(watch.Modified, epochTerminal(epochPod(epochA)))
			d := h.event(watch.Added, epochPod(epochB))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requests := 0
			h.client.PrependReactor("patch", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				requests++
				if mode == "conflict" {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, d.name, errors.New("conflict"))
				}
				return true, nil, context.DeadlineExceeded
			})
			if mode == "reset" {
				_, _ = h.session.Reset(ctx, []corev1.Pod{*epochPod(epochB)})
			}
			if mode == "cancel" {
				cancel()
			}
			if err := h.session.Deliver(ctx, d); err == nil {
				t.Fatal("expected refusal/error")
			}
			if err := h.session.Deliver(context.Background(), d); err == nil {
				t.Fatal("retry allowed")
			}
			want := 1
			if mode == "reset" || mode == "cancel" {
				want = 0
			}
			if requests != want {
				t.Fatalf("requests=%d want=%d", requests, want)
			}
		})
	}
	t.Run("concurrent-delivery", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		d := h.event(watch.Added, epochPod(epochB))
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = h.session.Deliver(context.Background(), d) }()
		}
		wg.Wait()
		if len(h.patches) != 1 {
			t.Fatal("multiple requests")
		}
	})
}
func TestPodEpochBounds(t *testing.T) {
	t.Run("identities", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		// 1 pod + 2 claim identities leave 4093 distinct pod slots.
		for i := 0; i < 4093; i++ {
			p := epochPod(fmt.Sprintf("30000000-0000-4000-8000-%012d", i))
			p.Labels = nil
			p.Spec.Volumes = nil
			h.event(watch.Added, p)
		}
		if h.session.overflow {
			t.Fatal("overflow below bound")
		}
		h.event(watch.Added, epochPod(epochB))
		if !h.session.overflow {
			t.Fatal("identity overflow not latched")
		}
		if h.event(watch.Added, epochPod(epochC)) != nil {
			t.Fatal("positive after overflow")
		}
		g, err := h.session.Reset(context.Background(), []corev1.Pod{*epochPod(epochA)})
		if err != nil {
			t.Fatal(err)
		}
		h.generation = g
		h.event(watch.Modified, epochTerminal(epochPod(epochA)))
		h.assertDecision(h.event(watch.Added, epochPod(epochB)), true)
	})
	t.Run("retained-bytes", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		p := epochPod(epochC)
		p.Labels = nil
		p.Spec.Volumes = nil
		p.Annotations = map[string]string{"large": strings.Repeat("x", PodEpochMaxMetadata)}
		h.event(watch.Added, p)
		if !h.session.overflow {
			t.Fatal("metadata cap")
		}
	})
	t.Run("producer-8KiB", func(t *testing.T) {
		h := newEpochHarness(t, epochPod(epochA))
		p := epochTerminal(epochPod(epochA))
		p.Status.ContainerStatuses[0].ContainerID = strings.Repeat("x", PodEpochMaxDocument)
		h.event(watch.Modified, p)
		d := h.event(watch.Added, epochPod(epochB))
		h.assertDecision(d, false)
		if len(d.Document()) > PodEpochMaxDocument {
			t.Fatal("oversize negative")
		}
	})
}
func TestPodEpochContractCorpus(t *testing.T) {
	roots := []struct {
		dir   string
		valid bool
	}{{"valid", true}, {"invalid", false}}
	for _, root := range roots {
		files, err := filepath.Glob("../../../fixtures/pod-epoch/" + root.dir + "/*.json")
		if err != nil || len(files) == 0 {
			t.Fatal("missing corpus", err)
		}
		for _, file := range files {
			t.Run(filepath.Base(file), func(t *testing.T) {
				b, err := os.ReadFile(file) //nolint:gosec // Paths come only from Glob in the fixed repository contract corpus.
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = DecodePodEpochDocument(b)
				if (err == nil) != root.valid {
					t.Fatalf("valid=%v err=%v", root.valid, err)
				}
			})
		}
	}
	b, err := os.ReadFile("../../../fixtures/pod-epoch/positive.json")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err := DecodePodEpochDocument(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name string
		fn   func(*PodEpochEvidence)
	}{
		{"uuid", func(e *PodEpochEvidence) { e.Successor = "AAAAAAAA-0000-4000-8000-000000000002" }},
		{"runtime", func(e *PodEpochEvidence) { e.Runtime = "containerd" }},
		{"version", func(e *PodEpochEvidence) { e.Version = 2 }},
		{"entry-duplicate", func(e *PodEpochEvidence) { e.Containers = append(e.Containers, e.Containers[0]) }},
		{"reverse-time", func(e *PodEpochEvidence) { e.SuccessorAdded = e.SessionStarted }},
		{"maintenance", func(e *PodEpochEvidence) {
			e.Maintenance = &PodEpochRemint{OperationID: "op", OldClaimUID: epochDinD, NewClaimUID: epochNewDinD}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			copy := *e
			change.fn(&copy)
			if _, err := EncodePodEpochEvidence(copy); err == nil {
				t.Fatal("invalid producer document")
			}
		})
	}
	// All documents above are consumed through the same exported strict decoder.
	if !reflect.DeepEqual(e.Data, PodEpochClaim{Name: dataPVCName("w"), UID: epochData, VolumeName: "data-pv"}) {
		t.Fatal(e.Data)
	}
}
