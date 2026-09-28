package kube

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Issue #1815: a RUN-BOUND worker (protocol.DesiredWorker.Ephemeral — "ephemeral" as
// in run-bound, NOT ephemeral-storage) gets its own /data size, decoupled from the
// CPU/memory preset. These tests pin the three halves: the run-bound /data follows
// the setting at every preset, persistent workers are untouched, and the default fits
// the chart's ceilings and storage budget.

// pvcSizes renders w's claims and returns them keyed by PVC name.
func pvcSizes(t *testing.T, cfg RenderConfig, w protocol.DesiredWorker, spec preset.Spec) map[string]resource.Quantity {
	t.Helper()
	out := map[string]resource.Quantity{}
	for _, p := range RenderPVCs(cfg, w, spec) {
		out[p.Name] = p.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	return out
}

func TestEphemeralWorkerDataPVCIsDecoupledFromThePreset(t *testing.T) {
	if len(preset.SizeNames()) == 0 {
		t.Fatal("no preset sizes: every assertion below would pass vacuously")
	}
	for _, tc := range []struct {
		name     string
		override string
		want     string
	}{
		{"default", "", ephemeralDataDefaultSize},
		{"override", "30Gi", "30Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, docker := range []bool{false, true} {
				cfg := dockerTestConfig()
				cfg.EphemeralDataSize = tc.override
				for _, size := range preset.SizeNames() {
					spec := testSpec(t, "base", size)
					id := "abc"
					persistent := protocol.DesiredWorker{ID: id, Template: "base", Size: size, Docker: docker}
					ephemeral := persistent
					ephemeral.Ephemeral = true

					eph := pvcSizes(t, cfg, ephemeral, spec)
					per := pvcSizes(t, cfg, persistent, spec)
					if len(eph) != len(per) {
						t.Fatalf("docker=%v size %q: run-bound worker renders %d claims, persistent %d; only a size may differ",
							docker, size, len(eph), len(per))
					}

					got, ok := eph[dataPVCName(id)]
					if !ok {
						t.Fatalf("docker=%v size %q: no -data claim", docker, size)
					}
					if want := resource.MustParse(tc.want); got.Cmp(want) != 0 {
						t.Errorf("docker=%v size %q: run-bound /data = %s, want %s (it REPLACES the preset's %s)",
							docker, size, got.String(), want.String(), spec.Size.DataSize.String())
					}
					// -nix and -dind-data are the persistent worker's, unchanged.
					for name, q := range per {
						if name == dataPVCName(id) {
							continue
						}
						if e, ok := eph[name]; !ok || e.Cmp(q) != 0 {
							t.Errorf("docker=%v size %q: %s = %v on a run-bound worker, want the persistent %s",
								docker, size, name, eph[name], q.String())
						}
					}
				}
			}
		})
	}
}

func TestPersistentWorkerDataPVCStaysThePresetsEvenWithAnOverride(t *testing.T) {
	cfg := dockerTestConfig()
	cfg.EphemeralDataSize = "30Gi"
	for _, docker := range []bool{false, true} {
		for _, size := range preset.SizeNames() {
			spec := testSpec(t, "base", size)
			w := protocol.DesiredWorker{ID: "abc", Template: "base", Size: size, Docker: docker}
			got := pvcSizes(t, cfg, w, spec)[dataPVCName("abc")]
			if got.Cmp(spec.Size.DataSize) != 0 {
				t.Errorf("docker=%v size %q: persistent /data = %s, want the preset's %s; the run-bound "+
					"override must never move a persistent worker's claim",
					docker, size, got.String(), spec.Size.DataSize.String())
			}
		}
	}
}

// The controller's run-bound /data default must fit BOTH tiers' chart ceilings, read
// out of values.yaml — the same cross-toolchain tie as
// TestDinDDataDefaultFitsTheChartsLimitRangeMax. A run-bound worker lands in either
// tier, so the default must fit each.
func TestEphemeralDataDefaultFitsTheChartsLimitRangeMax(t *testing.T) {
	restricted, docker := chartMaxPVCStorages(t)
	def := resource.MustParse(ephemeralDataDefaultSize)
	for _, tier := range []struct {
		key string
		max resource.Quantity
	}{
		{"workers.limitRange.maxPVCStorage", restricted},
		{"workers.docker.limitRange.maxPVCStorage", docker},
	} {
		if def.Cmp(tier.max) > 0 {
			t.Errorf("ephemeralDataDefaultSize = %s exceeds %s = %s: every run-bound worker without an "+
				"override would have its /data PVC rejected at admission. Raise the ceiling (and "+
				"quota.requestsStorage with it) or lower the constant.", def.String(), tier.key, tier.max.String())
		}
	}
}

