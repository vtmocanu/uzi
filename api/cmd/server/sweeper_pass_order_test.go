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
