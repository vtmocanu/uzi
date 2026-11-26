package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// Reconcile-level tests for the per-worker placement stamp (#4451): a PVC created while
// newPlacement is set carries uzi.dev/placement=new, and only a worker whose PVCs all
// carry it renders the new placement.

func globalOnly(cfg RenderConfig) RenderConfig {
	cfg.NewPlacementNodeSelector, cfg.NewPlacementTolerations = nil, nil
	return cfg
}

func npMatFor(t *testing.T, cfg RenderConfig, c Cordoner, drain DrainPolicy, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	client := enforcingClient(t, objs...)
	m := New(client, cfg, testResolver(t), c, drain, RecyclePolicy{Enabled: true, Cooldown: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return m5Now }
	return m, client
}

func npMat(t *testing.T, cfg RenderConfig, objs ...runtime.Object) (*Materializer, *fake.Clientset) {
	t.Helper()
	return npMatFor(t, cfg, nil, DrainPolicy{}, objs...)
}

func npTick(t *testing.T, m *Materializer, ws ...protocol.DesiredWorker) error {
	t.Helper()
	ctx := context.Background()
	o, err := m.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return m.Reconcile(ctx, ws, o)
}

// livePVCs renders the worker's claims as they would exist after creation, stamped or
// not, so size drift and expansion stay out of the way.
func livePVCs(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, stamped bool) []runtime.Object {
	t.Helper()
	var out []runtime.Object
	for _, p := range RenderPVCs(cfg, w, testSpec(t, w.Template, w.Size)) {
		if stamped {
			p.Annotations = map[string]string{AnnotationPlacement: placementNew}
		}
		out = append(out, p)
	}
	return out
}

func npPVC(t *testing.T, c *fake.Clientset, ns, name string) *corev1.PersistentVolumeClaim {
	t.Helper()
	p, err := c.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pvc %s: %v", name, err)
	}
	return p
}

func npDep(t *testing.T, c *fake.Clientset, ns, id string) *appsv1.Deployment {
	t.Helper()
	d, err := c.AppsV1().Deployments(ns).Get(context.Background(), deploymentName(id), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment %s: %v", id, err)
	}
	return d
}

func isStamped(p *corev1.PersistentVolumeClaim) bool {
	return p.Annotations[AnnotationPlacement] == placementNew
}

// writes lists every mutating action that reached the fake apiserver.
func writes(c *fake.Clientset) []string {
	var out []string
	for _, a := range c.Actions() {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete":
			out = append(out, a.GetVerb()+" "+a.GetResource().Resource)
		}
	}
	return out
}

// createdPVCs returns the claims a reconcile asked the apiserver to create, by name.
func createdPVCs(c *fake.Clientset) map[string]*corev1.PersistentVolumeClaim {
	out := map[string]*corev1.PersistentVolumeClaim{}
	for _, a := range c.Actions() {
		if a.GetResource().Resource == "persistentvolumeclaims" && a.GetVerb() == "create" {
			p := a.(k8stesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
			out[p.Name] = p
		}
	}
	return out
}

// podPlacement classifies a pod template: "new", "global", or the raw selector.
func podPlacement(d *appsv1.Deployment) string {
	switch {
	case maps.Equal(d.Spec.Template.Spec.NodeSelector, map[string]string{"pool": "new"}):
		return "new"
	case maps.Equal(d.Spec.Template.Spec.NodeSelector, map[string]string{"kubernetes.io/arch": "amd64"}):
		return "global"
	}
	return "other"
}

func depHash(d *appsv1.Deployment) string { return d.Spec.Template.Annotations[AnnotationSpecHash] }

// liveDeployment is a converged Deployment rendered under the given placement.
func liveDeployment(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, stamped bool) *appsv1.Deployment {
	t.Helper()
	return RenderDeployment(cfg.withPlacement(stamped), w, testSpec(t, w.Template, w.Size))
}

func hashFor(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, stamped bool) string {
	t.Helper()
	return SpecHashOf(cfg.withPlacement(stamped), w, testSpec(t, w.Template, w.Size))
}

// AC1: newPlacement unset changes nothing: no stamp on new claims, the same spec hash,
// and an existing stamp is neither removed nor acted on.
func TestNewPlacementAC1UnsetIsIdentical(t *testing.T) {
	cfg := globalOnly(placementConfig())
	ns := cfg.Namespace
	w := newWorkerFixture(desired("w1"))
	m, client := npMat(t, cfg)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatal(err)
	}
	created := createdPVCs(client)
	if len(created) != 2 {
		t.Fatalf("created pvcs = %v, want data and nix", created)
	}
	for name := range created {
		if isStamped(npPVC(t, client, ns, name)) {
			t.Errorf("pvc %s stamped with newPlacement unset", name)
		}
	}
	d := npDep(t, client, ns, "w1")
	if got, want := depHash(d), SpecHashOf(cfg, w, testSpec(t, "base", "m")); got != want {
		t.Errorf("deployment hash %s != render without the new fields %s", got, want)
	}

	// An already-stamped worker keeps its stamps, and nothing is written.
	w2 := evidence(desired("w2"), m5Now)
	m2, c2 := npMat(t, cfg, append(livePVCs(t, cfg, w2, true), liveDeployment(t, cfg, w2, false))...)
	if err := npTick(t, m2, w2); err != nil {
		t.Fatal(err)
	}
	if got := writes(c2); len(got) != 0 {
		t.Errorf("unset newPlacement wrote %v for a converged stamped worker", got)
	}
	for _, kind := range []string{dataPVCName("w2"), nixPVCName("w2")} {
		if !isStamped(npPVC(t, c2, ns, kind)) {
			t.Errorf("stamp on %s removed", kind)
		}
	}
}

