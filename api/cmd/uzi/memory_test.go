package main

import (
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestMemoryList(t *testing.T) {
	fc := &uzicli.FakeClient{Memories: []apitypes.AgentMemoryDTO{
		{ID: "m1", RepoID: "r1", RepoName: "org/repo", Title: "build flag", Body: "use -tags foo", CreatedAt: time.Unix(0, 0).UTC()},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "memory", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "m1") || !strings.Contains(out, "org/repo") || !strings.Contains(out, "build flag") {
		t.Errorf("list table = %q, want id/repo/title", out)
	}
}

func TestMemoryListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Memories: []apitypes.AgentMemoryDTO{
		{ID: "m1", RepoID: "r1", RepoName: "org/repo", Title: "t", Body: "b", CreatedAt: time.Unix(0, 0).UTC()},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "memory", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "m1"`) || !strings.Contains(out, `"repo_name": "org/repo"`) {
		t.Errorf("list --json = %q, want id/repo_name", out)
	}
}

func TestMemoryRm(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "memory", "rm", "m1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedMemoryID != "m1" {
		t.Fatalf("rm called DeleteMemory(%q), want m1", fc.LastDeletedMemoryID)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("rm output = %q, want a 'removed' confirmation", out)
	}
}

func TestMemoryRmJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "memory", "rm", "m1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"deleted": true`) || !strings.Contains(out, `"id": "m1"`) {
		t.Errorf("rm --json = %q, want deleted/id", out)
	}
}

// A foreign/unknown id is a 404 (exit 4); the CLI must surface it.
func TestMemoryRmNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitNotFound, "memory not found")}
	_, _, code := runCLI(t, fakeEnv(fc), "memory", "rm", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

func TestMemoryRmRequiresArg(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "memory", "rm")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDeletedMemoryID != "" {
		t.Error("rm with no id must not call DeleteMemory")
	}
}
