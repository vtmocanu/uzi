package kube

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Tests that a same-ID /data claim in another worker namespace never stands in for the
// worker's own /data claim: it must not authorize a delete, gate a create, count for
// pressure eligibility or the cooldown, or release the paused-restore and Deployment
// guards. In every fixture /nix is in the own namespace and the foreign object is a /data
// claim only.

var pvcRes = corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")

// caSetClaim rewrites a claim in place through the tracker.
func caSetClaim(t *testing.T, c *fake.Clientset, ns, name string, mutate func(*corev1.PersistentVolumeClaim)) {
	t.Helper()
	p := caClaim(t, c, ns, name)
	if p == nil {
		t.Fatalf("claim %s/%s not found", ns, name)
	}
	p = p.DeepCopy()
	mutate(p)
	if err := c.Tracker().Update(pvcRes, p, ns); err != nil {
		t.Fatal(err)
	}
}

func caTerminateClaim(t *testing.T, c *fake.Clientset, ns, name string) {
	t.Helper()
	caSetClaim(t, c, ns, name, func(p *corev1.PersistentVolumeClaim) {
		now := metav1.NewTime(m5Now)
		p.DeletionTimestamp = &now
	})
}

func caForeignData(uid string) *corev1.PersistentVolumeClaim {
	f := pvcForNS("w1", "data", caDockerNS)
	f.UID = "foreign-" + types.UID(uid)
	f.Annotations = map[string]string{"foreign/keep": "yes"}
	return f
}

// caRetainedNoOwnData is a retained zero-replica Deployment with a young live own /nix at
// size (so a later pressure tick is held by the cooldown), no own /data claim and a live
// foreign /data claim in the docker namespace.
func caRetainedNoOwnData(t *testing.T, pressure bool) (*Materializer, *fake.Clientset, protocol.DesiredWorker, RenderConfig) {
	t.Helper()
	cfg := caCfg()
	cfg.DockerNamespace = caDockerNS
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: pressure}, m5Now)
	dep := markedDeployment(t, cfg.withCodexCA(false), w, false)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	nix := pvcNix("w1", "20Gi")
	nix.CreationTimestamp = metav1.NewTime(m5Now)
	m, c := caMat(t, cfg, dep, caForeignData("a"), nix)
	return m, c, w, cfg
}

func caSameClaim(t *testing.T, c *fake.Clientset, ns, name string, want *corev1.PersistentVolumeClaim) {
	t.Helper()
	got := caClaim(t, c, ns, name)
	if got == nil || got.UID != want.UID || got.ResourceVersion != want.ResourceVersion || !reflect.DeepEqual(got.Annotations, want.Annotations) {
		t.Fatalf("claim %s/%s = %+v, want unchanged %+v", ns, name, got, want)
	}
}

// (a) A foreign claim alone cannot authorize a /data delete, even with disk pressure on
// and a controller restart in the middle: the own /data is provisioned, nothing is
// deleted and the foreign claim is untouched.
//
// What this discriminates: the create gate (the own /data is created although a foreign
// claim exists) and the restart recovery. The "no deletes" assertion alone also holds on
// the base, because the young /nix keeps the cooldown active; delete authorization is
// covered by (b) and (g).
func TestCodexCAForeignClaimAloneCannotAuthorizeADataDelete(t *testing.T) {
	m, c, w, cfg := caRetainedNoOwnData(t, true)
	ns := cfg.Namespace
	foreign := caClaim(t, c, caDockerNS, dataPVCName("w1")).DeepCopy()
	nixUID := caClaim(t, c, ns, nixPVCName("w1")).UID
	for i := 0; i < 2; i++ {
		caMustTick(t, m, w)
	}
	m = matrixRestart(m)
	for i := 0; i < 4; i++ {
		caMustTick(t, m, w)
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted: %v", got)
	}
	if n := caClaim(t, c, ns, nixPVCName("w1")); n == nil || n.UID != nixUID {
		t.Fatalf("nix claim = %+v, want uid %s", n, nixUID)
	}
	caSameClaim(t, c, caDockerNS, dataPVCName("w1"), foreign)
	if caClaim(t, c, ns, dataPVCName("w1")) == nil {
		t.Fatal("the own data claim was not created")
	}
	d := caDep(t, c, ns, "w1")
	if d == nil || caReplicas(d) != 1 || caHasEnv(d) {
		t.Fatalf("deployment = %v, want replicas 1 and no Codex CA env", d)
	}
}