// AC2: a legacy worker (no stamp anywhere) keeps its claims and its Deployment untouched
// across ticks while newPlacement is set.
func TestNewPlacementAC2LegacyWorkerIsUntouched(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := evidence(desired("w1"), m5Now)
	live := liveDeployment(t, cfg, w, false)
	m, client := npMat(t, cfg, append(livePVCs(t, cfg, w, false), live)...)
	if got, want := depHash(live), SpecHashOf(globalOnly(cfg), w, testSpec(t, "base", "m")); got != want {
		t.Fatalf("legacy render %s != pre-feature render %s", got, want)
	}
	for i := 0; i < 2; i++ {
		client.ClearActions()
		if err := npTick(t, m, w); err != nil {
			t.Fatal(err)
		}
		if got := writes(client); len(got) != 0 {
			t.Fatalf("tick %d wrote %v for a legacy worker", i+1, got)
		}
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("legacy pvc %s gained a stamp", n)
		}
	}
}

// AC3: workers with no claims (plain, Docker, ephemeral) get every claim stamped and render
// the new placement, in the same Reconcile.
func TestNewPlacementAC3NoClaimsWorkersAreStamped(t *testing.T) {
	plain := newWorkerFixture(desired("p1"))
	docker := newWorkerFixture(desiredDocker("d1"))
	eph := newWorkerFixture(desired("e1"))
	eph.Ephemeral = true
	cfg := placementConfig()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	m, client := npMat(t, cfg)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{plain, docker, eph}, nil); err != nil {
		t.Fatal(err)
	}
	created := createdPVCs(client)
	if len(created) != 7 {
		t.Fatalf("created pvcs = %d, want plain 2 + docker 3 + ephemeral 2", len(created))
	}
	for _, claim := range []struct{ ns, name string }{
		{cfg.Namespace, dataPVCName("p1")},
		{cfg.Namespace, nixPVCName("p1")},
		{cfg.DockerNamespace, dataPVCName("d1")},
		{cfg.DockerNamespace, nixPVCName("d1")},
		{cfg.DockerNamespace, dindDataPVCName("d1")},
		{cfg.Namespace, dataPVCName("e1")},
		{cfg.Namespace, nixPVCName("e1")},
	} {
		p := npPVC(t, client, claim.ns, claim.name)
		if p.Namespace != claim.ns || p.Name != claim.name || !isStamped(p) {
			t.Errorf("claim %s/%s = %s/%s, stamp=%q", claim.ns, claim.name, p.Namespace, p.Name, p.Annotations[AnnotationPlacement])
		}
	}
	for _, c := range []struct{ id, ns string }{{"p1", cfg.Namespace}, {"d1", cfg.DockerNamespace}, {"e1", cfg.Namespace}} {
		d := npDep(t, client, c.ns, c.id)
		if got := podPlacement(d); got != "new" {
			t.Errorf("%s renders %s placement", c.id, got)
		}
		if got := d.Spec.Template.Spec.Tolerations; len(got) != 1 || got[0].Value != "new" {
			t.Errorf("%s tolerations = %v", c.id, got)
		}
	}
}

// AC4: a legacy worker that lost one claim gets it back unstamped and stays global.
func TestNewPlacementAC4LegacyMissingClaimRecreatedUnstamped(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := evidence(desired("w1"), m5Now)
	live := liveDeployment(t, cfg, w, false)
	objs := []runtime.Object{live}
	for _, o := range livePVCs(t, cfg, w, false) {
		if o.(*corev1.PersistentVolumeClaim).Name == dataPVCName("w1") {
			objs = append(objs, o)
		}
	}
	m, client := npMat(t, cfg, objs...)
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if isStamped(npPVC(t, client, ns, nixPVCName("w1"))) {
		t.Error("recreated nix claim of a legacy worker is stamped")
	}
	if isStamped(npPVC(t, client, ns, dataPVCName("w1"))) {
		t.Error("existing data claim gained a stamp")
	}
	if got := podPlacement(npDep(t, client, ns, "w1")); got != "global" {
		t.Errorf("legacy worker placement = %s", got)
	}
}

// AC5: staggered deletion of a legacy worker. While one claim is Terminating nothing is
// created; once both are gone every claim is stamped and the worker renders new.
func TestNewPlacementAC5StaggeredDeletionOfLegacyWorker(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := newWorkerFixture(desired("w1"))
	m, client := npMat(t, cfg, pvcTerminating("w1", "data"))
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if got := writes(client); len(got) != 0 {
		t.Fatalf("a Terminating unstamped claim must hold every create; wrote %v", got)
	}
	// The remaining claim finishes deleting.
	if err := client.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), dataPVCName("w1"), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("%s not stamped after both claims were gone", n)
		}
	}
	if got := podPlacement(npDep(t, client, ns, "w1")); got != "new" {
		t.Errorf("placement after full reprovision = %s", got)
	}
}

