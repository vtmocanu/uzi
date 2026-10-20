package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Run-workdir layout migration guard (issue #3153): a custody-held Docker worker whose
// pods still use the old emptyDir layout must never have a pod replaced by this
// controller, whatever ForceRoll or the busy state say.

const lmID = "d1"

func lmNS() string { return dockerTestConfig().DockerNamespace }

func lmWorker(custody bool) protocol.DesiredWorker {
	return evidence(protocol.DesiredWorker{ID: lmID, Template: "base", Size: "m", Docker: true, CustodyHeld: custody}, m5Now)
}

// lmPod renders a pod from the current template (the NEW layout) and, for "old",
// rewrites it into the pre-#3153 shape: run-workdir emptyDir, no runner-shared mount.
func lmPod(t *testing.T, name, layout string, terminating bool) *corev1.Pod {
	t.Helper()
	cfg := dockerTestConfig()
	w := protocol.DesiredWorker{ID: lmID, Template: "base", Size: "m", Docker: true}
	spec := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template.Spec.DeepCopy()
	if layout == "old" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: legacyRunWorkdirVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		for _, list := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
			for i := range list {
				for j, vm := range list[i].VolumeMounts {
					if vm.MountPath == dindWorkdirDir {
						list[i].VolumeMounts[j] = corev1.VolumeMount{Name: legacyRunWorkdirVolume, MountPath: dindWorkdirDir}
					}
				}
			}
		}
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lmNS(), UID: types.UID("uid-" + name), Labels: objectLabels(lmID)}, Spec: *spec}
	if terminating {
		ts := metav1.NewTime(m5Now)
		p.DeletionTimestamp = &ts
	}
	return p
}

type lmFixture struct {
	m      *Materializer
	client *fake.Clientset
	cord   *fakeCordoner
	logs   *strings.Builder
}

// lmSetup seeds a drifted Docker worker (stale spec hash) with all three volumes live
// and the pods the layout names. "unknown" fails every pod list.
func lmSetup(t *testing.T, layout string, force bool, nixSize string, extra ...runtime.Object) *lmFixture {
	t.Helper()
	ns := lmNS()
	nix := pvcNix(lmID, nixSize)
	nix.Namespace = ns
	objs := []runtime.Object{deployedWorkerNS(lmID, ns, 0, "the-old-releases-hash"), nix, pvcForNS(lmID, "data", ns), pvcForNS(lmID, "dind-data", ns)}
	switch layout {
	case "old":
		objs = append(objs, lmPod(t, "p-old", "old", false))
	case "mixed":
		objs = append(objs, lmPod(t, "p-new", "new", false), lmPod(t, "p-old", "old", false))
	case "new":
		objs = append(objs, lmPod(t, "p-new", "new", false))
	}
	objs = append(objs, extra...)
	client := enforcingClient(t, objs...)
	if layout == "unknown" {
		client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("pods unavailable")
		})
	}
	logs := &strings.Builder{}
	cord := &fakeCordoner{}
	m := New(client, dockerTestConfig(), testResolver(t), cord, DrainPolicy{Deadline: 24 * time.Hour, ForceRoll: force},
		RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(logs, nil)))
	m.now = func() time.Time { return m5Now }
	return &lmFixture{m: m, client: client, cord: cord, logs: logs}
}

func (f *lmFixture) tick(t *testing.T, w protocol.DesiredWorker) []reconcile.ObservedWorker {
	t.Helper()
	obs, err := f.m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Pod-list failure is logged, not fatal, so unknown still reconciles.
	if err := f.m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return obs
}