// A full fleet of the worst-case worker must fit each tier's quota.requestsStorage.
// The worst case per claim is the largest across every preset AND both worker shapes
// (persistent and run-bound), so this is
//
//	deployments x (max(largest DataSize, ephemeralDataDefaultSize) + nix [+ dind data])
//
// computed by rendering rather than by a hand-kept list of claims. All sizes are the
// controller's defaults; the quota and fleet size are read out of values.yaml.
func TestShippedPVCDefaultsFitEachTiersStorageQuota(t *testing.T) {
	quotas := chartStorageQuotas(t)
	resolver := testResolver(t)
	cfg := RenderConfig{} // controller defaults: no dind or run-bound override

	for _, tier := range []struct {
		name   string
		docker bool
		q      tierStorageQuota
	}{
		{"restricted", false, quotas.restricted},
		{"docker", true, quotas.docker},
	} {
		largest := map[string]resource.Quantity{}
		for _, template := range preset.TemplateNames() {
			for _, size := range preset.SizeNames() {
				spec, err := resolver.Resolve(template, size)
				if err != nil {
					t.Fatalf("resolve %q/%q: %v", template, size, err)
				}
				for _, ephemeral := range []bool{false, true} {
					w := protocol.DesiredWorker{ID: "fleet", Template: template, Size: size, Docker: tier.docker, Ephemeral: ephemeral}
					for name, q := range pvcSizes(t, cfg, w, spec) {
						if cur, ok := largest[name]; !ok || q.Cmp(cur) > 0 {
							largest[name] = q
						}
					}
				}
			}
		}
		// Vacuity guard: /data and /nix on both tiers, plus dind-data on docker.
		wantClaims := 2
		if tier.docker {
			wantClaims = 3
		}
		if len(largest) != wantClaims {
			t.Fatalf("%s tier: %d distinct claims rendered, want %d; the sum below would be wrong",
				tier.name, len(largest), wantClaims)
		}
		perWorker := resource.Quantity{}
		for _, q := range largest {
			perWorker.Add(q)
		}
		if perWorker.Sign() <= 0 {
			t.Fatalf("%s tier: per-worker storage is %s; the assertion would be vacuous", tier.name, perWorker.String())
		}
		fleet := resource.Quantity{}
		for i := 0; i < tier.q.deployments; i++ {
			fleet.Add(perWorker)
		}
		if fleet.Cmp(tier.q.requestsStorage) > 0 {
			t.Errorf("%s tier: a full fleet (%d x %s = %s) exceeds quota.requestsStorage = %s; the last "+
				"workers' PVCs would be refused and they would provision and never appear",
				tier.name, tier.q.deployments, perWorker.String(), fleet.String(), tier.q.requestsStorage.String())
		}
	}
}

type tierStorageQuota struct {
	requestsStorage resource.Quantity
	deployments     int
}

// chartStorageQuotas reads both tiers' quota.requestsStorage and quota.deployments out
// of deploy/chart/values.yaml. Parsed, FATAL on any miss, same contract as
// chartDeploymentQuotas.
func chartStorageQuotas(t *testing.T) (out struct{ restricted, docker tierStorageQuota }) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "deploy", "chart", "values.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reads a test-controlled fixture/chart path (not user input)
	if err != nil {
		t.Fatalf("read %s: %v (the storage quota is read from the chart; it must not fall back to a hardcoded value)", path, err)
	}
	type quota struct {
		RequestsStorage string `json:"requestsStorage"`
	}
	var v struct {
		Workers struct {
			Quota  quota `json:"quota"`
			Docker struct {
				Quota quota `json:"quota"`
			} `json:"docker"`
		} `json:"workers"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	docker, plain := chartDeploymentQuotas(t)
	out.restricted = tierStorageQuota{
		requestsStorage: chartQuantity(t, path, "workers.quota.requestsStorage", v.Workers.Quota.RequestsStorage),
		deployments:     plain,
	}
	out.docker = tierStorageQuota{
		requestsStorage: chartQuantity(t, path, "workers.docker.quota.requestsStorage", v.Workers.Docker.Quota.RequestsStorage),
		deployments:     docker,
	}
	return out
}
