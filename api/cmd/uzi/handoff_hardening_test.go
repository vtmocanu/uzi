package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

var sshCommandGitCall = []string{"config", "--get", "core.sshCommand"}
var receivePackGitCall = []string{"config", "--get", "remote.origin.receivepack"}

// Origins whose host, user or port could differ between what we verify and what git
// connects to, or could forge a line of ssh -G output, are refused outright.
func TestParseRemoteRefusesSmuggledOrigins(t *testing.T) {
	for _, in := range []string{
		"ssh://attacker.example%2F@github.com/victim/repo.git",
		"ssh://git@github.com%2Fx/victim/repo.git",
		"ssh://a:b@github.com/victim/repo.git",
		"ssh://a:@github.com/victim/repo.git",
		"x\nhostname github.com@attacker.example:victim/repo.git",
		"ssh://x%0Ahostname%20github.com@attacker.example/victim/repo.git",
		"x\r\nhostname github.com@attacker.example:victim/repo.git",
		"git@host\x00name:victim/repo.git",
		"git@host\x7f:victim/repo.git",
		"a\tb@github.com:victim/repo.git",
		"git@github .com:victim/repo.git",
	} {
		got, err := parseRemote(in)
		if err == nil {
			t.Errorf("parseRemote(%q) = %+v, want refusal", in, got)
			continue
		}
		if strings.ContainsAny(err.Error(), "\n\r\x00\x7f\t") {
			t.Errorf("refusal for %q echoes a raw control character: %q", in, err.Error())
		}
	}
}

func TestHandoffRefusesNewlineUserWithoutCallingSSH(t *testing.T) {
	for _, origin := range []string{
		"x\nhostname github.com@attacker.example:acme/widgets.git",
		"ssh://x%0Ahostname%20github.com@attacker.example/acme/widgets.git",
	} {
		fc, rec, _, code := runHostCase(t, hostCase{origin: origin})
		if code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
			t.Errorf("origin %q: exit=%d repo=%q, want usage refusal", origin, code, fc.LastCreateTaskRepoID)
		}
		if n := len(rec.sshCfg.calls); n != 0 {
			t.Errorf("origin %q: ssh -G called %d times, want 0", origin, n)
		}
	}
}

// The hostname is accepted only from exactly one two-field line free of control characters.
func TestHandoffRejectsAmbiguousSSHConfigOutput(t *testing.T) {
	for name, out := range map[string]string{
		"two hostname lines":    "hostname github.com\nhostname github.com\n",
		"forged then real":      "user x\nhostname evil.example\nhostname github.com\n",
		"three-field line":      "hostname github.com extra\n",
		"hostname with no host": "hostname\n",
		"control char in value": "hostname github.com\x7f\n",
	} {
		t.Run(name, func(t *testing.T) {
			fc, rec, _, code := runHostCase(t, hostCase{origin: "git@github.com:acme/widgets.git", stubOut: out})
			if code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
				t.Fatalf("exit=%d repo=%q, want non-match", code, fc.LastCreateTaskRepoID)
			}
			if n := len(rec.sshCfg.calls); n != 1 {
				t.Errorf("ssh -G calls = %d, want 1", n)
			}
		})
	}
}

// A path-equal repo whose host differs must not advise --repo: no --repo can succeed.
func TestHandoffHostMismatchAdviceOmitsRepoFlag(t *testing.T) {
	_, _, stderr, code := runHostCase(t, hostCase{origin: "https://other.example/acme/widgets.git"})
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want usage", code)
	}
	if strings.Contains(stderr, "pass --repo") || !strings.Contains(stderr, "https remote") {
		t.Errorf("host-mismatch advice wrong:\n%s", stderr)
	}
	_, _, stderr, _ = runHostCase(t, hostCase{origin: "https://github.com/other/thing.git"})
	if !strings.Contains(stderr, "pass --repo") {
		t.Errorf("a pure path miss should still advise --repo:\n%s", stderr)
	}
}

// ssh facts (env vars, core.sshCommand, ssh -G) are only gathered when an ssh origin can
// still match a connected repo; they must never be gathered for an https origin.
func TestHandoffSSHFactsGatheredLazily(t *testing.T) {
	setup := func(origin string) (*uzicli.FakeClient, *handoffRecorder, Env, *int) {
		fc := &uzicli.FakeClient{
			Repos:          []apitypes.RepoDTO{{ID: "p1", PathWithNamespace: "acme/widgets", WebURL: "https://github.com/acme/widgets"}},
			CreatedTaskRun: taskRun("rl", "uzi/task/rl"),
			DispatchedRun:  taskRun("rl", "uzi/task/rl"),
		}
		rec := &handoffRecorder{gitOut: map[string]string{"remote get-url --push --all origin": origin}}
		env, _ := handoffEnv(fc, rec)
		lookups := 0
		env.LookupEnv = func(string) (string, bool) { lookups++; return "", false }
		return fc, rec, env, &lookups
	}
	assertUntouched := func(t *testing.T, rec *handoffRecorder, lookups *int) {
		t.Helper()
		if n := len(rec.sshCfg.calls); n != 0 {
			t.Errorf("ssh -G called %d times, want 0", n)
		}
		if *lookups != 0 {
			t.Errorf("LookupEnv called %d times, want 0", *lookups)
		}
		if hasGitCall(rec, sshCommandGitCall) {
			t.Errorf("git config --get core.sshCommand must not run: %v", rec.gitCalls)
		}
		if hasGitCall(rec, receivePackGitCall) {
			t.Errorf("git config --get remote.origin.receivepack must not run: %v", rec.gitCalls)
		}
	}

	t.Run("scp origin with no path-equal repo", func(t *testing.T) {
		fc, rec, env, lookups := setup("git@github.com:other/thing.git")
		if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
			t.Fatalf("exit=%d repo=%q, want usage refusal", code, fc.LastCreateTaskRepoID)
		}
		assertUntouched(t, rec, lookups)
	})
	t.Run("--repo names a path-mismatched repo with an scp origin", func(t *testing.T) {
		fc, rec, env, lookups := setup("git@github.com:other/thing.git")
		if _, _, code := runCLI(t, env, "handoff", "--repo", "p1", "-m", "x"); code != uzicli.ExitUsage || fc.LastCreateTaskRepoID != "" {
			t.Fatalf("exit=%d repo=%q, want usage refusal", code, fc.LastCreateTaskRepoID)
		}
		assertUntouched(t, rec, lookups)
	})
	t.Run("https origin", func(t *testing.T) {
		fc, rec, env, lookups := setup("https://github.com/acme/widgets.git")
		if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "p1" {
			t.Fatalf("exit=%d repo=%q, want a p1 match", code, fc.LastCreateTaskRepoID)
		}
		assertUntouched(t, rec, lookups)
	})
}
