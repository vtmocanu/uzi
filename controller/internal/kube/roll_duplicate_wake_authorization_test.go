package kube

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Planner-level tests for the duplicate-canary wake authorization (issue #4723). They
// read rollEntry flags, so they exist only with the planner change.

// planDup observes and plans one tick of the fixture without reconciling it.
func planDup(t *testing.T, wc *wakeCanary) (context.Context, *rollTick) {
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
	return ctx, tick
}

func TestDuplicateWakeAuthorizationFlags(t *testing.T) {
	invalid := func(wc *wakeCanary) {
		wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
			p.Annotations = map[string]string{annotationRoll: "{not json"}
		})
	}
	mirror := func(raw func(wc *wakeCanary) string) func(wc *wakeCanary) {
		return func(wc *wakeCanary) {
			r := raw(wc)
			wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: r}
			})
		}
	}
	cases := []struct {
		name     string
		mutate   func(*appsv1.Deployment)
		setup    func(wc *wakeCanary)
		w1Block  string
		w1Settle bool
		w1Shape  bool
	}{
		{name: "regression fixture", w1Settle: true, w1Shape: true},
		{name: "independent block on w1", setup: invalid, w1Block: "retained reservation invalid; explicit recovery required"},
		{name: "completed maintenance mirror", setup: func(wc *wakeCanary) {
			mirror(func(wc *wakeCanary) string { return wc.dep(t, "w1").Annotations[annotationRoll] })(wc)
			wc.at("w1").DindMaintenance = &protocol.DindMaintenance{DeploymentUID: string(wc.dep(t, "w1").UID), Phase: "complete"}
		}},
		{name: "differing valid mirror", setup: mirror(func(wc *wakeCanary) string {
			mk := wc.marker(t, "w1")
			mk.Nonce = "other-registration"
			raw, err := json.Marshal(mk)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		})},
		// Planner-level refusals of the behavioural controls. Each row is refused by
		// duplicateWakeSettle itself (settle false), not only at the wake write site.
		{name: "a stale waking canary", setup: func(wc *wakeCanary) {
			wc.at("w1").Hibernation = nil
			wc.edit(t, "w2", func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOldOp })
			wc.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: wakeCanaryOp}
		}, w1Shape: true},
		{name: "c1 template drift", mutate: func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-hash" }},
		// c2 is refused by the planner: the rendered template generation no longer matches
		// the Deployment's, which the generation term of the authorization reads. The wake
		// write site would also refuse it through obs.Generation != w.Generation.
		{name: "c2 observe generation drift", setup: func(wc *wakeCanary) { wc.at("w1").Generation++ }, w1Shape: true},
		{name: "d replicas zero", mutate: func(d *appsv1.Deployment) { z := int32(0); d.Spec.Replicas = &z }},
		{name: "h stuck", setup: func(wc *wakeCanary) {
			pod, err := wc.c.CoreV1().Pods(wc.m.cfg.Namespace).Get(context.Background(), "pod-w1", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  "worker",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}
			if err := wc.c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "i reservation mismatch", setup: func(wc *wakeCanary) {
			mk := wc.marker(t, "w1")
			mk.TargetVersion = "9.9.9"
			raw, err := json.Marshal(mk)
			if err != nil {
				t.Fatal(err)
			}
			wc.edit(t, "w1", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
		}},
		{name: "k bound with independent block", mutate: func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOp },
			setup: invalid, w1Block: "retained reservation invalid; explicit recovery required"},
		// f2 tick 1: the sibling's mirror cleanup succeeds and marks w2 mutated, so the
		// pool-wide mutated term refuses w1 in that tick.
		{name: "f2 sibling mirror cleanup tick", setup: func(wc *wakeCanary) { mirrorSibling(t, wc) }, w1Shape: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wc := newDupWake(t, tc.mutate)
			if tc.setup != nil {
				tc.setup(wc)
			}
			_, tick := planDup(t, wc)
			w1, w2 := tick.entries["w1"], tick.entries["w2"]
			if w1.blocked != dupHeld || w2.blocked != dupHeld {
				t.Fatalf("pool not held: %q / %q", w1.blocked, w2.blocked)
			}
			if !w1.duplicateHeld || !w2.duplicateHeld {
				t.Fatal("duplicateHeld not recorded")
			}
			if w1.independentBlock != tc.w1Block || w2.independentBlock != "" {
				t.Fatalf("independentBlock = %q / %q, want %q / empty", w1.independentBlock, w2.independentBlock, tc.w1Block)
			}
			if w1.duplicateWakeSettle != tc.w1Settle || w2.duplicateWakeSettle {
				t.Fatalf("duplicateWakeSettle = %t / %t, want %t / false", w1.duplicateWakeSettle, w2.duplicateWakeSettle, tc.w1Settle)
			}
			if got := currentCanaryShape(tick, w1); got != tc.w1Shape {
				t.Fatalf("currentCanaryShape(w1) = %t, want %t", got, tc.w1Shape)
			}
		})
	}
}

