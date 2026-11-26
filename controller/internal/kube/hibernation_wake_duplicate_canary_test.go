package kube

import (
	"context"
	"encoding/json"
	"fmt"
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
)

// Issue #4723: a waking worker that is the CURRENT canary of a pool held with
// "duplicate canary reservations unresolved; explicit recovery required" must still
// settle its wake (annotation-only op binding, then ready). The fixture is the wake
// canary of newWakeCanary plus a second, LIVE canary w2 whose reservation is stale and
// whose custody hold keeps the duplicate-cancellation proof from ever succeeding.

// newDupWake makes w1 the waking current canary and w2 a live stale duplicate canary.
// mutateW1 adjusts w1's Deployment as newWakeCanary does.
func newDupWake(t *testing.T, mutateW1 func(*appsv1.Deployment)) *wakeCanary {
	t.Helper()
	wc := newWakeCanary(t, mutateW1)
	mk := wc.marker(t, "w2")
	mk.Canary = true
	raw, err := json.Marshal(mk)
	if err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	wc.edit(t, "w2", func(d *appsv1.Deployment) {
		d.Annotations[annotationRoll] = string(raw)
		d.Spec.Replicas = &one
		d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-template-hash"
	})
	wc.at("w2").CustodyHeld = true
	wc.c.ClearActions()
	return wc
}

// bumpRV advances w1's resourceVersion out of band, as any apiserver update does.
func (wc *wakeCanary) bumpRV(t *testing.T) string {
	t.Helper()
	wc.edit(t, "w1", func(*appsv1.Deployment) {})
	return wc.dep(t, "w1").ResourceVersion
}

// dupReservations is every roll annotation the duplicate-canary pool carries.
func (wc *wakeCanary) dupReservations(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, id := range []string{"w1", "w2"} {
		out["dep "+id] = wc.dep(t, id).Annotations[annotationRoll]
		p, err := wc.c.CoreV1().PersistentVolumeClaims(wc.m.cfg.Namespace).Get(context.Background(), dataPVCName(id), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out["claim "+id] = p.Annotations[annotationRoll]
	}
	return out
}

func (wc *wakeCanary) requireDupReasons(t *testing.T, label string) {
	t.Helper()
	for _, id := range []string{"w1", "w2"} {
		if _, _, reason, _ := wc.pacing(id); reason != dupHeld {
			t.Fatalf("%s: %s reason = %q, want %q", label, id, reason, dupHeld)
		}
	}
}

func (wc *wakeCanary) requireReservations(t *testing.T, want map[string]string, label string) {
	t.Helper()
	got := wc.dupReservations(t)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s annotation changed:\n got %q\nwant %q", label, k, got[k], v)
		}
	}
}

func TestDuplicateCanaryWakeSettlesOpAndReady(t *testing.T) {
	for name, custody := range map[string]bool{"w1 without custody": false, "w1 with custody": true} {
		t.Run(name, func(t *testing.T) {
			wc := newDupWake(t, nil)
			wc.at("w1").CustodyHeld = custody
			before := wc.dep(t, "w1").DeepCopy()
			res := wc.dupReservations(t)

			// Tick 1: exactly the annotation-only op binding, conditioned on the tick-start RV.
			wc.c.ClearActions()
			wc.mustTick(t)
			wc.requirePatchCount(t, "w1", 1)
			requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
			wc.requirePatchCount(t, "w2", 0)
			if n := len(wc.readyCalls()); n != 0 {
				t.Fatalf("ready reported on the binding tick: %d", n)
			}
			if got := wc.dep(t, "w1").Annotations[annotationHibernationOp]; got != wakeCanaryOp {
				t.Fatalf("op annotation = %q", got)
			}
			wc.requireReservations(t, res, "tick 1")
			wc.requireDupReasons(t, "tick 1")
			r1 := wc.dep(t, "w1").ResourceVersion
			r2 := wc.bumpRV(t)
			if r1 == r2 {
				t.Fatalf("resourceVersion did not change: %s", r1)
			}

			// Tick 2: no write, and the ready carries the freshly observed RV.
			wc.c.ClearActions()
			wc.mustTick(t)
			wc.requirePatchCount(t, "w1", 0)
			wc.requirePatchCount(t, "w2", 0)
			ready := wc.readyCalls()
			if len(ready) != 1 || ready[0].OpID != wakeCanaryOp || ready[0].PodUID != "pod-uid-w1" || ready[0].ResourceVersion != r2 {
				t.Fatalf("ready calls = %+v, want one at RV %s", ready, r2)
			}
			wc.requireReservations(t, res, "tick 2")
			wc.requireDupReasons(t, "tick 2")
		})
	}
}

