package kube

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Characterization for #4475: the mapping-correction retarget in planPool (marker Hash
// and Image still match the target, only TargetVersion differs) rewrites the marker in
// place and preserves its Canary bit; it never sets the bit on another worker. These
// cases drive Observe/Reconcile (ForceRoll off) and assert that no tick leaves a second
// canary=true reservation on any Deployment or data-claim mirror, and that no worker
// other than w1 ever carries one. The retarget is proven to have run by checking that
// the marker's TargetVersion moved to the new version where a marker is observable.

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
	seen := map[string]bool{}
	for _, h := range holders {
		switch h {
		case "deployment/" + deploymentName("w1"), "pvc/" + dataPVCName("w1"), "pvc/" + nixPVCName("w1"):
			seen["w1"] = true
		default:
			t.Fatalf("%s: non-w1 canary holder %q (all: %v)", when, h, holders)
		}
	}
	deployCanaries := 0
	for _, h := range holders {
		if len(h) > 11 && h[:11] == "deployment/" {
			deployCanaries++
		}
	}
	if deployCanaries > 1 {
		t.Fatalf("%s: %d canary Deployments: %v", when, deployCanaries, holders)
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

		o.m.cfg.RollTargetVersion = newVersion
		retargeted := false
		for i := 0; i < 4; i++ {
			_ = o.tick(t) // holds and unconfirmed writes are legitimate here
			o.assertSingleCanary(t, "tick "+string(rune('1'+i)))
			if mk := o.marker(t, "w1"); mk != nil && mk.TargetVersion == newVersion {
				retargeted = true
				if !mk.Canary {
					t.Fatalf("retarget dropped the canary bit: %+v", mk)
				}
			}
			for _, id := range []string{"w2", "w3"} {
				if mk := o.marker(t, id); mk != nil && mk.Canary {
					t.Fatalf("tick %d: %s became a canary: %+v", i, id, mk)
				}
			}
		}
		t.Logf("retarget branch ran (w1 TargetVersion %q -> %q): %v", before.TargetVersion, newVersion, retargeted)
		if !retargeted {
			t.Fatalf("retarget never rewrote w1's marker; the case is vacuous")
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
		retargeted := false
		for i := 0; i < 4; i++ {
			_ = o.tick(t)
			o.assertSingleCanary(t, "tick "+string(rune('1'+i)))
			p, err := o.c.CoreV1().PersistentVolumeClaims(o.m.cfg.Namespace).Get(context.Background(), dataPVCName("w1"), metav1.GetOptions{})
			if err == nil {
				if m2, err := decodeRoll(p.Annotations[annotationRoll]); err == nil && m2 != nil && m2.TargetVersion == newVersion {
					retargeted = true
				}
			}
		}
		t.Logf("retarget branch ran on the mirror: %v", retargeted)
		if !retargeted {
			t.Fatalf("mirror marker never retargeted; the case is vacuous (holders %v)", o.canaryHolders(t))
		}
	})
}
