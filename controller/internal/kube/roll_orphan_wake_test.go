package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const orphanWakeOp = "00000000-0000-0000-0000-000000000001"

func TestOrphanRollCohortWakeIsNotAdoptionCandidate(t *testing.T) {
	m, tick, e := provenOrphanEntry(t)
	e.w.Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: orphanWakeOp}
	if m.orphanCanaryCandidate(tick, e) {
		t.Fatal("a waking worker became an adoption candidate")
	}
	if m.promoted(tick, e) || m.currentUsable(tick, e) {
		t.Fatal("a waking worker supplied serving coverage before wake settlement")
	}
}

func TestOrphanRollCohortWakeWaitsForObservedCanary(t *testing.T) {
	o := newOrphanCohort(t, 4, 3)
	o.recycle(t, "w2")
	o.prove(t, "w3")
	o.eraseCanary(t)
	var delayed k8stesting.Action
	released := false
	o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !released && isAdoptionPatch(a) && a.(k8stesting.PatchAction).GetName() == deploymentName("w3") {
			if delayed == nil {
				delayed = a.DeepCopy()
			}
			return true, nil, fmt.Errorf("withheld adoption")
		}
		return false, nil, nil
	})
	_ = o.tick(t)
	if delayed == nil || delayed.(k8stesting.PatchAction).GetName() != deploymentName("w3") {
		t.Fatal("larger w3 was not the outstanding adoption target")
	}
	o.m = matrixRestart(o.m)
	reports := &hibRec{fakeCordoner: o.cordon}
	o.m.cordoner = reports
	o.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationPreparing, OpID: orphanWakeOp}
	_ = o.tick(t)
	before := o.dep(t, "w2").DeepCopy()
	o.at("w2").Hibernation.State = protocol.HibernationWaking
	for tick := 0; tick < 3; tick++ {
		o.c.ClearActions()
		_ = o.tick(t) // retries of the same w3 adoption also remain unconfirmed
		after := o.dep(t, "w2")
		if !reflect.DeepEqual(after.Spec, before.Spec) || after.Annotations[annotationRoll] != before.Annotations[annotationRoll] {
			t.Fatalf("tick %d: wake unfenced the smaller participant before the canary was observed: %v", tick, actionTrace(o.c))
		}
		for _, target := range adoptionPatches(o.c) {
			if target != deploymentName("w3") {
				t.Fatalf("tick %d: a waking worker became an adoption candidate", tick)
			}
		}
	}
	if handled, _, err := o.base.React(delayed); !handled || err != nil {
		t.Fatalf("delayed w3 adoption did not commit: %v", err)
	}
	released = true
	for tick := 0; tick < 4 && *o.dep(t, "w2").Spec.Replicas == 0; tick++ {
		o.mustTick(t)
	}
	if replicas := o.dep(t, "w2").Spec.Replicas; replicas == nil || *replicas != 1 {
		t.Fatal("observed canary did not permit the ordinary admitted wake")
	}
	currentProof(t, o.c, o.at("w2"), m5Now)
	pod, err := o.c.CoreV1().Pods(o.m.cfg.Namespace).Get(context.Background(), "pod-w2", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := o.c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, pod.Namespace); err != nil {
		t.Fatal(err)
	}
	o.mustTick(t)
	ready := false
	for _, report := range reports.calls {
		ready = ready || report.Transition == protocol.TransitionReady
	}
	if !ready {
		t.Fatal("ready wake could not report settlement after its replica mutation")
	}
	// Model the API's ready/Register settlement of the wake back to none.
	o.at("w2").Hibernation = nil
	o.mustTick(t)
	if got := o.canaries(t); !equalStrings(got, []string{deploymentName("w3")}) {
		t.Fatalf("wake produced duplicate canaries: %v", got)
	}
}

func TestOrphanRollCohortWakeUsesExistingAdmission(t *testing.T) {
	o := newOrphanCohort(t, 3, 2)
	o.recycle(t, "w2")
	o.at("w2").Generation++ // wake now needs a template change, not just restoration
	o.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: orphanWakeOp}
	o.m.cordoner = &hibRec{fakeCordoner: o.cordon}
	manualWorker(o.at("w1")) // a canary bit alone is not promoted-canary proof
	before := o.dep(t, "w2").Spec.DeepCopy()
	for tick := 0; tick < 3; tick++ {
		o.mustTick(t)
		if !reflect.DeepEqual(o.dep(t, "w2").Spec, *before) {
			t.Fatal("wake bypassed the normal canary promotion/coverage gate")
		}
	}
	o.at("w1").ManualCordonedAt, o.at("w1").EffectiveCordoned = nil, nil
	// Existing reservations still occupy roll slots while waking. A new worker
	// must not consume a slot that their pending incarnations already hold.
	o.m.cfg.RollConcurrency = 1
	w4 := evidence(desired("w4"), m5Now)
	for _, obj := range physicalOld(t, deployedWorker("w4", 0, "old")) {
		if err := o.c.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	o.workers = append(o.workers, w4)
	for tick := 0; tick < 4 && *o.dep(t, "w2").Spec.Replicas == 0; tick++ {
		o.mustTick(t)
	}
	if *o.dep(t, "w2").Spec.Replicas != 1 {
		t.Fatal("wake did not resume once ordinary admission permitted it")
	}
	if o.marker(t, "w4") != nil {
		t.Fatal("waking reservations disappeared from ordinary concurrency accounting")
	}
}

func TestOrphanRollCohortWakeWithoutOpenReservationUnchanged(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			o := newOrphanCohort(t, 3, 2)
			o.recycle(t, "w2")
			o.eraseCanary(t)
			o.edit(t, "w2", func(d *appsv1.Deployment) {
				if !completed {
					delete(d.Annotations, annotationRoll)
					return
				}
				marker, err := decodeRoll(d.Annotations[annotationRoll])
				if err != nil {
					t.Fatal(err)
				}
				marker.Canary, marker.Completed = true, true
				raw, err := json.Marshal(marker)
				if err != nil {
					t.Fatal(err)
				}
				d.Annotations[annotationRoll] = string(raw)
				if _, err := decodeRoll(d.Annotations[annotationRoll]); err != nil {
					t.Fatalf("completed reservation fixture is invalid: %v", err)
				}
			})
			o.at("w2").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: orphanWakeOp}
			o.m.cordoner = &hibRec{fakeCordoner: o.cordon}
			for tick := 0; tick < 4 && *o.dep(t, "w2").Spec.Replicas == 0; tick++ {
				o.mustTick(t)
			}
			if *o.dep(t, "w2").Spec.Replicas != 1 {
				t.Fatal("ordinary wake was newly gated by a sibling orphan pool")
			}
		})
	}
}