// runDupControl drives two ticks and requires that nothing is written to either
// Deployment, no ready is reported, the reservations stay and the duplicate hold is shown.
func runDupControl(t *testing.T, wc *wakeCanary, tick func(i int)) {
	t.Helper()
	res := wc.dupReservations(t)
	for i := 0; i < 2; i++ {
		wc.c.ClearActions()
		tick(i)
		wc.requirePatchCount(t, "w1", 0)
		wc.requirePatchCount(t, "w2", 0)
		if n := len(wc.readyCalls()); n != 0 {
			t.Fatalf("tick %d: ready reported: %d", i, n)
		}
		wc.requireReservations(t, res, fmt.Sprintf("tick %d", i))
		wc.requireDupReasons(t, fmt.Sprintf("tick %d", i))
	}
}

func TestDuplicateCanaryWakeControlsStayUnwritten(t *testing.T) {
	zero := int32(0)
	cases := map[string]func(t *testing.T) (*wakeCanary, func(i int)){
		// (a) the waking worker is the stale duplicate, not the current canary.
		"a waking worker is the stale canary": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			wc.at("w1").Hibernation = nil
			wc.edit(t, "w2", func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOldOp })
			wc.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: wakeCanaryOp}
			return wc, func(int) { wc.mustTick(t) }
		},
		// (b) behavioural only: an undecodable mirror blocks w1 on its own.
		"b undecodable retained reservation": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: "{not json"}
			})
			return wc, func(int) { wc.mustTick(t) }
		},
		"c1 template drift": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-hash" })
			return wc, func(int) { _ = wc.tick(t) }
		},
		"c2 observe generation drift": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			wc.at("w1").Generation++
			return wc, func(int) { _ = wc.tick(t) }
		},
		"d replicas zero": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
			return wc, func(int) { _ = wc.tick(t) }
		},
		"h stuck roll phase": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
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
			obs, err := wc.m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range obs {
				if o.ID == "w1" && o.Roll.Phase != protocol.PhaseStuck {
					t.Fatalf("fixture is not stuck: %q", o.Roll.Phase)
				}
			}
			return wc, func(int) { _ = wc.tick(t) }
		},
		"i reservation marker mismatch": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			mk := wc.marker(t, "w1")
			mk.TargetVersion = "9.9.9"
			raw, err := json.Marshal(mk)
			if err != nil {
				t.Fatal(err)
			}
			wc.edit(t, "w1", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
			return wc, func(int) { _ = wc.tick(t) }
		},
		// (j) only the Deployment's generation annotation moves between Observe and the
		// roll snapshot, so hibernateWake's own observed-generation check still passes.
		"j template generation changes after observe": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			return wc, func(i int) {
				if i > 0 {
					_ = wc.tick(t)
					return
				}
				obs, err := wc.m.Observe(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				wc.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationGeneration] = "9999" })
				_ = wc.m.Reconcile(context.Background(), wc.workers, obs)
			}
		},
		// (k) the op is already bound and the worker Ready, but an independent block holds it.
		"k bound ready with independent block": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOp })
			wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: "{not json"}
			})
			return wc, func(int) { wc.mustTick(t) }
		},
		// (l) a completed maintenance mirror byte-identical to the Deployment marker stays.
		// maintenanceActive is false for phase "complete" (see maintenanceActive).
		"l completed maintenance mirror": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			raw := wc.dep(t, "w1").Annotations[annotationRoll]
			wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: raw}
			})
			wc.at("w1").DindMaintenance = &protocol.DindMaintenance{DeploymentUID: string(wc.dep(t, "w1").UID), Phase: "complete"}
			return wc, func(int) { _ = wc.tick(t) }
		},
		// (l2) a valid mirror that differs from the Deployment marker.
		"l2 differing valid mirror": func(t *testing.T) (*wakeCanary, func(int)) {
			wc := newDupWake(t, nil)
			mk := wc.marker(t, "w1")
			mk.Nonce = "other-registration"
			raw, err := json.Marshal(mk)
			if err != nil {
				t.Fatal(err)
			}
			wc.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				p.Annotations = map[string]string{annotationRoll: string(raw)}
			})
			return wc, func(int) { _ = wc.tick(t) }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			wc, tick := setup(t)
			wc.c.ClearActions()
			runDupControl(t, wc, tick)
		})
	}
}