// (b) With no own /data claim the pressure recycle is not eligible, whether or not a live
// foreign claim exists: /data is provisioned, /nix is untouched.
func TestCodexCAOwnDataAbsentIsNotRecycleEligible(t *testing.T) {
	for _, withForeign := range []bool{false, true} {
		name := "no-foreign"
		if withForeign {
			name = "live-foreign"
		}
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			cfg.DockerNamespace = caDockerNS
			cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
			w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
			dep := markedDeployment(t, cfg.withCodexCA(true), w, false)
			zero := int32(0)
			dep.Spec.Replicas = &zero
			objs := []runtime.Object{dep, pvcNix("w1", "20Gi")}
			if withForeign {
				objs = append(objs, caForeignData("b"))
			}
			m, c := caMat(t, cfg, objs...)
			ns := cfg.Namespace
			nixUID := caClaim(t, c, ns, nixPVCName("w1")).UID
			obs := caObserve(t, m, "w1")
			spec := testSpec(t, "base", "m")
			if m.pressureRecycleEligible(w, obs, ns) || m.legacyRecycleDue(w, obs, spec, ns) {
				t.Fatal("recycle reported eligible with no own /data claim")
			}
			caMustTick(t, m, w)
			if got := caPVCDeletes(c); len(got) != 0 {
				t.Fatalf("claims deleted: %v", got)
			}
			if caClaim(t, c, ns, dataPVCName("w1")) == nil {
				t.Fatal("the own data claim was not provisioned")
			}
			if n := caClaim(t, c, ns, nixPVCName("w1")); n == nil || n.UID != nixUID {
				t.Fatalf("nix claim = %+v, want uid %s", n, nixUID)
			}
		})
	}
}

// (c) The paused-restore gate reads the own /data claim: a live foreign claim neither
// restores the worker while the own claim is absent nor while it is Terminating.
func TestCodexCAPausedRestoreIgnoresAForeignDataClaim(t *testing.T) {
	t.Run("own-absent", func(t *testing.T) {
		m, c, w, cfg := caRetainedNoOwnData(t, false)
		ns := cfg.Namespace
		caMustTick(t, m, w)
		if caClaim(t, c, ns, dataPVCName("w1")) == nil {
			t.Fatal("the own data claim was not created")
		}
		if d := caDep(t, c, ns, "w1"); caReplicas(d) != 0 {
			t.Fatalf("replicas = %d on the tick that created the own claim, want 0", caReplicas(d))
		}
		for i := 0; i < 3 && caReplicas(caDep(t, c, ns, "w1")) != 1; i++ {
			caMustTick(t, m, w)
		}
		if d := caDep(t, c, ns, "w1"); caReplicas(d) != 1 {
			t.Fatalf("replicas = %d after the own claim exists, want 1", caReplicas(d))
		}
	})
	t.Run("own-terminating", func(t *testing.T) {
		m, c, w, cfg := caRetainedNoOwnData(t, false)
		ns := cfg.Namespace
		own := pvcFor("w1", "data")
		if _, err := c.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), own, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		caTerminateClaim(t, c, ns, dataPVCName("w1"))
		for i := 0; i < 3; i++ {
			caMustTick(t, m, w)
			if d := caDep(t, c, ns, "w1"); caReplicas(d) != 0 {
				t.Fatalf("replicas = %d while the own claim is Terminating, want 0", caReplicas(d))
			}
		}
	})
}

