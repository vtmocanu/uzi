package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Characterization for #4475: the mapping-correction retarget in planPool (marker Hash
// and Image still match the target, only TargetVersion differs) rewrites the entry's
// own marker in place: TargetVersion moves to the new version, Completed is reset to
// false, and Canary is copied from that same marker. The branch never reads another
// worker's marker, so it cannot hand the bit to a second worker. These cases drive
// Observe/Reconcile (ForceRoll off) and observe that no tick leaves a canary=true
// reservation on anything but w1's Deployment or claim mirrors, i.e. no second holder.
// That is an observation of these two scenarios, not a proof for every cohort shape.
// The first case also advances the roll to canary completion at the new version. The
// second cannot: w1's Deployment is gone and nothing in the fake cluster recreates it,
// so no current-incarnation proof is possible; its claim is limited to the period while
// the canary is unproven.
//
// Non-vacuity: the cases pin the in-place rewrite itself. In the first case, with the
// retarget arm disabled, the pin-cancellation arm cancels and re-admits w1 (its canary
// bit and Nonce change) and cancels w2/w3, so the w1 and participant assertions fail.
// In the second case, with the retarget arm fully disabled, w2/w3 are pin-cancelled and
// the pool is held (no reservation proof), so the tick-1 w1 pacing-state check fails
// (the w2/w3 in-place checks would too). With only its dep-less path disabled, w1 takes
// the retained-pin replacement arm, which writes an identical mirror marker but holds
// the pool, so the tick-1 pacing assertion (w1 not held, no pin-changed reason) fails.

