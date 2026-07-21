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

// -------------------------------------------------------------------------
// worker set-token (PRD #104 M3)
// -------------------------------------------------------------------------

func TestWorkerSetToken(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "console-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "console-key" {
		t.Fatalf("set-token called SetWorkerToken(%q,%q), want (w1,console-key)",
			fc.LastSetTokenWorkerID, fc.LastSetTokenLabel)
	}
	if !strings.Contains(out, "console-key") {
		t.Errorf("set-token output = %q, want it to name the token", out)
	}
}

// --default clears the binding, which the client expresses as an empty label.
func TestWorkerSetTokenDefault(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "--default")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "" {
		t.Fatalf("--default called SetWorkerToken(%q,%q), want (w1,\"\")",
			fc.LastSetTokenWorkerID, fc.LastSetTokenLabel)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("--default output = %q, want it to say the worker uses the default", out)
	}
}

// A label AND --default is ambiguous; so is neither. Both are usage errors rather
// than a silent choice of one meaning over the other.
func TestWorkerSetTokenAmbiguousArgs(t *testing.T) {
	for _, args := range [][]string{
		{"worker", "set-token", "w1", "console-key", "--default"},
		{"worker", "set-token", "w1"},
	} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (usage)", args, code, uzicli.ExitUsage)
		}
		if fc.LastSetTokenWorkerID != "" {
			t.Errorf("%v: must not reach the API", args)
		}
	}
}

// An unknown label is a 400 → exit 3; an unknown worker is a 404 → exit 4. Both
// must reach the caller as their documented codes rather than a generic failure.
func TestWorkerSetTokenErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown label", uzicli.Exitf(uzicli.ExitAuth, "no Anthropic token with that label"), uzicli.ExitAuth},
		{"unknown worker", uzicli.Exitf(uzicli.ExitNotFound, "worker not found"), uzicli.ExitNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Err: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "some-label")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}
