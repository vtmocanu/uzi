package kube

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vtmocanu/uzi/controller/internal/preset"
)

func TestRenderCPUSlots(t *testing.T) {
	sizes := preset.SizeNames()
	sort.Strings(sizes)
	expected := map[string]struct{ request, limit string }{
		"s": {"250m", "1"},
		"m": {"1", "4"},
		"l": {"1", "4"},
	}
	for _, size := range sizes {
		cpu, ok := expected[size]
		if !ok {
			t.Fatalf("size %s needs explicit CPU expectations", size)
		}
		for _, slots := range []int{0, 1, 2, 256} {
			for _, ephemeral := range []bool{false, true} {
				for _, tier := range []string{"plain", "docker", "isolated"} {
					t.Run(fmt.Sprintf("%s/slots=%d/ephemeral=%t/%s", size, slots, ephemeral, tier), func(t *testing.T) {
						cfg := laneTestConfig()
						cfg.MaxConcurrentRuns = slots
						w := desired("cpu-slots")
						w.Size = size
						w.Ephemeral = ephemeral
						w.Docker = tier == "docker"
						w.Isolated = tier == "isolated"
						spec := testSpec(t, "base", size)
						pod := RenderDeployment(cfg, w, spec).Spec.Template.Spec
						worker := pod.Containers[0]
						request := worker.Resources.Requests[corev1.ResourceCPU]
						limit := worker.Resources.Limits[corev1.ResourceCPU]
						if request.Cmp(resource.MustParse(cpu.request)) != 0 || limit.Cmp(resource.MustParse(cpu.limit)) != 0 {
							t.Errorf("worker CPU request/limit = %s/%s; want %s/%s", request.String(), limit.String(), cpu.request, cpu.limit)
						}
						wantSlots := slots
						if slots < 1 {
							wantSlots = 1
						}
						found := 0
						for _, env := range worker.Env {
							if env.Name == "WORKER_MAX_CONCURRENT_RUNS" {
								found++
								if env.Value != strconv.Itoa(wantSlots) {
									t.Errorf("run slots = %q; want %d", env.Value, wantSlots)
								}
							}
						}
						if found != 1 {
							t.Errorf("run slot env occurrences = %d; want 1", found)
						}
						cfg.MaxConcurrentRuns = 1
						baseline := RenderDeployment(cfg, w, spec).Spec.Template.Spec
						for _, group := range []struct {
							name      string
							got, want []corev1.Container
						}{
							{"init", pod.InitContainers, baseline.InitContainers},
							{"regular", pod.Containers, baseline.Containers},
						} {
							if len(group.got) != len(group.want) {
								t.Fatalf("%s container count = %d; baseline %d", group.name, len(group.got), len(group.want))
							}
							for i, container := range group.got {
								if container.Name != group.want[i].Name || !reflect.DeepEqual(container.Resources, group.want[i].Resources) {
									t.Errorf("%s container %s resources = %+v; slots=1 baseline %s = %+v", group.name, container.Name, container.Resources, group.want[i].Name, group.want[i].Resources)
								}
							}
						}
					})
				}
			}
		}
	}
}
