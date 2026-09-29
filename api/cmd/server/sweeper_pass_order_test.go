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
