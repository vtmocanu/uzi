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

// allPatches returns the bodies of every Deployment patch recorded for id, whatever it writes.
func (wc *wakeCanary) allPatches(t *testing.T, id string) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	for _, a := range wc.c.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || a.GetResource().Resource != "deployments" || p.GetName() != deploymentName(id) {
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

// requirePatchCount fails unless id's Deployment saw exactly want patches since the last ClearActions.
func (wc *wakeCanary) requirePatchCount(t *testing.T, id string, want int) {
	t.Helper()
	if n := len(wc.allPatches(t, id)); n != want {
		t.Fatalf("%s Deployment patches = %d, want %d; trace %v", id, n, want, actionTrace(wc.c))
	}
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
	peers := wc.peerSnapshot(t)

	wc.c.ClearActions()
	wc.mustTick(t)
	// The only Deployment write on w1 is the annotation-only op binding.
	wc.requirePatchCount(t, "w1", 1)
	requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
	wc.requirePeersHeld(t, peers)
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
	wc.requireUnadmitted(t)

	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 0)
	wc.requirePeersHeld(t, peers)
	ready := wc.readyCalls()
	if len(ready) != 1 || ready[0].OpID != wakeCanaryOp || ready[0].PodUID != "pod-uid-w1" {
		t.Fatalf("ready calls = %+v", ready)
	}
	wc.requireUnadmitted(t)
}

// A peer whose own Deployment template is also stale (not just its pod) is still held:
// the canary's binding tick writes only w1's op annotation and the peer stays drifted.
func TestWakeCanaryBindsOpWithStaleTemplatePeer(t *testing.T) {
	wc := newWakeCanary(t, nil)
	wc.edit(t, "w3", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-hash" })
	wc.requireUnadmitted(t)
	before := wc.dep(t, "w1").DeepCopy()
	peers := wc.peerSnapshot(t)
	for i := 0; i < 2; i++ {
		wc.c.ClearActions()
		wc.mustTick(t)
		if i == 0 {
			wc.requirePatchCount(t, "w1", 1)
			requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
		} else {
			wc.requirePatchCount(t, "w1", 0)
		}
		wc.requirePeersHeld(t, peers)
		wc.requireUnadmitted(t)
	}
	if n := len(wc.readyCalls()); n != 1 {
		t.Fatalf("ready calls = %d, want 1", n)
	}
}

// peerState is what holds a peer drifted: its Deployment spec and its still-old pod.
type peerState struct {
	spec appsv1.DeploymentSpec
	pods []string
}

// peerSnapshot records the drifted peers w2 and w3 of the open cohort. Their incarnations
// are not proven, their pods still carry the old template, and they keep the canary open.
func (wc *wakeCanary) peerSnapshot(t *testing.T) map[string]peerState {
	t.Helper()
	out := map[string]peerState{}
	for _, id := range []string{"w2", "w3"} {
		d := wc.dep(t, id)
		list, err := wc.c.CoreV1().Pods(wc.m.cfg.Namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var pods []string
		for _, p := range list.Items {
			if p.Name == "old-pod-"+deploymentName(id) {
				if p.Annotations[AnnotationSpecHash] == d.Spec.Template.Annotations[AnnotationSpecHash] {
					t.Fatalf("%s pod is not drifted from its Deployment template", id)
				}
				pods = append(pods, p.Name)
			}
		}
		if len(pods) != 1 {
			t.Fatalf("%s has no old pod: %v", id, pods)
		}
		out[id] = peerState{spec: d.Spec, pods: pods}
	}
	return out
}

// requirePeersHeld fails if a peer was patched, deleted or had its spec changed: the
// canary's op binding must not roll or admit anything else.
func (wc *wakeCanary) requirePeersHeld(t *testing.T, want map[string]peerState) {
	t.Helper()
	for id, w := range want {
		wc.requirePatchCount(t, id, 0)
		if got := wc.peerSnapshot(t)[id]; !reflect.DeepEqual(got, w) {
			t.Fatalf("%s peer changed: %+v want %+v", id, got, w)
		}
	}
	for _, a := range wc.c.Actions() {
		if a.GetVerb() == "delete" {
			t.Fatalf("a delete was issued: %v", actionTrace(wc.c))
		}
	}
}

// driveWake plans a tick against the fixture and calls hibernateWake for w1 directly with
// that tick's context, the way the pass-2 unadmitted branch does. mut may adjust the entry
// or tick first (a guard the Reconcile caller filters before the call, so only a direct
// call reaches the guard in hibernateWake); opID overrides the wake's op.
func (wc *wakeCanary) driveWake(t *testing.T, opID string, mut func(e *rollEntry, tick *rollTick)) error {
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
	// Force the unadmitted entry the pass-2 branch would hand to hibernateWake. A
	// zero-replica Deployment whose pod is gone never reaches that branch through
	// Reconcile: the pacer admits it, so Reconcile takes the admitted wake path
	// (TestWakeCanaryZeroReplicasNoPodTakesAdmittedWakePath). With its pod still live it
	// does reach pass 2 unadmitted, where wakeHeldForPods holds first
	// (TestWakeCanaryZeroReplicasLivePodThroughReconcile). The open wake reservation has
	// already cleared e.hibernation (the rejoin loop in roll_pacing.go).
	if e.hibernation {
		t.Fatal("w1 is still hibernation-owned; the rejoin loop did not run")
	}
	e.admitted = false
	if mut != nil {
		mut(e, tick)
	}
	w := *wc.at("w1")
	h := *w.Hibernation
	h.OpID = opID
	w.Hibernation = &h
	ns, err := wc.m.cfg.Placement(w)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := wc.m.resolveWorker(w)
	if err != nil {
		t.Fatal(err)
	}
	return wc.m.hibernateWake(context.WithValue(ctx, rollTickKey{}, tick), w, e.o, ns, spec)
}

// TestWakeCanaryDirectWakeGuards drives hibernateWake past the pass-2 filters: the same
// state that binds the op with a clean tick (control) must write nothing when the entry is
// blocked, its coverage is uncertain or migration-fenced, or the wake carries no op.
func TestWakeCanaryDirectWakeGuards(t *testing.T) {
	zero := int32(0)
	cases := map[string]struct {
		op      string
		mut     func(e *rollEntry, tick *rollTick)
		patches int
		zero    bool // Deployment at zero replicas with its pod removed, so no pod holds the wake
	}{
		"clean control": {op: wakeCanaryOp, patches: 1},
		"replicas zero": {op: wakeCanaryOp, zero: true},
		"blocked":       {op: wakeCanaryOp, mut: func(e *rollEntry, _ *rollTick) { e.blocked = "held" }},
		"uncertain": {op: wakeCanaryOp, mut: func(e *rollEntry, tick *rollTick) {
			tick.uncertain[coverageKey(e.coverage)] = true
		}},
		"migration fenced": {op: wakeCanaryOp, mut: func(e *rollEntry, tick *rollTick) {
			tick.migrationFences[migrationLane(e.coverage)] = true
		}},
		"empty op": {op: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var mutate func(*appsv1.Deployment)
			if tc.zero {
				mutate = func(d *appsv1.Deployment) { d.Spec.Replicas = &zero }
			}
			wc := newWakeCanary(t, mutate)
			if tc.zero {
				if err := wc.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), wc.m.cfg.Namespace, "pod-w1"); err != nil {
					t.Fatal(err)
				}
			}
			wc.c.ClearActions()
			if err := wc.driveWake(t, tc.op, tc.mut); err != nil {
				t.Fatal(err)
			}
			wc.requirePatchCount(t, "w1", tc.patches)
			if n := len(wc.readyCalls()); n != 0 {
				t.Fatalf("ready reported: %d", n)
			}
		})
	}
}