func TestObserveRecordsRunnerLayout(t *testing.T) {
	cases := []struct {
		name   string
		layout string
		extra  []runtime.Object
		want   reconcile.RunnerLayout
	}{
		{"no pods is none", "none", nil, reconcile.RunnerLayoutNone},
		{"new pod is new", "new", nil, reconcile.RunnerLayoutNew},
		{"old pod is old", "old", nil, reconcile.RunnerLayoutOld},
		{"mixed set is old", "mixed", nil, reconcile.RunnerLayoutOld},
		{"a terminating old pod beside a new one is old", "new", []runtime.Object{lmPod(t, "p-term", "old", true)}, reconcile.RunnerLayoutOld},
		{"pod list failure is unknown", "unknown", nil, reconcile.RunnerLayoutUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := lmSetup(t, c.layout, false, "20Gi", c.extra...)
			obs, err := f.m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(obs) != 1 || obs[0].RunnerLayout != c.want {
				t.Fatalf("observed layout = %+v, want %v", obs, c.want)
			}
		})
	}
	t.Run("new-layout mounts plus a leftover run-workdir volume is old", func(t *testing.T) {
		p := lmPod(t, "p-leftover", "new", false)
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: legacyRunWorkdirVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		f := lmSetup(t, "none", false, "20Gi", p)
		obs, _ := f.m.Observe(context.Background())
		if obs[0].RunnerLayout != reconcile.RunnerLayoutOld {
			t.Fatalf("layout = %v, want old", obs[0].RunnerLayout)
		}
	})
	t.Run("a pod missing the dind mount is old", func(t *testing.T) {
		p := lmPod(t, "p-half", "new", false)
		for i := range p.Spec.InitContainers {
			if p.Spec.InitContainers[i].Name == dindContainerName {
				p.Spec.InitContainers[i].VolumeMounts = nil
			}
		}
		f := lmSetup(t, "none", false, "20Gi", p)
		obs, _ := f.m.Observe(context.Background())
		if obs[0].RunnerLayout != reconcile.RunnerLayoutOld {
			t.Fatalf("layout = %v, want old", obs[0].RunnerLayout)
		}
	})
}

// The headline matrix. The expected outcome of a custody-held worker is derived from
// the SAME scenario run for a non-custody worker (positive control): the hold removes
// the roll for old, mixed and unknown layouts and changes nothing else.
func TestLayoutMigrationHoldMatrix(t *testing.T) {
	for _, layout := range []string{"old", "unknown", "mixed", "new", "none"} {
		for _, busy := range []bool{false, true} {
			for _, force := range []bool{false, true} {
				name := fmt.Sprintf("%s/busy=%v/force=%v", layout, busy, force)
				t.Run(name, func(t *testing.T) {
					control := lmSetup(t, layout, force, "20Gi")
					cw := lmWorker(false)
					cw.Busy = busy
					control.tick(t, cw)
					controlPatched := wasPatched(control.client)

					f := lmSetup(t, layout, force, "20Gi")
					w := lmWorker(true)
					w.Busy = busy
					f.tick(t, w)
					held := layout == "old" || layout == "unknown" || layout == "mixed"
					if want := controlPatched && !held; wasPatched(f.client) != want {
						t.Fatalf("patched = %v, want %v (non-custody control patched = %v)", wasPatched(f.client), want, controlPatched)
					}
					if held {
						if len(f.cord.calls) != 0 || len(f.cord.clearCalls) != 0 {
							t.Fatalf("a held worker's cordon state was touched: request=%v clear=%v", f.cord.calls, f.cord.clearCalls)
						}
						if del := deletedSet(f.client); len(del) != 0 {
							t.Fatalf("a held worker had objects deleted: %v", keysOf(del))
						}
					}
					if !held && !busy && !controlPatched {
						t.Fatal("positive control failed: an idle migrated or pod-less worker was not rolled")
					}
				})
			}
		}
	}
}

// Roll admission: the hold precedes the ForceRoll override, so a held worker gets the
// pacing state `held`, is no canary and holds no slot; a non-custody old-layout twin
// is admitted under the same ForceRoll.
func TestLayoutMigrationHeldWorkerIsNotAdmittedOrCanary(t *testing.T) {
	f := lmSetup(t, "old", true, "20Gi")
	f.tick(t, lmWorker(true))
	p := f.m.PacingOutcomes()[lmID]
	if p == nil || p.State != "held" || p.BlockingReason == nil || *p.BlockingReason != "layout_migration_custody" {
		t.Fatalf("pacing = %+v, want state held with reason layout_migration_custody", p)
	}
	if p.CanaryID != nil {
		t.Fatalf("a held worker became the canary: %v", *p.CanaryID)
	}
	if len(*p.BlockingReason) > 192 {
		t.Fatalf("blocking reason too long: %d", len(*p.BlockingReason))
	}

	twin := lmSetup(t, "old", true, "20Gi")
	twin.tick(t, lmWorker(false))
	if !wasPatched(twin.client) {
		t.Fatal("positive control: a non-custody old-layout worker must still roll under ForceRoll")
	}
}

