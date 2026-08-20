package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestRepoList(t *testing.T) {
	fc := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{
		{ID: "r1", PathWithNamespace: "org/repo", Enabled: false},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "r1") || !strings.Contains(out, "org/repo") {
		t.Errorf("list table = %q, want id/path", out)
	}
}

// --force skips the prompt and calls DeleteRepo with the id.
func TestRepoRemoveForce(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "remove", "r1", "--force")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedRepoID != "r1" {
		t.Fatalf("remove called DeleteRepo(%q), want r1", fc.LastDeletedRepoID)
	}
	if !strings.Contains(out, "Removed repo r1") {
		t.Errorf("remove output = %q, want a 'Removed repo r1' confirmation", out)
	}
}

// -f is the short form of --force.
func TestRepoRemoveForceShort(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "repo", "remove", "r9", "-f")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedRepoID != "r9" {
		t.Fatalf("-f remove called DeleteRepo(%q), want r9", fc.LastDeletedRepoID)
	}
}

// Without --force, a declined answer ("n") must NOT call DeleteRepo — the confirm
// gate is the destructive-action guard for the interactive path.
func TestRepoRemoveDeclined(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("n\n")
	_, errb, code := runCLI(t, env, "repo", "remove", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (declined is not an error)", code)
	}
	if fc.LastDeletedRepoID != "" {
		t.Fatalf("declined remove still called DeleteRepo(%q), want no call", fc.LastDeletedRepoID)
	}
	if !strings.Contains(errb, "aborted") {
		t.Errorf("declined remove stderr = %q, want 'aborted'", errb)
	}
}

// An empty answer (bare Enter / EOF) is the safe default: it declines.
func TestRepoRemoveEmptyDeclines(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("")
	_, _, code := runCLI(t, env, "repo", "remove", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedRepoID != "" {
		t.Fatalf("empty-answer remove called DeleteRepo(%q), want no call", fc.LastDeletedRepoID)
	}
}

// Without --force, an explicit "y" confirms and calls DeleteRepo.
func TestRepoRemoveConfirmed(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("y\n")
	_, _, code := runCLI(t, env, "repo", "remove", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedRepoID != "r1" {
		t.Fatalf("confirmed remove called DeleteRepo(%q), want r1", fc.LastDeletedRepoID)
	}
}

func TestRepoRemoveForceJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "remove", "r1", "--force", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"removed": true`) || !strings.Contains(out, `"id": "r1"`) {
		t.Errorf("remove --json = %q, want removed/id", out)
	}
}

// A 409 (enabled repo, or one with an in-flight run) is surfaced verbatim via the
// exit code the server maps to ExitConflict.
func TestRepoRemoveConflict(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "disable this repo before removing it")}
	_, errb, code := runCLI(t, fakeEnv(fc), "repo", "remove", "r1", "--force")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want ExitConflict (%d)", code, uzicli.ExitConflict)
	}
	if !strings.Contains(errb, "disable this repo before removing it") {
		t.Errorf("conflict stderr = %q, want the server message", errb)
	}
}

// A foreign/unknown id is a 404 (exit 4); the CLI must surface it.
func TestRepoRemoveNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitNotFound, "repo not found")}
	_, _, code := runCLI(t, fakeEnv(fc), "repo", "remove", "r1", "--force")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want ExitNotFound (%d)", code, uzicli.ExitNotFound)
	}
}
