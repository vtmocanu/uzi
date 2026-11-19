package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// Non-vacuity: the cases pin the in-place rewrite itself. With the retarget arm
// disabled, the following arms cancel and re-admit the entries instead (w1's Nonce
// changes, w2/w3 lose their reservations), so the Nonce and the participants'
// retargeted markers asserted below fail rather than pass through another arm.

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
			_ = o.tick(t)
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
		if m2.TargetVersion != newVersion || !m2.Canary || m2.Completed || m2.Nonce != mk.Nonce {
			t.Fatalf("tick 1: mirror not retargeted in place as an incomplete canary (completed history reopened): %+v", m2)
		}
		o.assertParticipantRetargeted(t, "tick 1", "w2", newVersion)
		o.assertParticipantRetargeted(t, "tick 1", "w3", newVersion)
		for i := 2; i <= 4; i++ {
			_ = o.tick(t)
			o.assertSingleCanary(t, fmt.Sprintf("tick %d", i))
		}
	})
}