// AC6: a create failing mid-loop leaves the first claim stamped; the next tick finishes the
// set stamped and renders the new placement.
func TestNewPlacementAC6CreateFailureMidLoop(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := newWorkerFixture(desired("w1"))
	m, client := npMat(t, cfg)
	calls := 0
	client.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 2 {
			return true, nil, errors.New("quota exceeded")
		}
		return false, nil, nil
	})
	if err := npTick(t, m, w); err == nil {
		t.Fatal("tick 1 returned no error")
	}
	if len(createdPVCs(client)) != 2 {
		t.Fatalf("tick 1 create attempts = %v", createdPVCs(client))
	}
	var first string
	pvcs, _ := client.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if len(pvcs.Items) != 1 || !isStamped(&pvcs.Items[0]) {
		t.Fatalf("after the failure want exactly one stamped claim, got %+v", pvcs.Items)
	}
	first = pvcs.Items[0].Name
	if deps, _ := client.AppsV1().Deployments(ns).List(context.Background(), metav1.ListOptions{}); len(deps.Items) != 0 {
		t.Fatal("deployment created despite the PVC failure")
	}
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("%s not stamped (first created: %s)", n, first)
		}
	}
	if got := podPlacement(npDep(t, client, ns, "w1")); got != "new" {
		t.Errorf("placement = %s", got)
	}
}

// AC7a: a Nix-only size recycle of a legacy worker re-mints the nix claim unstamped, so the
// worker stays on the global placement.
func TestNewPlacementAC7NixOnlySizeRecycleStaysLegacy(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m"}
	hash := hashFor(t, cfg, w, false)
	m, client := npMat(t, cfg, deployedWorker("w1", 0, hash), pvcNix("w1", "4Gi"), pvcFor("w1", "data"))
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if !deletedSet(client)["persistentvolumeclaims/uzi-hw-w1-nix"] {
		t.Fatalf("tick 1 did not recycle the nix claim: %v", keysOf(deletedSet(client)))
	}
	w = evidence(w, m5Now)
	retainedReplacementFixture(t, client, w)
	client.ClearActions()
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	if got := createdPVCs(client); len(got) != 1 || got[nixPVCName("w1")] == nil {
		t.Fatalf("tick 2 created %v, want only the nix claim", got)
	}
	if isStamped(npPVC(t, client, ns, nixPVCName("w1"))) || isStamped(npPVC(t, client, ns, dataPVCName("w1"))) {
		t.Error("a legacy worker's claims are stamped after a nix-only recycle")
	}
	if got := podPlacement(npDep(t, client, ns, "w1")); got != "global" {
		t.Errorf("placement = %s, want global", got)
	}
}

// AC7b / retained reservation: a plain data+Nix pressure recycle moves the worker. Both
// claims return stamped, and over the following ticks the retained zero-replica Deployment
// (carrying the roll reservation marker) reaches the new placement at replicas 1.
func TestNewPlacementAC7PressureRecycleMovesToNewPlacement(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w := evidence(protocol.DesiredWorker{ID: "w1", Template: "base", Size: "m", DiskPressure: true}, m5Now)
	dep := markedDeployment(t, cfg, w, true)
	zero := int32(0)
	dep.Spec.Replicas = &zero
	m, client := npMatFor(t, cfg, &fakeCordoner{}, DrainPolicy{Deadline: 24 * time.Hour}, dep, pvcNix("w1", "20Gi"), pvcFor("w1", "data"))
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	del := deletedSet(client)
	if !del["persistentvolumeclaims/uzi-hw-w1-nix"] || !del["persistentvolumeclaims/uzi-hw-w1-data"] {
		t.Fatalf("pressure recycle deletes = %v", keysOf(del))
	}
	w.DiskPressure = false
	var reached bool
	for i := 0; i < 8 && !reached; i++ {
		if err := npTick(t, m, w); err != nil {
			t.Fatalf("tick %d: %v", i+2, err)
		}
		d := npDep(t, client, ns, "w1")
		reached = podPlacement(d) == "new" && d.Spec.Replicas != nil && *d.Spec.Replicas == 1
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("%s re-minted unstamped", n)
		}
	}
	if !reached {
		d := npDep(t, client, ns, "w1")
		t.Fatalf("deployment never reached new placement at replicas 1: placement=%s replicas=%v", podPlacement(d), d.Spec.Replicas)
	}
}

// AC9: the controller restarts after the node-pool move procedure (Deployment scaled to zero,
// claims deleted). With the roll evidence the api reports for such a worker and a pool of
// peers, the claims come back stamped and the Deployment is re-rendered on the new placement
// at replicas 1.
func TestNewPlacementAC9RestartAfterProcedure(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w1 := evidence(desired("w1"), m5Now)
	zero := int32(0)
	d1 := liveDeployment(t, cfg, w1, false)
	d1.Spec.Replicas = &zero
	d1.UID = "dep-w1"
	objs := []runtime.Object{d1}
	workers := []protocol.DesiredWorker{w1}
	for _, id := range []string{"w2", "w3"} {
		w := evidence(desired(id), m5Now)
		workers = append(workers, w)
		objs = append(objs, liveDeployment(t, cfg, w, false))
		objs = append(objs, livePVCs(t, cfg, w, false)...)
	}
	m, client := npMatFor(t, cfg, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objs...)
	// The peers run a current, registered pod: the capacity a roll of w1 may lean on.
	for i := 1; i < len(workers); i++ {
		currentProof(t, client, &workers[i], m5Now)
	}
	var d *appsv1.Deployment
	for i := 0; i < 8; i++ {
		if err := npTick(t, m, workers...); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		d = npDep(t, client, ns, "w1")
		if podPlacement(d) == "new" && d.Spec.Replicas != nil && *d.Spec.Replicas == 1 {
			break
		}
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("%s not stamped", n)
		}
	}
	if podPlacement(d) != "new" || d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("w1 placement=%s replicas=%v; pacing=%+v", podPlacement(d), d.Spec.Replicas, m.PacingOutcomes()["w1"])
	}
	for _, id := range []string{"w2", "w3"} {
		if got := podPlacement(npDep(t, client, ns, id)); got != "global" {
			t.Errorf("%s moved to %s", id, got)
		}
	}
}