// TestWakeCanaryOpBindingRejectsUnprovenResponse: a patch response that does not prove the
// advanced resourceVersion on the same Deployment is an error, and no ready follows.
func TestWakeCanaryOpBindingRejectsUnprovenResponse(t *testing.T) {
	for name, tweak := range map[string]func(d *appsv1.Deployment){
		"unchanged resourceVersion": func(*appsv1.Deployment) {},
		"different uid":             func(d *appsv1.Deployment) { d.UID += "-other"; d.ResourceVersion += "-next" },
	} {
		t.Run(name, func(t *testing.T) {
			wc := newWakeCanary(t, nil)
			wc.requireUnadmitted(t)
			wc.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
				p := a.(k8stesting.PatchAction)
				if p.GetName() != deploymentName("w1") {
					return false, nil, nil
				}
				// Read the tracker, not the clientset: the fake holds its lock in a reactor.
				obj, err := wc.c.Tracker().Get(appsv1.SchemeGroupVersion.WithResource("deployments"), p.GetNamespace(), p.GetName())
				if err != nil {
					return true, nil, err
				}
				d := obj.(*appsv1.Deployment).DeepCopy()
				tweak(d)
				return true, d, nil
			})
			wc.c.ClearActions()
			if err := wc.tick(t); err == nil {
				t.Fatal("unproven patch response was not an error")
			}
			// Exactly the one op binding was attempted; nothing else was written to w1.
			wc.requirePatchCount(t, "w1", 1)
			if n := len(wc.readyCalls()); n != 0 {
				t.Fatalf("ready reported after an unproven binding: %d", n)
			}
		})
	}
}

