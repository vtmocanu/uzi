package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func manualWorker(w *protocol.DesiredWorker) {
	w.ManualCordonedAt = &m5Now
	value := true
	w.EffectiveCordoned = &value
}

func TestManualRollAdmission(t *testing.T) {
	for _, mode := range []string{"singleton", "busy", "covered", "all_manual", "manual_provider", "effective_provider", "omitted", "overlap", "checker"} {
		t.Run(mode, func(t *testing.T) {
			n := 2
			if mode == "singleton" || mode == "busy" {
				n = 1
			}
			m, c, ws, cordon := matrixFixture(t, n)
			manualWorker(&ws[0])
			want := true
			wantID := "w1"
			switch mode {
			case "busy":
				ws[0].Busy = true
				want = false
			case "all_manual":
				manualWorker(&ws[1])
				want = false
			case "manual_provider":
				ws[0].ManualCordonedAt, ws[0].EffectiveCordoned = nil, nil
				manualWorker(&ws[1])
				wantID = "w2" // The manual candidate can roll behind the serving w1.
			case "effective_provider":
				v := true
				ws[1].EffectiveCordoned = &v
				want = false
			case "omitted":
				ws[0].ManualCordonedAt, ws[0].EffectiveCordoned = nil, nil
			case "overlap":
				ws[0].RollEvidence.SchedulingCapabilities = []string{"x"}
				ws[1].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
			case "checker":
				ws[0].RollEvidence.OrdinaryCoverage = false
				ws[0].RollEvidence.CheckerCoverage = true
				ws[1].RollEvidence.CheckerCoverage = true
			}
			rollTickTest(t, m, ws)
			patches := templatePatches(c)
			if want {
				if len(patches) != 1 || patches[0] != deploymentName(wantID) {
					t.Fatalf("candidate not rolled: %v %+v", patches, m.PacingOutcomes())
				}
				d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName(wantID), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				r, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil || r == nil || !r.Canary {
					t.Fatalf("roll missing durable canary reservation: %+v %v", r, err)
				}
			} else if len(patches) != 0 {
				t.Fatalf("unsafe template replacement: %v", patches)
			}
			if mode == "busy" && (len(cordon.calls) != 1 || cordon.calls[0] != "w1") {
				t.Fatalf("manual busy worker must drain and wait: %v", cordon.calls)
			}
			if !want {
				for _, w := range ws {
					d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName(w.ID), metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					r, err := decodeRoll(d.Annotations[annotationRoll])
					if err != nil {
						t.Fatal(err)
					}
					if mode == "busy" {
						if r == nil || !r.Canary || r.Completed {
							t.Fatalf("busy manual canary lacks pending durable reservation: %+v", r)
						}
					} else if r != nil {
						t.Fatalf("uncovered pool reserved a replacement for %s: %+v", w.ID, r)
					}
				}
			}
			for _, action := range c.Actions() {
				if action.GetVerb() == "delete" {
					t.Fatalf("roll admission deleted object: %v", action)
				}
			}
			if mode != "omitted" && ws[0].ManualCordonedAt != nil && !ws[0].ManualCordonedAt.Equal(m5Now) {
				t.Fatal("manual cordon changed")
			}
		})
	}
}

func TestManualCanaryCompletionAndRelease(t *testing.T) {
	for _, mode := range []string{"manual", "manual_release", "release", "failed_release", "singleton"} {
		t.Run(mode, func(t *testing.T) {
			n := 2
			if mode == "singleton" {
				n = 1
			}
			m, c, ws, _ := matrixFixture(t, n)
			f := &mandatoryReasonCordoner{}
			m.cordoner = f
			if mode == "singleton" {
				manualWorker(&ws[0])
			}
			rollTickTest(t, m, ws)
			removeOldPod(t, c, "w1")
			currentProof(t, c, &ws[0], m5Now)
			if mode == "manual" || mode == "manual_release" {
				manualWorker(&ws[0])
			}
			if mode == "manual_release" || mode == "release" || mode == "failed_release" {
				ws[0].DrainingSince, ws[0].DrainingReason = recentDrain(), token("image_roll")
				v := true
				ws[0].EffectiveCordoned = &v
			}
			if mode == "failed_release" {
				f.clearErr = fmt.Errorf("release response lost")
			}
			m = matrixRestart(m)
			if mode == "failed_release" {
				matrixUnknownTick(t, m, ws)
			} else {
				rollTickTest(t, m, ws)
			}
			wantPatches := 1 // Explicit aggregate true requires a fresh poll after release.
			if got := templatePatches(c); len(got) != wantPatches {
				t.Fatalf("coverage after completion: %v, want %d; %+v", got, wantPatches, m.PacingOutcomes())
			}
			if mode == "release" || mode == "manual_release" || mode == "failed_release" {
				if len(f.clearCalls) != 1 || f.clearCalls[0] != "w1" || len(f.reasons) != 1 || f.reasons[0] != "image_roll" {
					t.Fatalf("reason-owned release not attempted first: %+v", f)
				}
			}
			if mode == "singleton" {
				d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName("w1"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				r, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil || r == nil || !r.Completed {
					t.Fatalf("manual singleton completion lost: %+v %v", r, err)
				}
			}
			if mode == "release" {
				released := false
				ws[0].EffectiveCordoned = &released
				ws[0].DrainingSince, ws[0].DrainingReason = nil, nil
				rollTickTest(t, matrixRestart(m), ws)
				if got := templatePatches(c); len(got) != 2 {
					t.Fatalf("fresh release poll did not restore canary: %v", got)
				}
			}
			if mode == "manual" || mode == "manual_release" {
				// A later owner uncordon restores coverage without losing the completed
				// incarnation proof or requiring another replacement.
				ws[0].ManualCordonedAt, ws[0].EffectiveCordoned = nil, nil
				ws[0].DrainingSince, ws[0].DrainingReason = nil, nil
				rollTickTest(t, matrixRestart(m), ws)
				if got := templatePatches(c); len(got) != 2 {
					t.Fatalf("owner uncordon did not restore canary: %v", got)
				}
			}
		})
	}
}

func TestManualCompletedReservationCleanup(t *testing.T) {
	canary, peer := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
	manualWorker(&peer)
	d := markedDeployment(t, testConfig(), peer, false)
	var r rollMarker
	if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &r); err != nil {
		t.Fatal(err)
	}
	r.Canary = false
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	d.Annotations[annotationRoll] = string(raw)
	m, c := newMat(t, markedDeployment(t, testConfig(), canary, true), d)
	m.now = func() time.Time { return m5Now }
	currentProof(t, c, &peer, m5Now)
	rollTickTest(t, matrixRestart(m), []protocol.DesiredWorker{canary, peer})
	got, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), d.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[annotationRoll] != "" {
		t.Fatal("manual peer's completed reservation was retained")
	}
	if len(templatePatches(c)) != 0 {
		t.Fatal("metadata cleanup replaced a manual worker")
	}
}

