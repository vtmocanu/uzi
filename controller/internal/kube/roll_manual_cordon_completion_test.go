package kube

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// A completed replacement still has an open marker until cohort cleanup. It
// must free a slot without becoming serving capacity while manually cordoned.
func TestManualCompletedReservationNextAdmission(t *testing.T) {
	for _, pool := range []string{"ordinary", "checker"} {
		for _, covered := range []bool{true, false} {
			name := pool + "/missing_y"
			if covered {
				name = pool + "/covered"
			}
			t.Run(name, func(t *testing.T) {
				ws := []protocol.DesiredWorker{
					evidence(desired("w1"), m5Now),
					evidence(desired("w2"), m5Now),
					evidence(desired("w3"), m5Now),
				}
				for i := range ws {
					ws[i].RollEvidence.SchedulingCapabilities = []string{"x", "y"}
					ws[i].RollEvidence.OrdinaryCoverage = pool == "ordinary"
					ws[i].RollEvidence.CheckerCoverage = pool == "checker"
				}
				if !covered {
					ws[0].RollEvidence.SchedulingCapabilities = []string{"x"}
				}
				manualWorker(&ws[1])
				canary := markedDeployment(t, testConfig(), ws[0], false)
				manual := markedDeployment(t, testConfig(), ws[1], false)
				for i, d := range []*appsv1.Deployment{canary, manual} {
					r, err := decodeRoll(d.Annotations[annotationRoll])
					if err != nil || r == nil {
						t.Fatalf("fixture reservation: %+v %v", r, err)
					}
					// Freeze the actual capabilities, including the negative
					// canary's x-only contract, which it continues to satisfy.
					r.Coverage.Scheduling = append([]string{}, ws[i].RollEvidence.SchedulingCapabilities...)
					r.Coverage.Ordinary = pool == "ordinary"
					r.Coverage.Checker = pool == "checker"
					r.Canary = i == 0
					raw, err := json.Marshal(r)
					if err != nil {
						t.Fatal(err)
					}
					d.Annotations[annotationRoll] = string(raw)
				}
				objects := physicalOld(t, deployedWorker("w3", 0, "old"))
				objects = append(objects, canary, manual)
				m, c := newMat(t, objects...)
				m.now = func() time.Time { return m5Now }
				m.cfg.RollConcurrency = 1
				for i := 0; i < 2; i++ {
					caps := ws[i].RollEvidence
					currentProof(t, c, &ws[i], m5Now)
					ws[i].RollEvidence.SchedulingCapabilities = caps.SchedulingCapabilities
					ws[i].RollEvidence.OrdinaryCoverage = caps.OrdinaryCoverage
					ws[i].RollEvidence.CheckerCoverage = caps.CheckerCoverage
				}
				m = matrixRestart(m)
				rollTickTest(t, m, ws)
				outcomes := m.PacingOutcomes()
				d, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), deploymentName("w3"), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				r, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				patches := templatePatches(c)
				if covered {
					if len(patches) != 1 || patches[0] != d.Name || workerImage(d) != workerImage(canary) {
						t.Fatalf("completed manual participant did not free its only slot: patches=%v outcomes=%+v", patches, outcomes)
					}
					if r == nil || r.Canary || r.Completed || r.Coverage.Ordinary != (pool == "ordinary") || r.Coverage.Checker != (pool == "checker") || len(r.Coverage.Scheduling) != 2 || r.Coverage.Scheduling[0] != "x" || r.Coverage.Scheduling[1] != "y" {
						t.Fatalf("candidate lacks durable non-canary reservation: %+v", r)
					}
					if p := outcomes["w3"]; p == nil || p.CanaryID == nil || *p.CanaryID != "w1" || p.State != "active" {
						t.Fatalf("manual participant became canary or candidate was not admitted: %+v", p)
					}
				} else {
					// Capability changes separate coverage pools. The x-only
					// canary remains healthy in its own pool; the xy candidate
					// must reach the coverage decision in its pool, with a free
					// slot, rather than fail on canary incarnation proof.
					p := outcomes["w3"]
					if len(patches) != 0 || r != nil || p == nil || p.BlockingReason == nil || *p.BlockingReason != "no usable coverage: w1 capability_mismatch" || p.BlockingWorkerID == nil || *p.BlockingWorkerID != "w1" {
						t.Fatalf("missing-y control did not reach coverage rejection: patches=%v marker=%+v outcome=%+v", patches, r, p)
					}
					if p.CanaryID != nil {
						t.Fatalf("manual participant counted as canary: %+v", p)
					}
				}
				// In the positive pool the drifted candidate prevents cohort
				// completion; the restart really consumed the open canary.
				got, err := c.AppsV1().Deployments(m.cfg.Namespace).Get(context.Background(), canary.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				cr, err := decodeRoll(got.Annotations[annotationRoll])
				if err != nil || cr == nil || !cr.Canary || cr.Completed != !covered {
					t.Fatalf("healthy canary has wrong completion state: %+v %v", cr, err)
				}
				for _, a := range c.Actions() {
					if a.GetVerb() == "delete" {
						t.Fatalf("admission deleted an object: %v", a)
					}
				}
			})
		}
	}
}
