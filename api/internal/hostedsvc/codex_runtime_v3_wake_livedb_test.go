package hostedsvc

import (
	"testing"
	"time"
)

func TestCodexRuntimeV3HostedSuppressionLiveDB(t *testing.T) {
	for _, current := range []bool{false, true} {
		name := "old-v2"
		if current {
			name = "current-v3"
		}
		t.Run(name, func(t *testing.T) {
			e := newWakeEnv(t, true)
			w := e.sleeper([]string{"jvm"}, []string{"codex_harness_v1", "codex_runtime_v2", "codex_runtime_v3", "codex_custom_model_v1"}, time.Hour)
			run := e.queuedRun("jvm")
			e.exec("UPDATE runs SET harness='codex' WHERE id=$1", run)
			if e.wake() != 1 {
				t.Fatal("current-runtime control did not create wake")
			}
			if !current {
				e.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'codex_runtime_v3') WHERE id=$1", w)
			}
			want := int64(1)
			if current {
				want = 0
			}
			if got := e.provision(); got != want || int64(e.ephemeralWorkers()) != want {
				t.Fatalf("hosted suppression current=%v provisioned=%d workers=%d want=%d", current, got, e.ephemeralWorkers(), want)
			}
		})
	}
}

// Exercise the hosted caller of ListHibernationWakeDemand, not a predicate mirror.
func TestCodexRuntimeV3HostedWakeLiveDB(t *testing.T) {
	e := newWakeEnv(t, false)
	old := []string{"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1"}
	w := e.sleeper(nil, old, time.Hour)
	run := e.queuedRun()
	e.exec("UPDATE runs SET harness='codex' WHERE id=$1", run)
	if n := e.wake(); n != 0 {
		t.Fatalf("hosted caller woke v2-only sleeper: %d", n)
	}
	if e.state(w) != "hibernated" || e.wakeRows() != 0 {
		t.Fatal("v2-only sleeper created wake side effects")
	}
	e.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,'codex_runtime_v3') WHERE id=$1", w)
	if n := e.wake(); n != 1 {
		t.Fatalf("hosted caller did not wake upgraded sleeper: %d", n)
	}
	if e.state(w) != "waking" || e.wakeRows() != 1 {
		t.Fatal("current runtime missing hosted wake result")
	}
}
