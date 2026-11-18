package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Regression for #4449: an origin on another host must not resolve to a connected repo
// that merely has the same owner/repo path. These tests use only symbols that exist
// before the fix, so they fail (not fail to compile) against the old path-only match.
func hostRegressEnv() (*uzicli.FakeClient, *handoffRecorder, Env) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{{
			ID: "p1", PathWithNamespace: "acme/widgets", WebURL: "https://github.com/acme/widgets",
		}},
		CreatedTaskRun: taskRun("rh", "uzi/task/rh"),
		DispatchedRun:  taskRun("rh", "uzi/task/rh"),
	}
	rec := &handoffRecorder{gitOut: map[string]string{"remote get-url origin": "https://other.example/acme/widgets.git"}}
	env, _ := handoffEnv(fc, rec)
	return fc, rec, env
}

func TestHandoffHostRegressAutoMatchRefusesOtherHost(t *testing.T) {
	fc, _, env := hostRegressEnv()

	_, _, code := runCLI(t, env, "handoff", "-m", "x")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage): a same-path repo on another host must not match", code, uzicli.ExitUsage)
	}
	if fc.LastCreateTaskRepoID != "" {
		t.Errorf("no run may be created; repo = %q", fc.LastCreateTaskRepoID)
	}
}

func TestHandoffHostRegressRepoFlagRefusesOtherHost(t *testing.T) {
	fc, rec, env := hostRegressEnv()

	_, stderr, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastCreateTaskRepoID != "" {
		t.Errorf("no run may be created; repo = %q", fc.LastCreateTaskRepoID)
	}
	for _, s := range rec.seq {
		if s == "create" || s == "dispatch" || strings.HasPrefix(s, "git:push") {
			t.Errorf("a host mismatch must not create, push or dispatch: %v", rec.seq)
		}
	}
	if !strings.Contains(stderr, "other.example") || !strings.Contains(stderr, "github.com") {
		t.Errorf("error should name both hosts:\n%s", stderr)
	}
}