// canaryHolders lists every Deployment and data-PVC mirror whose decoded roll marker
// has Canary=true, as "kind/name".
func (o *orphanCohort) canaryHolders(t *testing.T) []string {
	t.Helper()
	ns := o.m.cfg.Namespace
	var out []string
	deps, err := o.c.AppsV1().Deployments(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps.Items {
		if mk, err := decodeRoll(d.Annotations[annotationRoll]); err == nil && mk != nil && mk.Canary {
			out = append(out, "deployment/"+d.Name)
		}
	}
	pvcs, err := o.c.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pvcs.Items {
		if mk, err := decodeRoll(p.Annotations[annotationRoll]); err == nil && mk != nil && mk.Canary {
			out = append(out, "pvc/"+p.Name)
		}
	}
	return out
}

func (o *orphanCohort) assertSingleCanary(t *testing.T, when string) {
	t.Helper()
	holders := o.canaryHolders(t)
	for _, h := range holders {
		switch h {
		case "deployment/" + deploymentName("w1"), "pvc/" + dataPVCName("w1"), "pvc/" + nixPVCName("w1"):
		default:
			t.Fatalf("%s: non-w1 canary holder %q (all: %v)", when, h, holders)
		}
	}
}

// assertParticipantRetargeted requires id to hold a non-canary participant reservation
// at version. Without the retarget arm the participant reservations are cancelled.
func (o *orphanCohort) assertParticipantRetargeted(t *testing.T, when, id, version string) {
	t.Helper()
	mk := o.marker(t, id)
	if mk == nil || mk.Canary || mk.TargetVersion != version {
		t.Fatalf("%s: %s was not retargeted in place to %s as a non-canary participant: %+v", when, id, version, mk)
	}
}

func TestRetargetedNonCurrentCanaryNeverYieldsSecondCanary(t *testing.T) {
	const newVersion = "2.0.0"

	t.Run("drained podless incomplete canary with stale template hash", func(t *testing.T) {
		o := newOrphanCohort(t, 3, 2)
		zero := int32(0)
		o.edit(t, "w1", func(d *appsv1.Deployment) {
			d.Spec.Replicas = &zero
			d.Spec.Template.Annotations[AnnotationSpecHash] = "old-template-hash"
		})
		if err := o.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), o.m.cfg.Namespace, "pod-w1"); err != nil {
			t.Fatal(err)
		}
		before := o.marker(t, "w1")
		if before == nil || !before.Canary || before.Completed || before.TargetVersion == newVersion {
			t.Fatalf("fixture marker unusable: %+v", before)
		}
		origHash := before.Hash

		o.m.cfg.RollTargetVersion = newVersion
		_ = o.tick(t) // holds and unconfirmed writes are legitimate here
		o.assertSingleCanary(t, "tick 1")
		mk := o.marker(t, "w1")
		if mk == nil || !mk.Canary || mk.TargetVersion != newVersion {
			t.Fatalf("tick 1: w1 not retargeted in place keeping its canary bit: %+v", mk)
		}
		if mk.Nonce != before.Nonce {
			t.Fatalf("tick 1: w1 nonce %q -> %q; the marker was cancelled and re-admitted, not rewritten in place", before.Nonce, mk.Nonce)
		}
		o.assertParticipantRetargeted(t, "tick 1", "w2", newVersion)
		o.assertParticipantRetargeted(t, "tick 1", "w3", newVersion)
		for i := 2; i <= 3; i++ {
			_ = o.tick(t)
			o.assertSingleCanary(t, fmt.Sprintf("tick %d", i))
		}

		// Advance: w1 is brought back (scaled up, original template, a current
		// incarnation proving the new version) and so are the participants; the roll
		// must progress without ever producing a second canary.
		one := int32(1)
		o.edit(t, "w1", func(d *appsv1.Deployment) {
			d.Spec.Replicas = &one
			d.Spec.Template.Annotations[AnnotationSpecHash] = origHash
		})
		if err := o.c.Tracker().Delete(appsv1.SchemeGroupVersion.WithResource("replicasets"), o.m.cfg.Namespace, "rs-w1"); err != nil {
			t.Fatal(err)
		}
		currentProof(t, o.c, o.at("w1"), m5Now)
		for _, id := range []string{"w2", "w3"} {
			o.prove(t, id)
		}
		for _, id := range []string{"w1", "w2", "w3"} {
			v := newVersion
			o.at(id).RollEvidence.RegisteredVersion = &v
		}
		for i := 4; i <= 6; i++ {
			o.mustTick(t)
			o.assertSingleCanary(t, fmt.Sprintf("tick %d", i))
		}
		// The retargeted canary completed at the new version and the cohort closed:
		// the participants' reservations are cleaned up, w1 stays the only canary.
		if mk := o.marker(t, "w1"); mk == nil || !mk.Canary || !mk.Completed || mk.TargetVersion != newVersion {
			t.Fatalf("w1 did not complete as the canary at %s: %+v", newVersion, mk)
		}
		for _, id := range []string{"w2", "w3"} {
			if mk := o.marker(t, id); mk != nil {
				t.Fatalf("%s reservation not cleaned up after the canary completed: %+v", id, mk)
			}
		}
	})

	t.Run("completed canary history retained only on the claim mirror", func(t *testing.T) {
		o := newOrphanCohort(t, 3, 2)
		// State built: w1's Deployment is deleted (dep == nil); its data PVC carries the
		// canary marker with Completed=true; w2/w3 keep their incomplete participant
		// reservations on their Deployments. The version mapping then changes.
		mk := o.marker(t, "w1")
		mk.Completed = true
		raw, err := json.Marshal(mk)
		if err != nil {
			t.Fatal(err)
		}
		o.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
			p.Annotations = map[string]string{annotationRoll: string(raw)}
		})
		if err := o.c.Tracker().Delete(appsv1.SchemeGroupVersion.WithResource("deployments"), o.m.cfg.Namespace, deploymentName("w1")); err != nil {
			t.Fatal(err)
		}
		o.assertSingleCanary(t, "fixture")

		o.m.cfg.RollTargetVersion = newVersion
		_ = o.tick(t)
		o.assertSingleCanary(t, "tick 1")
		p, err := o.c.CoreV1().PersistentVolumeClaims(o.m.cfg.Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		m2, err := decodeRoll(p.Annotations[annotationRoll])
		if err != nil || m2 == nil {
			t.Fatalf("tick 1: w1 mirror marker unreadable: %v %+v", err, m2)
		}
		// The retarget reopens the completed history as an incomplete canary at the new
		// version, keeping its nonce.
		if m2.TargetVersion != newVersion || !m2.Canary || m2.Completed || m2.Nonce != mk.Nonce {
			t.Fatalf("tick 1: mirror not retargeted in place as an incomplete canary: %+v", m2)
		}
		// The retained-pin replacement arm writes an identical mirror marker but marks the
		// pin changed and holds the pool; the retarget arm does not.
		for _, id := range []string{"w1", "w2", "w3"} {
			if _, _, reason, _ := o.pacing(id); reason == "roll target pin changed; pool held until reservations settle" {
				t.Fatalf("tick 1: %s held by the retained-pin replacement arm, not the mapping retarget", id)
			}
		}
		if state, _, reason, _ := o.pacing("w1"); state == "held" {
			t.Fatalf("tick 1: w1 held (%q); the mapping retarget does not hold the pool", reason)
		}
		o.assertParticipantRetargeted(t, "tick 1", "w2", newVersion)
		o.assertParticipantRetargeted(t, "tick 1", "w3", newVersion)
		for i := 2; i <= 4; i++ {
			_ = o.tick(t)
			o.assertSingleCanary(t, fmt.Sprintf("tick %d", i))
		}
	})
}

// Absent duplicate canary (#4475). w1 is the current promoted canary; w2 is a recycled
// participant (scaled to zero, no pod, no claims) whose reservation was rewritten as a
// canary against a stale template.

const (
	dupHeld      = "duplicate canary reservations unresolved; explicit recovery required"
	dupCancelled = "duplicate canary cancelled for absent worker; re-observation required"
)

// staleDuplicate turns w2 of a two-worker cohort into the absent stale duplicate.
var (
	olderPinHash = strings.Repeat("c", 64)
	otherHash    = strings.Repeat("d", 64)
)

