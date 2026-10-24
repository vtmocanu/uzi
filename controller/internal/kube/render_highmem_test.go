package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

// Issue #3383: the l-highmem preset renders 1 CPU / 24Gi requests, 4 CPU / 32Gi limits.
// A persistent worker keeps the preset's 50Gi /data; a run-bound one gets the run-bound
// override (40Gi default) like every other preset.
func TestHighMemoryPresetRendersOnEveryTier(t *testing.T) {
	const size = "l-highmem"
	for _, tc := range []struct {
		name      string
		docker    bool
		isolated  bool
		ephemeral bool
		wantData  string
	}{
		{"persistent/plain", false, false, false, "50Gi"},
		{"persistent/docker", true, false, false, "50Gi"},
		{"ephemeral/plain", false, false, true, "40Gi"},
		{"ephemeral/docker", true, false, true, "40Gi"},
		{"ephemeral/isolated", false, true, true, "40Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := laneTestConfig()
			w := protocol.DesiredWorker{
				ID: "highmem", Template: "base", Size: size,
				Docker: tc.docker, Isolated: tc.isolated, Ephemeral: tc.ephemeral,
			}
			spec := testSpec(t, "base", size)
			pod := RenderDeployment(cfg, w, spec).Spec.Template.Spec
			worker := containerByName(t, pod.Containers, workerContainerName)
			for _, want := range []struct {
				kind     string
				got      resource.Quantity
				expected string
			}{
				{"cpu request", worker.Resources.Requests[corev1.ResourceCPU], "1"},
				{"cpu limit", worker.Resources.Limits[corev1.ResourceCPU], "4"},
				{"memory request", worker.Resources.Requests[corev1.ResourceMemory], "24Gi"},
				{"memory limit", worker.Resources.Limits[corev1.ResourceMemory], "32Gi"},
			} {
				if want.got.Cmp(resource.MustParse(want.expected)) != 0 {
					t.Errorf("worker %s = %s, want %s", want.kind, want.got.String(), want.expected)
				}
			}
			data, ok := pvcSizes(t, cfg, w, spec)[dataPVCName(w.ID)]
			if !ok {
				t.Fatal("no -data claim rendered")
			}
			if data.Cmp(resource.MustParse(tc.wantData)) != 0 {
				t.Errorf("/data = %s, want %s", data.String(), tc.wantData)
			}
		})
	}
}
