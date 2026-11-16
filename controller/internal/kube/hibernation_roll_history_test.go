package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// A worker preparing to hibernate whose only roll history is a completed canary marker
// must be able to sleep: the marker is retired by a conditioned write in the same tick,
// unless an in-scope peer still carries roll evidence.

// rhPatch is one Deployment patch the fake apiserver received.
type rhPatch struct {
	name string
	body map[string]any
}

func rhPatches(c *fake.Clientset) []rhPatch {
	var out []rhPatch
	for _, a := range c.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "deployments" {
			var body map[string]any
			_ = json.Unmarshal(p.GetPatch(), &body)
			out = append(out, rhPatch{name: p.GetName(), body: body})
		}
	}
	return out
}

func (p rhPatch) annotations() map[string]any {
	md, _ := p.body["metadata"].(map[string]any)
	a, _ := md["annotations"].(map[string]any)
	return a
}

func (p rhPatch) rv() string {
	md, _ := p.body["metadata"].(map[string]any)
	s, _ := md["resourceVersion"].(string)
	return s
}

func (p rhPatch) touchesReplicas() bool {
	spec, _ := p.body["spec"].(map[string]any)
	_, ok := spec["replicas"]
	return ok
}

// writesRoll reports a patch that sets or clears the roll annotation.
func (p rhPatch) writesRoll() bool {
	_, ok := p.annotations()[annotationRoll]
	return ok
}

func (p rhPatch) isFence() bool {
	_, ok := p.annotations()[annotationHibernationRefused]
	return ok
}

func rhRollWrites(c *fake.Clientset, name string) int {
	n := 0
	for _, p := range rhPatches(c) {
		if p.name == name && p.writesRoll() {
			n++
		}
	}
	return n
}

func rhWorkerPatches(c *fake.Clientset, name string) []rhPatch {
	var out []rhPatch
	for _, p := range rhPatches(c) {
		if p.name == name {
			out = append(out, p)
		}
	}
	return out
}

func rhEdit(t *testing.T, d *appsv1.Deployment, fn func(*rollMarker)) {
	t.Helper()
	var r rollMarker
	if err := json.Unmarshal([]byte(d.Annotations[annotationRoll]), &r); err != nil {
		t.Fatal(err)
	}
	fn(&r)
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	d.Annotations[annotationRoll] = string(raw)
}

func rhGet(t *testing.T, c *fake.Clientset, id string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func rhFresh(t *testing.T, c *fake.Clientset, rec *hibRec) *Materializer {
	t.Helper()
	m := hibFreshMaterializer(t, c, rec)
	m.now = func() time.Time { return m5Now }
	return m
}

// rhW is the hibernating worker w1 with API roll evidence for owner-A.
func rhW(state, op string) protocol.DesiredWorker {
	return evidence(hibW(state, op), m5Now)
}

func rhCanaryDep(t *testing.T, completed bool) *appsv1.Deployment {
	t.Helper()
	return markedDeployment(t, testConfig(), rhW(protocol.HibernationPreparing, "op-1"), completed)
}

// rhPlainDep is a peer's marker-free Deployment.
func rhPlainDep(t *testing.T, w protocol.DesiredWorker) *appsv1.Deployment {
	t.Helper()
	d := RenderDeployment(testConfig(), w, testSpec(t, w.Template, w.Size))
	d.UID, d.ResourceVersion = "plain-uid-"+types.UID(w.ID), "1"
	return d
}

// rhPeer is an awake worker with its own Deployment and claims. mark edits its marker.
func rhPeer(t *testing.T, id string, canary bool, mark func(*rollMarker)) (protocol.DesiredWorker, []runtime.Object) {
	t.Helper()
	w := evidence(desired(id), m5Now)
	d := markedDeployment(t, testConfig(), w, false)
	rhEdit(t, d, func(r *rollMarker) {
		r.Canary = canary
		if mark != nil {
			mark(r)
		}
	})
	return w, []runtime.Object{d, pvcNix(id, "20Gi"), pvcFor(id, "data")}
}

func rhObjs(extra ...[]runtime.Object) []runtime.Object {
	var out []runtime.Object
	for _, e := range extra {
		out = append(out, e...)
	}
	return out
}

func rhClaim(id, kind, value string) *corev1.PersistentVolumeClaim {
	p := pvcFor(id, kind)
	if kind == "nix" {
		p = pvcNix(id, "20Gi")
	}
	p.Annotations = map[string]string{annotationRoll: value}
	return p
}

func rhAssertRefused(t *testing.T, rec *hibRec, reason string) {
	t.Helper()
	if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionRefused || rec.calls[0].Reason != reason {
		t.Fatalf("calls = %+v, want one refused with %q", rec.calls, reason)
	}
}

func rhAssertSlept(t *testing.T, c *fake.Clientset, rec *hibRec) {
	t.Helper()
	if got := rec.transitions(); len(got) == 0 || got[len(got)-1] != protocol.TransitionScalingDown {
		t.Fatalf("transitions = %v, want to end with scaling_down", got)
	}
	d := rhGet(t, c, "w1")
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", d.Spec.Replicas)
	}
	if _, ok := d.Annotations[annotationRoll]; ok {
		t.Fatalf("slept with a roll annotation: %q", d.Annotations[annotationRoll])
	}
}