// AC9 variant: the operator left w1 manually cordoned (Deployment at zero, claims deleted).
// Manual cordon is not an automatic drain, so the cordon fields stay untouched and no drain
// is cleared; the claims return stamped and the Deployment is re-rendered at replicas 1.
func TestNewPlacementAC9RestartWhileManuallyCordoned(t *testing.T) {
	cfg := placementConfig()
	ns := cfg.Namespace
	w1 := evidence(desired("w1"), m5Now)
	manualWorker(&w1)
	zero := int32(0)
	d1 := liveDeployment(t, cfg, w1, false)
	d1.Spec.Replicas = &zero
	d1.UID = "dep-w1"
	objs := []runtime.Object{d1}
	workers := []protocol.DesiredWorker{w1}
	for _, id := range []string{"w2", "w3"} {
		w := evidence(desired(id), m5Now)
		workers = append(workers, w)
		objs = append(objs, liveDeployment(t, cfg, w, false))
		objs = append(objs, livePVCs(t, cfg, w, false)...)
	}
	cord := &fakeCordoner{}
	m, client := npMatFor(t, cfg, cord, DrainPolicy{Deadline: time.Hour}, objs...)
	for i := 1; i < len(workers); i++ {
		currentProof(t, client, &workers[i], m5Now)
	}
	var d *appsv1.Deployment
	for i := 0; i < 8; i++ {
		if err := npTick(t, m, workers...); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		d = npDep(t, client, ns, "w1")
		if podPlacement(d) == "new" && d.Spec.Replicas != nil && *d.Spec.Replicas == 1 {
			break
		}
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, ns, n)) {
			t.Errorf("%s not stamped", n)
		}
	}
	if podPlacement(d) != "new" || d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Fatalf("w1 placement=%s replicas=%v; pacing=%+v", podPlacement(d), d.Spec.Replicas, m.PacingOutcomes()["w1"])
	}
	if len(cord.clearCalls) != 0 {
		t.Errorf("manual cordon: ClearDrain called for %v", cord.clearCalls)
	}
	if w1.ManualCordonedAt == nil || !w1.ManualCordonedAt.Equal(m5Now) || w1.EffectiveCordoned == nil || !*w1.EffectiveCordoned {
		t.Errorf("cordon fields changed on the desired worker: %+v %v", w1.ManualCordonedAt, w1.EffectiveCordoned)
	}
}

// A migrated worker still needs the ordinary roll coverage. A manually cordoned
// sole provider cannot supply it; uncordoning that provider lets restoration proceed
// while the migrated worker's own manual cordon remains set.
func TestNewPlacementAC9RestoreWaitsForUsableCoverage(t *testing.T) {
	cfg := placementConfig()
	target := evidence(desired("w1"), m5Now)
	manualWorker(&target)
	peer := evidence(desired("w2"), m5Now)
	manualWorker(&peer)
	zero := int32(0)
	dep := liveDeployment(t, cfg, target, false)
	dep.Spec.Replicas, dep.UID = &zero, "dep-w1"
	objs := []runtime.Object{dep, liveDeployment(t, cfg, peer, false)}
	objs = append(objs, livePVCs(t, cfg, peer, false)...)
	m, client := npMatFor(t, cfg, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objs...)
	currentProof(t, client, &peer, m5Now)
	for i := 0; i < 4; i++ {
		if err := npTick(t, m, target, peer); err != nil {
			t.Fatal(err)
		}
	}
	if d := npDep(t, client, cfg.Namespace, target.ID); d.Spec.Replicas == nil || *d.Spec.Replicas != 0 {
		t.Fatal("restored replicas without usable peer coverage")
	}
	pacing := m.PacingOutcomes()[target.ID]
	if pacing == nil || pacing.BlockingReason == nil || !strings.Contains(*pacing.BlockingReason, "manual_cordon") {
		t.Fatalf("missing manual-cordon coverage explanation: %+v", pacing)
	}
	peer.ManualCordonedAt, peer.EffectiveCordoned = nil, nil
	for i := 0; i < 8; i++ {
		if err := npTick(t, m, target, peer); err != nil {
			t.Fatal(err)
		}
		if d := npDep(t, client, cfg.Namespace, target.ID); d.Spec.Replicas != nil && *d.Spec.Replicas == 1 {
			if got := podPlacement(d); got != "new" {
				t.Fatalf("restored placement=%s, want new", got)
			}
			for _, name := range []string{dataPVCName(target.ID), nixPVCName(target.ID)} {
				if !isStamped(npPVC(t, client, cfg.Namespace, name)) {
					t.Fatalf("restored claim %s is unstamped", name)
				}
			}
			return
		}
	}
	t.Fatalf("provider release did not unblock restoration: %+v", m.PacingOutcomes()[target.ID])
}

