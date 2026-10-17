package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

func TestSerialCompletedPeerDrift(t *testing.T) {
	for _, size := range []bool{false, true} {
		t.Run(map[bool]string{false: "generation", true: "size"}[size], func(t *testing.T) {
			w1, w2 := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
			d1, d2 := markedDeployment(t, testConfig(), w1, true), markedDeployment(t, testConfig(), w2, false)
			d2.Annotations = nil
			m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, d1, d2)
			currentProof(t, c, &w1, m5Now)
			currentProof(t, c, &w2, m5Now)
			if size {
				w2.Size = "l"
			} else {
				w2.Generation++
			}
			workers := []protocol.DesiredWorker{w1, w2}
			rollTickTest(t, matrixRestart(m), workers)
			peer, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), d2.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			r, err := decodeRoll(peer.Annotations[annotationRoll])
			if err != nil || r == nil || !r.Canary || r.Completed {
				t.Fatalf("peer drift did not reopen a durable canary: %v %+v", err, r)
			}
			rollTickTest(t, matrixRestart(m), workers)
			deps, err := c.AppsV1().Deployments(m.cfg.Namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			canaries := 0
			for _, dep := range deps.Items {
				marker, err := decodeRoll(dep.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				if marker != nil && marker.Canary {
					canaries++
				}
			}
			if canaries != 1 {
				t.Fatalf("restart changed deterministic canary count: %d", canaries)
			}
		})
	}
}

func TestSerialCurrentIndividualCoverage(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		t.Run(fmt.Sprint(frozen), func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 2)
			w := evidence(desired("w3"), m5Now)
			spec, err := m.resolver.Resolve(w.Template, w.Size)
			if err != nil {
				t.Fatal(err)
			}
			d := RenderDeployment(m.cfg, w, spec)
			if frozen {
				d = markedDeployment(t, m.cfg, w, false)
				r, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				r.Coverage.Scheduling = []string{"obsolete"}
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				d.Annotations[annotationRoll] = string(raw)
			}
			d.UID = "current-dep"
			d.ResourceVersion = "1"
			if err := c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("deployments"), d, m.cfg.Namespace); err != nil {
				t.Fatal(err)
			}
			currentProof(t, c, &w, m5Now)
			w.RollEvidence.SchedulingCapabilities = []string{"a", "b"}
			for i := range workers {
				workers[i].RollEvidence.SchedulingCapabilities = []string{"a"}
			}
			// One old peer is offline; the independently current worker is the only cover.
			workers[1].RollEvidence.LivenessValidUntil = nil
			workers[1].Busy = true
			workers = append(workers, w)
			rollTickTest(t, m, workers)
			if len(templatePatches(c)) != 1 {
				t.Fatalf("current individual did not cover old pool: %v", templatePatches(c))
			}
		})
	}
}

func TestSerialUnknownCurrentCannotCoverOld(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 2)
	spec, err := m.resolver.Resolve(workers[0].Template, workers[0].Size)
	if err != nil {
		t.Fatal(err)
	}
	d := RenderDeployment(m.cfg, workers[0], spec)
	d.UID = "target-dep"
	d.ResourceVersion = "1"
	removeOldPod(t, c, "w1")
	if err := c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), d, m.cfg.Namespace); err != nil {
		t.Fatal(err)
	}
	stale := workers[0].RollEvidence
	stale.PodUID = token("old-process-pod")
	stale.HeartbeatAt = &m5Now
	currentProof(t, c, &workers[0], m5Now)
	workers[0].RollEvidence = stale
	workers[0].RollEvidence.SchedulingCapabilities = []string{"z"}
	workers[1].RollEvidence.SchedulingCapabilities = []string{"a"}
	rollTickTest(t, m, workers)
	if len(templatePatches(c)) != 0 {
		t.Fatalf("stale API target pod admitted last real provider: %v", templatePatches(c))
	}
}