// 1: a stable completed canary with no peer retires and sleeps in one tick.
func TestHibernationRetiresCompletedCanaryAndSleepsInOneTick(t *testing.T) {
	t.Run("no peer", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		m, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		var rvAtCall []string
		client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj, err := client.Tracker().Get(a.GetResource(), a.GetNamespace(), a.(k8stesting.PatchAction).GetName())
			if err == nil {
				rvAtCall = append(rvAtCall, obj.(*appsv1.Deployment).ResourceVersion)
			}
			return false, nil, nil
		})
		if err := hibTick(t, m, w); err != nil {
			t.Fatal(err)
		}
		ps := rhWorkerPatches(client, "uzi-hw-w1")
		if len(ps) != 2 {
			t.Fatalf("patches = %d, want retirement then scale-down: %+v", len(ps), ps)
		}
		if !ps[0].writesRoll() || ps[0].annotations()[annotationRoll] != nil || ps[0].touchesReplicas() {
			t.Fatalf("first patch is not a roll-annotation removal: %+v", ps[0].body)
		}
		if !ps[1].touchesReplicas() || ps[1].writesRoll() || ps[1].isFence() {
			t.Fatalf("second patch is not the scale-down: %+v", ps[1].body)
		}
		// The scale-down carries the retirement response's resourceVersion: it is the
		// object's version when the second patch arrived, and not the original.
		if len(rvAtCall) != 2 || ps[1].rv() != rvAtCall[1] || ps[1].rv() == ps[0].rv() || ps[1].rv() == "" {
			t.Fatalf("scale-down rv = %q, retirement request rv = %q, object rv at calls = %v", ps[1].rv(), ps[0].rv(), rvAtCall)
		}
		if got := rec.transitions(); len(got) != 1 || got[0] != protocol.TransitionScalingDown {
			t.Fatalf("transitions = %v, want [scaling_down]", got)
		}
		rhAssertSlept(t, client, rec)
	})

	t.Run("busy is retired then refused as busy", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		w.Busy = true
		m, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		if err := hibTick(t, m, w); err != nil {
			t.Fatal(err)
		}
		rhAssertRefused(t, rec, refuseBusy)
		if replicaPatches(client) != 0 {
			t.Fatal("a busy worker was scaled down")
		}
		if _, ok := rhGet(t, client, "w1").Annotations[annotationRoll]; ok {
			t.Fatal("the completed canary marker was not retired")
		}
	})
}

