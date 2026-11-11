package kube

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const scratchEnvName = "UZI_EPOCH_SCRATCH_DELETION"

func TestEpochScratchTemplate(t *testing.T) {
	for _, w := range []protocol.DesiredWorker{
		{ID: canaryA}, {ID: canaryA, Isolated: true},
		{ID: canaryA, Docker: true, Isolated: true},
		{ID: canaryA, Docker: true, Ephemeral: true},
		{ID: canaryA, Docker: true},
	} {
		w.Template, w.Size = "base", "m"
		for _, selector := range []string{"", canaryA, canaryB} {
			t.Run(w.ID+selector, func(t *testing.T) {
				off := dockerTestConfig()
				off.IsolatedNamespace = "isolated"
				off.EpochReclaimDeletionCanaryID = selector
				on := off
				on.EpochScratchDeletion = true
				spec := testSpec(t, "base", "m")
				a, b := podTemplate(off, w, spec), podTemplate(on, w, spec)
				eligible := w.Docker && !w.Isolated && !w.Ephemeral
				if !eligible {
					if !reflect.DeepEqual(a, b) || SpecHashOf(off, w, spec) != SpecHashOf(on, w, spec) {
						t.Fatal("ineligible template/hash changed")
					}
					return
				}
				if SpecHashOf(off, w, spec) == SpecHashOf(on, w, spec) {
					t.Fatal("eligible hash unchanged")
				}
				count := 0
				for i := range b.Spec.Containers {
					c := &b.Spec.Containers[i]
					kept := c.Env[:0:0]
					for _, e := range c.Env {
						if e.Name == scratchEnvName {
							count++
							if e.Value != "1" || e.ValueFrom != nil || c.Name != workerContainerName {
								t.Fatalf("unexpected scratch env: %+v", e)
							}
							continue
						}
						kept = append(kept, e)
					}
					c.Env = kept
				}
				if count != 1 || !reflect.DeepEqual(a, b) {
					t.Fatal("change exceeds the single scratch env")
				}
				for _, enabled := range []bool{false, true} {
					cfg := off
					cfg.EpochScratchDeletion = enabled
					env := canaryWorkerEnv(t, podTemplate(cfg, w, spec))
					attempt := ""
					for _, e := range env {
						if e.Name == canaryEnvName {
							attempt = e.Value
						}
					}
					want := ""
					if selector == w.ID {
						want = "1"
					}
					if attempt != want {
						t.Fatalf("attempt env=%q want %q scratch=%v", attempt, want, enabled)
					}
				}
			})
		}
	}
}
