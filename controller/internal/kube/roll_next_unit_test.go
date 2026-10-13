package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func physicalOld(t *testing.T, d *appsv1.Deployment) []runtime.Object {
	t.Helper()
	d.UID = types.UID("dep-" + d.Name)
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "old-rs-" + d.Name, Namespace: d.Namespace, UID: types.UID("rs-" + d.Name), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &yes}}}}
	image := workerImage(d)
	if image == "" {
		image = "worker:old"
		d.Spec.Template.Spec.Containers = []corev1.Container{{Name: workerContainerName, Image: image}}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-pod-" + d.Name, Namespace: d.Namespace, UID: types.UID("pod-" + d.Name), Labels: d.Labels, Annotations: d.Spec.Template.Annotations, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}}, Spec: d.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: workerContainerName, Image: image, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	return []runtime.Object{d, rs, pod}
}
func rollTickTest(t *testing.T, m *Materializer, workers []protocol.DesiredWorker) {
	t.Helper()
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Reconcile(context.Background(), workers, obs); err != nil {
		t.Fatal(err)
	}
}
func removeOldPod(t *testing.T, c *fake.Clientset, id string) {
	t.Helper()
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), testConfig().Namespace, "old-pod-"+deploymentName(id)); err != nil {
		t.Fatal(err)
	}
}
func targetWaiting(t *testing.T, c *fake.Clientset, id, reason string) {
	t.Helper()
	p, err := c.CoreV1().Pods(testConfig().Namespace).Get(context.Background(), "pod-"+id, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Phase = corev1.PodPending
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	if err = c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
}
func TestRollNextUnitConcurrencyAndStuck(t *testing.T) {
	var workers []protocol.DesiredWorker
	var objects []runtime.Object
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("w%d", i)
		workers = append(workers, evidence(desired(id), m5Now))
		objects = append(objects, physicalOld(t, deployedWorker(id, 0, "old"))...)
	}
	m, c := newMat(t, objects...)
	m.now = func() time.Time { return m5Now }
	m.cfg.RollConcurrency = 2
	rollTickTest(t, m, workers)
	if n := len(templatePatches(c)); n != 1 {
		t.Fatalf("first canary patches=%d", n)
	}
	removeOldPod(t, c, workers[0].ID)
	currentProof(t, c, &workers[0], m5Now)
	rollTickTest(t, m, workers)
	if n := len(templatePatches(c)); n != 3 {
		t.Fatalf("postcanary patches=%d", n)
	}
	removeOldPod(t, c, workers[1].ID)
	currentProof(t, c, &workers[1], m5Now)
	removeOldPod(t, c, workers[2].ID)
	currentProof(t, c, &workers[2], m5Now)
	targetWaiting(t, c, "w3", "ContainerCreating")
	rollTickTest(t, m, workers)
	if n := len(templatePatches(c)); n != 4 {
		t.Fatalf("normal inflight blocked free slot: %d", n)
	}
	targetWaiting(t, c, "w3", "ImagePullBackOff")
	rollTickTest(t, m, workers)
	if n := len(templatePatches(c)); n != 4 {
		t.Fatalf("stuck admitted sibling: %d", n)
	}
	for id, p := range m.PacingOutcomes() {
		if p.BlockingReason == nil || *p.BlockingReason != "ImagePullBackOff" || p.BlockingWorkerID == nil || *p.BlockingWorkerID != "w3" {
			t.Fatalf("%s missing actual cause: %+v", id, p)
		}
	}
	p, _ := c.CoreV1().Pods(m.cfg.Namespace).Get(context.Background(), "pod-w3", metav1.GetOptions{})
	p.Status.Phase = corev1.PodRunning
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if err := c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
	rollTickTest(t, m, workers)
	if n := len(templatePatches(c)); n != 5 {
		t.Fatalf("health recovery did not resume: %d", n)
	}
}
func TestRollNextUnitMappingCorrection(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	m, c := newMat(t, deployedWorker("w1", 0, "old"))
	m.now = func() time.Time { return m5Now }
	m.cfg.RollTargetVersion = "wrong+gstamp"
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	currentProof(t, c, &w, m5Now)
	m.cfg.RollTargetVersion = "1.0.0"
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var marker rollMarker
	if err = json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
		t.Fatal(err)
	}
	if !marker.Completed || marker.Nonce != "prior-registration" || !marker.Canary || len(templatePatches(c)) != 1 {
		t.Fatalf("correction lost baseline or rerolled: %+v", marker)
	}
	if p := m.PacingOutcomes()[w.ID]; p != nil {
		t.Fatalf("historical completion emitted pacing: %+v", p)
	}
}
func TestRollNextUnitOldPhysicalCapacity(t *testing.T) {
	for _, reason := range []string{"absent", "CrashLoopBackOff"} {
		t.Run(reason, func(t *testing.T) {
			workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)}
			objects := physicalOld(t, deployedWorker("w1", 0, "old"))
			peer := physicalOld(t, deployedWorker("w2", 0, "old"))
			if reason == "absent" {
				peer = peer[:2]
			} else {
				p := peer[2].(*corev1.Pod)
				p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
			}
			objects = append(objects, peer...)
			m, c := newMat(t, objects...)
			m.now = func() time.Time { return m5Now }
			workers[1].Busy = true
			rollTickTest(t, m, workers)
			if n := len(templatePatches(c)); n != 0 {
				t.Fatalf("took last physical provider: %d", n)
			}
		})
	}
}

