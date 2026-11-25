package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Tests for the durable Codex CA record on a retained worker Deployment: a retained /data
// recycle syncs it to the claim's stamp before the claim delete, so the evidence survives
// a controller restart between the delete and the replacement claim's create.

const caDockerNS = "uzi-workers-docker"

var pvcGR = schema.GroupResource{Resource: "persistentvolumeclaims"}

// caRetained seeds a retained (reservation-marked, zero-replica) Deployment rendered for the
// given stamp, a data claim stamped or not, an optional record annotation on the
// Deployment, and any extra objects. docker adds the docker namespace to the scan.
func caRetained(t *testing.T, stamped bool, record string, docker bool, extra ...runtime.Object) (*Materializer, *fake.Clientset, protocol.DesiredWorker, RenderConfig) {
	t.Helper()
	cfg := caCfg()
	if docker {
		cfg.DockerNamespace = caDockerNS
		cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	}
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
	dep := markedDeployment(t, cfg.withCodexCA(stamped), w, false)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	if record != "" {
		dep.Annotations[AnnotationCodexExtraCA] = record
	}
	objs := append([]runtime.Object{dep}, caClaims("w1", stamped)...)
	objs = append(objs, extra...)
	m, c := caMat(t, cfg, objs...)
	return m, c, w, cfg
}

// caEnforcePVCUID makes the fake apiserver honour a delete UID precondition on claims.
func caEnforcePVCUID(c *fake.Clientset) {
	c.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		d := a.(k8stesting.DeleteAction)
		pre := d.GetDeleteOptions().Preconditions
		if pre == nil || pre.UID == nil {
			return false, nil, nil
		}
		obj, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), d.GetName())
		if err != nil {
			return true, nil, err
		}
		md, _ := meta.Accessor(obj)
		if md.GetUID() != *pre.UID {
			return true, nil, apierrors.NewConflict(pvcGR, d.GetName(), errors.New("delete UID precondition"))
		}
		return false, nil, nil
	})
}

// caIntercept wraps the reactors installed so far for one verb/resource: when match holds,
// f decides the outcome and may run the real handling through next (so the mutation
// commits) before returning a lost or altered response.
func caIntercept(c *fake.Clientset, verb, resource string, match func(k8stesting.Action) bool, f func(next func() (runtime.Object, error)) (runtime.Object, error)) {
	chain := append([]k8stesting.Reactor(nil), c.ReactionChain...)
	c.PrependReactor(verb, resource, func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !match(a) {
			return false, nil, nil
		}
		obj, err := f(func() (runtime.Object, error) {
			for _, r := range chain {
				if !r.Handles(a) {
					continue
				}
				if handled, o, e := r.React(a); handled {
					return o, e
				}
			}
			return nil, errors.New("no reactor handled the action")
		})
		return true, obj, err
	})
}

func caIsRecordPatch(a k8stesting.Action) bool {
	p, ok := a.(k8stesting.PatchAction)
	return ok && a.GetResource().Resource == "deployments" && bytes.Contains(p.GetPatch(), []byte(AnnotationCodexExtraCA))
}

// caInterceptRecordPatch intercepts the first n record patches.
func caInterceptRecordPatch(c *fake.Clientset, n int, f func(next func() (runtime.Object, error)) (runtime.Object, error)) {
	caIntercept(c, "patch", "deployments", func(a k8stesting.Action) bool {
		if !caIsRecordPatch(a) || n <= 0 {
			return false
		}
		n--
		return true
	}, f)
}

func caRecordPatches(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if caIsRecordPatch(a) {
			n++
		}
	}
	return n
}

func caPVCDeletes(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		if d, ok := a.(k8stesting.DeleteAction); ok && a.GetResource().Resource == "persistentvolumeclaims" {
			out = append(out, d.GetName())
		}
	}
	return out
}

func caRecord(d *appsv1.Deployment) (string, bool) {
	v, ok := d.Annotations[AnnotationCodexExtraCA]
	return v, ok
}

func caObserve(t *testing.T, m *Materializer, id string) reconcile.ObservedWorker {
	t.Helper()
	obs, err := m.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("worker %s not observed", id)
	return reconcile.ObservedWorker{}
}

func caReplicas(d *appsv1.Deployment) int32 {
	if d.Spec.Replicas == nil {
		return -1
	}
	return *d.Spec.Replicas
}