// The per-tick decision stored on the Reconcile context is authoritative: consumers handed
// an observation that implies the opposite decision (a mid-tick re-observation) must
// follow the map, for both the render and the stamp on a created claim.
func TestNewPlacementTickDecisionIsFrozen(t *testing.T) {
	for _, mapStamped := range []bool{true, false} {
		name := map[bool]string{true: "map-stamped-obs-legacy", false: "map-legacy-obs-stamped"}[mapStamped]
		t.Run(name, func(t *testing.T) {
			cfg := placementConfig()
			ns := cfg.Namespace
			w := evidence(desired("w1"), m5Now)
			spec := testSpec(t, w.Template, w.Size)
			// The observation reads the opposite of the map; the nix claim is missing.
			obs := npObserved("w1", !mapStamped, "")
			obs.HasNixPVC, obs.NixClaim = false, nil
			if newPlacementStamped(cfg, w, obs, ns) == mapStamped {
				t.Fatal("observation does not imply the opposite decision")
			}
			m, client := npMat(t, cfg)
			ctx := context.WithValue(context.Background(), placementTickKey{}, map[string]bool{"w1": mapStamped})

			want := map[bool]string{true: "new", false: "global"}[mapStamped]
			if got := podPlacement(RenderDeployment(m.renderCfgFor(ctx, w, obs), w, spec)); got != want {
				t.Errorf("render placement = %s, want %s (the map)", got, want)
			}

			deferred, _, err := m.ensurePVCs(ctx, w, obs, ns, spec, false)
			if err != nil || deferred {
				t.Fatalf("ensurePVCs deferred=%v err=%v", deferred, err)
			}
			created := createdPVCs(client)
			p, ok := created[nixPVCName("w1")]
			if !ok || len(created) != 1 {
				t.Fatalf("created claims = %v, want only the missing nix claim", created)
			}
			if isStamped(p) != mapStamped {
				t.Errorf("created claim stamped=%v, want %v (the map)", isStamped(p), mapStamped)
			}
		})
	}
}

// hand-built observation of a converged worker, for the consumers that take one directly.
func npObserved(id string, stamped bool, hash string) reconcile.ObservedWorker {
	claim := func(name string) *reconcile.ClaimObservation {
		return &reconcile.ClaimObservation{Name: name, PlacementNew: stamped}
	}
	return reconcile.ObservedWorker{
		ID: id, HasDeployment: true, SpecHash: hash,
		HasDataPVC: true, HasNixPVC: true, DataClaim: claim(dataPVCName(id)), NixClaim: claim(nixPVCName(id)),
	}
}

// AC8: every consumer of the rendered spec hash agrees for a stamped and for a legacy
// worker, ticks are quiescent, a fresh Materializer decides the same, and a drift roll of a
// stamped worker keeps the new placement.
func TestNewPlacementAC8EveryHashConsumerAgrees(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		name := map[bool]string{true: "stamped", false: "legacy"}[stamped]
		t.Run(name, func(t *testing.T) {
			cfg := placementConfig()
			ctx := context.Background()
			w := evidence(desired("w1"), m5Now)
			want := hashFor(t, cfg, w, stamped)
			other := hashFor(t, cfg, w, !stamped)
			if want == other {
				t.Fatal("the two placements hash alike")
			}
			objs := append(livePVCs(t, cfg, w, stamped), liveDeployment(t, cfg, w, stamped))
			m, client := npMat(t, cfg, objs...)

			// Normal render and quiescence: two ticks write nothing.
			for i := 0; i < 2; i++ {
				client.ClearActions()
				if err := npTick(t, m, w); err != nil {
					t.Fatal(err)
				}
				if got := writes(client); len(got) != 0 {
					t.Fatalf("tick %d wrote %v", i+1, got)
				}
			}
			// A fresh Materializer (controller restart) reaches the same decision.
			m2, client2 := npMat(t, cfg, objs...)
			if err := npTick(t, m2, w); err != nil {
				t.Fatal(err)
			}
			if got := writes(client2); len(got) != 0 {
				t.Fatalf("fresh materializer wrote %v", got)
			}

			obs, err := m.Observe(ctx)
			if err != nil || len(obs) != 1 {
				t.Fatalf("Observe: %v %v", obs, err)
			}
			o := obs[0]
			if o.SpecHash != want {
				t.Fatalf("observed hash %s != %s", o.SpecHash, want)
			}
			tickCtx := context.WithValue(ctx, placementTickKey{}, m.placementDecisions([]protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{"w1": o}))

			// Roll-pacing target.
			tick, _ := m.planRoll(tickCtx, []protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{"w1": o})
			if e := tick.entries["w1"]; e == nil || depHash(e.target) != want {
				t.Errorf("planRoll target hash differs from the render")
			}

			// Maintenance deadline: a worker at the rendered hash counts as undrifted.
			dw := w
			since := m5Now.Add(-2 * time.Hour)
			dw.DrainingSince = &since
			mm, _ := npMatFor(t, cfg, nil, DrainPolicy{Deadline: time.Hour})
			if !mm.maintenanceDespiteBusy(tickCtx, dw, npObserved("w1", stamped, want)) {
				t.Error("maintenance treats a worker at the rendered hash as drifted")
			}
			if mm.maintenanceDespiteBusy(tickCtx, dw, npObserved("w1", stamped, other)) {
				t.Error("maintenance treats a worker at the other placement's hash as undrifted")
			}

			// Hibernation create and wake renders.
			hw := w
			hw.Hibernation = &protocol.Hibernation{State: protocol.HibernationWaking, OpID: "wake-op"}
			hm, hc := npMat(t, cfg, livePVCs(t, cfg, w, stamped)...)
			if err := npTick(t, hm, hw); err != nil {
				t.Fatal(err)
			}
			if got := depHash(npDep(t, hc, cfg.Namespace, "w1")); got != want {
				t.Errorf("hibernation create hash differs from the render")
			}
			zero := int32(0)
			asleep := liveDeployment(t, cfg, w, !stamped)
			asleep.Spec.Replicas = &zero
			hm2, hc2 := npMat(t, cfg, append(livePVCs(t, cfg, w, stamped), asleep)...)
			if err := npTick(t, hm2, hw); err != nil {
				t.Fatal(err)
			}
			d := npDep(t, hc2, cfg.Namespace, "w1")
			if depHash(d) != want || d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
				t.Errorf("hibernation wake patched hash=%s replicas=%v", depHash(d), d.Spec.Replicas)
			}
		})
	}
}

