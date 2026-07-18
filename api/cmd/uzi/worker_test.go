package main

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerRm(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedWorkerID != "w1" {
		t.Fatalf("rm called DeleteWorker(%q), want w1", fc.LastDeletedWorkerID)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("rm output = %q, want a 'removed' confirmation", out)
	}
}

func TestWorkerRmJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"deleted": true`) || !strings.Contains(out, `"id": "w1"`) {
		t.Errorf("rm --json = %q, want deleted/id", out)
	}
}

// A worker with active runs is a 409 (exit 5); the CLI must surface it.
func TestWorkerRmConflict(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "worker has active runs")}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
}

func TestWorkerRmRequiresArg(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDeletedWorkerID != "" {
		t.Error("rm with no id must not call DeleteWorker")
	}
}