// recycleCurrent empties a worker whose pod and replicaset are the current-proof ones.
func (o *orphanCohort) recycleCurrent(t *testing.T, id string) {
	t.Helper()
	zero := int32(0)
	o.edit(t, id, func(d *appsv1.Deployment) { d.Spec.Replicas = &zero })
	if err := o.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), o.m.cfg.Namespace, "pod-"+id); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{nixPVCName(id), dataPVCName(id)} {
		if err := o.c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), o.m.cfg.Namespace, name); err != nil {
			t.Fatal(err)
		}
	}
}

func staleDuplicate(t *testing.T, o *orphanCohort, older bool) {
	t.Helper()
	o.recycle(t, "w2")
	zero := int32(0)
	o.setSetReplicas(t, "old-rs-"+deploymentName("w2"), &zero)
	mk := o.marker(t, "w2")
	mk.Canary = true
	if older {
		mk.Hash, mk.TargetVersion = olderPinHash, "0.9.0"
	}
	raw, err := json.Marshal(mk)
	if err != nil {
		t.Fatal(err)
	}
	o.edit(t, "w2", func(d *appsv1.Deployment) {
		d.Annotations[annotationRoll] = string(raw)
		d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-template-hash"
	})
}

// w2Cancellations lists patches that cleared w2's reservation annotation.
func w2Cancellations(c interface{ Actions() []k8stesting.Action }) int {
	n := 0
	for _, a := range c.Actions() {
		p, ok := a.(k8stesting.PatchAction)
		if !ok || a.GetResource().Resource != "deployments" || p.GetName() != deploymentName("w2") {
			continue
		}
		var body struct {
			Metadata struct{ Annotations map[string]*string }
		}
		if json.Unmarshal(p.GetPatch(), &body) != nil {
			continue
		}
		if v, present := body.Metadata.Annotations[annotationRoll]; present && v == nil {
			n++
		}
	}
	return n
}

func TestDuplicateCanaryCancelsProvablyAbsentReservation(t *testing.T) {
	for _, older := range []bool{false, true} {
		name := "same pin"
		if older {
			name = "older pin"
		}
		t.Run(name, func(t *testing.T) {
			o := newOrphanCohort(t, 2, 2)
			staleDuplicate(t, o, older)
			if got := o.canaries(t); !equalStrings(got, []string{deploymentName("w1"), deploymentName("w2")}) {
				t.Fatalf("fixture canaries = %v", got)
			}
			w1Before, w1Spec := o.marker(t, "w1"), o.dep(t, "w1").Spec.DeepCopy()
			o.c.ClearActions()

			o.mustTick(t)
			if got, want := actionTrace(o.c), []string{"patch deployments " + deploymentName("w2") + " metadata"}; !equalStrings(got, want) {
				t.Fatalf("tick 1 wrote %v, want exactly %v", got, want)
			}
			if got := o.dep(t, "w2").Annotations[annotationRoll]; got != "" {
				t.Fatalf("w2 reservation survived: %q", got)
			}
			if got := o.marker(t, "w1"); !reflect.DeepEqual(got, w1Before) {
				t.Fatalf("survivor marker changed: %+v -> %+v", w1Before, got)
			}
			if got := o.dep(t, "w1").Spec; !reflect.DeepEqual(got, *w1Spec) {
				t.Fatal("survivor Deployment spec changed")
			}
			for _, id := range []string{"w1", "w2"} {
				state, blocker, reason, _ := o.pacing(id)
				if state != "held" || blocker != "w2" || reason != dupCancelled {
					t.Fatalf("%s pacing = %s/%s/%q", id, state, blocker, reason)
				}
			}

			// The re-observed pool resumes ordinary admission of the recycled worker.
			var wrote []string
			for i := 0; i < 4 && len(wrote) == 0; i++ {
				o.c.ClearActions()
				o.mustTick(t)
				wrote = actionTrace(o.c)
			}
			found := false
			for _, line := range wrote {
				found = found || line == "create persistentvolumeclaims "+dataPVCName("w2")
			}
			if !found {
				t.Fatalf("admission did not resume; wrote %v", wrote)
			}
			if got := o.canaries(t); !equalStrings(got, []string{deploymentName("w1")}) {
				t.Fatalf("canaries after settlement = %v", got)
			}
		})
	}
}

func (o *orphanCohort) addPod(t *testing.T, p *corev1.Pod) {
	t.Helper()
	p.Namespace = o.m.cfg.Namespace
	if p.UID == "" {
		p.UID = types.UID("uid-" + p.Name)
	}
	if err := o.c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
}

func (o *orphanCohort) addClaim(t *testing.T, p *corev1.PersistentVolumeClaim) {
	t.Helper()
	p.Namespace = o.m.cfg.Namespace
	if err := o.c.Tracker().Create(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), p, p.Namespace); err != nil {
		t.Fatal(err)
	}
}

func (o *orphanCohort) addSet(t *testing.T, rs *appsv1.ReplicaSet) {
	t.Helper()
	rs.Namespace = o.m.cfg.Namespace
	if rs.UID == "" {
		rs.UID = types.UID("uid-" + rs.Name)
	}
	if err := o.c.Tracker().Create(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
		t.Fatal(err)
	}
}

