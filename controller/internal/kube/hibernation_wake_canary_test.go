package kube

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Issue #4659: the canary of an in-flight roll that is waking with a new op, whose
// Deployment already has one replica and the current template, must still bind the op.
// The fixture is the lost-canary cohort of newOrphanCohort: w1 is the open canary with a
// complete incarnation, w2 and w3 are participants whose incarnations are not proven, so
// the cohort cannot complete and w1 is not image-admitted.

const (
	wakeCanaryOp    = "00000000-0000-0000-0000-0000000046a1"
	wakeCanaryOldOp = "00000000-0000-0000-0000-0000000046a0"
)

type wakeCanary struct {
	*orphanCohort
	rec *hibRec
}

// newWakeCanary makes w1 the waking canary: one replica, current template, an older op
// annotation, and a Ready admission pod. mutate adjusts w1's Deployment first.
func newWakeCanary(t *testing.T, mutate func(*appsv1.Deployment)) *wakeCanary {
	t.Helper()
	o := newOrphanCohort(t, 3, 2)
	o.edit(t, "w1", func(d *appsv1.Deployment) {
		if d.Annotations == nil {
			d.Annotations = map[string]string{}
		}
		d.Annotations[annotationHibernationOp] = wakeCanaryOldOp
		if mutate != nil {
			mutate(d)
		}
	})
	pod, err := o.c.CoreV1().Pods(o.m.cfg.Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := o.c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
		t.Fatal(err)
	}
	o.at("w1").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: wakeCanaryOp}
	rec := &hibRec{fakeCordoner: o.cordon}
	o.m.cordoner = rec
	return &wakeCanary{orphanCohort: o, rec: rec}
}

// requireUnadmitted proves the fixture reaches the unadmitted pass-2 wake path.
func (wc *wakeCanary) requireUnadmitted(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	obs, err := wc.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]reconcile.ObservedWorker{}
	for _, o := range obs {
		byID[o.ID] = o
	}
	tick, _ := wc.m.planRoll(ctx, wc.workers, byID)
	e := tick.entries["w1"]
	if e == nil {
		t.Fatal("w1 has no roll entry")
	}
	if e.admitted || e.hibernation || e.blocked != "" {
		t.Fatalf("fixture is not the unadmitted, unblocked wake path: admitted=%t hibernation=%t blocked=%q", e.admitted, e.hibernation, e.blocked)
	}
	mk := wc.marker(t, "w1")
	if mk == nil || !mk.Canary || mk.Completed {
		t.Fatalf("w1 is not the open canary: %+v", mk)
	}
}

