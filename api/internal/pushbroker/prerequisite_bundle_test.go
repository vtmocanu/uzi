package pushbroker_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// prereqBundleFixture is a broker-only native Git seam; production lane coverage lives in workersvc/prerequisite_closure_native_test.go.
// Source is a merge
// requiring P1 and P2; single-prerequisite fixtures simply omit the second parent.
// All artifacts are generated at runtime, and consumers clone with --no-local.
type prereqBundleFixture struct {
	f      *gitFixture
	p1     string
	p2     string
	source string
	bundle string
}

func newPrereqBundleFixture(t *testing.T, two bool) *prereqBundleFixture {
	t.Helper()
	f := newGitFixture(t)
	p1 := f.commit("base.txt", "prerequisite one\n", "P1")
	f.pushMain()
	x := &prereqBundleFixture{f: f, p1: p1}
	if two {
		f.git("checkout", "--orphan", "second")
		f.git("rm", "-rf", ".")
		x.p2 = f.commit("second.txt", "prerequisite two\n", "P2")
		f.git("checkout", "main")
		f.git("merge", "--allow-unrelated-histories", "--no-ff", "second", "-m", "merge P2")
	}
	x.source = f.commit("source.txt", "captured source\n", "source")
	x.bundle = filepath.Join(t.TempDir(), "source.bundle")
	args := []string{"bundle", "create", x.bundle, "HEAD", "^" + p1}
	if two {
		args = append(args, "^"+x.p2)
	}
	f.git(args...)
	return x
}

func (x *prereqBundleFixture) rewriteDefault(t *testing.T) string {
	t.Helper()
	x.f.git("checkout", "--orphan", "replacement")
	x.f.git("rm", "-rf", ".")
	d := x.f.commit("replacement.txt", "new default\n", "replacement")
	x.f.git("push", "--force", "origin", "HEAD:main")
	return d
}

func (x *prereqBundleFixture) cleanClone(t *testing.T) string {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "restore")
	run(t, "", "git", "clone", "--no-local", x.f.bare, clone)
	return clone
}

func prereqGitFailure(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: runtime fixture paths and literal git operations.
	cmd.Env = cleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("git %v unexpectedly succeeded: %s", args, out)
	}
	return string(out)
}

func TestPrereqNativeBundleRestore(t *testing.T) {
	x := newPrereqBundleFixture(t, false)
	o := prereqOpts(newGitHTTPRemote(t, x.f, requireGitHTTPBackend(t)).cloneURL(x.f))
	trustLoopbackTLS(t)
	p, err := pushbroker.PreparePrereqCreate(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tip() != x.p1 {
		t.Fatal("wrong captured prerequisite")
	}
	if out, err := pushbroker.SendPrereqCreate(context.Background(), p); err != nil || out != pushbroker.PrereqCreateCreated {
		t.Fatalf("pin = %v, %v", out, err)
	}
	x.rewriteDefault(t)
	clone := x.cleanClone(t)
	missing := prereqGitFailure(t, clone, "bundle", "verify", x.bundle)
	if !strings.Contains(missing, x.p1) {
		t.Fatalf("missing P1 not named: %s", missing)
	}
	prereqGitFailure(t, clone, "cat-file", "-e", x.p1)
	run(t, clone, "git", "fetch", "origin", p.Ref()+":"+p.Ref())
	run(t, clone, "git", "bundle", "verify", x.bundle)
	run(t, clone, "git", "fetch", x.bundle, "HEAD:refs/heads/restored")
	if got := strings.TrimSpace(run(t, clone, "git", "rev-parse", "refs/heads/restored")); got != x.source {
		t.Fatalf("restored %s want %s", got, x.source)
	}
	// Delete the pin and prove a fresh consumer cannot restore.
	if err := pushbroker.Delete(context.Background(), pushbroker.DeleteOptions{CloneURL: o.CloneURL, Ref: p.Ref(), ExpectedOldTip: x.p1}); err != nil {
		t.Fatal(err)
	}
	clean := x.cleanClone(t)
	prereqGitFailure(t, clean, "fetch", "origin", p.Ref()+":"+p.Ref())
	if out := prereqGitFailure(t, clean, "bundle", "verify", x.bundle); !strings.Contains(out, x.p1) {
		t.Fatal(out)
	}
}

func TestPrereqDefaultRewrittenBeforePrepare(t *testing.T) {
	x := newPrereqBundleFixture(t, false)
	d := x.rewriteDefault(t)
	trustLoopbackTLS(t)
	url := newGitHTTPRemote(t, x.f, requireGitHTTPBackend(t)).cloneURL(x.f)
	p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
	if err != nil || p.Tip() != d {
		t.Fatalf("prepare = %v %v; want rewritten default %s", p, err, d)
	}
	if out, err := pushbroker.SendPrereqCreate(context.Background(), p); err != nil || out != pushbroker.PrereqCreateCreated {
		t.Fatalf("pin = %v, %v", out, err)
	}
	clean := x.cleanClone(t)
	run(t, clean, "git", "fetch", "origin", p.Ref()+":"+p.Ref())
	if out := prereqGitFailure(t, clean, "bundle", "verify", x.bundle); !strings.Contains(out, x.p1) {
		t.Fatal(out)
	}
}

func TestPrereqUndeclaredSecondPrerequisite(t *testing.T) {
	x := newPrereqBundleFixture(t, true)
	// Manifest declares only P1. P2 was never pushed to any forge ref or pin.
	trustLoopbackTLS(t)
	url := newGitHTTPRemote(t, x.f, requireGitHTTPBackend(t)).cloneURL(x.f)
	p, err := pushbroker.PreparePrereqCreate(context.Background(), prereqOpts(url))
	if err != nil {
		t.Fatal(err)
	}
	if p.Tip() != x.p1 {
		t.Fatal("wrong captured prerequisite")
	}
	if out, err := pushbroker.SendPrereqCreate(context.Background(), p); err != nil || out != pushbroker.PrereqCreateCreated {
		t.Fatalf("pin = %v, %v", out, err)
	}
	pin := p.Ref()
	x.rewriteDefault(t)
	reachable := run(t, "", "git", "-C", x.f.bare, "rev-list", "--all")
	if strings.Contains(reachable, x.p2) {
		t.Fatal("P2 leaked into forge reachability")
	}
	clean := x.cleanClone(t)
	run(t, clean, "git", "fetch", "origin", pin+":"+pin)
	prereqGitFailure(t, clean, "cat-file", "-e", x.p2)
	out := prereqGitFailure(t, clean, "bundle", "verify", x.bundle)
	if !strings.Contains(out, x.p2) {
		t.Fatalf("missing P2 not named: %s", out)
	}
	prereqGitFailure(t, clean, "fetch", x.bundle, "HEAD:refs/heads/restored")
	prereqGitFailure(t, clean, "cat-file", "-e", x.p2)
}