// AC8, layout migration: a run-bound Docker worker held by custody reports drift against the
// placement its claims decide.
func TestNewPlacementAC8LayoutMigrationTarget(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		cfg := placementConfig()
		cfg.DockerNamespace = "uzi-workers-docker"
		cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
		w := desiredDocker("w1")
		w.Ephemeral, w.CustodyHeld = true, true
		m, _ := npMat(t, cfg)
		for _, c := range []struct {
			hash   string
			active bool
		}{{hashFor(t, cfg, w, stamped), false}, {hashFor(t, cfg, w, !stamped), true}} {
			obs := npObserved("w1", stamped, c.hash)
			obs.RunnerLayout = reconcile.RunnerLayoutOld
			tick := &rollTick{outcomes: map[string]*protocol.Pacing{}}
			m.surfaceEphemeralLayoutHold(context.Background(), tick, w, obs)
			if _, held := tick.outcomes["w1"]; held != c.active {
				t.Errorf("stamped=%v observed hash of the %v placement: held=%v, want %v", stamped, c.active, held, c.active)
			}
		}
	}
}

// AC8: a drift roll (new worker image) of a stamped worker keeps the new placement.
func TestNewPlacementAC8ImageChangeKeepsNewPlacement(t *testing.T) {
	cfg := placementConfig()
	w := desired("w1")
	w.Ephemeral = true
	w = evidence(w, m5Now)
	old := liveDeployment(t, cfg, w, true)
	r, err := preset.NewResolver("harbor.example.com/uzi", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	m, client := npMat(t, cfg, append(livePVCs(t, cfg, w, true), old)...)
	m.resolver = r
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	d := npDep(t, client, cfg.Namespace, "w1")
	if got := podPlacement(d); got != "new" {
		t.Errorf("placement after the image roll = %s", got)
	}
	if !strings.Contains(workerImage(d), "v1.1.0") {
		t.Errorf("image = %s, want the v1.1.0 target", workerImage(d))
	}
}

// AC8: a size change (a different resolved pod template) of a stamped worker keeps the new
// placement and neither gains nor loses a claim stamp.
func TestNewPlacementAC8SizeChangeKeepsNewPlacement(t *testing.T) {
	cfg := placementConfig()
	w := desired("w1")
	w.Ephemeral = true
	w.Size = "l"
	w = evidence(w, m5Now)
	old := liveDeployment(t, cfg, w, true)
	oldMem := old.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String()
	resized := w
	resized.Size = "l-highmem"
	m, client := npMat(t, cfg, append(livePVCs(t, cfg, w, true), old)...)
	if err := npTick(t, m, resized); err != nil {
		t.Fatal(err)
	}
	d := npDep(t, client, cfg.Namespace, "w1")
	if got := d.Spec.Template.Spec.Containers[0].Resources.Limits.Memory().String(); got == oldMem {
		t.Fatalf("memory limit %s did not change; the size change never reached the Deployment", got)
	}
	if got := podPlacement(d); got != "new" {
		t.Errorf("placement after the size change = %s", got)
	}
	if got := d.Spec.Template.Spec.Tolerations; len(got) != 1 || got[0].Value != "new" {
		t.Errorf("tolerations after the size change = %v", got)
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, cfg.Namespace, n)) {
			t.Errorf("claim %s lost its stamp", n)
		}
	}
	if got := len(createdPVCs(client)); got != 0 {
		t.Errorf("size change created %d claims", got)
	}
}

// A maintenance refresh can remove a claim counted by the frozen placement decision.
// Defer until a fresh tick rather than creating its replacement under that old decision.
func TestNewPlacementTerminatingGateDefersStaleClaimPointer(t *testing.T) {
	cfg := placementConfig()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := desiredDocker("w1")
	m, client := npMat(t, cfg)
	claim := func(name string, terminating bool) *reconcile.ClaimObservation {
		return &reconcile.ClaimObservation{Name: name, PlacementNew: true, Terminating: terminating}
	}
	obs := reconcile.ObservedWorker{
		ID: "w1", HasDataPVC: true, HasNixPVC: true, HasDinDDataPVC: false,
		DataClaim: claim(dataPVCName("w1"), false), NixClaim: claim(nixPVCName("w1"), false),
		DinDClaim: claim(dindDataPVCName("w1"), true), // stale: gone in the refreshed flags
	}
	deferred, _, err := m.ensurePVCs(context.Background(), w, obs, cfg.DockerNamespace, testSpec(t, w.Template, w.Size), false)
	if err != nil || !deferred {
		t.Fatalf("ensurePVCs = %v, %v", deferred, err)
	}
	if got := writes(client); len(got) != 0 {
		t.Fatalf("stale counted claim must defer all writes, got %v", got)
	}
}

