package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Handoff pushes to origin's PUSH destination, so that is what it must validate (#4470).

const legacyFetchKey = "remote get-url origin"

func pushEnv(pushListing string) (*uzicli.FakeClient, *handoffRecorder, Env) {
	fc := &uzicli.FakeClient{
		Repos:          []apitypes.RepoDTO{{ID: "p1", PathWithNamespace: "acme/widgets", WebURL: "https://github.com/acme/widgets"}},
		CreatedTaskRun: taskRun("rp", "uzi/task/rp"),
		DispatchedRun:  taskRun("rp", "uzi/task/rp"),
	}
	rec := &handoffRecorder{gitOut: map[string]string{pushDestKey: pushListing}}
	env, _ := handoffEnv(fc, rec)
	return fc, rec, env
}

func assertNothingSent(t *testing.T, fc *uzicli.FakeClient, rec *handoffRecorder) {
	t.Helper()
	if fc.LastCreateTaskRepoID != "" {
		t.Errorf("no run may be created; repo = %q", fc.LastCreateTaskRepoID)
	}
	for _, s := range rec.seq {
		if s == "create" || s == "dispatch" || strings.HasPrefix(s, "git:push") {
			t.Errorf("must not create, push or dispatch: %v", rec.seq)
		}
	}
}

// Regression: a push URL rewritten to another host (pushurl / pushInsteadOf) while the
// fetch URL still names the connected repo must be refused.
func TestHandoffRefusesPushDestinationOnOtherHost(t *testing.T) {
	t.Run("https", func(t *testing.T) {
		fc, rec, env := pushEnv("https://evil.example/acme/widgets.git\n")
		rec.gitOut[legacyFetchKey] = "https://github.com/acme/widgets.git"
		_, _, code := runCLI(t, env, "handoff", "-m", "x")
		if code != uzicli.ExitUsage {
			t.Fatalf("exit = %d, want usage", code)
		}
		assertNothingSent(t, fc, rec)
	})
	t.Run("ssh", func(t *testing.T) {
		fc, rec, env := pushEnv("git@evil.example:acme/widgets.git\n")
		rec.gitOut[legacyFetchKey] = "https://github.com/acme/widgets.git"
		rec.sshCfg.out = "hostname evil.example\n"
		_, _, code := runCLI(t, env, "handoff", "-m", "x")
		if code != uzicli.ExitUsage {
			t.Fatalf("exit = %d, want usage", code)
		}
		assertNothingSent(t, fc, rec)
	})
}

func TestHandoffRefusesMultiplePushDestinations(t *testing.T) {
	for name, listing := range map[string]string{
		"same":      "https://github.com/acme/widgets.git\nhttps://github.com/acme/widgets.git\n",
		"different": "https://github.com/acme/widgets.git\nhttps://github.com/acme/other.git\n",
	} {
		t.Run(name, func(t *testing.T) {
			fc, rec, env := pushEnv(listing)
			_, stderr, code := runCLI(t, env, "handoff", "-m", "x")
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want usage", code)
			}
			if !strings.Contains(stderr, "2 push destinations") || !strings.Contains(stderr, "single push destination") {
				t.Errorf("message should name the count and the single-destination rule:\n%s", stderr)
			}
			assertNothingSent(t, fc, rec)
		})
	}
}

func TestHandoffMatchesOnPushDestinationNotFetchURL(t *testing.T) {
	fc, rec, env := pushEnv("https://github.com/acme/widgets.git\n")
	rec.gitOut[legacyFetchKey] = "https://other.example/acme/widgets.git"
	if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "p1" {
		t.Fatalf("exit=%d repo=%q, want OK and p1", code, fc.LastCreateTaskRepoID)
	}
}

func TestHandoffRefusesEmptyOrFailingPushListing(t *testing.T) {
	for name, listing := range map[string]string{"empty": "", "blank lines": "\n\n"} {
		t.Run(name, func(t *testing.T) {
			fc, rec, env := pushEnv(listing)
			_, stderr, code := runCLI(t, env, "handoff", "-m", "x")
			if code != uzicli.ExitUsage {
				t.Fatalf("exit = %d, want usage", code)
			}
			if !strings.Contains(stderr, "0 push destinations") {
				t.Errorf("message should name the count:\n%s", stderr)
			}
			assertNothingSent(t, fc, rec)
		})
	}
	t.Run("command fails", func(t *testing.T) {
		fc, rec, env := pushEnv("")
		rec.gitErr[pushDestKey] = errors.New("boom")
		_, _, code := runCLI(t, env, "handoff", "-m", "x")
		if code != uzicli.ExitUsage {
			t.Fatalf("exit = %d, want usage", code)
		}
		assertNothingSent(t, fc, rec)
	})
}