func TestManualTargetProofAndStrongerGuards(t *testing.T) {
	for _, mode := range []string{"target", "pressure", "maintenance", "old_force", "unknown_force"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "old_force" || mode == "unknown_force" {
				layout := "old"
				if mode == "unknown_force" {
					layout = "unknown"
				}
				f := lmSetup(t, layout, true, "4Gi")
				w := lmWorker(true)
				manualWorker(&w)
				w.DiskPressure = true
				f.tick(t, w)
				if len(templatePatches(f.client)) != 0 {
					t.Fatal("manual cordon bypassed layout/custody hold under ForceRoll")
				}
				for _, a := range f.client.Actions() {
					if a.GetVerb() == "delete" {
						t.Fatalf("protected object deleted: %v", a)
					}
				}
				return
			}
			candidate, provider := evidence(desired("w1"), m5Now), evidence(desired("w2"), m5Now)
			manualWorker(&provider)
			objects := append(physicalOld(t, deployedWorker("w1", 0, "old")), RenderDeployment(testConfig(), provider, testSpec(t, provider.Template, provider.Size)))
			m, c := newMat(t, objects...)
			m.now = func() time.Time { return m5Now }
			currentProof(t, c, &provider, m5Now)
			switch mode {
			case "pressure":
				provider.DiskPressure = true
			case "maintenance":
				provider.DindMaintenance = &protocol.DindMaintenance{Phase: "requested"}
			}
			rollTickTest(t, m, []protocol.DesiredWorker{candidate, provider})
			if len(templatePatches(c)) != 0 {
				t.Fatal("manual provider granted serving coverage")
			}
			if p := m.PacingOutcomes()[provider.ID]; mode == "target" && p.BlockingReason != nil && *p.BlockingReason == "target template has no reservation proof" {
				t.Fatal("manual target's incarnation proof was discarded")
			}
		})
	}
}

func TestManualMigrationComposition(t *testing.T) {
	for _, mode := range []string{"manual_provider", "manual_candidate", "effective_provider", "third_provider", "checker_mismatch", "failed_release"} {
		t.Run(mode, func(t *testing.T) {
			opts := []lmOpts{migrationHeldOpt("a"), migrationHeldOpt("b")}
			if mode == "third_provider" {
				opts = append(opts, migrationHeldOpt("c"))
			}
			if mode == "failed_release" {
				opts[1] = lmOpts{id: "b", layout: "new", promoted: true}
			}
			f, ws, cord := migrationFixture(t, opts...)
			switch mode {
			case "manual_candidate":
				manualWorker(&ws[0])
			case "effective_provider":
				v := true
				ws[1].EffectiveCordoned = &v
			case "checker_mismatch":
				manualWorker(&ws[0])
				ws[0].RollEvidence.CheckerCoverage = true
			case "failed_release":
				ws[1].DrainingSince, ws[1].DrainingReason = recentDrain(), token("image_roll")
				cord.clearErr = fmt.Errorf("release response lost")
			default:
				manualWorker(&ws[1])
			}
			if mode == "failed_release" {
				matrixUnknownTick(t, f.m, ws)
			} else {
				rollTickTest(t, f.m, ws)
			}
			want := mode == "manual_candidate" || mode == "third_provider"
			found := false
			for i, id := range cord.calls {
				if id == "a" && i < len(cord.reasons) && cord.reasons[i] == "image_roll" {
					found = true
				}
			}
			if found != want {
				t.Fatalf("migration admission=%t want %t calls=%v reasons=%v outcomes=%+v", found, want, cord.calls, cord.reasons, f.m.PacingOutcomes())
			}
			if got := templatePatches(f.client); len(got) != 0 {
				t.Fatalf("migration custody patched a template: %v", got)
			}
			for _, a := range f.client.Actions() {
				if a.GetVerb() == "delete" {
					t.Fatalf("migration custody caused delete: %v", a)
				}
			}
		})
	}
}
