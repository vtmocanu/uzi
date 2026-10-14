package kube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// The API's custody boolean describes protected sources, not whether the live
// worker can serve its frozen coverage. No free-slot guarantee is needed here.
func TestRollCustodyBusyPoolProgresses(t *testing.T) {
	workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)}
	var objects []runtime.Object
	for i := range workers {
		workers[i].Busy, workers[i].CustodyHeld = true, true
		objects = append(objects, physicalOld(t, deployedWorker(workers[i].ID, 0, "old"))...)
	}
	f := &fakeCordoner{}
	m, c := newMatWithDrain(t, f, DrainPolicy{Deadline: time.Hour}, objects...)
	rollTickTest(t, m, workers)
	if len(f.calls) != 1 || f.calls[0] != "w1" || m.PacingOutcomes()["w1"].State != "active" {
		t.Fatalf("busy custody pool did not admit and cordon one canary: cordons=%v outcomes=%+v", f.calls, m.PacingOutcomes())
	}
	if len(templatePatches(c)) != 0 {
		t.Fatal("busy canary rolled before drain settlement")
	}
	data, err := c.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	workers[0].Busy = false
	workers[0].DrainingSince, workers[0].DrainingReason = recentDrain(), token("image_roll")
	rollTickTest(t, m, workers)
	if got := templatePatches(c); len(got) != 1 || got[0] != deploymentName("w1") {
		t.Fatalf("settled canary did not roll: %v", got)
	}
	removeOldPod(t, c, "w1")
	currentProof(t, c, &workers[0], m5Now)
	rollTickTest(t, m, workers)
	if len(f.calls) != 2 || f.calls[1] != "w2" {
		t.Fatalf("custody on proven current canary blocked next admission: %v", f.calls)
	}
	if len(f.clearCalls) != 0 {
		t.Fatalf("serving coverage released a custody-protected drain: %v", f.clearCalls)
	}
	retained, err := c.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
	if err != nil || retained.UID != data.UID {
		t.Fatalf("custody-protected data claim changed: %v", err)
	}
}

func TestRollCustodyCurrentProvider(t *testing.T) {
	candidate, provider := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
	candidate.Busy, candidate.CustodyHeld, provider.Busy, provider.CustodyHeld = true, true, true, true
	d := RenderDeployment(testConfig(), provider, testSpec(t, provider.Template, provider.Size))
	objects := append(physicalOld(t, deployedWorker("w1", 0, "old")), d)
	f := &fakeCordoner{}
	m, c := newMatWithDrain(t, f, DrainPolicy{Deadline: time.Hour}, objects...)
	currentProof(t, c, &provider, m5Now)
	rollTickTest(t, m, []protocol.DesiredWorker{candidate, provider})
	if len(f.calls) != 1 || f.calls[0] != candidate.ID {
		t.Fatalf("current custody-held provider lost serving coverage: %v", f.calls)
	}
}