// The record is written before the claim is deleted, and the delete is fenced to the
// original claim's UID.
func TestCodexCARetainedRecycleWritesTheRecordBeforeTheFencedDelete(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	uid := caClaim(t, c, cfg.Namespace, dataPVCName("w1")).UID
	if uid == "" {
		t.Fatal("fixture: the data claim has no UID")
	}
	caRecycleTick1(t, m, c, w)
	patchAt, deleteAt := -1, -1
	for i, a := range c.Actions() {
		if caIsRecordPatch(a) && patchAt < 0 {
			patchAt = i
		}
		if d, ok := a.(k8stesting.DeleteAction); ok && d.GetName() == dataPVCName("w1") && a.GetResource().Resource == "persistentvolumeclaims" {
			deleteAt = i
			pre := d.GetDeleteOptions().Preconditions
			if pre == nil || pre.UID == nil || *pre.UID != uid {
				t.Fatalf("data delete preconditions = %+v, want UID %s", pre, uid)
			}
		}
	}
	if patchAt < 0 || deleteAt < 0 || patchAt > deleteAt {
		t.Fatalf("record patch at %d, data delete at %d: the record must come first", patchAt, deleteAt)
	}
	if v, ok := caRecord(caDep(t, c, cfg.Namespace, "w1")); !ok || v != codexExtraCAWritten {
		t.Fatalf("record = %q, %v, want written", v, ok)
	}
}

// A delete that lands after the replacement exists is fenced off by the original UID.
func TestCodexCALateDataDeleteCannotHitTheReplacement(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	ns := cfg.Namespace
	caEnforcePVCUID(c)
	oldUID := caClaim(t, c, ns, dataPVCName("w1")).UID
	caRecycleTick1(t, m, c, w)
	w.DiskPressure = false
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	repl := caClaim(t, c, ns, dataPVCName("w1"))
	if !caStamped(repl) || string(repl.UID) == string(oldUID) {
		t.Fatalf("fixture: replacement stamped=%v uid=%s (old %s)", caStamped(repl), repl.UID, oldUID)
	}
	uid := oldUID
	err := c.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), dataPVCName("w1"), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if !apierrors.IsConflict(err) {
		t.Fatalf("late delete err = %v, want Conflict", err)
	}
	if caClaim(t, c, ns, dataPVCName("w1")) == nil {
		t.Fatal("the late delete removed the replacement")
	}
}

// A Conflict on the fenced /data delete means the claim we meant to delete is gone: it is
// logged, not an error.
func TestCodexCADataDeleteConflictIsNotAnError(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	caEnforcePVCUID(c)
	caIntercept(c, "delete", "persistentvolumeclaims", func(a k8stesting.Action) bool {
		return a.(k8stesting.DeleteAction).GetName() == dataPVCName("w1")
	}, func(func() (runtime.Object, error)) (runtime.Object, error) {
		return nil, apierrors.NewConflict(pvcGR, dataPVCName("w1"), errors.New("replaced"))
	})
	if err := caTick(t, m, w); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if caClaim(t, c, cfg.Namespace, dataPVCName("w1")) == nil {
		t.Fatal("fixture: the data claim should survive the conflicted delete")
	}
}

// A stale record on the retained Deployment with a live UNSTAMPED claim is cleared with a
// confirmed patch before the delete, so a restart cannot promote the replacement.
func TestCodexCAStaleRecordIsClearedBeforeTheDelete(t *testing.T) {
	m, c, w, cfg := caRetained(t, false, codexExtraCAWritten, false)
	ns := cfg.Namespace
	before := depHash(caDep(t, c, ns, "w1"))
	caRecycleTick1(t, m, c, w)
	if v, ok := caRecord(caDep(t, c, ns, "w1")); ok {
		t.Fatalf("record %q survived the recycle of an unstamped claim", v)
	}
	m = matrixRestart(m)
	w.DiskPressure = false
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("replacement claim stamped from a stale record")
	}
	d := caDep(t, c, ns, "w1")
	if caHasEnv(d) || caReplicas(d) != 1 || depHash(d) != before {
		t.Fatalf("deployment env=%v replicas=%d hash %s -> %s, want no env, 1 replica, same hash", caHasEnv(d), caReplicas(d), before, depHash(d))
	}
}