func TestSerialLegacyOldLiveness(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 2)
	workers[1].RollEvidence.Registered = false
	workers[1].RollEvidence.RegisterNonce = nil
	rollTickTest(t, m, workers)
	if patches := templatePatches(c); len(patches) != 1 || patches[0] != deploymentName("w1") {
		t.Fatalf("legacy physical old liveness rejected: %v", patches)
	}
}

func TestSerialDeadlineRestartThenIdle(t *testing.T) {
	for _, recycle := range []bool{false, true} {
		t.Run(fmt.Sprintf("recycle=%t", recycle), func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 3)
			for i := range workers {
				workers[i].Busy = true
			}
			if recycle {
				workers[0].Size = "l"
			}
			since := m5Now.Add(-48 * time.Hour)
			workers[0].DrainingSince = &since
			workers[0].DrainingReason = token("image_roll")
			before, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rollTickTest(t, m, workers)
			rollTickTest(t, matrixRestart(m), workers)
			assertBusyDriftUnchanged(t, c, before)
			// Idle still obeys serial admission: only the reserved canary may
			// install a template; its unproven replacement fences siblings.
			for i := range workers {
				workers[i].Busy = false
			}
			rollTickTest(t, matrixRestart(m), workers)
			if len(templatePatches(c)) != 1 {
				t.Fatalf("idle canary did not roll: %v", templatePatches(c))
			}
			rollTickTest(t, matrixRestart(m), workers)
			matrixNoSibling(t, c)
		})
	}
}

func TestSerialIdleRestartDesiredFences(t *testing.T) {
	for _, mode := range []string{"generation", "custody"} {
		t.Run(mode, func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 3)
			for i := range workers {
				workers[i].Busy = true
			}
			rollTickTest(t, m, workers)
			data, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			workers[0].Busy = false
			if mode == "generation" {
				workers[0].Generation = 1
			} else {
				workers[0].CustodyHeld = true
			}
			rollTickTest(t, matrixRestart(m), workers)
			if patches := templatePatches(c); len(patches) != 1 || patches[0] != deploymentName("w1") {
				t.Fatalf("idle reservation failed serial turn: %v", patches)
			}
			dep, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "generation" && dep.Spec.Template.Annotations[AnnotationGeneration] != "1" {
				t.Fatal("restart installed the obsolete reserved generation")
			}
			retained, err := c.CoreV1().PersistentVolumeClaims(m.cfg.Namespace).Get(context.Background(), data.Name, metav1.GetOptions{})
			if err != nil || retained.UID != data.UID {
				t.Fatalf("idle custody/generation turn replaced protected data: %v", err)
			}
			rollTickTest(t, matrixRestart(m), workers)
			matrixNoSibling(t, c)
		})
	}
}

func TestSerialOldWorkerDeadlineWithoutPromotionProof(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 2)
	for i := range workers {
		workers[i].Busy = true
		workers[i].RollEvidence.ValidUntil = workers[i].RollEvidence.EvaluatedAt
		workers[i].RollEvidence.HeartbeatAt = nil
		workers[i].RollEvidence.PodUID = nil
	}
	since := m5Now.Add(-2 * time.Hour)
	workers[0].DrainingSince = &since
	workers[0].DrainingReason = token("image_roll")
	before, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = m.Reconcile(context.Background(), workers, obs)
	if err != nil {
		t.Fatal(err)
	}
	assertBusyDriftUnchanged(t, c, before)
}

func TestSerialSameImageLegacyTemplateCapacity(t *testing.T) {
	var objects []runtime.Object
	var workers []protocol.DesiredWorker
	for _, id := range []string{"w1", "w2"} {
		w := evidence(desired(id), m5Now)
		w.RollEvidence.RegisteredVersion = token("1.0.0")
		w.RollEvidence.ValidUntil = w.RollEvidence.EvaluatedAt
		d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
		d.UID, d.ResourceVersion = types.UID("old-"+id), "1"
		d.Spec.Template.Annotations[AnnotationSpecHash] = "legacy-template-without-incarnation-env"
		objects = append(objects, physicalOld(t, d)...)
		workers = append(workers, w)
	}
	m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objects...)
	rollTickTest(t, m, workers)
	if len(templatePatches(c)) != 1 {
		t.Fatalf("first same-image controller upgrade could not retain legacy API capacity: %v", templatePatches(c))
	}
	rollTickTest(t, matrixRestart(m), workers)
	matrixNoSibling(t, c)
}