func TestRollCustodyCoverageRefusals(t *testing.T) {
	for _, current := range []bool{false, true} {
		for _, refusal := range []string{"stale liveness", "disk pressure", "maintenance", "capability mismatch", "unknown evidence"} {
			t.Run(map[bool]string{false: "old", true: "current"}[current]+"/"+refusal, func(t *testing.T) {
				candidate, provider := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
				candidate.CustodyHeld, provider.CustodyHeld = true, true
				candidate.RollEvidence.ProtocolCapabilities = []string{"required"}
				provider.RollEvidence.ProtocolCapabilities = []string{"required"}
				var objects []runtime.Object
				objects = append(objects, physicalOld(t, deployedWorker("w1", 0, "old"))...)
				if current {
					objects = append(objects, RenderDeployment(testConfig(), provider, testSpec(t, provider.Template, provider.Size)))
				} else {
					objects = append(objects, physicalOld(t, deployedWorker("w2", 0, "old"))...)
				}
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				if current {
					currentProof(t, c, &provider, m5Now)
					provider.RollEvidence.ProtocolCapabilities = []string{"required"}
				}
				observed, err := m.Observe(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				byID := map[string]reconcile.ObservedWorker{}
				for _, worker := range observed {
					byID[worker.ID] = worker
				}
				tick, _ := m.planRoll(context.Background(), []protocol.DesiredWorker{candidate, provider}, byID)
				peer := tick.entries[provider.ID]
				peer.admitted = false
				if !m.remainingCoverage(tick, []*rollEntry{tick.entries[candidate.ID], peer}, tick.entries[candidate.ID]) {
					t.Fatal("positive control: custody-held provider was not usable before the independent refusal")
				}
				// The pool's coverage is frozen before these independent refusals.
				switch refusal {
				case "stale liveness":
					old := m5Now.Add(-time.Second)
					peer.w.RollEvidence.LivenessValidUntil, peer.w.RollEvidence.ValidUntil = &old, old
				case "disk pressure":
					peer.w.DiskPressure = true
				case "maintenance":
					peer.w.DindMaintenance = &protocol.DindMaintenance{Phase: "requested"}
				case "capability mismatch":
					peer.w.RollEvidence.ProtocolCapabilities = []string{}
				case "unknown evidence":
					peer.w.RollEvidence.LivenessValidUntil, peer.w.RollEvidence.PodUID = nil, nil
				}
				peer.admitted = false
				if m.remainingCoverage(tick, []*rollEntry{tick.entries[candidate.ID], peer}, tick.entries[candidate.ID]) {
					t.Fatalf("custody bypassed %s serving refusal", refusal)
				}
			})
		}
	}
}

func TestRollCustodySingletonPolicy(t *testing.T) {
	w := evidence(desired("w1"), m5Now)
	w.Busy, w.CustodyHeld = true, true
	f := &fakeCordoner{}
	m, _ := newMatWithDrain(t, f, DrainPolicy{Deadline: time.Hour}, physicalOld(t, deployedWorker(w.ID, 0, "old"))...)
	rollTickTest(t, m, []protocol.DesiredWorker{w})
	if len(f.calls) != 1 || f.calls[0] != w.ID {
		t.Fatalf("single-worker interruption policy changed: %v", f.calls)
	}
}

func TestRollCustodyCompletedReservationProtected(t *testing.T) {
	for _, custody := range []bool{false, true} {
		t.Run(map[bool]string{false: "released", true: "held"}[custody], func(t *testing.T) {
			canary, peer := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
			peer.CustodyHeld = custody
			d := markedDeployment(t, testConfig(), peer, false)
			var marker rollMarker
			if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &marker); err != nil {
				t.Fatal(err)
			}
			marker.Canary = false
			raw, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			d.Annotations[annotationRoll] = string(raw)
			m, c := newMat(t, markedDeployment(t, testConfig(), canary, true), d)
			m.now = func() time.Time { return m5Now }
			currentProof(t, c, &peer, m5Now)
			rollTickTest(t, m, []protocol.DesiredWorker{canary, peer})
			got, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if (got.Annotations[annotationRoll] != "") != custody {
				t.Fatalf("custody=%t completed-peer reservation=%q", custody, got.Annotations[annotationRoll])
			}
		})
	}
}

func TestRollCustodyBlockingReason(t *testing.T) {
	for _, slot := range []bool{false, true} {
		t.Run(map[bool]string{false: "coverage", true: "slot"}[slot], func(t *testing.T) {
			workers := []protocol.DesiredWorker{evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)}
			var objects []runtime.Object
			for i := range workers {
				workers[i].Busy, workers[i].CustodyHeld = true, true
				objects = append(objects, physicalOld(t, deployedWorker(workers[i].ID, 0, "old"))...)
			}
			if !slot {
				workers[1].RollEvidence.LivenessValidUntil = nil
			}
			f := &fakeCordoner{}
			m, _ := newMatWithDrain(t, f, DrainPolicy{Deadline: time.Hour}, objects...)
			rollTickTest(t, m, workers)
			id, term := "w1", "coverage"
			if slot {
				id, term = "w2", "roll slot"
			}
			p := m.PacingOutcomes()[id]
			if p.BlockingReason == nil || !strings.Contains(*p.BlockingReason, term) || p.BlockingWorkerID == nil {
				t.Fatalf("missing %s blocker identity/predicate: %+v", term, p)
			}
			if !slot && (!strings.Contains(*p.BlockingReason, "liveness") || *p.BlockingWorkerID != "w2") {
				t.Fatalf("coverage reason does not identify missing liveness of w2: %+v", p)
			}
		})
	}
}