// A clear whose response is lost: no claim is deleted that tick; the next tick needs no
// patch and deletes.
func TestCodexCAStaleRecordClearWithLostResponse(t *testing.T) {
	m, c, w, cfg := caRetained(t, false, codexExtraCAWritten, false)
	caInterceptRecordPatch(c, 1, func(next func() (runtime.Object, error)) (runtime.Object, error) {
		if _, err := next(); err != nil {
			return nil, err
		}
		return nil, errors.New("lost patch response")
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("lost patch response was reported as confirmed")
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted despite an unconfirmed record write: %v", got)
	}
	if _, ok := caRecord(caDep(t, c, cfg.Namespace, "w1")); ok {
		t.Fatal("fixture: the clear did not commit")
	}
	c.ClearActions()
	caMustTick(t, m, w)
	if n := caRecordPatches(c); n != 0 {
		t.Fatalf("next tick patched the record %d times", n)
	}
	if del := deletedSet(c); !del["persistentvolumeclaims/"+dataPVCName("w1")] {
		t.Fatalf("next tick did not delete the data claim: %v", keysOf(del))
	}
}

// A committed sync with a lost response behaves the same for the stamped target.
func TestCodexCAStampedRecordWriteWithLostResponse(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	caInterceptRecordPatch(c, 1, func(next func() (runtime.Object, error)) (runtime.Object, error) {
		if _, err := next(); err != nil {
			return nil, err
		}
		return nil, errors.New("lost patch response")
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("lost patch response was reported as confirmed")
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted despite an unconfirmed record write: %v", got)
	}
	if v, _ := caRecord(caDep(t, c, cfg.Namespace, "w1")); v != codexExtraCAWritten {
		t.Fatal("fixture: the write did not commit")
	}
	c.ClearActions()
	caMustTick(t, m, w)
	if n := caRecordPatches(c); n != 0 {
		t.Fatalf("next tick patched the record %d times", n)
	}
	del := deletedSet(c)
	if !del["persistentvolumeclaims/"+dataPVCName("w1")] || !del["persistentvolumeclaims/"+nixPVCName("w1")] {
		t.Fatalf("next tick did not delete both claims: %v", keysOf(del))
	}
}

// A stamped claim whose Deployment already carries the record needs no patch.
func TestCodexCAMatchingRecordNeedsNoPatch(t *testing.T) {
	m, c, w, _ := caRetained(t, true, codexExtraCAWritten, false)
	caRecycleTick1(t, m, c, w)
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "deployments" && a.GetVerb() == "patch" {
			t.Fatalf("deployment patched although the record already matched: %v", a)
		}
	}
}

// An unconfirmed record write deletes nothing.
func TestCodexCAUnconfirmedRecordWriteDeletesNothing(t *testing.T) {
	cases := map[string]func(next func() (runtime.Object, error)) (runtime.Object, error){
		"uncommitted error": func(func() (runtime.Object, error)) (runtime.Object, error) {
			return nil, errors.New("apiserver unavailable")
		},
		"conflict": func(func() (runtime.Object, error)) (runtime.Object, error) {
			return nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, deploymentName("w1"), errors.New("stale"))
		},
		"different uid": func(next func() (runtime.Object, error)) (runtime.Object, error) {
			o, err := next()
			if err != nil {
				return nil, err
			}
			d := o.(*appsv1.Deployment).DeepCopy()
			d.UID = types.UID("someone-else")
			return d, nil
		},
		"wrong annotation state": func(next func() (runtime.Object, error)) (runtime.Object, error) {
			o, err := next()
			if err != nil {
				return nil, err
			}
			d := o.(*appsv1.Deployment).DeepCopy()
			delete(d.Annotations, AnnotationCodexExtraCA)
			return d, nil
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			m, c, w, _ := recycleFixture(t, true)
			caInterceptRecordPatch(c, 1, f)
			if err := caTick(t, m, w); err == nil {
				t.Fatal("tick succeeded despite an unconfirmed record write")
			}
			if got := caPVCDeletes(c); len(got) != 0 {
				t.Fatalf("claims deleted: %v", got)
			}
		})
	}
}