// A held worker beside a rollable peer in one pool does not take the peer's slot: the
// held worker serves (Running pod), so the peer has coverage, is admitted and rolls.
func TestLayoutMigrationHeldWorkerTakesNoRollSlot(t *testing.T) {
	hObjs, h := lmObjects(t, lmOpts{id: "d1", custody: true, stale: true, layout: "old"})
	pObjs, peer := lmObjects(t, lmOpts{id: "d2", stale: true, layout: "new"})
	f := lmCluster(t, false, append(hObjs, pObjs...)...)
	f.tickAll(t, h, peer)
	po := f.m.PacingOutcomes()
	if po["d1"] == nil || po["d1"].State != "held" {
		t.Fatalf("held worker pacing = %+v", po["d1"])
	}
	if lmRolled(t, f, "d1") {
		t.Fatal("the held worker's deployment was patched")
	}
	if !lmRolled(t, f, "d2") {
		t.Fatalf("the peer was not rolled (pacing %+v)", po["d2"])
	}
	if po["d2"] != nil && po["d2"].CanaryID != nil && *po["d2"].CanaryID == "d1" {
		t.Fatal("the held worker was chosen canary")
	}
}

// With the pod list failing the roll snapshot fails too; under ForceRoll pacing must
// still report the worker held rather than overridden.
func TestLayoutMigrationHeldReportedWhenSnapshotFailsUnderForceRoll(t *testing.T) {
	f := lmSetup(t, "unknown", true, "20Gi")
	f.tick(t, lmWorker(true))
	p := f.m.PacingOutcomes()[lmID]
	if p == nil || p.State != "held" || p.BlockingReason == nil || *p.BlockingReason != layoutMigrationReason {
		t.Fatalf("pacing = %+v, want held/%s", p, layoutMigrationReason)
	}
	if wasPatched(f.client) {
		t.Fatal("rolled")
	}
}

// Observe and the roll snapshot read the pods at different instants; the snapshot's
// reading is the one admission uses.
func TestLayoutMigrationRollSnapshotRefreshesLayout(t *testing.T) {
	swap := func(t *testing.T, f *lmFixture, to string) {
		t.Helper()
		pods := f.client.CoreV1().Pods(lmNS())
		list, _ := pods.List(context.Background(), metav1.ListOptions{})
		for _, p := range list.Items {
			if err := pods.Delete(context.Background(), p.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pods.Create(context.Background(), lmPod(t, "p-swapped", to, false), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		observed, snapshot string
		wantPatched        bool
	}{{"old", "new", true}, {"new", "old", false}} {
		t.Run(c.observed+" then "+c.snapshot, func(t *testing.T) {
			f := lmSetup(t, c.observed, true, "20Gi")
			obs, err := f.m.Observe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			swap(t, f, c.snapshot)
			if err := f.m.Reconcile(context.Background(), []protocol.DesiredWorker{lmWorker(true)}, obs); err != nil {
				t.Fatal(err)
			}
			if wasPatched(f.client) != c.wantPatched {
				t.Fatalf("patched = %v, want %v", wasPatched(f.client), c.wantPatched)
			}
		})
	}
}

// B1: a held worker carrying its own incomplete reservation must not wedge its pool.
// The hold withdraws the reservation (metadata only); once custody releases, the
// worker is admitted afresh and rolls.
func TestLayoutMigrationHeldWorkerWithIncompleteReservationDoesNotWedgePool(t *testing.T) {
	for _, variant := range []string{"canary", "participant"} {
		t.Run(variant, func(t *testing.T) {
			hObjs, h := lmObjects(t, lmOpts{id: "d1", custody: true, stale: true, layout: "old", marker: variant})
			objs := hObjs
			workers := []protocol.DesiredWorker{h}
			if variant == "participant" {
				cObjs, canary := lmObjects(t, lmOpts{id: "d2", layout: "new", marker: "canary", promoted: true})
				objs, workers = append(objs, cObjs...), append(workers, canary)
			}
			f := lmCluster(t, false, objs...)
			for i := 0; i < 3; i++ {
				f.tickAll(t, workers...)
				if lmRolled(t, f, "d1") {
					t.Fatalf("tick %d: the held worker was rolled", i)
				}
			}
			if d, _ := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName("d1"), metav1.GetOptions{}); d.Annotations[annotationRoll] != "" {
				t.Fatal("the held worker kept its incomplete reservation")
			}
			workers[0].CustodyHeld = false
			for i := 0; i < 8 && !lmRolled(t, f, "d1"); i++ {
				f.tickAll(t, workers...)
			}
			if !lmRolled(t, f, "d1") {
				t.Fatalf("after custody released the worker never rolled; pacing %+v", f.m.PacingOutcomes()["d1"])
			}
		})
	}
}

// Ephemeral (run-bound) workers get no pacing entry; the drift arm itself must hold.
func TestLayoutMigrationHoldsEphemeralWorkerWithoutPacingEntry(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			f := lmSetup(t, "old", force, "20Gi")
			w := lmWorker(true)
			w.Ephemeral = true
			f.tick(t, w)
			if f.m.PacingOutcomes()[lmID] != nil {
				t.Fatal("an ephemeral worker unexpectedly has a pacing entry")
			}
			if wasPatched(f.client) {
				t.Fatal("an ephemeral custody-held old-layout worker was rolled")
			}
			if !strings.Contains(f.logs.String(), "layout-migration-held-custody worker="+lmID) {
				t.Fatalf("hold not logged: %s", f.logs.String())
			}
		})
	}
	t.Run("positive control: migrated ephemeral worker rolls under ForceRoll", func(t *testing.T) {
		f := lmSetup(t, "new", true, "20Gi")
		w := lmWorker(true)
		w.Ephemeral = true
		f.tick(t, w)
		if !wasPatched(f.client) {
			t.Fatal("a migrated custody-held worker must still roll")
		}
	})
}

