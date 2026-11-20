package kube

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"github.com/vtmocanu/uzi/controller/internal/reconcile"
)

// PRD #4213: a DesiredWorker carries its size's integers and the controller builds the
// pod spec from them. These tests drive that through the real Materializer paths.

// sizeSeedsFromAPI reads the api's builtin size seed contract across the tree, the same
// file the preset contract test reads.
func sizeSeedsFromAPI(t *testing.T) map[string]*protocol.SizeResources {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "hosted_sizes.json"))
	if err != nil {
		t.Fatalf("read hosted_sizes.json: %v", err)
	}
	var doc struct {
		Sizes []struct {
			Name string `json:"name"`
			protocol.SizeResources
		} `json:"sizes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]*protocol.SizeResources{}
	for _, s := range doc.Sizes {
		r := s.SizeResources
		out[s.Name] = &r
	}
	if len(out) == 0 {
		t.Fatal("hosted_sizes.json carries no sizes")
	}
	return out
}

// customXS is an admin-added size no compiled-in table carries.
func customXS() *protocol.SizeResources {
	return &protocol.SizeResources{
		CPURequestMillicores: 250, CPULimitMillicores: 1000,
		MemoryRequestMiB: 1024, MemoryLimitMiB: 2048,
		DataGiB: 5, DinDDataGiB: 10,
	}
}

func xsWorker(id string) protocol.DesiredWorker {
	return protocol.DesiredWorker{ID: id, Template: "base", Size: "xs", Resources: customXS()}
}

func resolveVia(t *testing.T, w protocol.DesiredWorker) preset.Spec {
	t.Helper()
	m, _ := newMat(t)
	spec, err := m.resolveWorker(w)
	if err != nil {
		t.Fatalf("resolveWorker(%s/%s): %v", w.Template, w.Size, err)
	}
	return spec
}

func hashVia(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker) string {
	t.Helper()
	return SpecHashOf(cfg, w, resolveVia(t, w))
}

// Byte identity: every builtin row of the size spec-hash golden, recomputed from the
// api's integers through the resolve helper, equals the committed golden value.
func TestResolveWorkerIntegersReproduceTheSizeSpecHashGolden(t *testing.T) {
	seeds := sizeSeedsFromAPI(t)
	raw, err := os.ReadFile(sizeSpecHashFixture)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	cases := sizeSpecHashCases(t)
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases {
		res, ok := seeds[c.w.Size]
		if !ok {
			t.Fatalf("%s: size %q missing from hosted_sizes.json", c.key, c.w.Size)
		}
		w := c.w
		w.Resources = res
		if got := hashVia(t, c.cfg, w); got != want[c.key] {
			t.Errorf("%s: hash from integers %s, golden %s", c.key, got, want[c.key])
		}
	}
}

// Without integers (an older api) every builtin resolves exactly to the compiled-in spec.
func TestResolveWorkerWithoutIntegersIsTheCompiledInSpec(t *testing.T) {
	for _, c := range sizeSpecHashCases(t) {
		compiled := testSpec(t, c.w.Template, c.w.Size)
		if got, want := hashVia(t, c.cfg, c.w), SpecHashOf(c.cfg, c.w, compiled); got != want {
			t.Errorf("%s: nil-resources hash %s != compiled-in %s", c.key, got, want)
		}
	}
	m, _ := newMat(t)
	_, err := m.resolveWorker(protocol.DesiredWorker{ID: "x", Template: "base", Size: "xs"})
	if !preset.IsUnknown(err) {
		t.Fatalf("a custom size name without integers must be an unknown-size miss, got %v", err)
	}
}

// (f) A custom size renders a pod and claims whose quantities equal the integers.
func TestCustomSizeRendersExactlyItsIntegers(t *testing.T) {
	w := newWorkerFixture(xsWorker("x1"))
	m, client := newMat(t)
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, nil); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	d := npDep(t, client, testConfig().Namespace, "x1")
	c := workerContainer(t, d.Spec.Template.Spec)
	for name, got := range map[string]string{
		"cpu request": c.Resources.Requests.Cpu().String(),
		"cpu limit":   c.Resources.Limits.Cpu().String(),
		"mem request": c.Resources.Requests.Memory().String(),
		"mem limit":   c.Resources.Limits.Memory().String(),
	} {
		want := map[string]string{"cpu request": "250m", "cpu limit": "1", "mem request": "1Gi", "mem limit": "2Gi"}[name]
		if got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	created := createdPVCs(client)
	data := created[dataPVCName("x1")]
	if data == nil {
		t.Fatal("data claim not created")
	}
	if q := data.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "5Gi" {
		t.Errorf("data claim = %s, want 5Gi", q.String())
	}
}

// (a) A memory edit on an `l` worker changes the spec hash and the existing roll fires;
// the unedited integers leave it settled.
func TestMemoryEditOnABuiltinSizeRollsTheDeployment(t *testing.T) {
	base := *sizeSeedsFromAPI(t)["l"]
	w := protocol.DesiredWorker{ID: "w1", Template: "base", Size: "l", Resources: &base}
	hash := hashVia(t, testConfig(), w)
	m, client := newMat(t, deployedWorker("w1", 0, hash))
	observed := []reconcile.ObservedWorker{{ID: "w1", HasDeployment: true, SpecHash: hash}}

	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{evidence(w, m.now())}, observed); err != nil {
		t.Fatal(err)
	}
	if wasPatched(client) {
		t.Fatal("unedited integers rolled a settled worker")
	}

	edited := base
	edited.MemoryLimitMiB += 1024
	w.Resources = &edited
	client.ClearActions()
	if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{evidence(w, m.now())}, observed); err != nil {
		t.Fatal(err)
	}
	patch := findPatch(t, client)
	newHash := hashVia(t, testConfig(), w)
	if newHash == hash || !strings.Contains(patch, newHash) {
		t.Errorf("memory edit did not restamp the spec hash (old %s new %s):\n%s", hash, newHash, patch)
	}
	assertNoSecretReads(t, client)
}

// (b) A data_gib increase grows the desired claim in place with no spec-hash change.
func TestDataGiBIncreaseGrowsTheClaimWithoutARoll(t *testing.T) {
	res := *sizeSeedsFromAPI(t)["l"]
	res.DataGiB, res.DinDDataGiB = 80, 60
	f := newExpandFx(t, expandOpt{
		expand:   boolp(true),
		workerFn: func(w *protocol.DesiredWorker) { w.Resources = &res },
	})
	hashBefore := podTemplateOf(t, f.client, f.ns).Spec.Template.Annotations[AnnotationSpecHash]
	f.mustTick()
	if got := f.request(dataPVCName(f.w.ID)); got != "80Gi" {
		t.Errorf("data claim = %s, want 80Gi", got)
	}
	if got := f.request(dindDataPVCName(f.w.ID)); got != "60Gi" {
		t.Errorf("dind claim = %s, want 60Gi", got)
	}
	f.assertNoPodOrRollMutation()
	if got := podTemplateOf(t, f.client, f.ns).Spec.Template.Annotations[AnnotationSpecHash]; got != hashBefore {
		t.Errorf("spec hash moved %s -> %s", hashBefore, got)
	}
}

// (c) planRoll resolves through the integers: a custom size is planned and its target
// carries the integers; without them it is unplanned.
func TestPlanRollUsesTheIntegers(t *testing.T) {
	m, _ := newMat(t)
	ctx := context.Background()
	w := evidence(xsWorker("x1"), m5Now)
	tick, _ := m.planRoll(ctx, []protocol.DesiredWorker{w}, map[string]reconcile.ObservedWorker{})
	e := tick.entries["x1"]
	if e == nil {
		t.Fatalf("custom size not planned; unplanned = %v", tick.unplanned)
	}
	limits := workerContainer(t, e.target.Spec.Template.Spec).Resources.Limits
	if got := limits.Memory().String(); got != "2Gi" {
		t.Errorf("planned target memory limit = %s, want 2Gi", got)
	}
	bare := w
	bare.Resources = nil
	tick, _ = m.planRoll(ctx, []protocol.DesiredWorker{bare}, map[string]reconcile.ObservedWorker{})
	if tick.entries["x1"] != nil {
		t.Error("a custom size without integers was planned")
	}
}

// materializer.go's non-admitted DinD maintenance path (the first resolve site in
// Reconcile) needs a resolvable spec: a custom-size Docker worker under dind pressure
// gets its maintenance request only because the integers resolve.
func TestDinDMaintenanceResolvesACustomSize(t *testing.T) {
	for _, withIntegers := range []bool{true, false} {
		cfg := dockerTestConfig()
		w := protocol.DesiredWorker{ID: "x1", Template: "base", Size: "xs", Docker: true, DiskPressure: true, DiskPressureVolumes: []string{"dind"}}
		if withIntegers {
			w.Resources = customXS()
		}
		var objs []runtime.Object
		if withIntegers {
			spec := resolveVia(t, w)
			d := RenderDeployment(cfg, w, spec)
			d.UID = "dep-uid"
			objs = append(objs, d)
			for _, p := range RenderPVCs(cfg, w, spec) {
				p.UID = types.UID("uid-" + p.Name)
				p.Status.Phase = corev1.ClaimBound
				objs = append(objs, p)
			}
		}
		m, _ := newMat(t, objs...)
		m.cfg = cfg
		f := &maintenanceFake{}
		m.cordoner = f
		observed, err := m.Observe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_ = m.Reconcile(context.Background(), []protocol.DesiredWorker{w}, observed)
		requested := f.op != nil && f.op.Phase == "requested"
		if requested != withIntegers {
			t.Errorf("withIntegers=%v: dind maintenance requested=%v", withIntegers, requested)
		}
	}
}

// (d) migration drain: a peer whose size exists only as inline integers still counts as
// resolvable provider coverage, and its spec-hash is compared against those integers.
func TestMigrationCoverResolvesAPeersInlineResources(t *testing.T) {
	for _, mode := range []string{"custom-xs", "custom-xs-no-integers", "edited-builtin"} {
		t.Run(mode, func(t *testing.T) {
			objsA, a := lmObjects(t, migrationHeldOpt("a"))
			objsB, b := lmObjects(t, lmOpts{id: "b", layout: "new", promoted: true})
			switch mode {
			case "custom-xs", "custom-xs-no-integers":
				b.Size, b.Resources = "xs", customXS()
			case "edited-builtin":
				r := *sizeSeedsFromAPI(t)["m"]
				r.MemoryLimitMiB += 2048
				b.Resources = &r
			}
			hash := hashVia(t, dockerTestConfig(), b)
			for _, o := range objsB {
				switch v := o.(type) {
				case *appsv1.Deployment:
					v.Spec.Template.Annotations[AnnotationSpecHash] = hash
				case *corev1.Pod:
					v.Annotations[AnnotationSpecHash] = hash
				}
			}
			if mode == "custom-xs-no-integers" {
				b.Resources = nil
			}
			f := lmCluster(t, false, append(objsA, objsB...)...)
			c := &migrationCordoner{}
			f.m.cordoner = c
			f.tickAll(t, a, b)
			got := strings.Join(c.calls, ",")
			want := "a"
			if mode == "custom-xs-no-integers" {
				want = ""
			}
			if got != want {
				t.Errorf("migration drain calls = %q, want %q", got, want)
			}
		})
	}
}

// (e) surfaceEphemeralLayoutHold compares the observed hash against a render built
// from the worker's own integers.
func TestLayoutHoldComparesAgainstTheIntegerRender(t *testing.T) {
	cfg := dockerTestConfig()
	w := xsWorker("x1")
	w.Docker, w.Ephemeral, w.CustodyHeld = true, true, true
	m, _ := npMat(t, cfg)
	own := hashVia(t, cfg, w)
	for _, c := range []struct {
		name string
		hash string
		held bool
	}{{"hash of the integer render", own, false}, {"hash of another render", "stale-hash", true}} {
		obs := npObserved("x1", false, c.hash)
		obs.RunnerLayout = reconcile.RunnerLayoutOld
		tick := &rollTick{outcomes: map[string]*protocol.Pacing{}}
		m.surfaceEphemeralLayoutHold(context.Background(), tick, w, obs)
		if _, held := tick.outcomes["x1"]; held != c.held {
			t.Errorf("%s: held=%v want %v", c.name, held, c.held)
		}
	}
	bare := w
	bare.Resources = nil
	obs := npObserved("x1", false, "stale-hash")
	obs.RunnerLayout = reconcile.RunnerLayoutOld
	tick := &rollTick{outcomes: map[string]*protocol.Pacing{}}
	m.surfaceEphemeralLayoutHold(context.Background(), tick, bare, obs)
	if len(tick.outcomes) != 0 {
		t.Error("an unresolvable custom size surfaced a hold")
	}
}

// (g) New placement with a custom size: claims are created at the integers with the
// placement stamp, and once stamped the render, hash and maintenance paths leave the
// claims alone.
func TestNewPlacementCustomSize(t *testing.T) {
	cfg := placementConfig()
	cfg.DockerNamespace = "uzi-workers-docker"
	cfg.DinDImage = "docker:28-dind-rootless@sha256:deadbeef"
	w := xsWorker("x1")
	w.Docker = true

	t.Run("creates stamped claims at the integers", func(t *testing.T) {
		m, client := npMat(t, cfg)
		if err := m.Reconcile(context.Background(), []protocol.DesiredWorker{newWorkerFixture(w)}, nil); err != nil {
			t.Fatal(err)
		}
		created := createdPVCs(client)
		for name, want := range map[string]string{dataPVCName("x1"): "5Gi", dindDataPVCName("x1"): "10Gi"} {
			p := created[name]
			if p == nil {
				t.Fatalf("claim %s not created", name)
			}
			if q := p.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(resource.MustParse(want)) != 0 {
				t.Errorf("%s = %s, want %s", name, q.String(), want)
			}
			if !isStamped(p) {
				t.Errorf("%s lacks the placement stamp", name)
			}
		}
	})

	t.Run("stamped worker renders and maintains from the integers", func(t *testing.T) {
		spec := resolveVia(t, w)
		ew := evidence(w, m5Now)
		var objs []runtime.Object
		for _, p := range RenderPVCs(cfg.withPlacement(true), ew, spec) {
			p.Annotations = map[string]string{AnnotationPlacement: placementNew}
			p.Status.Phase = corev1.ClaimBound
			objs = append(objs, p)
		}
		objs = append(objs, RenderDeployment(cfg.withPlacement(true), ew, spec))
		m, client := npMat(t, cfg, objs...)
		cord := &maintenanceFake{}
		m.cordoner = cord
		if hashFor2 := SpecHashOf(cfg.withPlacement(true), ew, spec); hashFor2 == "" {
			t.Fatal("empty hash")
		}
		if err := npTick(t, m, ew); err != nil {
			t.Fatal(err)
		}
		if got := writes(client); len(got) != 0 {
			t.Fatalf("a converged stamped custom-size worker was written to: %v", got)
		}
		// The maintenance entry points see the same spec and touch no claim.
		obs, err := m.Observe(context.Background())
		if err != nil || len(obs) != 1 {
			t.Fatalf("Observe: %v %v", obs, err)
		}
		pressured := ew
		pressured.DiskPressure, pressured.DiskPressureVolumes = true, []string{"dind"}
		ctx := context.WithValue(context.Background(), placementTickKey{}, m.placementDecisions([]protocol.DesiredWorker{pressured}, map[string]reconcile.ObservedWorker{ew.ID: obs[0]}))
		client.ClearActions()
		if _, err := m.reconcileDinD(ctx, pressured, obs[0], cfg.DockerNamespace, spec); err != nil {
			t.Fatal(err)
		}
		for _, wr := range writes(client) {
			if strings.Contains(wr, "persistentvolumeclaims") {
				t.Errorf("maintenance wrote a claim: %v", writes(client))
			}
		}
		for _, n := range []string{dataPVCName("x1"), dindDataPVCName("x1")} {
			if !isStamped(npPVC(t, client, cfg.DockerNamespace, n)) {
				t.Errorf("claim %s lost its stamp", n)
			}
		}
	})

	// A stamped worker whose live Deployment was rendered from DIFFERENT integers must
	// be detected as drifted and rolled to the xs integers: skipping or mis-resolving the
	// custom size would leave the stale Deployment (and its hash) untouched.
	t.Run("stamped worker with a stale deployment rolls to the integers", func(t *testing.T) {
		spec := resolveVia(t, w)
		ew := evidence(w, m5Now)
		stale := xsWorker("x1")
		stale.Docker = true
		stale.Resources = &protocol.SizeResources{
			CPURequestMillicores: 500, CPULimitMillicores: 2000,
			MemoryRequestMiB: 2048, MemoryLimitMiB: 4096,
			DataGiB: 5, DinDDataGiB: 10,
		}
		staleSpec := resolveVia(t, stale)
		wantHash := SpecHashOf(cfg.withPlacement(true), ew, spec)
		if staleHash := SpecHashOf(cfg.withPlacement(true), evidence(stale, m5Now), staleSpec); staleHash == wantHash {
			t.Fatal("fixture: stale and current integers hash alike")
		}
		var objs []runtime.Object
		for _, p := range RenderPVCs(cfg.withPlacement(true), ew, spec) {
			p.Annotations = map[string]string{AnnotationPlacement: placementNew}
			p.Status.Phase = corev1.ClaimBound
			objs = append(objs, p)
		}
		staleDep := RenderDeployment(cfg.withPlacement(true), evidence(stale, m5Now), staleSpec)
		objs = append(objs, physicalOld(t, staleDep)...)
		m, client := npMatFor(t, cfg, &fakeCordoner{}, DrainPolicy{Deadline: time.Hour}, objs...)
		if err := npTick(t, m, ew); err != nil {
			t.Fatal(err)
		}
		if got := templatePatches(client); len(got) != 1 || got[0] != deploymentName("x1") {
			t.Fatalf("drifted custom-size worker was not rolled: patches=%v outcome=%+v", got, m.PacingOutcomes()["x1"])
		}
		// Neither recreate nor shrink the claims.
		for _, wr := range writes(client) {
			if strings.Contains(wr, "persistentvolumeclaims") {
				t.Errorf("drift handling wrote a claim: %v", writes(client))
			}
		}
		for _, n := range []string{dataPVCName("x1"), dindDataPVCName("x1")} {
			if !isStamped(npPVC(t, client, cfg.DockerNamespace, n)) {
				t.Errorf("claim %s lost its stamp", n)
			}
		}
		dep := npDep(t, client, staleDep.Namespace, "x1")
		if got := dep.Spec.Template.Annotations[AnnotationSpecHash]; got != wantHash {
			t.Errorf("deployment spec-hash = %q, want the xs-integer hash %q (drift not rolled)", got, wantHash)
		}
		var cpu string
		for _, c := range dep.Spec.Template.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok && c.Name != "dind" {
				cpu = q.String()
			}
		}
		if cpu != "" && cpu != "250m" {
			t.Errorf("a container requests cpu %s, want 250m from the xs integers", cpu)
		}
	})
}