func TestWakeCanaryOpBindingControlsStayUnwritten(t *testing.T) {
	two := int32(2)
	for name, mutate := range map[string]func(*appsv1.Deployment){
		"replicas two": func(d *appsv1.Deployment) { d.Spec.Replicas = &two },
		"replicas nil": func(d *appsv1.Deployment) { d.Spec.Replicas = nil },
		"drifted hash": func(d *appsv1.Deployment) {
			d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-hash"
		},
	} {
		t.Run(name, func(t *testing.T) {
			wc := newWakeCanary(t, mutate)
			for i := 0; i < 3; i++ {
				wc.c.ClearActions()
				_ = wc.tick(t) // a control may legitimately return roll errors; only writes matter
				wc.requirePatchCount(t, "w1", 0)
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
	wc.requirePatchCount(t, "w1", 0)
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
	wc.c.ClearActions()
	wc.mustTick(t) // a Conflict is a stale observation, not a failure
	// The one attempted patch is the conflicting op binding; nothing else was written.
	wc.requirePatchCount(t, "w1", 1)
	if conflicts != 1 {
		t.Fatalf("conflict was not injected: %d", conflicts)
	}
	if n := len(wc.readyCalls()); n != 0 {
		t.Fatal("ready reported after a conflicted binding")
	}
	if wc.dep(t, "w1").Annotations[annotationHibernationOp] != wakeCanaryOldOp {
		t.Fatal("conflicted patch landed")
	}
	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 1)
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
	wc.requirePatchCount(t, "w1", 1)
	requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
}

// A zero-replica canary with a live pod is not the op-binding case: Reconcile writes nothing
// to w1 and reports no ready.
func TestWakeCanaryZeroReplicasLivePodThroughReconcile(t *testing.T) {
	zero := int32(0)
	wc := newWakeCanary(t, func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
	// This state reaches the pass-2 wake unadmitted; both the zero-replica pod hold and
	// the op binding's replicas == 1 term keep it unwritten.
	wc.requireUnadmitted(t)
	for i := 0; i < 3; i++ {
		wc.c.ClearActions()
		wc.mustTick(t)
		wc.requirePatchCount(t, "w1", 0)
		if n := len(wc.readyCalls()); n != 0 {
			t.Fatalf("tick %d: ready reported", i)
		}
	}
}

// Replicas zero with the pod removed is the premise of the direct-drive zero case: the
// pacer admits w1, so Reconcile takes the admitted full wake path (replicas restored, op
// bound) and reports no ready that tick.
func TestWakeCanaryZeroReplicasNoPodTakesAdmittedWakePath(t *testing.T) {
	zero := int32(0)
	wc := newWakeCanary(t, func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
	if err := wc.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), wc.m.cfg.Namespace, "pod-w1"); err != nil {
		t.Fatal(err)
	}
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
	if e := tick.entries["w1"]; e == nil || !e.admitted {
		t.Fatalf("planning did not admit w1: %+v", e)
	}
	wc.c.ClearActions()
	wc.mustTick(t)
	d := wc.dep(t, "w1")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("replicas not restored to 1: %v", d.Spec.Replicas)
	}
	if d.Annotations[annotationHibernationOp] != wakeCanaryOp {
		t.Fatalf("op not bound: %q", d.Annotations[annotationHibernationOp])
	}
	if n := len(wc.readyCalls()); n != 0 {
		t.Fatalf("ready reported on the wake tick: %d", n)
	}
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
	// A missing image admission does not stop the write.
	if !wakeAnnotationPermitted(build(func(e *rollEntry, _ *rollTick) { e.admitted = false }), "w") {
		t.Fatal("an unadmitted entry was refused")
	}
	if wakeAnnotationPermitted(context.Background(), "w") {
		t.Fatal("no tick in context was permitted")
	}
	if wakeAnnotationPermitted(build(nil), "other") {
		t.Fatal("nil entry was permitted")
	}
	// An entry block alone refuses; the duplicate-canary wake authorization lifts only
	// that term.
	if !wakeAnnotationPermitted(build(func(e *rollEntry, _ *rollTick) { e.blocked, e.duplicateWakeSettle = "held", true }), "w") {
		t.Fatal("a blocked entry with duplicateWakeSettle was refused")
	}
	for name, mut := range map[string]func(e *rollEntry, tick *rollTick){
		"settle: hibernation": func(e *rollEntry, _ *rollTick) {
			e.blocked, e.duplicateWakeSettle, e.hibernation = "held", true, true
		},
		"settle: snapshotIncomplete": func(e *rollEntry, _ *rollTick) {
			e.blocked, e.duplicateWakeSettle, e.snapshotIncomplete = "held", true, true
		},
		"settle: historyRetireUnknown": func(e *rollEntry, _ *rollTick) {
			e.blocked, e.duplicateWakeSettle, e.historyRetireUnknown = "held", true, true
		},
		"settle: uncertain": func(e *rollEntry, tick *rollTick) {
			e.blocked, e.duplicateWakeSettle = "held", true
			tick.uncertain[coverageKey(e.coverage)] = true
		},
		"settle: migration fenced": func(e *rollEntry, tick *rollTick) {
			e.blocked, e.duplicateWakeSettle = "held", true
			tick.migrationFences[migrationLane(e.coverage)] = true
		},
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