func (o *orphanCohort) setSetReplicas(t *testing.T, name string, replicas *int32) {
	t.Helper()
	rs, err := o.c.AppsV1().ReplicaSets(o.m.cfg.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rs.Spec.Replicas = replicas
	if err := o.c.Tracker().Update(appsv1.SchemeGroupVersion.WithResource("replicasets"), rs, rs.Namespace); err != nil {
		t.Fatal(err)
	}
}

// ownedBy is a controller owner reference to kind/name/uid.
func ownedBy(kind, name string, uid types.UID) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: &yes}}
}

// setMarker rewrites id's reservation through f.
func (o *orphanCohort) setMarker(t *testing.T, id string, f func(*rollMarker)) {
	t.Helper()
	mk := o.marker(t, id)
	f(mk)
	raw, err := json.Marshal(mk)
	if err != nil {
		t.Fatal(err)
	}
	o.edit(t, id, func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = string(raw) })
}

func TestDuplicateCanaryAbsenceProofFailClosed(t *testing.T) {
	var w2Target string // w2's rendered target hash, captured before the fixture is reshaped
	w2uid := types.UID("dep-" + deploymentName("w2"))
	now := metav1.NewTime(m5Now)
	one := int32(1)
	zero := int32(0)
	evidenceOf := func(o *orphanCohort, id string) *protocol.RollEvidence { return o.at(id).RollEvidence }
	cases := []struct {
		name      string
		n         int
		listFails string
		// reasons overrides the expected w1/w2 blocking reason (default: the duplicate hold).
		// A row that sets it never reaches the duplicate block: an earlier arm holds or
		// splits the pool, and the row only proves that nothing cancels the duplicate.
		reasons [2]string
		wantErr bool
		mutate  func(t *testing.T, o *orphanCohort)
	}{
		{name: "D replicas 1", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Replicas = &one })
		}},
		{name: "owned ReplicaSet still requests a replica without pods", mutate: func(t *testing.T, o *orphanCohort) {
			o.setSetReplicas(t, "old-rs-"+deploymentName("w2"), &one)
		}},
		{name: "owned ReplicaSet replicas are unknown", mutate: func(t *testing.T, o *orphanCohort) {
			o.setSetReplicas(t, "old-rs-"+deploymentName("w2"), nil)
		}},
		{name: "terminating noncanonical owned ReplicaSet still requests a replica", mutate: func(t *testing.T, o *orphanCohort) {
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "late-producer", DeletionTimestamp: &now,
				OwnerReferences: ownedBy("Deployment", deploymentName("w2"), w2uid)}, Spec: appsv1.ReplicaSetSpec{Replicas: &one}})
		}},
		{name: "labelled pod remains", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stray-w2", Labels: objectLabels("w2")}})
		}},
		{name: "pod with missing labels matched by name prefix", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: deploymentName("w2") + "-abc"}})
		}},
		{name: "pod with conflicting worker label matched by replicaset ancestry", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-1", Labels: map[string]string{LabelWorkerID: "other"},
				OwnerReferences: ownedBy("ReplicaSet", "old-rs-"+deploymentName("w2"), types.UID("rs-"+deploymentName("w2")))}})
		}},
		{name: "pod labelled with the worker id only", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-0", Labels: map[string]string{LabelWorkerID: "w2"}}})
		}},
		{name: "pod owned by D's uid under another name", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-6", OwnerReferences: ownedBy("Deployment", "renamed", w2uid)}})
		}},
		{name: "pod owned by D's name without a uid", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-7", OwnerReferences: ownedBy("Deployment", deploymentName("w2"), "")}})
		}},
		{name: "terminating pod", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dying-w2", Labels: objectLabels("w2"), DeletionTimestamp: &now}})
		}},
		{name: "noncanonical pod under a terminating noncanonical replicaset of D", mutate: func(t *testing.T, o *orphanCohort) {
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "zzz", DeletionTimestamp: &now, OwnerReferences: ownedBy("Deployment", deploymentName("w2"), w2uid)}, Spec: appsv1.ReplicaSetSpec{Replicas: &zero}})
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-2", OwnerReferences: ownedBy("ReplicaSet", "zzz", "uid-zzz")}})
		}},
		{name: "noncanonical pod owned directly by D", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-3", OwnerReferences: ownedBy("Deployment", deploymentName("w2"), w2uid)}})
		}},
		{name: "pod owned by an unlisted replicaset with D's name prefix", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-4", OwnerReferences: ownedBy("ReplicaSet", deploymentName("w2")+"-gone", "gone")}})
		}},
		{name: "pod under a listed ownerless ReplicaSet with D prefix", mutate: func(t *testing.T, o *orphanCohort) {
			name := deploymentName("w2") + "-orphan"
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "orphan-set"}, Spec: appsv1.ReplicaSetSpec{Replicas: &zero}})
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-orphan", OwnerReferences: ownedBy("ReplicaSet", name, "orphan-set")}})
		}},
		{name: "pod linked only by a labelled zero-replica noncanonical ReplicaSet", mutate: func(t *testing.T, o *orphanCohort) {
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "labelled-orphan", UID: "labelled-set", Labels: map[string]string{LabelWorkerID: "w2"}}, Spec: appsv1.ReplicaSetSpec{Replicas: &zero}})
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled-child", OwnerReferences: ownedBy("ReplicaSet", "labelled-orphan", "labelled-set")}})
		}},
		{name: "pod under a listed foreign-owned ReplicaSet with D prefix", mutate: func(t *testing.T, o *orphanCohort) {
			name := deploymentName("w2") + "-foreign"
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, UID: "foreign-set",
				OwnerReferences: ownedBy("Deployment", "somewhere-else", "other-deployment")}, Spec: appsv1.ReplicaSetSpec{Replicas: &zero}})
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-foreign", OwnerReferences: ownedBy("ReplicaSet", name, "foreign-set")}})
		}},
		{name: "pod owned by an unlisted replicaset uid whose name another deployment's replicaset reuses", mutate: func(t *testing.T, o *orphanCohort) {
			reused := deploymentName("w2") + "-old"
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: reused, UID: "uid-reused", OwnerReferences: ownedBy("Deployment", "someone-else", "uid-someone-else")}})
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-8", OwnerReferences: ownedBy("ReplicaSet", reused, "uid-original")}})
		}},
		{name: "pod mounting a canonical claim", mutate: func(t *testing.T, o *orphanCohort) {
			o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "odd-5"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dataPVCName("w2")}}}}}})
		}},
		{name: "D template hash matches its reservation", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = olderPinHash })
			o.setMarker(t, "w2", func(m *rollMarker) { m.Hash = olderPinHash })
		}},
		{name: "D not drifted from the target", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w2", func(m *rollMarker) { m.Hash = olderPinHash })
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = w2Target })
		}},
		{name: "remaining labelled claim", mutate: func(t *testing.T, o *orphanCohort) {
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "leftover", Labels: map[string]string{LabelWorkerID: "w2"}}})
		}},
		{name: "canonical claim with missing labels", mutate: func(t *testing.T, o *orphanCohort) {
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: nixPVCName("w2")}})
		}},
		{name: "canonical claim with conflicting labels", mutate: func(t *testing.T, o *orphanCohort) {
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dindDataPVCName("w2"), Labels: map[string]string{LabelWorkerID: "zz"}}})
		}},
		{name: "terminating claim", mutate: func(t *testing.T, o *orphanCohort) {
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "dying", Labels: objectLabels("w2"), DeletionTimestamp: &now}})
		}},
		{name: "claim mirror", mutate: func(t *testing.T, o *orphanCohort) {
			raw := o.dep(t, "w2").Annotations[annotationRoll]
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataPVCName("w2"), Labels: objectLabels("w2"), Annotations: map[string]string{annotationRoll: raw}}})
		}},
		{name: "custody held on D", mutate: func(t *testing.T, o *orphanCohort) { o.at("w2").CustodyHeld = true }},
		{name: "maintenance on D", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w2").DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "requested"}
		}},
		{name: "D deployment terminating", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.DeletionTimestamp, d.Finalizers = &now, []string{"hold"} })
		}},
		{name: "D busy", mutate: func(t *testing.T, o *orphanCohort) { o.at("w2").Busy = true }},
		{name: "migration fence in the lane", mutate: func(t *testing.T, o *orphanCohort) {
			// w9 has no coverage evidence, so it stays outside the pool while its pending
			// migration cordon fences the lane.
			w9 := desired("w9")
			w9.CustodyHeld, w9.DrainingReason = true, token("image_roll")
			o.workers = append(o.workers, w9)
			o.m.migrationPending = map[string]rollCoverage{"w9": {Owner: "owner-A", Lane: o.m.cfg.Namespace}}
		}},
		{name: "survivor unavailable", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w1").CustodyHeld = true
			o.at("w1").DrainingReason = token("image_roll")
			o.m.migrationPending = map[string]rollCoverage{"w1": {Owner: "owner-A", Lane: o.m.cfg.Namespace}}
		}},
		{name: "stray annotated deployment", mutate: func(t *testing.T, o *orphanCohort) {
			ghost := o.dep(t, "w2").DeepCopy()
			ghost.Name, ghost.UID, ghost.ResourceVersion = "uzi-hw-ghost", "ghost-uid", ""
			ghost.Labels = objectLabels("ghost")
			if _, err := o.c.AppsV1().Deployments(ghost.Namespace).Create(context.Background(), ghost, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "two deployments for one worker", mutate: func(t *testing.T, o *orphanCohort) { o.duplicate(t, "w1", "zzz-dup") }},
		{name: "unplanned same-namespace worker", mutate: func(t *testing.T, o *orphanCohort) {
			bad := desired("w9")
			bad.Template = "no-such-template"
			o.workers = append(o.workers, bad)
		}},
		{name: "unplanned worker with unknown namespace", mutate: func(t *testing.T, o *orphanCohort) {
			// Isolated plus Docker is a placement refusal, so the namespace is unknown.
			bad := desired("w9")
			bad.Isolated, bad.Docker = true, true
			o.workers = append(o.workers, bad)
		}},
		{name: "three canaries in the pool", n: 3, mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w3", func(m *rollMarker) { m.Canary = true })
		}},
		{name: "survivor with nil replicas", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Replicas = nil })
		}},
		{name: "D with nil replicas", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Replicas = nil })
		}},
		{name: "survivor claim mirror differing from its deployment marker", mutate: func(t *testing.T, o *orphanCohort) {
			mk := o.marker(t, "w1")
			mk.Nonce += "-other"
			raw, err := json.Marshal(mk)
			if err != nil {
				t.Fatal(err)
			}
			o.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[annotationRoll] = string(raw)
			})
		}},
		{name: "survivor deployment gone, canary only on its claim mirror", mutate: func(t *testing.T, o *orphanCohort) {
			raw := o.dep(t, "w1").Annotations[annotationRoll]
			o.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[annotationRoll] = raw
			})
			if err := o.c.Tracker().Delete(appsv1.SchemeGroupVersion.WithResource("deployments"), o.m.cfg.Namespace, deploymentName("w1")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "D with an empty deployment uid", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.UID = "" })
		}},
		{name: "unlabelled canonical data claim", mutate: func(t *testing.T, o *orphanCohort) {
			o.addClaim(t, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: dataPVCName("w2")}})
		}},
		{name: "survivor unavailable without a fence on the pool", mutate: func(t *testing.T, o *orphanCohort) {
			// A pending drain frozen under another owner and lane marks w1 unavailable
			// but fences neither this pool's lane nor its coverage key.
			o.m.migrationPending = map[string]rollCoverage{"w1": {Owner: "owner-B", Lane: "elsewhere"}}
		}},
		{name: "malformed third pool reservation", n: 3, mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w3", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = "{not json" })
		}},
		{name: "third pool member with no marker and an empty-valued annotation on its data claim", n: 3, mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w3", func(d *appsv1.Deployment) { delete(d.Annotations, annotationRoll) })
			o.editClaim(t, dataPVCName("w3"), func(p *corev1.PersistentVolumeClaim) {
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[annotationRoll] = ""
			})
		}},
		{name: "malformed peer reservation with differing current coverage", mutate: func(t *testing.T, o *orphanCohort) {
			o.outsider(t, false, true)
			o.edit(t, "w9", func(d *appsv1.Deployment) { d.Annotations[annotationRoll] = "{not json" })
		}},
		{name: "out-of-pool canary", mutate: func(t *testing.T, o *orphanCohort) { o.outsider(t, true, false) }},
		{name: "out-of-pool open reservation", mutate: func(t *testing.T, o *orphanCohort) { o.outsider(t, false, false) }},
		{name: "waking pool member with a reservation", mutate: func(t *testing.T, o *orphanCohort) {
			o.at("w1").Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: orphanWakeOp}
		}},
		{name: "both canaries absent", mutate: func(t *testing.T, o *orphanCohort) {
			o.recycleCurrent(t, "w1")
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "stale-template-hash" })
		}},
		{name: "two live canaries", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w2", func(d *appsv1.Deployment) { d.Spec.Replicas = &one })
			currentProof(t, o.c, o.at("w2"), m5Now)
		}},
		{name: "survivor absent but matching its reservation", mutate: func(t *testing.T, o *orphanCohort) { o.recycleCurrent(t, "w1") }},
		{name: "survivor custody held", mutate: func(t *testing.T, o *orphanCohort) { o.at("w1").CustodyHeld = true }},
		{name: "survivor raw reservation on non-data claim", mutate: func(t *testing.T, o *orphanCohort) {
			o.editClaim(t, nixPVCName("w1"), func(p *corev1.PersistentVolumeClaim) { p.Annotations = map[string]string{annotationRoll: "{not-json"} })
		}},
		{name: "survivor deployment terminating", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.DeletionTimestamp, d.Finalizers = &now, []string{"hold"} })
		}},
		{name: "survivor reservation hash differs from the target", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Hash = otherHash })
		}},
		{name: "survivor reservation image differs from the target", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Image = "worker:other" })
		}},
		{name: "survivor reservation version differs from the target", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.TargetVersion = "0.0.1" })
		}},
		{name: "survivor template differs from its reservation", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = "drifted-hash" })
		}},
		{name: "survivor image differs from its reservation", mutate: func(t *testing.T, o *orphanCohort) {
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "worker:other" })
		}},
		{name: "survivor pinned to an older template that its reservation matches", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Hash = otherHash })
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Annotations[AnnotationSpecHash] = otherHash })
		}},
		{name: "survivor image older than the target but matching its reservation", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Image = "worker:older" })
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Template.Spec.Containers[0].Image = "worker:older" })
		}},
		{name: "survivor with two replicas", mutate: func(t *testing.T, o *orphanCohort) {
			two := int32(2)
			o.edit(t, "w1", func(d *appsv1.Deployment) { d.Spec.Replicas = &two })
		}},
		{name: "survivor claim mirror cleaned this tick", mutate: func(t *testing.T, o *orphanCohort) {
			raw := o.dep(t, "w1").Annotations[annotationRoll]
			o.editClaim(t, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations[annotationRoll] = raw
			})
		}},
		{name: "survivor stuck", mutate: func(t *testing.T, o *orphanCohort) { targetWaiting(t, o.c, "w1", "ImagePullBackOff") }},
		{name: "survivor evidence unreadable: replicaset list fails", listFails: "replicasets",
			reasons: [2]string{"incomplete deployment/pod/ancestry snapshot", "incomplete deployment/pod/ancestry snapshot"}},
		{name: "survivor different owner", reasons: [2]string{"reservation owner or placement does not match API evidence", ""}, mutate: func(t *testing.T, o *orphanCohort) { evidenceOf(o, "w1").OwnerID = "owner-B" }},
		{name: "survivor different lane", mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Coverage.Lane = "another-namespace" })
		}},
		{name: "survivor in a different pool", reasons: [2]string{"waiting for current-pod registration and nonce heartbeat proof", ""}, mutate: func(t *testing.T, o *orphanCohort) {
			o.setMarker(t, "w1", func(m *rollMarker) { m.Coverage.Scheduling = []string{"zzz"} })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.n
			if n == 0 {
				n = 2
			}
			o := newOrphanCohort(t, n, 2)
			w2Target = o.dep(t, "w2").Spec.Template.Annotations[AnnotationSpecHash]
			staleDuplicate(t, o, false)
			if tc.listFails != "" {
				o.c.PrependReactor("list", tc.listFails, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("%s list denied", tc.listFails)
				})
			}
			if tc.mutate != nil {
				tc.mutate(t, o)
			}
			reservations := map[string]string{}
			for i := 1; i <= n; i++ {
				id := fmt.Sprintf("w%d", i)
				dep, err := o.c.AppsV1().Deployments(o.m.cfg.Namespace).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
				if apierrors.IsNotFound(err) {
					continue // Some negatives deliberately remove a canary's Deployment.
				}
				if err != nil {
					t.Fatal(err)
				}
				raw := dep.Annotations[annotationRoll]
				mk, err := decodeRoll(raw)
				if i <= 2 || (err == nil && mk != nil && mk.Canary) {
					reservations[id] = raw
				}
			}
			o.c.ClearActions()
			err := o.tick(t)
			if (err != nil) != tc.wantErr {
				t.Fatalf("tick error = %v, want error: %v", err, tc.wantErr)
			}
			for _, line := range actionTrace(o.c) {
				for id := range reservations {
					if line == "patch deployments "+deploymentName(id)+" metadata" {
						t.Fatalf("a canary reservation was written: %v", actionTrace(o.c))
					}
				}
			}
			for id, raw := range reservations {
				if got := o.dep(t, id).Annotations[annotationRoll]; got != raw {
					t.Fatalf("%s reservation changed: %q -> %q", id, raw, got)
				}
			}
			for i, id := range []string{"w1", "w2"} {
				want := tc.reasons[i]
				if tc.reasons == [2]string{} {
					want = dupHeld
				}
				if _, _, reason, _ := o.pacing(id); reason != want {
					t.Fatalf("%s reason = %q, want %q", id, reason, want)
				}
			}
		})
	}
}