// wakePatches returns the bodies of Deployment patches on w1 that carry the op annotation.
func (wc *wakeCanary) wakePatches(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	for _, a := range wc.c.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || a.GetResource().Resource != "deployments" || p.GetName() != deploymentName("w1") {
			continue
		}
		var body struct {
			Metadata struct {
				Annotations map[string]*string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(p.GetPatch(), &body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body.Metadata.Annotations[annotationHibernationOp]; !ok {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(p.GetPatch(), &raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}
	return out
}

func (wc *wakeCanary) readyCalls() []protocol.HibernationTransition {
	var out []protocol.HibernationTransition
	for _, c := range wc.rec.calls {
		if c.Transition == protocol.TransitionReady {
			out = append(out, c)
		}
	}
	return out
}

// requireOnlyOpBinding fails unless the body is exactly uid, resourceVersion and the op.
func requireOnlyOpBinding(t *testing.T, raw map[string]json.RawMessage, uid, rv string) {
	t.Helper()
	if len(raw) != 1 {
		t.Fatalf("patch has top-level keys beyond metadata: %v", raw)
	}
	var md map[string]json.RawMessage
	if err := json.Unmarshal(raw["metadata"], &md); err != nil {
		t.Fatal(err)
	}
	if len(md) != 3 {
		t.Fatalf("metadata has keys beyond uid/resourceVersion/annotations: %s", raw["metadata"])
	}
	var got struct {
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Annotations     map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(raw["metadata"], &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{annotationHibernationOp: wakeCanaryOp}
	if got.UID != uid || got.ResourceVersion != rv || !reflect.DeepEqual(got.Annotations, want) {
		t.Fatalf("patch metadata = %+v, want uid=%s rv=%s annotations=%v", got, uid, rv, want)
	}
}

func TestWakeCanaryBindsOpWithoutAdmissionThenReportsReady(t *testing.T) {
	wc := newWakeCanary(t, nil)
	wc.requireUnadmitted(t)
	before := wc.dep(t, "w1").DeepCopy()

	wc.c.ClearActions()
	wc.mustTick(t)
	patches := wc.wakePatches(t)
	if len(patches) != 1 {
		t.Fatalf("want exactly one op patch on w1, got %d; trace %v", len(patches), actionTrace(wc.c))
	}
	requireOnlyOpBinding(t, patches[0], string(before.UID), before.ResourceVersion)
	after := wc.dep(t, "w1")
	if after.Annotations[annotationHibernationOp] != wakeCanaryOp {
		t.Fatalf("op annotation = %q", after.Annotations[annotationHibernationOp])
	}
	if after.Annotations[annotationRoll] != before.Annotations[annotationRoll] {
		t.Fatal("roll marker annotation changed")
	}
	if !reflect.DeepEqual(after.Spec, before.Spec) {
		t.Fatal("annotation-only binding changed the Deployment spec")
	}
	if n := len(wc.readyCalls()); n != 0 {
		t.Fatalf("ready reported on the binding tick: %d", n)
	}

	wc.c.ClearActions()
	wc.mustTick(t)
	if n := len(wc.wakePatches(t)); n != 0 {
		t.Fatalf("second tick patched the op again: %d", n)
	}
	ready := wc.readyCalls()
	if len(ready) != 1 || ready[0].OpID != wakeCanaryOp || ready[0].PodUID != "pod-uid-w1" {
		t.Fatalf("ready calls = %+v", ready)
	}
}

func TestWakeCanaryOpBindingControlsStayUnwritten(t *testing.T) {
	zero, two := int32(0), int32(2)
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"replicas zero": func(d *appsv1.Deployment) { d.Spec.Replicas = &zero },
		"replicas two":  func(d *appsv1.Deployment) { d.Spec.Replicas = &two },
		"replicas nil":  func(d *appsv1.Deployment) { d.Spec.Replicas = nil },
		"drifted hash": func(d *appsv1.Deployment) {
			d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-hash"
		},
	} {
		t.Run(name, func(t *testing.T) {
			wc := newWakeCanary(t, mutate)
			for i := 0; i < 3; i++ {
				wc.c.ClearActions()
				_ = wc.tick(t) // a control may legitimately return roll errors; only writes matter
				if n := len(wc.wakePatches(t)); n != 0 {
					t.Fatalf("tick %d: unadmitted wake wrote the op annotation: %v", i, actionTrace(wc.c))
				}
				if n := len(wc.readyCalls()); n != 0 {
					t.Fatalf("tick %d: ready reported without the op bound", i)
				}
			}
		})
	}
}

func TestWakeCanaryOpAlreadyBoundReportsReadyWithoutPatch(t *testing.T) {
	wc := newWakeCanary(t, func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOp })
	wc.requireUnadmitted(t)
	wc.c.ClearActions()
	wc.mustTick(t)
	if n := len(wc.wakePatches(t)); n != 0 {
		t.Fatalf("patched an already-bound op: %d", n)
	}
	if n := len(wc.readyCalls()); n != 1 {
		t.Fatalf("ready calls = %d, want 1", n)
	}
}

func TestWakeCanaryOpBindingConflictReobserves(t *testing.T) {
	wc := newWakeCanary(t, nil)
	wc.requireUnadmitted(t)
	conflicts := 0
	wc.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		var body struct {
			Metadata struct {
				Annotations map[string]*string `json:"annotations"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(p.GetPatch(), &body)
		if _, ok := body.Metadata.Annotations[annotationHibernationOp]; ok && conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, p.GetName(), nil)
		}
		return false, nil, nil
	})
	wc.mustTick(t) // a Conflict is a stale observation, not a failure
	if conflicts != 1 {
		t.Fatalf("conflict was not injected: %d", conflicts)
	}
	if n := len(wc.readyCalls()); n != 0 {
		t.Fatal("ready reported after a conflicted binding")
	}
	if wc.dep(t, "w1").Annotations[annotationHibernationOp] != wakeCanaryOldOp {
		t.Fatal("conflicted patch landed")
	}
	wc.mustTick(t)
	if wc.dep(t, "w1").Annotations[annotationHibernationOp] != wakeCanaryOp {
		t.Fatal("retry tick did not bind the op")
	}
	wc.mustTick(t)
	if n := len(wc.readyCalls()); n != 1 {
		t.Fatalf("ready calls after retry = %d, want 1", n)
	}
}

func TestWakeCanaryOpBindingOmitsOperatorMetadata(t *testing.T) {
	wc := newWakeCanary(t, nil)
	wc.m.cfg.DeploymentLabels = map[string]string{"team": "platform"}
	wc.m.cfg.DeploymentAnnotations = map[string]string{"policy.example.com/skip-probes": "true"}
	wc.requireUnadmitted(t)
	before := wc.dep(t, "w1").DeepCopy()
	wc.c.ClearActions()
	wc.mustTick(t)
	patches := wc.wakePatches(t)
	if len(patches) != 1 {
		t.Fatalf("want one op patch, got %d; trace %v", len(patches), actionTrace(wc.c))
	}
	requireOnlyOpBinding(t, patches[0], string(before.UID), before.ResourceVersion)
}

func TestWakeAnnotationPermittedGuards(t *testing.T) {
	cov := rollCoverage{Owner: "owner-A", Lane: "ordinary"}
	build := func(mut func(e *rollEntry, tick *rollTick)) context.Context {
		e := &rollEntry{coverage: cov}
		tick := &rollTick{
			entries:         map[string]*rollEntry{"w": e},
			uncertain:       map[string]bool{},
			migrationFences: map[string]bool{},
		}
		if mut != nil {
			mut(e, tick)
		}
		return context.WithValue(context.Background(), rollTickKey{}, tick)
	}
	if !wakeAnnotationPermitted(build(nil), "w") {
		t.Fatal("clean entry was refused")
	}
	// A pool-level hold without a block on the entry does not stop the write, and neither
	// does a missing image admission.
	if !wakeAnnotationPermitted(build(func(e *rollEntry, _ *rollTick) { e.admitted = false }), "w") {
		t.Fatal("an unadmitted entry was refused")
	}
	if wakeAnnotationPermitted(context.Background(), "w") {
		t.Fatal("no tick in context was permitted")
	}
	if wakeAnnotationPermitted(build(nil), "other") {
		t.Fatal("nil entry was permitted")
	}
	for name, mut := range map[string]func(e *rollEntry, tick *rollTick){
		"hibernation":          func(e *rollEntry, _ *rollTick) { e.hibernation = true },
		"blocked":              func(e *rollEntry, _ *rollTick) { e.blocked = "held" },
		"snapshotIncomplete":   func(e *rollEntry, _ *rollTick) { e.snapshotIncomplete = true },
		"historyRetireUnknown": func(e *rollEntry, _ *rollTick) { e.historyRetireUnknown = true },
		"uncertain": func(e *rollEntry, tick *rollTick) {
			tick.uncertain[coverageKey(e.coverage)] = true
		},
		"migration fenced": func(e *rollEntry, tick *rollTick) {
			tick.migrationFences[migrationLane(e.coverage)] = true
		},
	} {
		if wakeAnnotationPermitted(build(mut), "w") {
			t.Errorf("%s: write was permitted", name)
		}
	}
}