// Direct drift-arm test: no patch, and no cordon or DrainingSince traffic even for a
// busy, already-draining worker.
func TestLayoutMigrationHeldDriftArmDoesNotPatchOrTouchDrainState(t *testing.T) {
	for _, layout := range []reconcile.RunnerLayout{reconcile.RunnerLayoutOld, reconcile.RunnerLayoutUnknown} {
		f := lmSetup(t, "none", true, "20Gi")
		since := m5Now.Add(-time.Minute)
		w := lmWorker(true)
		w.Busy, w.DrainingSince, w.DrainingReason = true, &since, token("image_roll")
		obs := reconcile.ObservedWorker{ID: lmID, HasDeployment: true, DeploymentUID: "uid-" + deploymentName(lmID), DeploymentResourceVersion: "1", SpecHash: "the-old-releases-hash", Namespace: lmNS(), RunnerLayout: layout}
		if err := f.m.reconcileWorker(context.Background(), w, obs); err != nil {
			t.Fatal(err)
		}
		if wasPatched(f.client) || len(f.cord.calls) != 0 || len(f.cord.clearCalls) != 0 {
			t.Fatalf("layout %v: patched=%v cordon=%v clear=%v", layout, wasPatched(f.client), f.cord.calls, f.cord.clearCalls)
		}
		if w.DrainingSince == nil || !w.DrainingSince.Equal(since) {
			t.Fatal("DrainingSince changed")
		}
	}
}