func TestDuplicateCanaryLabelledSettledReplicaSetWithoutMatchingPod(t *testing.T) {
	for _, staleUID := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale pod owner UID=%v", staleUID), func(t *testing.T) {
			o := newOrphanCohort(t, 2, 2)
			staleDuplicate(t, o, false)
			zero := int32(0)
			o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "labelled-orphan", UID: "current-set", Labels: map[string]string{LabelWorkerID: "w2"}}, Spec: appsv1.ReplicaSetSpec{Replicas: &zero}})
			if staleUID {
				o.addPod(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-child", OwnerReferences: ownedBy("ReplicaSet", "labelled-orphan", "previous-set")}})
			}
			o.c.ClearActions()
			o.mustTick(t)
			if w2Cancellations(o.c) != 1 {
				t.Fatalf("settled set without a UID-matched pod prevented cancellation: %v", actionTrace(o.c))
			}
		})
	}
}

func TestDuplicateCanaryRechecksPodsAfterReplicaSets(t *testing.T) {
	for _, mode := range []string{"late pod", "list error", "continued list"} {
		t.Run(mode, func(t *testing.T) {
			o := newOrphanCohort(t, 2, 2)
			staleDuplicate(t, o, false)
			before := o.dep(t, "w2").Annotations[annotationRoll]
			ctx := context.Background()
			obs, err := o.m.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			setsListed, podLists := false, 0
			o.c.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				setsListed = true
				return false, nil, nil
			})
			o.c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				podLists++
				if podLists == 1 {
					return false, nil, nil // The roll snapshot predates the late observation.
				}
				if !setsListed {
					t.Fatal("pod recheck preceded the ReplicaSet snapshot")
				}
				switch mode {
				case "list error":
					return true, nil, fmt.Errorf("fresh pod list denied")
				case "continued list":
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "unobserved-page"}}, nil
				default:
					return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "late-child", UID: "late-child", OwnerReferences: ownedBy("ReplicaSet", "old-rs-"+deploymentName("w2"), types.UID("rs-"+deploymentName("w2")))}}}}, nil
				}
			})
			o.c.ClearActions()
			if err := o.m.Reconcile(ctx, o.workers, obs); err != nil {
				t.Fatal(err)
			}
			if w2Cancellations(o.c) != 0 || o.dep(t, "w2").Annotations[annotationRoll] != before {
				t.Fatalf("cancelled without a fresh complete absence proof: %v", actionTrace(o.c))
			}
			if podLists < 2 {
				t.Fatal("no pod recheck after the ReplicaSet snapshot")
			}
		})
	}
}

