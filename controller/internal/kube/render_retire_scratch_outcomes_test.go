package kube

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const retireOutcomesEnvName = "UZI_RETIRE_SCRATCH_OUTCOMES"

func TestRetireScratchOutcomesTemplate(t *testing.T) {
	workers := map[string]protocol.DesiredWorker{
		"plain":  {ID: canaryA},
		"docker": {ID: canaryA, Docker: true},
	}
	for name, w := range workers {
		w.Template, w.Size = "base", "m"
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t, "base", "m")
			def := dockerTestConfig()
			off := def
			off.RetireScratchOutcomesDisabled = true

			defEnv := canaryWorkerEnv(t, podTemplate(def, w, spec))
			for _, e := range defEnv {
				if e.Name == retireOutcomesEnvName {
					t.Fatalf("default render carries %s: %+v", retireOutcomesEnvName, e)
				}
			}

			offTpl := podTemplate(off, w, spec)
			count := 0
			for i := range offTpl.Spec.Containers {
				c := &offTpl.Spec.Containers[i]
				kept := c.Env[:0:0]
				for _, e := range c.Env {
					if e.Name == retireOutcomesEnvName {
						count++
						if e.Value != "0" || e.ValueFrom != nil || c.Name != workerContainerName {
							t.Fatalf("unexpected env: %+v in %s", e, c.Name)
						}
						continue
					}
					kept = append(kept, e)
				}
				c.Env = kept
			}
			if count != 1 {
				t.Fatalf("opt-out env count=%d want 1", count)
			}
			// Apart from the single env, the disabled template matches the default one
			// except for the spec-hash annotation.
			defTpl := podTemplate(def, w, spec)
			if SpecHashOf(def, w, spec) == SpecHashOf(off, w, spec) {
				t.Fatal("opt-out must change the spec hash so workers pick it up")
			}
			delete(offTpl.Annotations, AnnotationSpecHash)
			delete(defTpl.Annotations, AnnotationSpecHash)
			if !reflect.DeepEqual(defTpl, offTpl) {
				t.Fatal("opt-out changed more than the single env")
			}
		})
	}
}