// (e) A Conflict on the op binding is one attempt that writes nothing and reports no ready.
func TestDuplicateCanaryWakeConflictWritesNothing(t *testing.T) {
	wc := newDupWake(t, nil)
	before := wc.dep(t, "w1").DeepCopy()
	res := wc.dupReservations(t)
	attempts := 0
	wc.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		p := a.(k8stesting.PatchAction)
		if p.GetName() != deploymentName("w1") {
			return false, nil, nil
		}
		attempts++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, p.GetName(), nil)
	})
	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 1)
	requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
	wc.requirePatchCount(t, "w2", 0)
	if after := wc.dep(t, "w1"); !reflect.DeepEqual(after, before) {
		t.Fatalf("w1 Deployment changed after a Conflict:\n got %+v\nwant %+v", after, before)
	}
	wc.requireReservations(t, res, "conflict")
	if n := len(wc.readyCalls()); n != 0 {
		t.Fatalf("ready reported: %d", n)
	}
	wc.requireDupReasons(t, "conflict")
}

// mirrorSibling puts a claim mirror byte-identical to w2's Deployment marker on w2's data claim.
func mirrorSibling(t *testing.T, wc *wakeCanary) {
	t.Helper()
	raw := wc.dep(t, "w2").Annotations[annotationRoll]
	wc.editClaim(t, dataPVCName("w2"), func(p *corev1.PersistentVolumeClaim) {
		p.Annotations = map[string]string{annotationRoll: raw}
	})
}

// (f) A sibling's mirror cleanup that fails leaves w2 blocked "unconfirmed" and, in the same
// tick, marks the pool mutated; the pool-wide terms together refuse w1's settlement. The
// Isolation test is what isolates each term.
func TestDuplicateCanaryWakeFailedSiblingMirrorCleanup(t *testing.T) {
	wc := newDupWake(t, nil)
	mirrorSibling(t, wc)
	hits := 0
	wc.c.PrependReactor("patch", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() != dataPVCName("w2") {
			return false, nil, nil
		}
		hits++
		return true, nil, fmt.Errorf("claim patch failed")
	})
	res := wc.dupReservations(t)
	for i := 0; i < 2; i++ {
		wc.c.ClearActions()
		_ = wc.tick(t)
		wc.requirePatchCount(t, "w1", 0)
		wc.requirePatchCount(t, "w2", 0)
		if n := len(wc.readyCalls()); n != 0 {
			t.Fatalf("tick %d: ready reported", i)
		}
		wc.requireReservations(t, res, fmt.Sprintf("tick %d", i))
	}
	if hits == 0 {
		t.Fatal("the w2 claim patch was never attempted")
	}
}