// 2: a same-key incomplete peer keeps the canary marker and reads as cleanup pending.
func TestHibernationCompletedCanaryRefusedWhileSameKeyPeerIsIncomplete(t *testing.T) {
	w := rhW(protocol.HibernationPreparing, "op-1")
	w2, peerObjs := rhPeer(t, "w2", false, nil)
	canary := rhCanaryDep(t, true)
	orig := canary.Annotations[annotationRoll]
	m, client, rec := hibEnv(t, rhObjs([]runtime.Object{canary, hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
	if err := hibTick(t, m, w, w2); err != nil {
		t.Fatal(err)
	}
	rhAssertRefused(t, rec, refuseRollCohortPending)
	if rec.calls[0].Reason != "roll_cohort_cleanup_pending" {
		t.Fatalf("reason literal = %q", rec.calls[0].Reason)
	}
	if got := rhGet(t, client, "w1").Annotations[annotationRoll]; got != orig {
		t.Fatalf("canary marker changed: %q", got)
	}
	if replicaPatches(client) != 0 || rhRollWrites(client, "uzi-hw-w1") != 0 {
		t.Fatalf("unexpected writes: %+v", rhPatches(client))
	}
	out := m.PacingOutcomes()
	p := out["w2"]
	if p == nil || p.BlockingReason == nil || !strings.HasPrefix(*p.BlockingReason, "completed cohort cleanup pending") {
		t.Fatalf("peer pacing = %+v", p)
	}
	for id, o := range out {
		if o.BlockingReason != nil && strings.Contains(*o.BlockingReason, "participant reservation without canary") {
			t.Fatalf("%s reads without canary: %s", id, *o.BlockingReason)
		}
		if o.CanaryID != nil && *o.CanaryID == "w1" {
			t.Fatalf("the hibernating worker is a pacing canary: %+v", o)
		}
	}
	if _, ok := out["w1"]; ok {
		t.Fatal("hibernating worker exported a pacing outcome")
	}
}

// 3: restart and ambiguous-write behaviour.
func TestHibernationHistoryRetirementAcrossRestartsAndAmbiguousWrites(t *testing.T) {
	t.Run("fresh materializer cleans the peer, canary sleeps on a later op", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		w2, peerObjs := rhPeer(t, "w2", false, nil)
		m, client, rec := hibEnv(t, rhObjs([]runtime.Object{rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
		currentProof(t, client, &w2, m5Now)
		// The environment's own materializer is discarded: a fresh one sees the cohort
		// only through the completed canary marker on the cluster.
		_ = m
		m1 := rhFresh(t, client, rec)
		if err := hibTick(t, m1, w, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertRefused(t, rec, refuseRollCohortPending)
		if _, ok := rhGet(t, client, "w2").Annotations[annotationRoll]; ok {
			t.Fatal("peer cleanup was not written")
		}
		if rhRollWrites(client, "uzi-hw-w1") != 0 {
			t.Fatal("the canary marker was written while a peer was pending")
		}
		rec.calls = nil
		w.Hibernation.OpID = "op-2"
		m2 := rhFresh(t, client, rec)
		if err := hibTick(t, m2, w, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
	})

	retire := func(p rhPatch) bool { return p.writesRoll() && p.annotations()[annotationRoll] == nil }
	setup := func(t *testing.T, land bool) (*fake.Clientset, *hibRec, protocol.DesiredWorker, *bool) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		_, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		armed := true
		client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			pa := a.(k8stesting.PatchAction)
			if !armed || !retire(rhPatch{name: pa.GetName(), body: rhDecode(pa.GetPatch())}) {
				return false, nil, nil
			}
			armed = false
			if land {
				old, err := client.Tracker().Get(a.GetResource(), a.GetNamespace(), pa.GetName())
				if err != nil {
					return true, nil, err
				}
				raw, _ := json.Marshal(old)
				merged, err := jsonpatch.MergePatch(raw, pa.GetPatch())
				if err != nil {
					return true, nil, err
				}
				next := &appsv1.Deployment{}
				if err := json.Unmarshal(merged, next); err != nil {
					return true, nil, err
				}
				next.ResourceVersion = "9000"
				if err := client.Tracker().Update(a.GetResource(), next, a.GetNamespace()); err != nil {
					return true, nil, err
				}
			}
			return true, nil, errors.New("timeout: outcome unknown")
		})
		return client, rec, w, &armed
	}

	for name, land := range map[string]bool{"retirement landed but its response was lost": true, "retirement did not land": false} {
		t.Run(name, func(t *testing.T) {
			client, rec, w, _ := setup(t, land)
			m := rhFresh(t, client, rec)
			if err := hibTick(t, m, w); err == nil {
				t.Fatal("an unconfirmed retirement was swallowed")
			}
			if len(rec.calls) != 0 {
				t.Fatalf("reported %v", rec.transitions())
			}
			ps := rhWorkerPatches(client, "uzi-hw-w1")
			if len(ps) != 1 || !retire(ps[0]) {
				t.Fatalf("patches = %+v, want only the retirement attempt (no fence, refusal or replicas write)", ps)
			}
			_, has := rhGet(t, client, "w1").Annotations[annotationRoll]
			if has == land {
				t.Fatalf("marker present = %v after land=%v", has, land)
			}
			m2 := rhFresh(t, client, rec)
			if err := hibTick(t, m2, w); err != nil {
				t.Fatal(err)
			}
			rhAssertSlept(t, client, rec)
			want := 1
			if !land {
				want = 2
			}
			if got := rhRollWrites(client, "uzi-hw-w1"); got != want {
				t.Fatalf("roll writes = %d, want %d", got, want)
			}
		})
	}

	t.Run("crash after a confirmed retirement, before the scale-down", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		_, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		fail := true
		client.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if fail && (rhPatch{body: rhDecode(a.(k8stesting.PatchAction).GetPatch())}).touchesReplicas() {
				fail = false
				return true, nil, errors.New("timeout: outcome unknown")
			}
			return false, nil, nil
		})
		m := rhFresh(t, client, rec)
		if err := hibTick(t, m, w); err == nil {
			t.Fatal("the failed scale-down was swallowed")
		}
		if _, ok := rhGet(t, client, "w1").Annotations[annotationRoll]; ok {
			t.Fatal("retirement did not land")
		}
		m2 := rhFresh(t, client, rec)
		if err := hibTick(t, m2, w); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
		if got := rhRollWrites(client, "uzi-hw-w1"); got != 1 {
			t.Fatalf("roll writes = %d, want exactly the one retirement", got)
		}
	})
}

func rhDecode(raw []byte) map[string]any {
	var b map[string]any
	_ = json.Unmarshal(raw, &b)
	return b
}

// 4: the worker's own roll evidence still refuses, and nothing about it is rewritten.
func TestHibernationOwnRollEvidenceStillRefuses(t *testing.T) {
	type tc struct {
		objs   func(t *testing.T) []runtime.Object
		setup  func(*fake.Clientset)
		reason string
	}
	completed := func(t *testing.T) string { return rhCanaryDep(t, true).Annotations[annotationRoll] }
	different := func(t *testing.T) string {
		d := rhCanaryDep(t, false)
		rhEdit(t, d, func(r *rollMarker) { r.Hash = "different" })
		return d.Annotations[annotationRoll]
	}
	pod := func() runtime.Object { return hibPod("pod-uid-1", "x", true, false) }
	cases := map[string]tc{
		"open incomplete marker": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{rhCanaryDep(t, false), pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pod()}
		}},
		"malformed annotation": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			d := rhCanaryDep(t, true)
			d.Annotations[annotationRoll] = "{not json"
			return []runtime.Object{d, pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pod()}
		}},
		"garbage claim mirror": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{hibCurrent(t, 1, ""), pvcNix("w1", "20Gi"), rhClaim("w1", "data", "{not json"), pod()}
		}},
		"decodable claim mirror": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{hibCurrent(t, 1, ""), pvcNix("w1", "20Gi"), rhClaim("w1", "data", completed(t)), pod()}
		}},
		"completed marker and identical claim mirror": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{rhCanaryDep(t, true), pvcNix("w1", "20Gi"), rhClaim("w1", "data", completed(t)), pod()}
		}},
		"completed marker and different incomplete claim mirror": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{rhCanaryDep(t, true), pvcNix("w1", "20Gi"), rhClaim("w1", "data", different(t)), pod()}
		}},
		"raw annotation on the nix claim": {reason: refuseRollReservation, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{rhCanaryDep(t, true), rhClaim("w1", "nix", "x"), pvcFor("w1", "data"), pod()}
		}},
		"blocked snapshot with a completed marker": {reason: refuseRollBlocked, objs: func(t *testing.T) []runtime.Object {
			return []runtime.Object{rhCanaryDep(t, true), pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pod()}
		}, setup: func(c *fake.Clientset) {
			c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("replicasets unavailable")
			})
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := &hibRec{fakeCordoner: &fakeCordoner{}}
			m, client := newMatWithCordoner(t, rec, c.objs(t)...)
			if c.setup != nil {
				c.setup(client)
			}
			before := rhGet(t, client, "w1").Annotations[annotationRoll]
			claimBefore := ""
			if pvc, err := client.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{}); err == nil {
				claimBefore = pvc.Annotations[annotationRoll]
			}
			if err := hibTick(t, m, rhW(protocol.HibernationPreparing, "op-1")); err != nil {
				t.Fatal(err)
			}
			rhAssertRefused(t, rec, c.reason)
			if replicaPatches(client) != 0 {
				t.Fatalf("scale-down issued: %+v", rhPatches(client))
			}
			// A decodable mirror may be restored onto the Deployment by the snapshot; every
			// other case leaves both annotations exactly as found.
			if name != "decodable claim mirror" && name != "completed marker and identical claim mirror" {
				if got := rhGet(t, client, "w1").Annotations[annotationRoll]; got != before {
					t.Fatalf("deployment annotation changed: %q -> %q", before, got)
				}
				if rhRollWrites(client, "uzi-hw-w1") != 0 {
					t.Fatal("a roll annotation write was issued")
				}
			}
			// The snapshot's own cleanup of an identical mirror is the one claim write allowed.
			if pvc, err := client.CoreV1().PersistentVolumeClaims(testConfig().Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{}); err == nil && pvc.Annotations[annotationRoll] != claimBefore && name != "completed marker and identical claim mirror" {
				t.Fatalf("claim annotation changed: %q -> %q", claimBefore, pvc.Annotations[annotationRoll])
			}
		})
	}
}