func TestDuplicateCanaryCancellationConflictHolds(t *testing.T) {
	o := newOrphanCohort(t, 2, 2)
	staleDuplicate(t, o, false)
	w1Raw, w2Raw := o.dep(t, "w1").Annotations[annotationRoll], o.dep(t, "w2").Annotations[annotationRoll]
	attempts := 0
	o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() != deploymentName("w2") {
			return false, nil, nil
		}
		attempts++
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, deploymentName("w2"), fmt.Errorf("stale"))
	})
	o.c.ClearActions()
	if err := o.tick(t); err == nil {
		t.Fatal("an unconfirmed cancellation was reported as settled")
	}
	if attempts != 1 {
		t.Fatalf("cancellation attempts = %d, want 1", attempts)
	}
	if got, want := actionTrace(o.c), []string{"patch deployments " + deploymentName("w2") + " metadata"}; !equalStrings(got, want) {
		t.Fatalf("tick wrote %v, want only the one conditioned attempt %v", got, want)
	}
	if o.dep(t, "w1").Annotations[annotationRoll] != w1Raw || o.dep(t, "w2").Annotations[annotationRoll] != w2Raw {
		t.Fatal("stored reservations changed")
	}
	for _, id := range []string{"w1", "w2"} {
		if _, _, reason, _ := o.pacing(id); reason != dupHeld {
			t.Fatalf("%s reason = %q, want %q", id, reason, dupHeld)
		}
	}
}

