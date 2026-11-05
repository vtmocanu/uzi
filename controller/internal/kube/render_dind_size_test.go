package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
	"github.com/vtmocanu/uzi/controller/internal/protocol"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The DinD data-root PVC defaults per preset (issue #3890): s/m 20Gi, l/l-highmem 50Gi.
func TestRenderPVCsDinDDataSizeIsPerPreset(t *testing.T) {
	for size, want := range map[string]string{"s": "20Gi", "m": "20Gi", "l": "50Gi", "l-highmem": "50Gi"} {
		w := protocol.DesiredWorker{ID: "abc", Template: "base", Size: size, Docker: true}
		got := pvcSizes(t, dockerTestConfig(), w, testSpec(t, "base", size))
		q, ok := got[dindDataPVCName("abc")]
		if !ok {
			t.Fatalf("size %s: no dind-data claim rendered", size)
		}
		if q.String() != want {
			t.Errorf("size %s: dind-data = %s, want %s", size, q.String(), want)
		}
	}
}

// An explicit cluster override wins at every preset, and a run-bound worker's /data
// override stays exact while dind still follows the preset.
func TestRenderPVCsDinDDataOverrideWinsAndEphemeralDataStaysExact(t *testing.T) {
	for _, size := range preset.SizeNames() {
		cfg := dockerTestConfig()
		cfg.DinDDataSize = "30Gi"
		w := protocol.DesiredWorker{ID: "abc", Template: "base", Size: size, Docker: true}
		got := pvcSizes(t, cfg, w, testSpec(t, "base", size))
		if q := got[dindDataPVCName("abc")]; q.String() != "30Gi" {
			t.Errorf("size %s: override dind-data = %s, want 30Gi", size, q.String())
		}
	}
	for size, want := range map[string]string{"s": "20Gi", "l": "50Gi"} {
		cfg := dockerTestConfig()
		cfg.EphemeralDataSize = "35Gi"
		w := protocol.DesiredWorker{ID: "abc", Template: "base", Size: size, Docker: true, Ephemeral: true}
		got := pvcSizes(t, cfg, w, testSpec(t, "base", size))
		if q := got[dataPVCName("abc")]; q.String() != "35Gi" {
			t.Errorf("size %s: run-bound data = %s, want exactly 35Gi", size, q.String())
		}
		if q := got[dindDataPVCName("abc")]; q.String() != want {
			t.Errorf("size %s: run-bound dind-data = %s, want preset %s", size, q.String(), want)
		}
	}
}

// A plain worker has no dind claim; nix stays 20Gi for a docker worker.
func TestRenderPVCsPlainHasNoDinDClaimAndNixUnchanged(t *testing.T) {
	plain := protocol.DesiredWorker{ID: "abc", Template: "base", Size: "l"}
	got := pvcSizes(t, testConfig(), plain, testSpec(t, "base", "l"))
	if _, ok := got[dindDataPVCName("abc")]; ok {
		t.Error("a plain worker rendered a dind-data claim")
	}
	dock := protocol.DesiredWorker{ID: "abc", Template: "base", Size: "l", Docker: true}
	got = pvcSizes(t, dockerTestConfig(), dock, testSpec(t, "base", "l"))
	if q := got[nixPVCName("abc")]; q.String() != "20Gi" {
		t.Errorf("nix = %s, want 20Gi", q.String())
	}
}

// No size quantity may enter the pod template or the spec hash: changing the cluster
// override or the preset's data sizes must never roll a worker.
func TestDinDAndDataSizesNeverEnterThePodTemplateOrSpecHash(t *testing.T) {
	w := protocol.DesiredWorker{ID: "abc", Template: "base", Size: "l", Docker: true}
	spec := testSpec(t, "base", "l")
	cfgA := dockerTestConfig()
	cfgA.DinDDataSize = "20Gi"
	cfgB := dockerTestConfig()

	spec2 := spec
	spec2.Size.DinDDataSize = resource.MustParse("7Gi")
	spec2.Size.DataSize = resource.MustParse("9Gi")

	a := RenderDeployment(cfgA, w, spec)
	b := RenderDeployment(cfgB, w, spec2)
	if !reflect.DeepEqual(a.Spec.Template, b.Spec.Template) {
		t.Error("pod template differs when only dind/data PVC sizes differ")
	}
	if ha, hb := a.Spec.Template.Annotations[AnnotationSpecHash], b.Spec.Template.Annotations[AnnotationSpecHash]; ha == "" || ha != hb {
		t.Errorf("spec hash differs or is empty: %q vs %q", ha, hb)
	}
}

// Every preset's default must fit the chart's docker ceiling is covered by
// TestDinDDataPresetsFitTheChartsDockerLimitRangeMax. Here: the chart's empty-path
// literal must equal the maximum over the presets, so the render-time guard checks the
// worst case an empty dindDataSize can claim.
func TestDinDDataChartLiteralIsTheMaxPresetDefault(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "chart", "templates", "worker-invariants.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: reads a test-controlled chart path (not user input)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := regexp.MustCompile(`\$dindSize = "([^"]+)"`).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("no `$dindSize = \"...\"` substitution literal found in %s; the guard's empty path cannot be tied to the preset table", path)
	}
	lit := resource.MustParse(string(m[1]))
	max := resource.Quantity{}
	names := preset.SizeNames()
	if len(names) == 0 {
		t.Fatal("no preset sizes")
	}
	for _, size := range names {
		spec := testSpec(t, "base", size)
		if spec.Size.DinDDataSize.Cmp(max) > 0 {
			max = spec.Size.DinDDataSize.DeepCopy()
		}
	}
	if lit.Cmp(max) != 0 {
		t.Errorf("worker-invariants.yaml substitutes %s for an empty dindDataSize, but the largest preset DinDDataSize is %s; move them together",
			lit.String(), max.String())
	}
}