func TestSerialAdmittedSameImageAcrossPools(t *testing.T) {
	var objects []runtime.Object
	var workers []protocol.DesiredWorker
	for i, id := range []string{"w1", "w2"} {
		w := evidence(desired(id), m5Now)
		w.RollEvidence.SchedulingCapabilities = []string{fmt.Sprintf("cap%d", i)}
		d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
		d.UID, d.ResourceVersion = types.UID("current-"+id), "1"
		objects = append(objects, d)
		workers = append(workers, w)
	}
	m, c := newMatWithDrain(t, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objects...)
	for i := range workers {
		currentProof(t, c, &workers[i], m5Now)
		workers[i].RollEvidence.SchedulingCapabilities = []string{fmt.Sprintf("cap%d", i)}
		workers[i].Generation++
	}
	rollTickTest(t, m, workers)
	if len(templatePatches(c)) != 1 {
		t.Fatalf("same-image independent pools removed all lane providers: %v", templatePatches(c))
	}
}

func TestSerialConfirmedMirrorDoesNotRequireNoopRevision(t *testing.T) {
	m, c, workers, _ := matrixFixture(t, 2)
	d := markedDeployment(t, testConfig(), workers[0], false)
	r, err := decodeRoll(d.Annotations[annotationRoll])
	if err != nil {
		t.Fatal(err)
	}
	p := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "retained-data", Namespace: m.cfg.Namespace, UID: "claim-uid", ResourceVersion: "8", Annotations: map[string]string{annotationRoll: d.Annotations[annotationRoll]}}}
	c.PrependReactor("patch", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		// The real apiserver may return an unchanged revision for a no-op.
		return true, p.DeepCopy(), nil
	})
	e := &rollEntry{w: workers[0], ns: m.cfg.Namespace, marker: r, claim: p}
	if err := m.retainRollReservation(context.Background(), e); err != nil {
		t.Fatalf("committed unchanged mirror stalled maintenance: %v", err)
	}
}

func TestSerialEffectiveDockerCoverage(t *testing.T) {
	a, b := evidence(desired("a"), m5Now), evidence(desired("b"), m5Now)
	a.Docker, b.Docker = true, true
	a.RollEvidence.SchedulingCapabilities = []string{"docker"}
	b.RollEvidence.SchedulingCapabilities = []string{}
	ca, _ := coverageOf(a, testConfig().DockerNamespace)
	cb, _ := coverageOf(b, testConfig().DockerNamespace)
	if coverageKey(ca) != coverageKey(cb) || !covers(b, ca) {
		t.Fatal("API-authorized Docker placement lost implicit claim capability")
	}
}

func TestSerialFreshDeadline(t *testing.T) {
	for _, recycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "template", true: "recycle"}[recycle], func(t *testing.T) {
			m, c, workers, _ := matrixFixture(t, 2)
			workers[0].Busy = true
			workers[1].Busy = true
			if recycle {
				workers[0].Size = "l"
			}
			workers[0].DrainingSince = &m5Now
			workers[0].DrainingReason = token("image_roll")
			before, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rollTickTest(t, m, workers)
			rollTickTest(t, matrixRestart(m), workers)
			assertBusyDriftUnchanged(t, c, before)
			workers[0].Busy = false
			rollTickTest(t, matrixRestart(m), workers)
			if patches := templatePatches(c); len(patches) != 1 || patches[0] != before.Name {
				t.Fatalf("idle worker failed guarded turn after fresh drain: %v", patches)
			}
			matrixNoSibling(t, c)
		})
	}
}
