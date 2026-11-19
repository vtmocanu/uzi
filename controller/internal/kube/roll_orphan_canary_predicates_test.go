package kube

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// A withheld adoption request may commit later. A participant counts as unable to
// accept it only from what is observed now, and every controller write that touches a
// blocked participant must leave it that way.
func orphanEntry(replicas int32, image, hash string) *rollEntry {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "uzi-hw-w2", UID: types.UID("dep-w2"), ResourceVersion: "1"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationSpecHash: hash}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: workerContainerName, Image: image}}},
		}},
	}
	return &rollEntry{
		w:      protocol.DesiredWorker{ID: "w2"},
		ns:     "uzi-workers",
		dep:    dep,
		marker: &rollMarker{Version: 1, Hash: "h1", Image: "img:1", TargetVersion: "1.0.0", Coverage: rollCoverage{Owner: "owner-A", Lane: "uzi-workers"}},
	}
}

func TestOrphanRollCohortFencedStaysFenced(t *testing.T) {
	m, _ := newMat(t)
	tick := &rollTick{outcomes: map[string]*protocol.Pacing{"w2": {TargetVersion: "1.0.0"}}, unavailable: map[string]bool{}, migrationFences: map[string]bool{}}

	fencedBy := map[string]func() *rollEntry{
		"scaled to zero":         func() *rollEntry { return orphanEntry(0, "img:1", "h1") },
		"scaled to two":          func() *rollEntry { return orphanEntry(2, "img:1", "h1") },
		"template hash changed":  func() *rollEntry { return orphanEntry(1, "img:1", "h2") },
		"template image changed": func() *rollEntry { return orphanEntry(1, "img:2", "h1") },
		"deployment gone":        func() *rollEntry { e := orphanEntry(1, "img:1", "h1"); e.dep = nil; return e },
		"deployment terminating": func() *rollEntry {
			e := orphanEntry(1, "img:1", "h1")
			now := metav1.Now()
			e.dep.DeletionTimestamp = &now
			return e
		},
		"reservation completed": func() *rollEntry { e := orphanEntry(1, "img:1", "h1"); e.marker.Completed = true; return e },
		"reservation absent":    func() *rollEntry { e := orphanEntry(1, "img:1", "h1"); e.marker = nil; return e },
	}
	// The controller's own writes to a participant: mapping correction rewrites the
	// marker's version and clears Completed; pin, layout and historic cancellation
	// withdraw the marker. Deployment shape is untouched by all of them.
	writes := map[string]func(*rollEntry){
		"mapping correction": func(e *rollEntry) {
			if e.marker != nil {
				e.marker.TargetVersion, e.marker.Completed = "2.0.0", false
			}
		},
		"pin cancellation":      func(e *rollEntry) { e.marker = nil },
		"layout cancellation":   func(e *rollEntry) { e.marker = nil },
		"historic cancellation": func(e *rollEntry) { e.marker = nil },
	}
	for name, build := range fencedBy {
		for write, apply := range writes {
			t.Run(name+" then "+write, func(t *testing.T) {
				e := build()
				if !orphanPatchFenced(e) {
					t.Fatal("not fenced before the write")
				}
				apply(e)
				if name != "reservation completed" && name != "reservation absent" && !orphanPatchFenced(e) {
					t.Fatalf("%s lifted the fence", write)
				}
				if m.orphanCanaryCandidate(tick, e) {
					t.Fatalf("%s made a fenced participant a candidate", write)
				}
			})
		}
	}
	t.Run("unfenced shape is not fenced", func(t *testing.T) {
		if orphanPatchFenced(orphanEntry(1, "img:1", "h1")) {
			t.Fatal("a one-replica participant on its reserved template reads as fenced")
		}
		e := orphanEntry(1, "img:1", "h1")
		e.dep.Spec.Replicas = nil
		if orphanPatchFenced(e) {
			t.Fatal("nil replicas (defaulted to one) reads as fenced")
		}
	})
}