func TestDuplicateCanaryUncertainCommitDoesNotCancelTwice(t *testing.T) {
	o := newOrphanCohort(t, 2, 2)
	staleDuplicate(t, o, false)
	o.c.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.PatchAction).GetName() != deploymentName("w2") {
			return false, nil, nil
		}
		if _, _, err := o.base.React(a); err != nil {
			return true, nil, err
		}
		return true, nil, fmt.Errorf("response lost after commit")
	})
	o.c.ClearActions()
	if err := o.tick(t); err == nil {
		t.Fatal("a lost cancellation response was reported as confirmed")
	}
	if w2Cancellations(o.c) != 1 {
		t.Fatalf("tick 1 cancellations = %d", w2Cancellations(o.c))
	}
	for _, id := range []string{"w1", "w2"} {
		if _, _, reason, _ := o.pacing(id); reason != dupHeld {
			t.Fatalf("%s reason = %q, want %q", id, reason, dupHeld)
		}
	}
	if o.dep(t, "w2").Annotations[annotationRoll] != "" {
		t.Fatal("fixture: the cancellation did not commit")
	}
	o.c.ClearActions()
	_ = o.tick(t)
	if n := w2Cancellations(o.c); n != 0 {
		t.Fatalf("tick 2 cancelled w2 again %d time(s)", n)
	}
}