// 5: any in-scope peer evidence refuses; out-of-scope peers do not.
func TestHibernationPeerEvidenceRefusesCohortPending(t *testing.T) {
	type tc struct {
		build func(t *testing.T) (protocol.DesiredWorker, []runtime.Object)
	}
	completedCanary := func(r *rollMarker) { r.Canary, r.Completed = true, true }
	cases := map[string]tc{
		"incomplete marker in the same key": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			return rhPeer(t, "w2", false, nil)
		}},
		"completed canary marker in another key": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w, objs := rhPeer(t, "w2", true, func(r *rollMarker) { completedCanary(r); r.Coverage.Scheduling = []string{"other"} })
			w.RollEvidence.SchedulingCapabilities = []string{"other"}
			return w, objs
		}},
		"decoded marker on a blocked peer": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := evidence(desired("w2"), m5Now)
			raw := markedDeployment(t, testConfig(), w, true).Annotations[annotationRoll]
			claim := rhClaim("w2", "data", raw)
			now := metav1.Now()
			claim.DeletionTimestamp = &now
			claim.Finalizers = []string{"test/hold"}
			return w, []runtime.Object{pvcNix("w2", "20Gi"), claim}
		}},
		"decoded marker on an unknown-owner peer": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := desired("w2")
			d := markedDeployment(t, testConfig(), evidence(w, m5Now), false)
			return w, []runtime.Object{d, pvcNix("w2", "20Gi"), pvcFor("w2", "data")}
		}},
		"owner-mismatched peer": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w, objs := rhPeer(t, "w2", false, nil)
			w.RollEvidence.OwnerID = "owner-X"
			return w, objs
		}},
		"completed marker and a different incomplete claim mirror on a peer": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := evidence(desired("w2"), m5Now)
			d := markedDeployment(t, testConfig(), w, true)
			other := markedDeployment(t, testConfig(), w, false)
			rhEdit(t, other, func(r *rollMarker) { r.Hash = "different" })
			return w, []runtime.Object{d, pvcNix("w2", "20Gi"), rhClaim("w2", "data", other.Annotations[annotationRoll])}
		}},
		"raw annotation only": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := evidence(desired("w2"), m5Now)
			return w, []runtime.Object{rhPlainDep(t, w), rhClaim("w2", "nix", "x"), pvcFor("w2", "data")}
		}},
		"claim annotation only": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := evidence(desired("w2"), m5Now)
			inc := markedDeployment(t, testConfig(), w, false).Annotations[annotationRoll]
			return w, []runtime.Object{rhPlainDep(t, w), pvcNix("w2", "20Gi"), rhClaim("w2", "data", inc)}
		}},
		"blocked peer with no marker": {func(t *testing.T) (protocol.DesiredWorker, []runtime.Object) {
			w := evidence(desired("w2"), m5Now)
			d := rhPlainDep(t, w)
			d.Annotations = map[string]string{annotationRoll: "{not json"}
			return w, []runtime.Object{d, pvcNix("w2", "20Gi"), pvcFor("w2", "data")}
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := rhW(protocol.HibernationPreparing, "op-1")
			w2, peerObjs := c.build(t)
			canary := rhCanaryDep(t, true)
			orig := canary.Annotations[annotationRoll]
			m, client, rec := hibEnv(t, rhObjs([]runtime.Object{canary, hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
			if err := hibTick(t, m, w, w2); err != nil {
				t.Fatal(err)
			}
			rhAssertRefused(t, rec, refuseRollCohortPending)
			if got := rhGet(t, client, "w1").Annotations[annotationRoll]; got != orig {
				t.Fatalf("canary marker changed: %q", got)
			}
			if replicaPatches(client) != 0 || rhRollWrites(client, "uzi-hw-w1") != 0 {
				t.Fatalf("unexpected writes on the candidate: %+v", rhWorkerPatches(client, "uzi-hw-w1"))
			}
			// Once the peer evidence clears (the peer leaves the desired set), the canary
			// retires and sleeps on a later op.
			rec.calls = nil
			w.Hibernation.OpID = "op-2"
			if err := hibTick(t, rhFresh(t, client, rec), w); err != nil {
				t.Fatal(err)
			}
			rhAssertSlept(t, client, rec)
		})
	}

	t.Run("the peer's marker clears while it stays desired", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		w2, peerObjs := rhPeer(t, "w2", false, nil)
		m, client, rec := hibEnv(t, rhObjs([]runtime.Object{rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
		if err := hibTick(t, m, w, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertRefused(t, rec, refuseRollCohortPending)
		peer := rhGet(t, client, "w2")
		delete(peer.Annotations, annotationRoll)
		peer.ResourceVersion = "777"
		if err := client.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("deployments"), peer, testConfig().Namespace); err != nil {
			t.Fatal(err)
		}
		rec.calls = nil
		w.Hibernation.OpID = "op-2"
		if err := hibTick(t, rhFresh(t, client, rec), w, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
	})

	t.Run("control: a peer validated for another owner with its own completed canary does not block", func(t *testing.T) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		w2, peerObjs := rhPeer(t, "w2", true, func(r *rollMarker) { r.Completed = true; r.Coverage.Owner = "owner-B" })
		w2.RollEvidence.OwnerID = "owner-B"
		m, client, rec := hibEnv(t, rhObjs([]runtime.Object{rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
		if err := hibTick(t, m, w, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
		if _, ok := rhGet(t, client, "w2").Annotations[annotationRoll]; !ok {
			t.Fatal("the other owner's marker was touched")
		}
	})
}

// 6: a hibernated worker supplies nothing to pacing, coverage or slots.
func TestHibernatedWorkerWithCompletedCanaryMarkerSuppliesNothing(t *testing.T) {
	t.Run("hibernated holder", func(t *testing.T) {
		w1 := rhW(protocol.HibernationHibernated, "op-1")
		hd := rhCanaryDep(t, true)
		zero := int32(0)
		hd.Spec.Replicas = &zero
		hd.Annotations[annotationHibernationOp] = "op-1"
		sibling := deployedWorker("w2", 0, "the-old-releases-hash")
		w2 := evidence(protocol.DesiredWorker{ID: "w2", Template: "base", Size: "m"}, m5Now)
		rec := &hibRec{fakeCordoner: &fakeCordoner{}}
		m, client := newMatWithCordoner(t, rec, hd, sibling, pvcNix("w1", "20Gi"), pvcFor("w1", "data"), pvcNix("w2", "20Gi"), pvcFor("w2", "data"))
		if err := hibTick(t, m, w1, w2); err != nil {
			t.Fatal(err)
		}
		for _, p := range rhPatches(client) {
			if p.name == "uzi-hw-w1" {
				t.Fatalf("the hibernated worker was written: %+v", p.body)
			}
		}
		for _, p := range rhWorkerPatches(client, "uzi-hw-w1") {
			if _, ok := p.body["spec"].(map[string]any)["template"]; ok {
				t.Fatalf("hibernated worker got a template patch: %+v", p.body)
			}
		}
		out := m.PacingOutcomes()
		if _, ok := out["w1"]; ok {
			t.Fatal("hibernated worker exported pacing")
		}
		for _, o := range out {
			if o.CanaryID != nil && *o.CanaryID == "w1" {
				t.Fatalf("hibernated worker is a canary: %+v", o)
			}
		}
		patched := false
		for _, id := range templatePatches(client) {
			patched = patched || id == "uzi-hw-w2"
			if id == "uzi-hw-w1" {
				t.Fatal("hibernated worker rolled")
			}
		}
		if !patched {
			t.Fatal("the awake drifted worker did not roll past the hibernated marker holder")
		}
	})

	t.Run("anchored candidate", func(t *testing.T) {
		w1 := rhW(protocol.HibernationPreparing, "op-1")
		w2, peerObjs := rhPeer(t, "w2", false, nil)
		m, client, rec := hibEnv(t, rhObjs([]runtime.Object{rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
		if err := hibTick(t, m, w1, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertRefused(t, rec, refuseRollCohortPending)
		for _, id := range templatePatches(client) {
			t.Fatalf("template patch on %s", id)
		}
		for id, o := range m.PacingOutcomes() {
			if o.CanaryID != nil && *o.CanaryID == "w1" {
				t.Fatalf("%s: anchored candidate is the canary: %+v", id, o)
			}
		}
	})
}

// 7a and 9: after a retirement sleep, a target change and both wake arms carry no history.
func TestHibernationSleepTargetChangeWakeLeavesNoRollHistory(t *testing.T) {
	sleep := func(t *testing.T) (*fake.Clientset, *hibRec, *Materializer) {
		w := rhW(protocol.HibernationPreparing, "op-1")
		m, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		if err := hibTick(t, m, w); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
		return client, rec, m
	}
	assertClean := func(t *testing.T, c *fake.Clientset) {
		t.Helper()
		if d, err := c.AppsV1().Deployments(testConfig().Namespace).Get(context.Background(), "uzi-hw-w1", metav1.GetOptions{}); err == nil {
			if _, ok := d.Annotations[annotationRoll]; ok {
				t.Fatalf("deployment carries roll history: %q", d.Annotations[annotationRoll])
			}
		}
		pvcs, err := c.CoreV1().PersistentVolumeClaims(testConfig().Namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pvcs.Items {
			if _, ok := p.Annotations[annotationRoll]; ok {
				t.Fatalf("claim %s carries roll history", p.Name)
			}
		}
	}

	t.Run("existing Deployment wake", func(t *testing.T) {
		client, rec, m := sleep(t)
		assertClean(t, client)
		// The target changes while asleep.
		m.cfg.RollTargetVersion = "2.0.0+g123"
		rec.calls = nil
		if err := hibTick(t, rhFresh(t, client, rec), rhW(protocol.HibernationWaking, "wake-op")); err != nil {
			t.Fatal(err)
		}
		assertClean(t, client)
		for _, p := range rhWorkerPatches(client, "uzi-hw-w1") {
			if p.writesRoll() && p.annotations()[annotationRoll] != nil {
				t.Fatalf("wake wrote roll history: %+v", p.body)
			}
		}
	})

	t.Run("missing Deployment wake", func(t *testing.T) {
		client, rec, _ := sleep(t)
		if err := client.AppsV1().Deployments(testConfig().Namespace).Delete(context.Background(), "uzi-hw-w1", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := client.CoreV1().Pods(testConfig().Namespace).Delete(context.Background(), "pod-pod-uid-1", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		rec.calls = nil
		if err := hibTick(t, rhFresh(t, client, rec), rhW(protocol.HibernationWaking, "wake-op")); err != nil {
			t.Fatal(err)
		}
		d := rhGet(t, client, "w1")
		if d.Annotations[annotationHibernationOp] != "wake-op" {
			t.Fatalf("the wake did not create the Deployment: %+v", d.Annotations)
		}
		assertClean(t, client)
	})

	// An unfinished old cohort beside the old canary: the peer never reads "without
	// canary", the cohort is not closed early, and once the peer proves its incarnation
	// the history retires on drift and a new canary is admitted.
	t.Run("unfinished old cohort across a target change", func(t *testing.T) {
		w1 := rhW(protocol.HibernationPreparing, "op-1")
		w2, peerObjs := rhPeer(t, "w2", false, nil)
		m, client, rec := hibEnv(t, rhObjs([]runtime.Object{rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false)}, peerObjs)...)
		if err := hibTick(t, m, w1, w2); err != nil {
			t.Fatal(err)
		}
		rhAssertRefused(t, rec, refuseRollCohortPending)
		if _, ok := rhGet(t, client, "w1").Annotations[annotationRoll]; !ok {
			t.Fatal("old canary marker lost")
		}
		// The api withdraws the preparation; the old canary serves on. The target moves.
		w1.Hibernation = nil
		m.cfg.RollTargetVersion = "2.0.0+g123"
		var err error
		if m.resolver, err = preset.NewResolver("harbor.example.com/uzi", "v2.0.0"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			rollTickTest(t, m, []protocol.DesiredWorker{w1, w2})
			for id, o := range m.PacingOutcomes() {
				if o.BlockingReason != nil && strings.Contains(*o.BlockingReason, "without canary") {
					t.Fatalf("%s reads without canary: %s", id, *o.BlockingReason)
				}
			}
			if len(templatePatches(client)) != 0 {
				t.Fatalf("the unfinished cohort was closed or a new roll admitted early: %v", templatePatches(client))
			}
		}
		if _, ok := rhGet(t, client, "w2").Annotations[annotationRoll]; !ok {
			t.Fatal("the unproven peer's reservation was cleaned without proof")
		}
		currentProof(t, client, &w2, m5Now)
		for i := 0; i < 4; i++ {
			rollTickTest(t, m, []protocol.DesiredWorker{w1, w2})
		}
		if _, ok := rhGet(t, client, "w2").Annotations[annotationRoll]; ok {
			t.Fatal("peer cleanup did not complete after proof")
		}
		if raw, ok := rhGet(t, client, "w1").Annotations[annotationRoll]; ok {
			var r rollMarker
			_ = json.Unmarshal([]byte(raw), &r)
			if r.Completed {
				t.Fatalf("old history was not retired on drift: %+v", r)
			}
		}
		if len(templatePatches(client)) == 0 {
			t.Fatal("no new canary was admitted after the old cohort finished")
		}
	})
}

// 7c: a preparation withdrawn into waking leaves no retired history behind.
func TestHibernationWithdrawnPreparationWakesWithoutRetiredHistory(t *testing.T) {
	for _, arm := range []string{"existing Deployment", "missing Deployment"} {
		t.Run(arm, func(t *testing.T) {
			w := rhW(protocol.HibernationPreparing, "op-1")
			w.Busy = true
			m, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
			if err := hibTick(t, m, w); err != nil {
				t.Fatal(err)
			}
			rhAssertRefused(t, rec, refuseBusy)
			if arm == "missing Deployment" {
				if err := client.AppsV1().Deployments(testConfig().Namespace).Delete(context.Background(), "uzi-hw-w1", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := client.CoreV1().Pods(testConfig().Namespace).Delete(context.Background(), "pod-pod-uid-1", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			rec.calls = nil
			if err := hibTick(t, rhFresh(t, client, rec), rhW(protocol.HibernationWaking, "wake-op")); err != nil {
				t.Fatal(err)
			}
			if _, ok := rhGet(t, client, "w1").Annotations[annotationRoll]; ok {
				t.Fatal("the woken worker carries retired history")
			}
		})
	}
}

// 8: the controller sees no reason text, so the same inputs behave the same way run
// after run, whatever op ids the api assigns.
func TestHibernationHistoryRetirementIsDeterministicAcrossRuns(t *testing.T) {
	var runs [][]string
	for _, op := range []string{"op-a", "op-b"} {
		w := rhW(protocol.HibernationPreparing, op)
		m, client, rec := hibEnv(t, rhCanaryDep(t, true), hibPod("pod-uid-1", "x", true, false))
		if err := hibTick(t, m, w); err != nil {
			t.Fatal(err)
		}
		rhAssertSlept(t, client, rec)
		runs = append(runs, append(rec.transitions(), strconv.Itoa(len(rhPatches(client)))))
	}
	if strings.Join(runs[0], ",") != strings.Join(runs[1], ",") {
		t.Fatalf("runs differ: %v vs %v", runs[0], runs[1])
	}
}
