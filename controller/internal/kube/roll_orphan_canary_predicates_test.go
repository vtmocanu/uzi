package kube

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// A withheld adoption request may commit later. A participant counts as unable to
// accept it only from what is observed now, and every controller write that touches a
// blocked participant must leave it that way.
// Build the proof through the real cohort/snapshot seams so each fence starts
// from a candidate; missing pod or registration evidence cannot mask a failure.
func provenOrphanEntry(t *testing.T) (*Materializer, *rollTick, *rollEntry) {
	t.Helper()
	o := newOrphanCohort(t, 3, 2)
	o.prove(t, "w2")
	observed, err := o.m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]reconcile.ObservedWorker{}
	for _, worker := range observed {
		byID[worker.ID] = worker
	}
	tick, _ := o.m.planRoll(context.Background(), o.workers, byID)
	e := tick.entries["w2"]
	if e == nil || !o.m.orphanCanaryCandidate(tick, e) || orphanPatchFenced(e) {
		t.Fatal("positive control is not an unfenced, proven adoption candidate")
	}
	return o.m, tick, e
}

func TestOrphanRollCohortFencedStaysFenced(t *testing.T) {
	fencedBy := map[string]func(*rollEntry){
		"scaled to zero": func(e *rollEntry) { zero := int32(0); e.dep.Spec.Replicas = &zero },
		"scaled to two":  func(e *rollEntry) { two := int32(2); e.dep.Spec.Replicas = &two },
		"template hash changed": func(e *rollEntry) {
			e.dep.Spec.Template.Annotations[AnnotationSpecHash] = "different-hash"
		},
		"template image changed": func(e *rollEntry) {
			e.dep.Spec.Template.Spec.Containers[0].Image = "worker:different"
		},
		"deployment gone": func(e *rollEntry) { e.dep = nil },
		"deployment terminating": func(e *rollEntry) {
			now := metav1.Now()
			e.dep.DeletionTimestamp = &now
		},
		"reservation completed": func(e *rollEntry) { e.marker.Completed = true },
		"reservation absent":    func(e *rollEntry) { e.marker = nil },
	}
	// The controller's own writes to a participant: mapping correction rewrites the
	// marker's version and clears Completed; pin, layout and historic cancellation
	// withdraw the marker. Deployment shape is untouched by all of them.
	writes := map[string]func(*rollEntry, *rollTick){
		"mapping correction": func(e *rollEntry, tick *rollTick) {
			if e.marker != nil {
				e.marker.TargetVersion, e.marker.Completed = "2.0.0", false
				// Keep version evidence consistent: otherwise a stale version,
				// rather than the fence, would make the candidate check fail.
				e.w.RollEvidence.RegisteredVersion = token("2.0.0")
				tick.outcomes[e.w.ID].TargetVersion = "2.0.0"
			}
		},
		"pin cancellation":      func(e *rollEntry, _ *rollTick) { e.marker = nil },
		"layout cancellation":   func(e *rollEntry, _ *rollTick) { e.marker = nil },
		"historic cancellation": func(e *rollEntry, _ *rollTick) { e.marker = nil },
	}
	t.Run("mapping correction alone keeps the candidate proven", func(t *testing.T) {
		m, tick, e := provenOrphanEntry(t)
		writes["mapping correction"](e, tick)
		if !m.orphanCanaryCandidate(tick, e) || orphanPatchFenced(e) {
			t.Fatal("mapping correction invalidated the proof independently of a fence")
		}
	})
	for name, fence := range fencedBy {
		for write, apply := range writes {
			// Completed-cohort planning returns before mapping correction; do
			// not model the unreachable operation that clears its Completed bit.
			if name == "reservation completed" && write == "mapping correction" {
				continue
			}
			t.Run(name+" then "+write, func(t *testing.T) {
				m, tick, e := provenOrphanEntry(t)
				fence(e)
				if !orphanPatchFenced(e) || m.orphanCanaryCandidate(tick, e) {
					t.Fatal("the fence did not exclude the proven candidate")
				}
				apply(e, tick)
				if !orphanPatchFenced(e) {
					t.Fatalf("%s lifted the fence", write)
				}
				if m.orphanCanaryCandidate(tick, e) {
					t.Fatalf("%s made a fenced participant a candidate", write)
				}
			})
		}
	}
	t.Run("unfenced shape is not fenced", func(t *testing.T) {
		_, _, e := provenOrphanEntry(t)
		e.dep.Spec.Replicas = nil
		if orphanPatchFenced(e) {
			t.Fatal("nil replicas (defaulted to one) reads as fenced")
		}
	})
}