// (d) The Deployment placement guard reads the own /data claim: with the own claim
// Terminating no Deployment is created although a foreign claim is live. The control
// leaves the own claim live and expects the Deployment.
func TestCodexCADeploymentGuardIgnoresAForeignDataClaim(t *testing.T) {
	for _, terminate := range []bool{true, false} {
		name := "own-live-control"
		if terminate {
			name = "own-terminating"
		}
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			cfg.DockerNamespace = caDockerNS
			cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
			w := caWorker("w1")
			m, c := caMat(t, cfg, append(caClaims("w1", true), caForeignData("d"))...)
			ns := cfg.Namespace
			if terminate {
				caTerminateClaim(t, c, ns, dataPVCName("w1"))
			}
			for i := 0; i < 3; i++ {
				caMustTick(t, m, w)
			}
			d := caDep(t, c, ns, "w1")
			if terminate && d != nil {
				t.Fatalf("deployment %v created over a Terminating own data claim", d.Name)
			}
			if !terminate && d == nil {
				t.Fatal("control: no deployment created with a live own data claim")
			}
		})
	}
}

// (e) New placement: a foreign /data claim, scanned last, whether Terminating or live and
// unstamped, neither holds back the create of the own /data claim nor lends it its (missing)
// placement stamp: the own claim is created stamped new.
func TestCodexCAForeignTerminatingDataClaimDoesNotDeferTheOwnCreate(t *testing.T) {
	for _, terminating := range []bool{true, false} {
		name := "live-unstamped-foreign"
		if terminating {
			name = "terminating-foreign"
		}
		t.Run(name, func(t *testing.T) {
			cfg := caCfg()
			cfg.DockerNamespace = caDockerNS
			cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
			cfg.NodeSelector = map[string]string{"kubernetes.io/arch": "amd64"}
			cfg.Tolerations = uziTaint
			cfg.NewPlacementNodeSelector = map[string]string{"pool": "new"}
			cfg.NewPlacementTolerations = newPlacementTaint
			w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}, m5Now)
			dep := markedDeployment(t, cfg.withPlacement(true).withCodexCA(true), w, false)
			zero := int32(0)
			dep.Spec.Replicas = &zero
			nix := pvcNix("w1", "20Gi")
			nix.Annotations = map[string]string{AnnotationPlacement: placementNew}
			foreign := caForeignData("e")
			if terminating {
				now := metav1.NewTime(m5Now)
				foreign.DeletionTimestamp = &now
			}
			m, c := caMat(t, cfg, dep, nix, foreign)
			obs := caObserve(t, m, "w1")
			if obs.DataClaim == nil || obs.DataClaim.Namespace != caDockerNS || obs.DataClaim.Terminating != terminating {
				t.Fatalf("fixture: DataClaim = %+v, want the foreign claim (terminating=%v)", obs.DataClaim, terminating)
			}
			caMustTick(t, m, w)
			own := caClaim(t, c, cfg.Namespace, dataPVCName("w1"))
			if own == nil {
				t.Fatal("the own data claim was not created")
			}
			if got := own.Annotations[AnnotationPlacement]; got != placementNew {
				t.Fatalf("own data claim placement stamp = %q, want %q", got, placementNew)
			}
		})
	}
}

// (f) The recycle cooldown reads the own /data claim's age, not the last-scanned one.
func TestCodexCARecycleCooldownIgnoresAForeignDataClaimAge(t *testing.T) {
	setup := func(t *testing.T, ownAge, foreignAge time.Duration) (*Materializer, *fake.Clientset, protocol.DesiredWorker) {
		foreign := caForeignData("f")
		foreign.CreationTimestamp = metav1.NewTime(m5Now.Add(-foreignAge))
		m, c, w, cfg := caRetained(t, true, "", true, foreign)
		caSetClaim(t, c, cfg.Namespace, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
			p.CreationTimestamp = metav1.NewTime(m5Now.Add(-ownAge))
		})
		obs := caObserve(t, m, "w1")
		if obs.DataPVCCreatedAt == nil || !obs.DataPVCCreatedAt.Equal(m5Now.Add(-foreignAge)) {
			t.Fatalf("fixture: DataPVCCreatedAt = %v, want the foreign value", obs.DataPVCCreatedAt)
		}
		return m, c, w
	}
	t.Run("old-foreign-young-own", func(t *testing.T) {
		m, c, w := setup(t, 10*time.Minute, 48*time.Hour)
		caMustTick(t, m, w)
		if got := caPVCDeletes(c); len(got) != 0 {
			t.Fatalf("claims deleted within the own cooldown: %v", got)
		}
	})
	t.Run("young-foreign-old-own", func(t *testing.T) {
		m, c, w := setup(t, 48*time.Hour, 10*time.Minute)
		caMustTick(t, m, w)
		del := deletedSet(c)
		if !del["persistentvolumeclaims/"+dataPVCName("w1")] || !del["persistentvolumeclaims/"+nixPVCName("w1")] {
			t.Fatalf("recycle did not proceed: %v", keysOf(del))
		}
	})
}

