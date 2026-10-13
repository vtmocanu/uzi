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

type serialConfirmer struct {
	fakeCordoner
	fresh        protocol.DesiredWorker
	confirmErr   error
	afterConfirm func()
}

func (f *serialConfirmer) ConfirmImageRollDrain(ctx context.Context, id string) (protocol.DesiredWorker, error) {
	if err := f.RequestDrainWithReason(ctx, id, "image_roll"); err != nil {
		return protocol.DesiredWorker{}, err
	}
	if f.afterConfirm != nil {
		f.afterConfirm()
	}
	return f.fresh, f.confirmErr
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

func TestSerialDeadlineConfirmationFences(t *testing.T) {
	for _, recycle := range []bool{false, true} {
		for _, mode := range []string{"expired", "api-clock-offset", "generation", "custody", "lane", "poll-error", "missing-channel", "expired-evidence"} {
			t.Run(fmt.Sprintf("recycle=%t/%s", recycle, mode), func(t *testing.T) {
				m, c, workers, _ := matrixFixture(t, 3)
				for i := range workers {
					workers[i].Busy = true
				}
				if recycle {
					workers[0].Size = "l"
				}
				since := m5Now.Add(-2 * time.Hour)
				workers[0].DrainingSince = &since
				workers[0].DrainingReason = token("image_roll")
				fresh := workers[0]
				confirmer := &serialConfirmer{fresh: fresh}
				switch mode {
				case "api-clock-offset":
					p := *fresh.RollEvidence
					p.EvaluatedAt = m5Now.Add(24 * time.Hour)
					p.ValidUntil = p.EvaluatedAt.Add(time.Minute)
					p.LivenessValidUntil = &p.ValidUntil
					shifted := p.EvaluatedAt.Add(-2 * time.Hour)
					confirmer.fresh.RollEvidence = &p
					confirmer.fresh.DrainingSince = &shifted
				case "generation":
					confirmer.fresh.Generation++
				case "custody":
					confirmer.fresh.CustodyHeld = true
				case "lane":
					confirmer.fresh.Docker = true
				case "expired-evidence":
					confirmer.afterConfirm = func() { m.now = func() time.Time { return m5Now.Add(2 * time.Minute) } }
				case "poll-error":
					confirmer.confirmErr = fmt.Errorf("poll uncertain")
				}
				m.cordoner = confirmer
				if mode == "missing-channel" {
					m.cordoner = &noConfirmCordoner{}
				}
				if mode == "expired" || mode == "api-clock-offset" {
					rollTickTest(t, m, workers)
					if len(templatePatches(c)) != 1 {
						t.Fatalf("fresh expired proof did not permit turn: %v", templatePatches(c))
					}
				} else {
					matrixUnknownTick(t, m, workers)
					if len(templatePatches(c)) != 0 {
						t.Fatalf("uncertain fresh proof mutated deployment: %v", templatePatches(c))
					}
					if len(confirmer.calls) > 1 {
						t.Fatalf("uncertainty did not halt siblings: %v", confirmer.calls)
					}
				}
			})
		}
	}
}

type noConfirmCordoner struct{}

func (*noConfirmCordoner) RequestDrain(context.Context, string) error { return nil }
func (*noConfirmCordoner) ClearDrain(context.Context, string) error   { return nil }
func (*noConfirmCordoner) RequestDrainWithReason(context.Context, string, ...string) error {
	return nil
}
func (*noConfirmCordoner) ClearDrainWithReason(context.Context, string, ...string) error { return nil }

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
	m.cordoner = &serialConfirmer{fresh: workers[0]}
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = m.Reconcile(context.Background(), workers, obs)
	if err != nil || len(templatePatches(c)) != 1 {
		t.Fatalf("fresh old-worker drain incorrectly required promotion proof: err=%v patches=%v", err, templatePatches(c))
	}
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
			since := m5Now.Add(-2 * time.Hour)
			workers[0].DrainingSince = &since
			workers[0].DrainingReason = token("image_roll")
			fresh := workers[0]
			fresh.DrainingSince = &m5Now
			m.cordoner = &serialConfirmer{fresh: fresh}
			rollTickTest(t, m, workers)
			if len(templatePatches(c)) != 0 {
				t.Fatalf("fresh re-cordon reused expired snapshot: %v", templatePatches(c))
			}
		})
	}
}
