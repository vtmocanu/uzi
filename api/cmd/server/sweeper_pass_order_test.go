package main

import (
	"os"
	"strings"
	"testing"
)

// TestJobUnservablePassRunsBeforeEphemeralReap pins the PRD #1908 D-A2 ordering: the sweeper runs
// its passes in slice order, and ephemeral_job_unservable_fail must precede ephemeral_workers_reap.
// The reap would otherwise delete a never-booted ephemeral worker silently and leave the
// still-queued job to be re-provisioned every deadline instead of failing it with a typed origin.
//
// The passes are an inline slice literal in run(), so this is a source-order scan of main.go
// rather than a call into a pass-list function (factoring one out of run() is a large refactor).
func TestJobUnservablePassRunsBeforeEphemeralReap(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	fail := strings.Index(text, `Name: "ephemeral_job_unservable_fail"`)
	reap := strings.Index(text, `Name: "ephemeral_workers_reap"`)
	if fail < 0 || reap < 0 {
		t.Fatalf("pass names not found in main.go (fail at %d, reap at %d); update this test if they were renamed", fail, reap)
	}
	if fail > reap {
		t.Fatal("ephemeral_job_unservable_fail must be listed BEFORE ephemeral_workers_reap (PRD #1908 D-A2)")
	}
}

// TestHibernationWakePassRunsBeforeEphemeralProvision pins PRD #4205 M5: the demand-wake pass must be
// listed before ephemeral_workers_provision so a sleeping worker is used first and provisioning
// sees the recorded wake that suppresses a duplicate ephemeral worker on the same tick. Like its
// sibling above it is a source-order scan of the inline pass slice in main.go.
func TestHibernationWakePassRunsBeforeEphemeralProvision(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	wake := strings.Index(text, `Name: "worker_hibernation_wake"`)
	provision := strings.Index(text, `Name: "ephemeral_workers_provision"`)
	if wake < 0 || provision < 0 {
		t.Fatalf("pass names not found in main.go (wake at %d, provision at %d); update this test if they were renamed", wake, provision)
	}
	if wake > provision {
		t.Fatal("worker_hibernation_wake must be listed BEFORE ephemeral_workers_provision (PRD #4205 M5)")
	}
}

// TestHibernationAutoPassRunsBetweenSettleAndWake pins issue #4207: the automatic hibernation pass
// must be listed after worker_hibernation_settle (a wake settled this tick has stamped its grace
// anchor) and before worker_hibernation_wake. A source-order scan of the inline pass slice in
// main.go, like its siblings above.
func TestHibernationAutoPassRunsBetweenSettleAndWake(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	settle := strings.Index(text, `Name: "worker_hibernation_settle"`)
	auto := strings.Index(text, `Name: "worker_hibernation_auto"`)
	wake := strings.Index(text, `Name: "worker_hibernation_wake"`)
	if settle < 0 || auto < 0 || wake < 0 {
		t.Fatalf("pass names not found in main.go (settle %d, auto %d, wake %d); update this test if they were renamed", settle, auto, wake)
	}
	if settle >= auto || auto >= wake {
		t.Fatal("worker_hibernation_auto must be listed after worker_hibernation_settle and before worker_hibernation_wake (issue #4207)")
	}
}

// TestRunEnvRetiredSweepIsRegisteredNextToTerminalPreservation pins PRD #4416 M2: the retired-version
// sweeper is registered, and sits right after terminal_preservation (the run-bound custody passes) and
// before prerequisite_closure. A source-order scan of the inline pass slice in main.go, like its siblings.
func TestRunEnvRetiredSweepIsRegisteredNextToTerminalPreservation(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	terminal := strings.Index(text, `Name: "terminal_preservation"`)
	sweep := strings.Index(text, `Name: "run_env_retired_sweep"`)
	closure := strings.Index(text, `Name: "prerequisite_closure"`)
	if terminal < 0 || sweep < 0 || closure < 0 {
		t.Fatalf("pass names not found in main.go (terminal %d, sweep %d, closure %d); update this test if they were renamed", terminal, sweep, closure)
	}
	if terminal >= sweep || sweep >= closure {
		t.Fatal("run_env_retired_sweep must be listed after terminal_preservation and before prerequisite_closure (PRD #4416 M2)")
	}
	if strings.Count(text, `Name: "run_env_retired_sweep"`) != 1 {
		t.Fatal("run_env_retired_sweep must be registered exactly once")
	}
}