// The initial observation counts an unstamped, Terminating DinD claim, so placement
// is frozen as legacy. If the claim disappears before maintenance's fresh list, no
// replacement may be minted using that stale decision. The following tick observes
// only the stamped durable siblings and recreates DinD with the new placement.
func TestNewPlacementDinDMaintenanceDefersDisappearedCountedClaim(t *testing.T) {
	cfg := placementConfig()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := desiredDocker("w1")
	w.DindMaintenance = &protocol.DindMaintenance{
		ID: "op", Phase: "recycling", Fenced: true,
		DeploymentUID: "old-deployment", PVCUID: "old-dind",
	}
	claims := livePVCs(t, cfg, w, true)
	for _, obj := range claims {
		p := obj.(*corev1.PersistentVolumeClaim)
		if p.Name == dindDataPVCName(w.ID) {
			delete(p.Annotations, AnnotationPlacement)
			p.UID = "old-dind"
			now := metav1.NewTime(m5Now)
			p.DeletionTimestamp = &now
		}
	}
	m, client := npMat(t, cfg, claims...)
	m.cordoner = &maintenanceFake{op: w.DindMaintenance}
	ctx := context.Background()
	observed, err := m.Observe(ctx)
	if err != nil || len(observed) != 1 {
		t.Fatalf("initial Observe: %v, %v", observed, err)
	}
	obs := observed[0]
	if !obs.HasDinDDataPVC || obs.DinDClaim == nil || !obs.DinDClaim.Terminating || obs.DinDClaim.PlacementNew {
		t.Fatalf("initial DinD observation does not describe the unstamped deleting claim: %+v", obs.DinDClaim)
	}
	decisions := m.placementDecisions([]protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{w.ID: obs})
	if decisions[w.ID] {
		t.Fatal("mixed initial claims must freeze legacy placement")
	}
	tickCtx := context.WithValue(ctx, placementTickKey{}, decisions)
	// Finalization completes after Observe, before reconcileDinD re-lists claims.
	if err := client.CoreV1().PersistentVolumeClaims(cfg.DockerNamespace).Delete(ctx, dindDataPVCName(w.ID), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	handled, err := m.reconcileDinD(tickCtx, w, obs, cfg.DockerNamespace, testSpec(t, w.Template, w.Size))
	if err != nil || !handled {
		t.Fatalf("maintenance handled=%v, err=%v", handled, err)
	}
	if got := writes(client); len(got) != 0 {
		t.Fatalf("disappeared counted claim must defer the worker, wrote %v", got)
	}
	observed, err = m.Observe(ctx)
	if err != nil || len(observed) != 1 {
		t.Fatalf("next Observe: %v, %v", observed, err)
	}
	obs = observed[0]
	decisions = m.placementDecisions([]protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{w.ID: obs})
	if !decisions[w.ID] {
		t.Fatal("fresh observation of stamped durable siblings must select new placement")
	}
	tickCtx = context.WithValue(ctx, placementTickKey{}, decisions)
	client.ClearActions()
	handled, err = m.reconcileDinD(tickCtx, w, obs, cfg.DockerNamespace, testSpec(t, w.Template, w.Size))
	if err != nil || !handled {
		t.Fatalf("next maintenance handled=%v, err=%v", handled, err)
	}
	if !isStamped(npPVC(t, client, cfg.DockerNamespace, dindDataPVCName(w.ID))) {
		t.Fatal("fresh DinD claim was recreated unstamped")
	}
	if got := podPlacement(npDep(t, client, cfg.DockerNamespace, w.ID)); got != "new" {
		t.Fatalf("replacement Deployment placement=%s, want new", got)
	}
}

// With newPlacement unset a Terminating claim does not gate creates: the missing sibling is
// still created, unstamped (today's behaviour).
func TestNewPlacementUnsetTerminatingClaimDoesNotGate(t *testing.T) {
	cfg := globalOnly(placementConfig())
	w := newWorkerFixture(desired("w1"))
	m, client := npMat(t, cfg, pvcTerminating("w1", "data"))
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	p := createdPVCs(client)[nixPVCName("w1")]
	if p == nil {
		t.Fatalf("sibling nix claim not created; created = %v", createdPVCs(client))
	}
	if isStamped(p) {
		t.Error("sibling created stamped with newPlacement unset")
	}
}

// A claim flagged present but with no observation is not proof of a stamp.
func TestNewPlacementStampedNeedsReadableClaim(t *testing.T) {
	cfg := placementConfig()
	if newPlacementStamped(cfg, protocol.DesiredWorker{ID: "w1"}, reconcile.ObservedWorker{ID: "w1", HasDataPVC: true}, cfg.Namespace) {
		t.Error("present claim with nil observation counted as stamped")
	}
}

// AC10: clearing newPlacement sends a stamped worker back to the global placement and leaves
// its stamps alone.
func TestNewPlacementAC10ClearedConfigRendersGlobalAndKeepsStamps(t *testing.T) {
	set := placementConfig()
	cleared := globalOnly(set)
	w := desired("w1")
	w.Ephemeral = true
	w = evidence(w, m5Now)
	m, client := npMat(t, cleared, append(livePVCs(t, set, w, true), liveDeployment(t, set, w, true))...)
	if err := npTick(t, m, w); err != nil {
		t.Fatal(err)
	}
	d := npDep(t, client, cleared.Namespace, "w1")
	if got := podPlacement(d); got != "global" {
		t.Errorf("placement with newPlacement cleared = %s", got)
	}
	if depHash(d) != hashFor(t, cleared, w, true) {
		t.Error("hash differs from the plain global render")
	}
	for _, n := range []string{dataPVCName("w1"), nixPVCName("w1")} {
		if !isStamped(npPVC(t, client, cleared.Namespace, n)) {
			t.Errorf("stamp on %s was removed", n)
		}
	}
	for _, w := range writes(client) {
		if strings.Contains(w, "persistentvolumeclaims") {
			t.Errorf("claim write while clearing newPlacement: %v", w)
		}
	}
}

// AlreadyExists on a stamped create is not proof the existing claim is stamped: the worker
// is left alone for the tick (no more claims, no Deployment) and no error is raised. With
// newPlacement unset AlreadyExists stays plain success.
func TestNewPlacementAlreadyExistsDefersTheWorker(t *testing.T) {
	for _, set := range []bool{true, false} {
		cfg := placementConfig()
		if !set {
			cfg = globalOnly(cfg)
		}
		w := newWorkerFixture(desired("w1"))
		m, client := npMat(t, cfg)
		attempts := 0
		client.PrependReactor("create", "persistentvolumeclaims", func(a k8stesting.Action) (bool, runtime.Object, error) {
			attempts++
			if attempts == 1 {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, "x")
			}
			return false, nil, nil
		})
		if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
			t.Fatalf("set=%v: AlreadyExists surfaced as %v", set, err)
		}
		deps, _ := client.AppsV1().Deployments(cfg.Namespace).List(context.Background(), metav1.ListOptions{})
		if set {
			if attempts != 1 || len(deps.Items) != 0 {
				t.Errorf("set: attempts=%d deployments=%d, want 1 and 0", attempts, len(deps.Items))
			}
		} else if attempts != 2 || len(deps.Items) != 1 {
			t.Errorf("unset: attempts=%d deployments=%d, want 2 and 1 (today's behaviour)", attempts, len(deps.Items))
		}
	}
}