// (g) Defensive: a Data recycle request with no own /data claim deletes no /data claim
// and writes no record.
func TestCodexCARecycleWithoutOwnDataClaimWritesNothing(t *testing.T) {
	m, c, w, cfg := caRetainedNoOwnData(t, true)
	// A stamped foreign claim, so a carry change would be possible if it were read.
	caSetClaim(t, c, caDockerNS, dataPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
		p.Annotations[AnnotationCodexExtraCA] = codexExtraCAWritten
	})
	if m.codexCACarried("w1", cfg.Namespace) {
		t.Fatal("fixture: carry set before the recycle")
	}
	obs := caObserve(t, m, "w1")
	err := m.recycleWorkerVolumes(context.Background(), w, obs, cfg.Namespace, recycleVolumes{Nix: true, Data: true, Pressure: true})
	if err != nil {
		t.Fatalf("recycle: %v", err)
	}
	var nixDeleted bool
	for _, name := range caPVCDeletes(c) {
		if name == dataPVCName("w1") {
			t.Fatal("a data claim was deleted")
		}
		if name == nixPVCName("w1") {
			nixDeleted = true
		}
	}
	// The missing /data claim must not stop the rest of the recycle: the own /nix goes.
	if !nixDeleted {
		t.Fatalf("the own nix claim was not deleted: %v", caPVCDeletes(c))
	}
	if n := caRecordPatches(c); n != 0 {
		t.Fatalf("record patches = %d, want 0", n)
	}
	if m.codexCACarried("w1", cfg.Namespace) {
		t.Fatal("the carry was set by a recycle with no own data claim")
	}
}

// (h) Compatibility: a nix-only size-drift recycle proceeds on a retained Deployment
// while the own /data claim is Terminating.
func TestCodexCANixOnlyRecycleProceedsWithATerminatingOwnData(t *testing.T) {
	m, c, w, cfg := caRetained(t, true, "", false)
	w.DiskPressure = false
	ns := cfg.Namespace
	caSetClaim(t, c, ns, nixPVCName("w1"), func(p *corev1.PersistentVolumeClaim) {
		p.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}
	})
	caTerminateClaim(t, c, ns, dataPVCName("w1"))
	caMustTick(t, m, w)
	del := deletedSet(c)
	if !del["persistentvolumeclaims/"+nixPVCName("w1")] {
		t.Fatalf("the undersized nix claim was not recycled: %v", keysOf(del))
	}
	if del["persistentvolumeclaims/"+dataPVCName("w1")] {
		t.Fatal("the Terminating own data claim was deleted again")
	}
}

// (i) Regression: a stamped own /data claim recycled while a live foreign claim exists
// keeps the Codex CA across a restart between the delete and the replacement create. This
// is red on the base (the foreign claim shadowed the own one), so it is a regression test,
// not a compatibility control.
func TestCodexCAForeignDataClaimRetainedRecycleRestart(t *testing.T) {
	foreign := caForeignData("i")
	m, c, w, cfg := caRetained(t, true, "", true, foreign)
	ns := cfg.Namespace
	caEnforcePVCUID(c)
	want := caClaim(t, c, caDockerNS, dataPVCName("w1")).DeepCopy()
	for i := 0; i < 3 && caClaim(t, c, ns, dataPVCName("w1")) != nil; i++ {
		caMustTick(t, m, w)
	}
	if caClaim(t, c, ns, dataPVCName("w1")) != nil {
		t.Fatal("fixture: the own data claim was not deleted")
	}
	m = matrixRestart(m)
	w.DiskPressure = false
	for i := 0; i < 6; i++ {
		caMustTick(t, m, w)
	}
	if p := caClaim(t, c, ns, dataPVCName("w1")); p == nil || !caStamped(p) {
		t.Fatalf("replacement data claim = %+v, want stamped", p)
	}
	d := caDep(t, c, ns, "w1")
	if d == nil || caReplicas(d) != 1 || !caHasEnv(d) {
		t.Fatalf("deployment = %v, want replicas 1 with the env", d)
	}
	caSameClaim(t, c, caDockerNS, dataPVCName("w1"), want)
}

