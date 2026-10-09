package kube

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestRenderCrossCheckSlots(t *testing.T) {
	for _, slots := range []int{0, 1, 16} {
		for _, ephemeral := range []bool{false, true} {
			for _, docker := range []bool{false, true} {
				t.Run("slots="+strconv.Itoa(slots)+"/ephemeral="+strconv.FormatBool(ephemeral)+"/docker="+strconv.FormatBool(docker), func(t *testing.T) {
					cfg := dockerTestConfig()
					cfg.CrossCheckSlots = slots
					cfg.MaxConcurrentRuns = 3
					w := desired("abc")
					w.Ephemeral = ephemeral
					w.Docker = docker
					spec := testSpec(t, "base", "m")
					dep := RenderDeployment(cfg, w, spec)
					env := map[string]string{}
					for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
						if _, exists := env[e.Name]; exists {
							t.Fatalf("duplicate env %s", e.Name)
						}
						env[e.Name] = e.Value
					}
					if got := env["WORKER_CROSS_CHECK_SLOTS"]; got != strconv.Itoa(slots) {
						t.Errorf("cross-check env = %q, want %d", got, slots)
					}
					if got := env["WORKER_MAX_CONCURRENT_RUNS"]; got != "3" {
						t.Errorf("run cap = %q, want unchanged 3", got)
					}
					other := cfg
					other.CrossCheckSlots = (slots + 1) % 17
					if SpecHashOf(cfg, w, spec) == SpecHashOf(other, w, spec) {
						t.Error("slot change must change the spec hash to roll existing workers")
					}
					if !reflect.DeepEqual(RenderPVCs(cfg, w, spec), RenderPVCs(other, w, spec)) {
						t.Error("slot change changed PVC specs")
					}
				})
			}
		}
	}
}

func TestRenderZeroConfigDisablesCrossCheckLane(t *testing.T) {
	dep := RenderDeployment(RenderConfig{}, protocol.DesiredWorker{ID: "abc"}, testSpec(t, "base", "m"))
	env := map[string]string{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["WORKER_CROSS_CHECK_SLOTS"] != "0" || env["WORKER_MAX_CONCURRENT_RUNS"] != "1" {
		t.Fatalf("zero config env = %v, want cross-check off and run cap fallback 1", env)
	}
}