// Every case replans a fixture whose other terms all hold, with exactly one preset the
// planner's own snapshot would not have produced. The tick comes from planRoll and is
// reset to its pre-duplicate-branch state before planPool runs again on its pool.
func TestDuplicateWakeAuthorizationIsolation(t *testing.T) {
	cases := map[string]struct {
		preset func(tick *rollTick, w1, w2 *rollEntry)
		want   bool
	}{
		"control":           {preset: func(*rollTick, *rollEntry, *rollEntry) {}, want: true},
		"prior block":       {preset: func(_ *rollTick, w1, _ *rollEntry) { w1.blocked = "held by an earlier guard" }},
		"sibling unconfirm": {preset: func(_ *rollTick, _, w2 *rollEntry) { w2.blocked = "retained reservation cleanup unconfirmed" }},
		"sibling mutated":   {preset: func(_ *rollTick, _, w2 *rollEntry) { w2.mutated = true }},
		"uncertain": {preset: func(tick *rollTick, w1, _ *rollEntry) {
			tick.uncertain[coverageKey(w1.coverage)] = true
		}},
		"identity ambiguous": {preset: func(_ *rollTick, w1, _ *rollEntry) { w1.identityAmbiguous = true }},
		"stray reservation":  {preset: func(_ *rollTick, w1, _ *rollEntry) { w1.strayReservation = true }},
		"unavailable": {preset: func(tick *rollTick, w1, _ *rollEntry) {
			tick.unavailable = map[string]bool{"w1": true}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			wc := newDupWake(t, nil)
			ctx, tick := planDup(t, wc)
			var pool []*rollEntry
			for _, id := range []string{"w1", "w2", "w3"} {
				e := tick.entries[id]
				e.blocked, e.independentBlock = e.independentBlock, ""
				e.duplicateHeld, e.duplicateWakeSettle = false, false
				pool = append(pool, e)
			}
			w1, w2 := tick.entries["w1"], tick.entries["w2"]
			tc.preset(tick, w1, w2)
			wc.m.planPool(ctx, tick, pool)
			if !w1.duplicateHeld || w1.blocked != dupHeld {
				t.Fatalf("pool was not held again: held=%t blocked=%q", w1.duplicateHeld, w1.blocked)
			}
			if w1.duplicateWakeSettle != tc.want {
				t.Fatalf("w1 duplicateWakeSettle = %t, want %t", w1.duplicateWakeSettle, tc.want)
			}
			if w2.duplicateWakeSettle || tick.entries["w3"].duplicateWakeSettle {
				t.Fatal("a non-waking sibling was authorized")
			}
		})
	}
}

// (j) Only the Deployment's template generation annotation moves between Observe and
// planRoll. The planner refuses it through the generation term; hibernateWake's own
// observed-generation check would still pass on the stale observation.
func TestDuplicateWakeAuthorizationGenerationEditedBetweenObserveAndPlan(t *testing.T) {
	wc := newDupWake(t, nil)
	ctx := context.Background()
	obs, err := wc.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wc.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationGeneration] = "9999" })
	byID := map[string]reconcile.ObservedWorker{}
	for _, o := range obs {
		byID[o.ID] = o
	}
	tick, _ := wc.m.planRoll(ctx, wc.workers, byID)
	w1 := tick.entries["w1"]
	if !w1.duplicateHeld || w1.duplicateWakeSettle {
		t.Fatalf("duplicateHeld = %t, duplicateWakeSettle = %t, want true / false", w1.duplicateHeld, w1.duplicateWakeSettle)
	}
}