func TestRollNextUnitCanaryProofAndHealth(t *testing.T) {
	for _, mode := range []string{"oldUID", "missingNonce", "priorNonce", "pending", "badImage", "dindFlapping", "valid"} {
		t.Run(mode, func(t *testing.T) {
			workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now), evidence(desired("w3"), m5Now)}
			var objects []runtime.Object
			for _, w := range workers {
				objects = append(objects, physicalOld(t, deployedWorker(w.ID, 0, "old"))...)
			}
			m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objects...)
			m.now = func() time.Time { return m5Now }
			// Same-version replacement still requires a new nonce and current UID.
			m.cfg.RollTargetVersion = "0.1.0"
			rollTickTest(t, m, workers)
			removeOldPod(t, c, "w1")
			currentProof(t, c, &workers[0], m5Now)
			version := "0.1.0"
			workers[0].RollEvidence.RegisteredVersion = &version
			switch mode {
			case "oldUID":
				uid := "old-incarnation"
				workers[0].RollEvidence.PodUID = &uid
			case "missingNonce":
				workers[0].RollEvidence.RegisterNonce = nil
			case "priorNonce":
				nonce := "prior-registration"
				workers[0].RollEvidence.RegisterNonce = &nonce
			case "pending":
				targetWaiting(t, c, "w1", "ContainerCreating")
			case "badImage":
				targetWaiting(t, c, "w1", "ImagePullBackOff")
			case "dindFlapping":
				p, err := c.CoreV1().Pods(m.cfg.Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				p.CreationTimestamp = metav1.NewTime(m5Now.Add(-time.Hour))
				p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: "dind", RestartCount: stuckRestartThreshold, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(m5Now.Add(-time.Minute))}}, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137}}})
				if err = c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "badImage" {
				at := m5Now.Add(-48 * time.Hour)
				reason := "image_roll"
				workers[0].Busy = true
				workers[0].DrainingSince = &at
				workers[0].DrainingReason = &reason
			}
			rollTickTest(t, m, workers)
			n := len(templatePatches(c))
			if mode == "valid" {
				if n != 2 {
					t.Fatalf("valid same-version proof failed: %d", n)
				}
			} else if n != 1 {
				t.Fatalf("%s promoted without proof/health: %d", mode, n)
			}
			if mode == "badImage" {
				for id, p := range m.PacingOutcomes() {
					if p.State != "held" || p.BlockingReason == nil || *p.BlockingReason != "ImagePullBackOff" {
						t.Fatalf("%s lost bad image hold: %+v", id, p)
					}
				}
			}
		})
	}
}
func TestRollNextUnitUnconfirmedReleaseIndependentPool(t *testing.T) {
	var workers []protocol.DesiredWorker
	var objects []runtime.Object
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("w%d", i)
		w := evidence(desired(id), m5Now)
		if i == 4 {
			w.RollEvidence.OwnerID = "owner-B"
			w.Busy = true
		}
		workers = append(workers, w)
		objects = append(objects, physicalOld(t, deployedWorker(id, 0, "old"))...)
	}
	cordon := &fakeCordoner{clearErr: fmt.Errorf("unknown API release")}
	m, c := newMatWithDrain(t, cordon, DrainPolicy{}, objects...)
	m.now = func() time.Time { return m5Now }
	rollTickTest(t, m, workers)
	// Owner A has a normally unresolved canary: its held, drained old peer must be released.
	at := m5Now.Add(-time.Hour)
	reason := "image_roll"
	workers[2].DrainingSince = &at
	workers[2].DrainingReason = &reason
	// The independent owner B can now finish its previously admitted roll.
	workers[3].Busy = false
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := len(templatePatches(c))
	err = m.Reconcile(context.Background(), workers, obs)
	if err == nil {
		t.Fatal("unknown uncordon swallowed")
	}
	if len(cordon.clearCalls) == 0 {
		t.Fatal("uncordon not attempted")
	}
	if len(templatePatches(c)) != before+1 || templatePatches(c)[before] != deploymentName("w4") {
		t.Fatal("failed release did not isolate affected pool")
	}
	// Independent pool isn't marked uncertain: its report reflects its own pin transition.
	p := m.PacingOutcomes()["w4"]
	if p == nil || (p.BlockingWorkerID != nil && *p.BlockingWorkerID == "w3") {
		t.Fatalf("other owner held by failed release: %+v", p)
	}
}
func TestRollNextUnitLateOldTemplateFence(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	m, c := newMat(t, deployedWorker("w1", 0, "old"))
	m.now = func() time.Time { return m5Now }
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	old, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.RollTargetVersion = "2.0.0+g123"
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": old.UID, "resourceVersion": old.ResourceVersion}, "spec": map[string]any{"template": old.Spec.Template}})
	_, err = c.AppsV1().Deployments(m.cfg.Namespace).Patch(context.Background(), old.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err == nil {
		t.Fatal("late old template accepted after pin cancellation")
	}
}
func TestRollNextUnitInvalidOwnerAndFrozenCoverage(t *testing.T) {
	for _, invalid := range []bool{true, false} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			w := evidence(desired("w1"), m5Now)
			d := markedDeployment(t, testConfig(), w, false)
			var marker rollMarker
			_ = json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker)
			if invalid {
				marker.Coverage.Owner = "corrupt-owner"
			} else {
				marker.Coverage.Scheduling = []string{"original"}
			}
			raw, _ := json.Marshal(marker)
			d.Annotations[annotationRoll] = string(raw)
			m, c := newMat(t, d)
			m.now = func() time.Time { return m5Now }
			currentProof(t, c, &w, m5Now)
			// Registration capability changes are legitimate but do not release original coverage.
			rollTickTest(t, m, []protocol.DesiredWorker{w})
			got, _ := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
			_ = json.Unmarshal([]byte(got.Annotations[annotationRoll]), &marker)
			if marker.Completed {
				t.Fatal("invalid owner or missing original capability promoted")
			}
			if invalid && m.PacingOutcomes()[w.ID].State != "waiting_evidence" {
				t.Fatal("invalid owner did not fail closed")
			}
			if !invalid {
				w.RollEvidence.SchedulingCapabilities = []string{"original"}
				rollTickTest(t, m, []protocol.DesiredWorker{w})
				got, _ = c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
				_ = json.Unmarshal([]byte(got.Annotations[annotationRoll]), &marker)
				if !marker.Completed {
					t.Fatal("restored original coverage failed")
				}
			}
		})
	}
}