// (f2) A sibling's mirror cleanup that succeeds is a mutation of the tick: w1 waits one
// tick, then settles.
func TestDuplicateCanaryWakeSiblingMirrorCleanupThenSettles(t *testing.T) {
	wc := newDupWake(t, nil)
	mirrorSibling(t, wc)
	before := wc.dep(t, "w1").DeepCopy()
	depRes := map[string]string{"w1": wc.dep(t, "w1").Annotations[annotationRoll], "w2": wc.dep(t, "w2").Annotations[annotationRoll]}
	requireDeps := func(label string) {
		t.Helper()
		for id, want := range depRes {
			if got := wc.dep(t, id).Annotations[annotationRoll]; got != want {
				t.Fatalf("%s: %s deployment reservation changed", label, id)
			}
		}
	}
	claimMirror := func() string {
		p, err := wc.c.CoreV1().PersistentVolumeClaims(wc.m.cfg.Namespace).Get(context.Background(), dataPVCName("w2"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return p.Annotations[annotationRoll]
	}

	wc.c.ClearActions()
	_ = wc.tick(t)
	if claimMirror() != "" {
		t.Fatal("tick 1: w2's claim mirror was not removed")
	}
	wc.requirePatchCount(t, "w1", 0)
	wc.requirePatchCount(t, "w2", 0)
	requireDeps("tick 1")

	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 1)
	requireOnlyOpBinding(t, wc.allPatches(t, "w1")[0], string(before.UID), before.ResourceVersion)
	requireDeps("tick 2")
	r2 := wc.bumpRV(t)

	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 0)
	ready := wc.readyCalls()
	if len(ready) != 1 || ready[0].ResourceVersion != r2 {
		t.Fatalf("ready calls = %+v, want one at RV %s", ready, r2)
	}
	requireDeps("tick 3")
}

// (g) is TestDuplicateCanaryWakeSettlesOpAndReady's custody subtest. (k+) follows.
func TestDuplicateCanaryWakeBoundReadyReportsWithRefreshedRV(t *testing.T) {
	wc := newDupWake(t, func(d *appsv1.Deployment) { d.Annotations[annotationHibernationOp] = wakeCanaryOp })
	res := wc.dupReservations(t)
	r2 := wc.bumpRV(t)
	wc.c.ClearActions()
	wc.mustTick(t)
	wc.requirePatchCount(t, "w1", 0)
	wc.requirePatchCount(t, "w2", 0)
	ready := wc.readyCalls()
	if len(ready) != 1 || ready[0].ResourceVersion != r2 {
		t.Fatalf("ready calls = %+v, want one at RV %s", ready, r2)
	}
	wc.requireReservations(t, res, "ready")
	wc.requireDupReasons(t, "ready")
}

// Two waking current canaries in one duplicate-held pool are both authorized: each binds
// only its own op, neither reservation changes, and the pool keeps the duplicate reason.
func TestDuplicateCanaryTwoWakingCurrentCanariesEachBindOwnOp(t *testing.T) {
	wc := newWakeCanary(t, nil)
	mk := wc.marker(t, "w2")
	mk.Canary = true
	raw, err := json.Marshal(mk)
	if err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	wc.edit(t, "w2", func(d *appsv1.Deployment) {
		d.Annotations[annotationRoll] = string(raw)
		d.Annotations[annotationHibernationOp] = wakeCanaryOldOp
		d.Spec.Replicas = &one
	})
	wc.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: wakeCanaryOp}
	before := map[string]*appsv1.Deployment{"w1": wc.dep(t, "w1").DeepCopy(), "w2": wc.dep(t, "w2").DeepCopy()}
	res := wc.dupReservations(t)

	wc.c.ClearActions()
	wc.mustTick(t)
	for _, id := range []string{"w1", "w2"} {
		wc.requirePatchCount(t, id, 1)
		requireOnlyOpBinding(t, wc.allPatches(t, id)[0], string(before[id].UID), before[id].ResourceVersion)
		if got := wc.dep(t, id).Annotations[annotationHibernationOp]; got != wakeCanaryOp {
			t.Fatalf("%s op annotation = %q", id, got)
		}
	}
	wc.requireReservations(t, res, "tick 1")
	wc.requireDupReasons(t, "tick 1")
}