// A nix-only retained recycle never touches the record.
func TestCodexCANixOnlyRetainedRecycleLeavesTheRecord(t *testing.T) {
	cfg := caCfg()
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}, m5Now)
	dep := markedDeployment(t, cfg.withCodexCA(false), w, false)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	dep.Annotations[AnnotationCodexExtraCA] = codexExtraCAWritten
	m, c := caMat(t, cfg, dep, pvcFor("w1", "data"), pvcNix("w1", "4Gi")) // undersized nix: size-drift recycle
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	del := deletedSet(c)
	if !del["persistentvolumeclaims/"+nixPVCName("w1")] {
		t.Fatalf("fixture: the nix recycle did not run: %v", keysOf(del))
	}
	if del["persistentvolumeclaims/"+dataPVCName("w1")] {
		t.Fatal("a nix-only recycle deleted /data")
	}
	if n := caRecordPatches(c); n != 0 {
		t.Fatalf("record patched %d times by a nix-only recycle", n)
	}
	if v, _ := caRecord(caDep(t, c, cfg.Namespace, "w1")); v != codexExtraCAWritten {
		t.Fatalf("record = %q, want it untouched", v)
	}
}

// Derivation: only an exact "written" on a live Deployment in the worker's own namespace
// counts, and the own claim, when there is one, decides alone.
func TestCodexCARecordDerivation(t *testing.T) {
	ns := caCfg().Namespace
	w := newWorkerFixture(desired("w1"))
	m, _ := caMat(t, caCfg())
	if got, _, _, _ := m.deriveCodexCA(w, reconcile.ObservedWorker{DeploymentCodexExtraCA: map[string]bool{ns: true}}); !got {
		t.Error("own-namespace record without a claim did not count")
	}
	if got, _, _, _ := m.deriveCodexCA(w, reconcile.ObservedWorker{DeploymentCodexExtraCA: map[string]bool{caDockerNS: true}}); got {
		t.Error("a record in another namespace counted")
	}
	unstamped := reconcile.ClaimObservation{Namespace: ns, Name: dataPVCName("w1")}
	obs := reconcile.ObservedWorker{HasDataPVC: true, DataClaim: &unstamped, DataClaimsByNamespace: map[string]reconcile.ClaimObservation{ns: unstamped}, DeploymentCodexExtraCA: map[string]bool{ns: true}}
	if got, _, _, _ := m.deriveCodexCA(w, obs); got {
		t.Error("a record overrode an own unstamped claim")
	}
}

func TestCodexCAMalformedOrTerminatingRecordDoesNotCount(t *testing.T) {
	now := metav1.NewTime(m5Now)
	cases := map[string]func(d *appsv1.Deployment){
		"empty value": func(d *appsv1.Deployment) { d.Annotations[AnnotationCodexExtraCA] = "" },
		"wrong value": func(d *appsv1.Deployment) { d.Annotations[AnnotationCodexExtraCA] = "yes" },
		"terminating": func(d *appsv1.Deployment) {
			d.Annotations[AnnotationCodexExtraCA] = codexExtraCAWritten
			d.DeletionTimestamp = &now
		},
		"valid record": func(d *appsv1.Deployment) { d.Annotations[AnnotationCodexExtraCA] = codexExtraCAWritten },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}, m5Now)
			dep := markedDeployment(t, cfg.withCodexCA(false), w, false)
			mutate(dep)
			m, _ := caMat(t, cfg, dep)
			obs := caObserve(t, m, "w1")
			want := name == "valid record"
			if got := obs.DeploymentCodexExtraCA[cfg.Namespace]; got != want {
				t.Fatalf("observed record = %v, want %v", got, want)
			}
			if got, _, _, _ := m.deriveCodexCA(w, obs); got != want {
				t.Fatalf("decision = %v, want %v", got, want)
			}
		})
	}
}

// Namespace fold: a same-ID Deployment scanned later (docker namespace) must not erase the
// record observed in the worker's own namespace.
func TestCodexCARecordSurvivesALaterScannedForeignDeployment(t *testing.T) {
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deploymentName("w1"), Namespace: caDockerNS, Labels: objectLabels("w1")}}
	m, _, w, cfg := caRetained(t, true, codexExtraCAWritten, true, foreign)
	obs := caObserve(t, m, "w1")
	if !obs.DeploymentCodexExtraCA[cfg.Namespace] || obs.DeploymentCodexExtraCA[caDockerNS] {
		t.Fatalf("record set = %v, want only %s", obs.DeploymentCodexExtraCA, cfg.Namespace)
	}
	// No claim observed in the own namespace: the record decides.
	obs.DataClaim, obs.DataClaimsByNamespace, obs.HasDataPVC = nil, nil, false
	if got, _, _, _ := m.deriveCodexCA(w, obs); !got {
		t.Fatal("record lost to the foreign Deployment")
	}
}