func TestHandoffReceivePackRefusedForSSHOrigin(t *testing.T) {
	const sshOrigin = "git@github.com:acme/widgets.git\n"
	t.Run("set", func(t *testing.T) {
		fc, rec, env := pushEnv(sshOrigin)
		delete(rec.gitErr, receivePackKey)
		rec.gitOut[receivePackKey] = "/usr/bin/other-receive-pack"
		_, stderr, code := runCLI(t, env, "handoff", "-m", "x")
		if code != uzicli.ExitUsage || !strings.Contains(stderr, "remote.origin.receivepack") {
			t.Fatalf("exit=%d, want usage naming remote.origin.receivepack:\n%s", code, stderr)
		}
		assertNothingSent(t, fc, rec)
	})
	t.Run("lookup error", func(t *testing.T) {
		fc, rec, env := pushEnv(sshOrigin)
		rec.gitErr[receivePackKey] = exitCodeErr{2}
		_, stderr, code := runCLI(t, env, "handoff", "-m", "x")
		if code != uzicli.ExitUsage || !strings.Contains(stderr, "remote.origin.receivepack") {
			t.Fatalf("exit=%d, want usage naming remote.origin.receivepack:\n%s", code, stderr)
		}
		assertNothingSent(t, fc, rec)
	})
	t.Run("https origin ignores it", func(t *testing.T) {
		fc, rec, env := pushEnv("https://github.com/acme/widgets.git\n")
		delete(rec.gitErr, receivePackKey)
		rec.gitOut[receivePackKey] = "/usr/bin/other-receive-pack"
		if _, _, code := runCLI(t, env, "handoff", "-m", "x"); code != uzicli.ExitOK || fc.LastCreateTaskRepoID != "p1" {
			t.Fatalf("exit=%d repo=%q, want OK and p1", code, fc.LastCreateTaskRepoID)
		}
		if hasGitCall(rec, receivePackGitCall) {
			t.Errorf("receivepack lookup must not run for an https origin: %v", rec.gitCalls)
		}
	})
}

// The real git behaviour the handoff relies on: --push --all applies pushInsteadOf, pushurl
// and extra url values, and `config --get` exits 1 for an unset key.
func TestRealGitPushDestinationListing(t *testing.T) {
	isolateRealGit(t)
	newRepo := func() string {
		dir := t.TempDir()
		if _, err := realGit(dir, "init", "-q"); err != nil {
			t.Skipf("git unavailable: %v", err)
		}
		return dir
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if _, err := realGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	list := func(dir string) string {
		t.Helper()
		out, err := realGit(dir, "remote", "get-url", "--push", "--all", "origin")
		if err != nil {
			t.Fatalf("get-url: %v", err)
		}
		return out
	}

	dir := newRepo()
	must(dir, "config", "remote.origin.url", "https://github.com/acme/w.git")
	if got := list(dir); got != "https://github.com/acme/w.git" {
		t.Fatalf("plain = %q", got)
	}
	must(dir, "config", "url.git@evil.example:.pushInsteadOf", "https://github.com/")
	if got := list(dir); got != "git@evil.example:acme/w.git" {
		t.Fatalf("pushInsteadOf = %q", got)
	}
	must(dir, "config", "--unset", "url.git@evil.example:.pushInsteadOf")
	must(dir, "config", "--add", "remote.origin.url", "https://github.com/acme/x.git")
	if got := list(dir); got != "https://github.com/acme/w.git\nhttps://github.com/acme/x.git" {
		t.Fatalf("two urls = %q", got)
	}

	dir2 := newRepo()
	must(dir2, "config", "remote.origin.url", "https://github.com/acme/w.git")
	must(dir2, "config", "remote.origin.pushurl", "https://evil.example/acme/w.git")
	if got := list(dir2); got != "https://evil.example/acme/w.git" {
		t.Fatalf("pushurl = %q", got)
	}

	out, err := realGit(dir2, "config", "--get", "remote.origin.receivepack")
	if set, cerr := gitConfigKeySet(out, err); set || cerr != nil {
		t.Errorf("unset receivepack: set=%v err=%v (lookup err %v)", set, cerr, err)
	}
	must(dir2, "config", "remote.origin.receivepack", "/bin/true")
	out, err = realGit(dir2, "config", "--get", "remote.origin.receivepack")
	if set, cerr := gitConfigKeySet(out, err); !set || cerr != nil {
		t.Errorf("set receivepack: set=%v err=%v", set, cerr)
	}
}