// Paused-restore arm: with the pod list KNOWN empty the retained zero-replica
// Deployment is restored; with the pod list unknown it is held.
func TestLayoutMigrationPausedRestore(t *testing.T) {
	ns := lmNS()
	w := lmWorker(true)
	dep := markedDeployment(t, dockerTestConfig(), w, false)
	dep.Namespace = ns
	zero := int32(0)
	dep.Spec.Replicas = &zero
	nix := pvcNix(lmID, "20Gi")
	nix.Namespace = ns
	build := func(unknown bool) (*lmFixture, []reconcile.ObservedWorker) {
		client := enforcingClient(t, dep.DeepCopy(), nix.DeepCopy(), pvcForNS(lmID, "data", ns), pvcForNS(lmID, "dind-data", ns))
		if unknown {
			client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("pods unavailable")
			})
		}
		cord := &fakeCordoner{}
		m := New(client, dockerTestConfig(), testResolver(t), cord, DrainPolicy{Deadline: 24 * time.Hour},
			RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		m.now = func() time.Time { return m5Now }
		f := &lmFixture{m: m, client: client, cord: cord}
		obs, err := m.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return f, obs
	}
	replicas := func(f *lmFixture) int32 {
		d, err := f.client.AppsV1().Deployments(ns).Get(context.Background(), deploymentName(lmID), metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			t.Fatalf("get deployment: %v", err)
		}
		return *d.Spec.Replicas
	}
	t.Run("known-empty pod list restores", func(t *testing.T) {
		f, obs := build(false)
		if err := f.m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, obs); err != nil {
			t.Fatal(err)
		}
		if replicas(f) != 1 {
			t.Fatal("a paused worker with known-empty pods was not restored")
		}
	})
	t.Run("unknown pod list holds at the drift arm", func(t *testing.T) {
		f, obs := build(true)
		if obs[0].RunnerLayout != reconcile.RunnerLayoutUnknown {
			t.Fatalf("layout = %v", obs[0].RunnerLayout)
		}
		// Reach the arm itself: a tick carrying the retained deployment and no pods.
		e := &rollEntry{w: w, dep: dep.DeepCopy(), target: RenderDeployment(dockerTestConfig(), w, testSpec(t, "base", "m"))}
		ctx := context.WithValue(context.Background(), rollTickKey{}, &rollTick{entries: map[string]*rollEntry{lmID: e}})
		o := obs[0]
		o.HasNixPVC, o.NixPVCSize = true, ptrQuantity("20Gi")
		o.HasDataPVC, o.DataPVCLive, o.HasDinDDataPVC, o.DinDPVCLive = true, true, true, true
		o.DeploymentUID, o.DeploymentResourceVersion = string(dep.UID), dep.ResourceVersion
		o.SpecHash, o.Generation = dep.Spec.Template.Annotations[AnnotationSpecHash], w.Generation
		if err := f.m.reconcileWorker(ctx, w, o); err != nil {
			t.Fatal(err)
		}
		if replicas(f) != 0 || wasPatched(f.client) {
			t.Fatal("a paused custody-held worker with an unknown pod list was restored")
		}
		o.RunnerLayout = reconcile.RunnerLayoutNone
		if err := f.m.reconcileWorker(ctx, w, o); err != nil {
			t.Fatal(err)
		}
		if replicas(f) != 1 {
			t.Fatal("positive control: known-empty pods must restore")
		}
	})
}

func ptrQuantity(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

// /nix size-drift recycle scales to zero or deletes the Deployment: held for an
// old-layout custody-held worker, performed for migrated and non-custody ones.
func TestLayoutMigrationHoldsNixRecycle(t *testing.T) {
	run := func(t *testing.T, layout string, custody, force bool) *lmFixture {
		f := lmSetup(t, layout, force, "10Gi")
		w := lmWorker(custody)
		w.Busy = true
		f.tick(t, w)
		return f
	}
	for _, layout := range []string{"old", "unknown", "mixed"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("held/%s/force=%v", layout, force), func(t *testing.T) {
				f := run(t, layout, true, force)
				if del := deletedSet(f.client); len(del) != 0 {
					t.Fatalf("deletes = %v", keysOf(del))
				}
				for _, a := range f.client.Actions() {
					if a.GetVerb() == "patch" || a.GetVerb() == "update" {
						t.Fatalf("held worker saw write %s %s", a.GetVerb(), a.GetResource().Resource)
					}
				}
				if len(f.cord.calls) != 0 {
					t.Fatalf("cordon requested: %v", f.cord.calls)
				}
			})
		}
	}
	// Direct call, as the disk-pressure caller would make it.
	t.Run("recycleWorkerVolumes itself refuses", func(t *testing.T) {
		f := lmSetup(t, "none", true, "10Gi")
		obs, _ := f.m.Observe(context.Background())
		o := obs[0]
		o.RunnerLayout = reconcile.RunnerLayoutOld
		o.DeploymentUID, o.DeploymentResourceVersion = "uid-"+deploymentName(lmID), "1"
		if err := f.m.recycleWorkerVolumes(context.Background(), lmWorker(true), o, lmNS(), recycleVolumes{Nix: true, Data: true, Pressure: true}); err != nil {
			t.Fatal(err)
		}
		if del := deletedSet(f.client); len(del) != 0 {
			t.Fatalf("deletes = %v", keysOf(del))
		}
	})
	for _, c := range []struct {
		name    string
		layout  string
		custody bool
	}{{"migrated custody-held worker recycles", "new", true}, {"non-custody old-layout worker recycles", "old", false}, {"pod-less custody-held worker recycles", "none", true}} {
		t.Run("positive control: "+c.name, func(t *testing.T) {
			f := run(t, c.layout, c.custody, true)
			if !deletedSet(f.client)["deployments/"+deploymentName(lmID)] && !deletedSet(f.client)["persistentvolumeclaims/"+nixPVCName(lmID)] {
				t.Fatalf("no recycle happened: %v", keysOf(deletedSet(f.client)))
			}
		})
	}
}

