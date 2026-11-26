package kube

import (
	"context"
	"reflect"
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

func caDeleteClaim(t *testing.T, c *fake.Clientset, ns, name string) {
	t.Helper()
	if err := c.Tracker().Delete(pvcRes, ns, name); err != nil {
		t.Fatal(err)
	}
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
// Terminating no Deployment is created although a foreign claim is live.
func TestCodexCADeploymentGuardIgnoresAForeignDataClaim(t *testing.T) {
	cfg := caCfg()
	cfg.DockerNamespace = caDockerNS
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}, m5Now)
	w.JoinToken = token("uzw_w1")
	own := pvcFor("w1", "data")
	m, c := caMat(t, cfg, own, pvcNix("w1", "20Gi"), caForeignData("d"))
	ns := cfg.Namespace
	caTerminateClaim(t, c, ns, dataPVCName("w1"))
	for i := 0; i < 3; i++ {
		caMustTick(t, m, w)
	}
	if d := caDep(t, c, ns, "w1"); d != nil {
		t.Fatalf("deployment %v created over a Terminating own data claim", d.Name)
	}
}

// (e) New placement: a Terminating foreign /data claim, scanned last, does not hold back
// the create of the own /data claim.
func TestCodexCAForeignTerminatingDataClaimDoesNotDeferTheOwnCreate(t *testing.T) {
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
	now := metav1.NewTime(m5Now)
	foreign.DeletionTimestamp = &now
	m, c := caMat(t, cfg, dep, nix, foreign)
	obs := caObserve(t, m, "w1")
	if obs.DataClaim == nil || obs.DataClaim.Namespace != caDockerNS || !obs.DataClaim.Terminating {
		t.Fatalf("fixture: DataClaim = %+v, want the Terminating foreign claim", obs.DataClaim)
	}
	caMustTick(t, m, w)
	if caClaim(t, c, cfg.Namespace, dataPVCName("w1")) == nil {
		t.Fatal("the own data claim was not created")
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
	obs := caObserve(t, m, "w1")
	err := m.recycleWorkerVolumes(context.Background(), w, obs, cfg.Namespace, recycleVolumes{Nix: true, Data: true, Pressure: true})
	if err != nil {
		t.Fatalf("recycle: %v", err)
	}
	for _, name := range caPVCDeletes(c) {
		if name == dataPVCName("w1") {
			t.Fatal("a data claim was deleted")
		}
	}
	if n := caRecordPatches(c); n != 0 {
		t.Fatalf("record patches = %d, want 0", n)
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

// (i) Compatibility: a stamped own /data claim recycled while a live foreign claim exists
// keeps the Codex CA across a restart between the delete and the replacement create.
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