// Namespace fold (PVC a): a stamped live claim in the own namespace keeps its stamp and its
// UID although a same-ID /data claim scanned later in another namespace overwrote the scalar
// DataClaim; the record is written and the delete fences the OWN claim.
func TestCodexCAOwnClaimWinsOverALaterScannedForeignClaim(t *testing.T) {
	foreign := pvcForNS("w1", "data", caDockerNS)
	foreign.UID = types.UID("foreign-uid")
	m, c, w, cfg := caRetained(t, true, "", true, foreign)
	ns := cfg.Namespace
	own := caClaim(t, c, ns, dataPVCName("w1"))
	obs := caObserve(t, m, "w1")
	if got := ownDataClaim(w, obs, ns); got == nil || !got.CodexExtraCA || got.UID != string(own.UID) {
		t.Fatalf("ownDataClaim = %+v, want the stamped own claim %s", got, own.UID)
	}
	caEnforcePVCUID(c)
	caMustTick(t, m, w)
	if n := caRecordPatches(c); n != 1 {
		t.Fatalf("record patches = %d, want 1", n)
	}
	if v, _ := caRecord(caDep(t, c, ns, "w1")); v != codexExtraCAWritten {
		t.Fatalf("record = %q, want written", v)
	}
	if caClaim(t, c, ns, dataPVCName("w1")) != nil {
		t.Fatal("the own data claim was not deleted")
	}
	if caClaim(t, c, caDockerNS, dataPVCName("w1")) == nil {
		t.Fatal("the foreign claim was deleted")
	}
}

// Namespace fold (PVC b): an unstamped own claim decides false even with a stale record and
// a stamped same-ID claim elsewhere.
func TestCodexCAUnstampedOwnClaimDecidesAgainstARecordAndAForeignStamp(t *testing.T) {
	foreign := pvcForNS("w1", "data", caDockerNS)
	foreign.Annotations = map[string]string{AnnotationCodexExtraCA: codexExtraCAWritten}
	m, c, w, cfg := caRetained(t, false, codexExtraCAWritten, true, foreign)
	obs := caObserve(t, m, "w1")
	if got, _, _, _ := m.deriveCodexCA(w, obs); got {
		t.Fatal("decision true for an unstamped own claim")
	}
	caMustTick(t, m, w)
	if v, ok := caRecord(caDep(t, c, cfg.Namespace, "w1")); ok {
		t.Fatalf("stale record %q not cleared", v)
	}
}

// Namespace fold (PVC c): the own /data claim is absent but a foreign same-ID claim makes the
// scalar observation read live. Nothing is deleted and the recycle reports the missing
// evidence.
func TestCodexCAForeignClaimAloneCannotAuthorizeADataDelete(t *testing.T) {
	foreign := pvcForNS("w1", "data", caDockerNS)
	cfg := caCfg()
	cfg.DockerNamespace = caDockerNS
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
	dep := markedDeployment(t, cfg.withCodexCA(true), w, false)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	m, c := caMat(t, cfg, dep, foreign, pvcNix("w1", "20Gi"))
	err := caTick(t, m, w)
	if err == nil || !strings.Contains(err.Error(), "recycle data claim evidence unavailable") {
		t.Fatalf("tick err = %v, want the evidence-unavailable error", err)
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted: %v", got)
	}
}

