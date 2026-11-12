package kube

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Seed both the Deployment and its running pod with the initial scratch setting.
func scratchCluster(t *testing.T, enabled bool, opts ...lmOpts) (*lmFixture, []protocol.DesiredWorker) {
	t.Helper()
	var objects []runtime.Object
	var workers []protocol.DesiredWorker
	for _, opt := range opts {
		objs, w := lmObjects(t, opt)
		if enabled {
			cfg := dockerTestConfig()
			cfg.EpochScratchDeletion = true
			tpl := RenderDeployment(cfg, w, testSpec(t, "base", "m")).Spec.Template
			for _, obj := range objs {
				switch o := obj.(type) {
				case *appsv1.Deployment:
					o.Spec.Template = tpl
				case *corev1.Pod:
					o.Annotations[AnnotationSpecHash] = tpl.Annotations[AnnotationSpecHash]
					for i := range o.Spec.Containers {
						if o.Spec.Containers[i].Name == workerContainerName {
							o.Spec.Containers[i].Env = append(o.Spec.Containers[i].Env, corev1.EnvVar{Name: scratchEnvName, Value: "1"})
						}
					}
				}
			}
		}
		objects = append(objects, objs...)
		workers = append(workers, w)
	}
	f := lmCluster(t, false, objects...)
	f.m.cfg.EpochScratchDeletion = enabled
	return f, workers
}

func TestEpochScratchReconcile(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, state := range []string{"idle", "busy", "custody"} {
			t.Run(fmt.Sprintf("initial=%v/%s", initial, state), func(t *testing.T) {
				opt := lmOpts{id: canaryA, layout: "new", promoted: true}
				if state == "custody" {
					opt.layout, opt.custody = "old", true
				}
				f, ws := scratchCluster(t, initial, opt)
				if state == "idle" {
					f.tickAll(t, ws...)
					if wasPatched(f.client) {
						t.Fatal("steady state patched")
					}
				}
				ws[0].Busy = state == "busy"
				f.m.cfg.EpochScratchDeletion = !initial
				f.tickAll(t, ws...)
				patches := templatePatches(f.client)
				if state == "idle" {
					if len(patches) != 1 || patches[0] != deploymentName(canaryA) {
						t.Fatalf("patches=%v", patches)
					}
					want := ""
					if !initial {
						want = "1"
					}
					if got := canaryDeploymentEnv(t, f, canaryA)[scratchEnvName]; got != want {
						t.Fatalf("scratch=%q want %q", got, want)
					}
				} else {
					if len(patches) != 0 {
						t.Fatalf("protected worker patched: %v", patches)
					}
					if state == "busy" && (len(f.cord.calls) != 1 || f.cord.calls[0] != canaryA) {
						t.Fatalf("cordons=%v", f.cord.calls)
					}
					if state == "custody" && !strings.Contains(f.logs.String(), "layout-migration-held-custody") {
						t.Fatalf("missing custody hold: %s", f.logs.String())
					}
				}
				if del := canaryPVCDeletes(f); len(del) != 0 {
					t.Fatalf("deleted PVCs: %v", del)
				}
			})
		}
	}
}

func TestEpochScratchFleetPacing(t *testing.T) {
	for _, initial := range []bool{false, true} {
		for _, scenario := range []string{"ordinary", "no-coverage", "busy-peers"} {
			t.Run(fmt.Sprintf("initial=%v/%s", initial, scenario), func(t *testing.T) {
				f, ws := scratchCluster(t, initial,
					lmOpts{id: canaryA, layout: "new", promoted: true},
					lmOpts{id: canaryB, layout: "new", promoted: true},
					lmOpts{id: "11111111-1111-1111-1111-111111111111", layout: "new", promoted: true})
				if scenario == "no-coverage" {
					for i := range ws {
						ws[i].ManualCordonedAt = &m5Now
					}
				}
				if scenario == "busy-peers" {
					ws[1].Busy, ws[2].Busy = true, true
				}
				f.m.cfg.EpochScratchDeletion = !initial
				f.tickAll(t, ws...)
				want := 1
				if scenario == "no-coverage" {
					want = 0
				}
				if got := templatePatches(f.client); len(got) != want {
					t.Fatalf("patches=%v want %d; logs=%s", got, want, f.logs.String())
				}
				if len(f.cord.calls) != 0 {
					t.Fatalf("unadmitted peers cordoned: %v", f.cord.calls)
				}
				// An unfinished replacement must keep consuming capacity on the next tick.
				f.tickAll(t, ws...)
				if got := templatePatches(f.client); len(got) != want {
					t.Fatalf("unfinished replacement admitted siblings: %v", got)
				}
				if del := canaryPVCDeletes(f); len(del) != 0 {
					t.Fatalf("deleted PVCs: %v", del)
				}
			})
		}
	}
}