// (b2) The pressure arm's execution guard reads the own /data claim. Inside the cooldown
// the arm logs the fixed token disk-recycle-skipped-cooldown; an own-absent worker with a
// live foreign claim must not enter the arm at all, while an own-present one does.
func TestCodexCAOwnDataAbsentDoesNotEnterThePressureArm(t *testing.T) {
	const prefix = "disk-recycle-skipped-"
	t.Run("own-absent-live-foreign", func(t *testing.T) {
		m, c, w, cfg := caRetainedNoOwnData(t, true)
		// Precondition: the young /nix keeps the cooldown active, which is what holds a
		// pressure recycle back if the arm were entered.
		if !m.withinRecycleCooldown(w, caObserve(t, m, "w1"), cfg.Namespace) {
			t.Fatal("fixture: the recycle cooldown is not active")
		}
		var buf bytes.Buffer
		m.log = slog.New(slog.NewTextHandler(&buf, nil))
		c.ClearActions()
		caMustTick(t, m, w) // the tick that creates the own claim; it was absent when observed
		if strings.Contains(buf.String(), prefix) {
			t.Fatalf("pressure arm entered with no own /data claim; logs:\n%s", buf.String())
		}
		if got := caPVCDeletes(c); len(got) != 0 {
			t.Errorf("claims deleted: %v", got)
		}
		if n := replicaPatches(c); n != 0 {
			t.Errorf("replica patches = %d, want no scale-down", n)
		}
	})
	t.Run("own-present-control", func(t *testing.T) {
		m, c, w, cfg := caRetained(t, true, "", true, caForeignData("b2"))
		young := metav1.NewTime(m5Now)
		for _, name := range []string{dataPVCName("w1"), nixPVCName("w1")} {
			caSetClaim(t, c, cfg.Namespace, name, func(p *corev1.PersistentVolumeClaim) { p.CreationTimestamp = young })
		}
		var buf bytes.Buffer
		m.log = slog.New(slog.NewTextHandler(&buf, nil))
		caMustTick(t, m, w)
		if tok := prefix + "cooldown worker=w1"; !strings.Contains(buf.String(), tok) {
			t.Fatalf("control: logs missing %q; got:\n%s", tok, buf.String())
		}
	})
}

// A retained marked Deployment running at replicas 1 whose own /data claim is held
// Terminating (deleted externally, pvc-protection) under disk pressure still scales to 0
// first; only once the pods are gone does the recycle refuse, and nothing is deleted.
func TestCodexCARetainedRecycleScalesDownBeforeRefusingATerminatingOwnData(t *testing.T) {
	cfg := caCfg()
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
	dep := markedDeployment(t, cfg.withCodexCA(true), w, false)
	nix := pvcNix("w1", "20Gi")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: cfg.Namespace, UID: "p1", Labels: objectLabels("w1")}}
	m, c := caMat(t, cfg, append([]runtime.Object{dep, nix, pod}, caClaims("w1", true)[0])...)
	ns := cfg.Namespace
	if caReplicas(caDep(t, c, ns, "w1")) != 1 {
		t.Fatal("fixture: deployment not at replicas 1")
	}
	caTerminateClaim(t, c, ns, dataPVCName("w1"))
	recycle := func() error { return caTick(t, m, w) }
	if err := recycle(); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if got := caReplicas(caDep(t, c, ns, "w1")); got != 0 {
		t.Fatalf("replicas = %d after tick 1, want the scale-down to 0", got)
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted on tick 1: %v", got)
	}
	if err := c.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ns, pod.Name); err != nil {
		t.Fatal(err)
	}
	err := recycle()
	if err == nil || !strings.Contains(err.Error(), "recycle data claim evidence unavailable") {
		t.Fatalf("tick 2 err = %v, want the evidence refusal", err)
	}
	if got := caPVCDeletes(c); len(got) != 0 {
		t.Fatalf("claims deleted despite the refusal: %v", got)
	}
}