// Every desired id, including workers Observe has never seen, gets the tick decision: a
// brand-new persistent worker and an ephemeral one both render the new placement, while an
// observed legacy worker in the same tick stays global.
func TestNewPlacementEveryDesiredIDIsDecided(t *testing.T) {
	cfg := placementConfig()
	legacy := evidence(desired("old"), m5Now)
	fresh := newWorkerFixture(desired("fresh"))
	eph := newWorkerFixture(desired("eph"))
	eph.Ephemeral = true
	m, client := npMat(t, cfg, append(livePVCs(t, cfg, legacy, false), liveDeployment(t, cfg, legacy, false))...)
	if err := npTick(t, m, legacy, fresh, eph); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"old": "global", "fresh": "new", "eph": "new"} {
		if got := podPlacement(npDep(t, client, cfg.Namespace, id)); got != want {
			t.Errorf("%s renders %s, want %s", id, got, want)
		}
	}
}

// AC8, DinD maintenance wantHash: in the final "recycling" step the replacement Deployment
// is only reported ready and completed when its observed spec hash equals the hash the
// worker's own placement renders. Stamped and legacy workers each complete at their own
// hash and refuse at the other placement's.
func TestNewPlacementAC8DinDMaintenanceWantHash(t *testing.T) {
	for _, stamped := range []bool{true, false} {
		for _, atOwn := range []bool{true, false} {
			cfg := placementConfig()
			cfg.DockerNamespace = "uzi-workers-docker"
			cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
			w := desiredDocker("w1")
			w.DiskPressure, w.DiskPressureVolumes = true, []string{"dind"}
			w.DindMaintenance = &protocol.DindMaintenance{ID: "op", Phase: "recycling", Fenced: true, DeploymentUID: "old-deployment", PVCUID: "old-pvc"}
			dep := liveDeployment(t, cfg, w, stamped == atOwn)
			dep.UID = "new-deployment"
			objs := []runtime.Object{dep}
			for _, o := range livePVCs(t, cfg, w, stamped) {
				p := o.(*corev1.PersistentVolumeClaim)
				p.UID = types.UID(p.Name)
				p.Status.Phase = corev1.ClaimBound
				objs = append(objs, p)
			}
			m, client := npMat(t, cfg, objs...)
			m.drain.ForceRoll = true
			f := &maintenanceFake{op: w.DindMaintenance}
			m.cordoner = f
			ready := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: cfg.DockerNamespace, UID: "new-pod", Labels: dep.Spec.Template.Labels, Annotations: dep.Spec.Template.Annotations}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			stampMaintenancePodOwner(ready, dep.Name)
			ctx := context.Background()
			if _, err := client.AppsV1().ReplicaSets(cfg.DockerNamespace).Create(ctx, maintenanceReplicaSet(ready, dep), metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.CoreV1().Pods(cfg.DockerNamespace).Create(ctx, ready, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := npTick(t, m, w); err != nil {
				t.Fatal(err)
			}
			completed := slices.Contains(f.calls, "complete")
			if completed != atOwn {
				t.Errorf("stamped=%v deployment at own placement hash=%v: completed=%v (calls %v)", stamped, atOwn, completed, f.calls)
			}
		}
	}
}