// /nix goes first and the controller restarts before the /data delete: the record written
// before the loop carries the evidence.
func TestCodexCAPartialDeletionRestartKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	ns := cfg.Namespace
	before := depHash(caDep(t, c, ns, "w1"))
	failed := false
	c.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.DeleteAction).GetName() != dataPVCName("w1") || failed {
			return false, nil, nil
		}
		failed = true
		return true, nil, errors.New("data delete failed before commit")
	})
	if err := caTick(t, m, w); err == nil {
		t.Fatal("failed data delete was not reported")
	}
	if caClaim(t, c, ns, nixPVCName("w1")) != nil || caClaim(t, c, ns, dataPVCName("w1")) == nil {
		t.Fatal("fixture: want /nix deleted and /data kept")
	}
	m = matrixRestart(m)
	for i := 0; i < 3 && caClaim(t, c, ns, dataPVCName("w1")) != nil; i++ {
		caMustTick(t, m, w) // pressure still on: the data claim is recycled once /nix is back
	}
	if caClaim(t, c, ns, dataPVCName("w1")) != nil {
		t.Fatal("fixture: the data claim was not deleted after the restart")
	}
	w.DiskPressure = false
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("replacement claim is not stamped")
	}
	d := caDep(t, c, ns, "w1")
	if caReplicas(d) != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("deployment replicas=%d env=%v hash %s -> %s", caReplicas(d), caHasEnv(d), before, depHash(d))
	}
}

// The data claim stays Terminating across a restart: the Deployment stays at zero until the
// claim is gone, then the replacement is stamped and the env kept.
func TestCodexCATerminatingDataClaimAcrossARestartKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	ns := cfg.Namespace
	c.PrependReactor("delete", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.DeleteAction).GetName()
		if name != dataPVCName("w1") {
			return false, nil, nil
		}
		p, err := c.Tracker().Get(a.GetResource(), a.GetNamespace(), name)
		if err != nil {
			return true, nil, err
		}
		keep := p.DeepCopyObject().(*corev1.PersistentVolumeClaim)
		now := metav1.NewTime(m5Now)
		keep.DeletionTimestamp = &now
		return true, nil, c.Tracker().Update(a.GetResource(), keep, a.GetNamespace())
	})
	before := depHash(caDep(t, c, ns, "w1"))
	caMustTick(t, m, w)
	if p := caClaim(t, c, ns, dataPVCName("w1")); p == nil || p.DeletionTimestamp == nil {
		t.Fatal("fixture: the data claim is not Terminating")
	}
	m = matrixRestart(m)
	w.DiskPressure = false
	for i := 0; i < 2; i++ {
		caMustTick(t, m, w)
		if d := caDep(t, c, ns, "w1"); caReplicas(d) != 0 {
			t.Fatalf("replicas = %d while the data claim is Terminating, want 0", caReplicas(d))
		}
	}
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), ns, dataPVCName("w1")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("replacement data claim is not stamped")
	}
	d := caDep(t, c, ns, "w1")
	if caReplicas(d) != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("deployment replicas=%d env=%v hash %s -> %s", caReplicas(d), caHasEnv(d), before, depHash(d))
	}
}

// The replacement claim's create commits stamped but the response is lost, then the
// controller restarts.
func TestCodexCALostReplacementCreateThenRestartKeepsTheEnv(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, true)
	ns := cfg.Namespace
	before := depHash(caDep(t, c, ns, "w1"))
	caRecycleTick1(t, m, c, w)
	caDataPVCCreate(c, "w1", 1, true)
	w.DiskPressure = false
	if err := caTick(t, m, w); err == nil {
		t.Fatal("lost create response was reported as confirmed")
	}
	if !caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Fatal("fixture: the stamped create did not commit")
	}
	m = matrixRestart(m)
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	d := caDep(t, c, ns, "w1")
	if caReplicas(d) != 1 || !caHasEnv(d) || depHash(d) != before {
		t.Fatalf("deployment replicas=%d env=%v hash %s -> %s", caReplicas(d), caHasEnv(d), before, depHash(d))
	}
}

// A non-retained recycle deletes the Deployment, so there is no record to carry the evidence
// across a restart: that path stays carry-only and fails closed (no env, unstamped).
func TestCodexCANonRetainedRecycleRestartStaysCarryOnly(t *testing.T) {
	m, c, w, cfg := recycleFixture(t, false)
	ns := cfg.Namespace
	caRecycleTick1(t, m, c, w)
	if caRecordPatches(c) != 0 {
		t.Fatal("a non-retained recycle wrote a record")
	}
	m = matrixRestart(m)
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	if caStamped(caClaim(t, c, ns, dataPVCName("w1"))) {
		t.Error("replacement claim stamped after a restart")
	}
	if d := caDep(t, c, ns, "w1"); d != nil && caHasEnv(d) {
		t.Error("recreated deployment kept the env without evidence")
	}
}
