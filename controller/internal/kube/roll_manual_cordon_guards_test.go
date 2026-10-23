package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestManualCoverageDiagnostic(t *testing.T) {
	for _, manual := range []bool{false, true} {
		candidate := &rollEntry{w: evidence(desired("w1"), m5Now), ns: testConfig().Namespace}
		peer := &rollEntry{w: evidence(desired("w2"), m5Now), ns: candidate.ns}
		candidate.coverage, _ = coverageOf(candidate.w, candidate.ns)
		peer.coverage, _ = coverageOf(peer.w, peer.ns)
		fenced := true
		peer.w.EffectiveCordoned = &fenced
		want := "effective_cordon"
		if manual {
			manualWorker(&peer.w)
			want = "manual_cordon"
		}
		tick := &rollTick{entries: map[string]*rollEntry{candidate.w.ID: candidate, peer.w.ID: peer}}
		_, reason := (&Materializer{}).coverageBlocker(tick, candidate)
		if !strings.HasSuffix(reason, " "+want) {
			t.Fatalf("intake suspension reported as missing incarnation proof: %s", reason)
		}
	}
}

func TestManualProviderCoverage(t *testing.T) {
	for _, incarnation := range []string{"old", "current"} {
		for _, state := range []string{"manual", "manual_omitted", "manual_false", "effective", "omitted", "explicit_false"} {
			t.Run(incarnation+"/"+state, func(t *testing.T) {
				candidate, provider := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
				provider.Busy = true
				switch state {
				case "manual":
					manualWorker(&provider)
				case "manual_omitted":
					provider.ManualCordonedAt = &m5Now
				case "manual_false":
					provider.ManualCordonedAt = &m5Now
					value := false
					provider.EffectiveCordoned = &value
				case "effective":
					value := true
					provider.EffectiveCordoned = &value
				case "explicit_false":
					value := false
					provider.EffectiveCordoned = &value
				}
				objects := physicalOld(t, deployedWorker("w1", 0, "old"))
				if incarnation == "old" {
					objects = append(objects, physicalOld(t, deployedWorker("w2", 0, "old"))...)
				} else {
					objects = append(objects, RenderDeployment(testConfig(), provider, testSpec(t, provider.Template, provider.Size)))
				}
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				if incarnation == "current" {
					currentProof(t, c, &provider, m5Now)
				}
				rollTickTest(t, matrixRestart(m), []protocol.DesiredWorker{candidate, provider})
				rolled := false
				for _, name := range templatePatches(c) {
					if name == deploymentName(candidate.ID) {
						rolled = true
					}
				}
				if want := state == "omitted" || state == "explicit_false"; rolled != want {
					t.Fatalf("candidate replacement=%t want %t: %v %+v", rolled, want, templatePatches(c), m.PacingOutcomes())
				}
				for _, action := range c.Actions() {
					if action.GetVerb() == "delete" {
						t.Fatalf("coverage decision deleted object: %v", action)
					}
				}
			})
		}
	}
}

func TestManualForceBusyWithoutCustody(t *testing.T) {
	m, c, ws, cord := matrixFixture(t, 1)
	manualWorker(&ws[0])
	ws[0].Busy = true
	m.drain.ForceRoll = true
	rollTickTest(t, matrixRestart(m), ws)
	if got := templatePatches(c); len(got) != 1 || got[0] != deploymentName("w1") {
		t.Fatalf("ForceRoll did not replace manual busy worker without custody: %v", got)
	}
	if len(cord.calls) != 0 || ws[0].ManualCordonedAt == nil || !ws[0].ManualCordonedAt.Equal(m5Now) {
		t.Fatalf("ForceRoll altered manual intake suspension or requested drain: %+v", cord)
	}
	for _, action := range c.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatalf("image replacement deleted object: %v", action)
		}
	}
}

func TestManualForceDoesNotRecycleDinD(t *testing.T) {
	f := lmSetup(t, "new", true, "4Gi")
	w := lmWorker(false)
	manualWorker(&w)
	w.Busy, w.DiskPressure = true, true
	w.DiskPressureVolumes = []string{"dind"}
	before, err := f.client.CoreV1().PersistentVolumeClaims(lmNS()).Get(context.Background(), dindDataPVCName(w.ID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(t, w)
	for _, action := range f.client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "persistentvolumeclaims" &&
			action.(k8stesting.DeleteAction).GetName() == dindDataPVCName(w.ID) {
			t.Fatal("manual busy ForceRoll bypassed independent DinD lifecycle")
		}
	}
	after, err := f.client.CoreV1().PersistentVolumeClaims(lmNS()).Get(context.Background(), before.Name, metav1.GetOptions{})
	if err != nil || after.UID != before.UID {
		t.Fatalf("DinD identity changed outside maintenance: %v", err)
	}
}