// reconcileReplacement (the fenced DinD rebuild path) passes through the same guarded
// drift arm.
func TestLayoutMigrationReconcileReplacementIsGuarded(t *testing.T) {
	for _, force := range []bool{false, true} {
		f := lmSetup(t, "old", force, "20Gi")
		obs, err := f.m.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.m.reconcileReplacement(context.Background(), lmWorker(true), obs[0]); err != nil {
			t.Fatal(err)
		}
		if wasPatched(f.client) {
			t.Fatalf("force=%v: reconcileReplacement rolled a held worker", force)
		}
	}
	f := lmSetup(t, "new", true, "20Gi")
	obs, _ := f.m.Observe(context.Background())
	if err := f.m.reconcileReplacement(context.Background(), lmWorker(true), obs[0]); err != nil {
		t.Fatal(err)
	}
	if !wasPatched(f.client) {
		t.Fatal("positive control: a migrated worker's replacement must proceed")
	}
}

func TestLayoutMigrationReserveMaintenanceRollSkipsHeld(t *testing.T) {
	f := lmSetup(t, "none", true, "20Gi")
	held := &rollEntry{w: lmWorker(true), drift: true, o: reconcile.ObservedWorker{RunnerLayout: reconcile.RunnerLayoutOld}}
	if err := f.m.reserveMaintenanceRoll(held); err == nil {
		t.Fatal("a held worker reserved a maintenance roll")
	}
	free := &rollEntry{w: lmWorker(true), drift: true, o: reconcile.ObservedWorker{RunnerLayout: reconcile.RunnerLayoutNew}}
	if err := f.m.reserveMaintenanceRoll(free); err != nil {
		t.Fatalf("positive control: %v", err)
	}
}

func TestLayoutMigrationLogsHeldCountEachTick(t *testing.T) {
	f := lmSetup(t, "old", false, "20Gi")
	f.tick(t, lmWorker(true))
	if !strings.Contains(f.logs.String(), "layout-migration-held-custody count=1 workers="+lmID) {
		t.Fatalf("missing count line: %s", f.logs.String())
	}
	g := lmSetup(t, "new", false, "20Gi")
	g.tick(t, lmWorker(true))
	if strings.Contains(g.logs.String(), "layout-migration-held-custody") {
		t.Fatalf("count line logged with nothing held: %s", g.logs.String())
	}
}

// Ordinary template drift of a Docker custody-held worker: the migrated pod rolls, its
// old-layout twin is held.
func TestDockerCustodyDriftRollMigratedRollsOldLayoutHeld(t *testing.T) {
	migrated := lmSetup(t, "new", false, "20Gi")
	migrated.tick(t, lmWorker(true))
	if !wasPatched(migrated.client) {
		t.Fatal("a migrated custody-held worker must roll its pod")
	}
	old := lmSetup(t, "old", false, "20Gi")
	old.tick(t, lmWorker(true))
	if wasPatched(old.client) {
		t.Fatal("an old-layout custody-held worker was rolled")
	}
}