func TestDuplicateCanarySnapshotKeepsTerminatingSetsOutOfAncestry(t *testing.T) {
	o := newOrphanCohort(t, 2, 2)
	w2uid := types.UID("dep-" + deploymentName("w2"))
	now := metav1.NewTime(m5Now)
	o.addSet(t, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "zzz-terminating", DeletionTimestamp: &now, OwnerReferences: ownedBy("Deployment", deploymentName("w2"), w2uid)}})
	ctx := context.Background()
	obs, err := o.m.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]reconcile.ObservedWorker{}
	for _, w := range obs {
		byID[w.ID] = w
	}
	tick, _ := o.m.planRoll(ctx, o.workers, byID)
	e := tick.entries["w2"]
	if e == nil {
		t.Fatal("no w2 entry")
	}
	if _, ok := e.sets["zzz-terminating"]; ok {
		t.Fatal("a terminating replicaset entered serving-proof ancestry")
	}
	fact, listed := e.absenceOwners.lookup(metav1.OwnerReference{Kind: "ReplicaSet", Name: "zzz-terminating", UID: "uid-zzz-terminating"})
	if !listed || len(fact.owners) != 1 || fact.owners[0].Name != deploymentName("w2") || fact.owners[0].UID != w2uid {
		t.Fatalf("absence fact = %v (listed %v)", fact, listed)
	}
}