type lmOpts struct {
	id       string
	custody  bool
	stale    bool   // deployed template carries a stale spec hash (drift)
	layout   string // pod layout: "old" or "new"
	marker   string // "", "canary" or "participant": an incomplete reservation
	promoted bool   // the pod carries current-incarnation registration proof
}

const lmStaleHash = "the-old-releases-hash"

// lmObjects renders a Docker worker's Deployment (with a real container template so
// serving proof can hold), its volumes, ReplicaSet and one Running pod.
func lmObjects(t *testing.T, o lmOpts) ([]runtime.Object, protocol.DesiredWorker) {
	t.Helper()
	cfg, ns := dockerTestConfig(), lmNS()
	w := evidence(protocol.DesiredWorker{ID: o.id, Template: "base", Size: "m", Docker: true, CustodyHeld: o.custody}, m5Now)
	dep := RenderDeployment(cfg, w, testSpec(t, "base", "m"))
	dep.Namespace, dep.UID, dep.ResourceVersion = ns, types.UID("uid-"+dep.Name), "1"
	if o.marker != "" {
		dep = markedDeployment(t, cfg, w, false)
		dep.Namespace, dep.UID, dep.ResourceVersion = ns, types.UID("uid-"+dep.Name), "1"
		if o.marker == "participant" {
			m, err := decodeRoll(dep.Annotations[annotationRoll])
			if err != nil {
				t.Fatal(err)
			}
			m.Canary = false
			raw, _ := json.Marshal(m)
			dep.Annotations[annotationRoll] = string(raw)
		}
	}
	if o.stale {
		dep.Spec.Template.Annotations[AnnotationSpecHash] = lmStaleHash
	}
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs-" + o.id, Namespace: ns, UID: types.UID("rs-uid-" + o.id),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &yes}}}}
	spec := dep.Spec.Template.Spec.DeepCopy()
	if o.layout == "old" {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: legacyRunWorkdirVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		for _, list := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
			for i := range list {
				for j, vm := range list[i].VolumeMounts {
					if vm.MountPath == dindWorkdirDir {
						list[i].VolumeMounts[j] = corev1.VolumeMount{Name: legacyRunWorkdirVolume, MountPath: dindWorkdirDir}
					}
				}
			}
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + o.id, Namespace: ns, UID: types.UID("pod-uid-" + o.id), Labels: objectLabels(o.id),
		Annotations:     map[string]string{AnnotationSpecHash: dep.Spec.Template.Annotations[AnnotationSpecHash]},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &yes}}},
		Spec: *spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: workerContainerName, Image: workerImage(dep), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if o.promoted {
		version, nonce, uid := "1.0.0", "new-"+o.id, string(pod.UID)
		w.RollEvidence.RegisteredVersion, w.RollEvidence.RegisterNonce, w.RollEvidence.PodUID = &version, &nonce, &uid
		at := m5Now
		w.RollEvidence.HeartbeatAt = &at
	}
	nix := pvcNix(o.id, "20Gi")
	nix.Namespace = ns
	return []runtime.Object{dep, rs, pod, nix, pvcForNS(o.id, "data", ns), pvcForNS(o.id, "dind-data", ns)}, w
}

func lmCluster(t *testing.T, force bool, objs ...runtime.Object) *lmFixture {
	t.Helper()
	client := enforcingClient(t, objs...)
	logs := &strings.Builder{}
	cord := &fakeCordoner{}
	m := New(client, dockerTestConfig(), testResolver(t), cord, DrainPolicy{Deadline: 24 * time.Hour, ForceRoll: force},
		RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(logs, nil)))
	m.now = func() time.Time { return m5Now }
	return &lmFixture{m: m, client: client, cord: cord, logs: logs}
}

func (f *lmFixture) tickAll(t *testing.T, ws ...protocol.DesiredWorker) {
	t.Helper()
	obs, err := f.m.Observe(context.Background())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := f.m.Reconcile(context.Background(), ws, obs); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// lmRolled reports that the worker's deployed template no longer carries the stale hash.
func lmRolled(t *testing.T, f *lmFixture, id string) bool {
	t.Helper()
	d, err := f.client.AppsV1().Deployments(lmNS()).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return d.Spec.Template.Annotations[AnnotationSpecHash] != lmStaleHash
}
