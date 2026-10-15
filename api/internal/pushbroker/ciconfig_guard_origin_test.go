package pushbroker_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Guarded runs (PRD #2865): the checkpoint and held paths refuse a tip that touches a protected
// CI-config path before anything is sent. The origin is a local bare repo over file://; a refusal
// is proven by the checkpoint or held ref staying absent on it.

var ciFloor = []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", ".github/workflows/**", ".forgejo/workflows/**", ".gitea/workflows/**"}

// ciTip builds an origin whose main holds a.txt and a tip one commit ahead that adds path.
func ciTip(t *testing.T, path string) (f *gitFixture, url, tip string, pack []byte) {
	t.Helper()
	f = newGitFixture(t)
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.work, path)), 0o750); err != nil {
		t.Fatal(err)
	}
	tip = f.commit(path, "changed\n", "tip")
	pack = f.pack(tip, base)
	return f, f.cloneURL(), tip, pack
}

func publishOpts(url, tip string, pack []byte, guard bool, globs []string) pushbroker.Options {
	return pushbroker.Options{
		CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(), Branch: "main", DefaultBranch: "main",
		DeclaredTip: tip, Pack: pack, CIConfigGuard: guard, CIConfigPaths: globs,
	}
}

func TestCheckpointPublishCIConfigGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("guarded tip touching a floor path is refused before any send", func(t *testing.T) {
		f, url, tip, pack := ciTip(t, ".github/workflows/ci.yml")
		_, err := pushbroker.Publish(ctx, publishOpts(url, tip, pack, true, ciFloor))
		if !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
		if got := f.originRef("refs/uzi-checkpoints/main"); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
	})

	t.Run("guarded tip touching only a server-added path is refused", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, "build/release.sh")
		_, err := pushbroker.Publish(ctx, publishOpts(url, tip, pack, true, append([]string{"build/**"}, ciFloor...)))
		if !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("clean guarded tip proceeds", func(t *testing.T) {
		f, url, tip, pack := ciTip(t, "b.txt")
		if _, err := pushbroker.Publish(ctx, publishOpts(url, tip, pack, true, ciFloor)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := f.originRef("refs/uzi-checkpoints/main"); got != tip {
			t.Fatalf("checkpoint = %q, want %s", got, tip)
		}
	})

	t.Run("unguarded run is unaffected", func(t *testing.T) {
		f, url, tip, pack := ciTip(t, ".github/workflows/ci.yml")
		if _, err := pushbroker.Publish(ctx, publishOpts(url, tip, pack, false, ciFloor)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := f.originRef("refs/uzi-checkpoints/main"); got != tip {
			t.Fatalf("checkpoint = %q, want %s", got, tip)
		}
	})

	t.Run("guarded run with no default branch fetched fails closed", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, "b.txt")
		o := publishOpts(url, tip, pack, true, ciFloor)
		o.DefaultBranch = "no-such-branch"
		if _, err := pushbroker.Publish(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
	})
}

func TestHeldPrepareCIConfigGuard(t *testing.T) {
	ctx := context.Background()
	guarded := func(o pushbroker.HeldPackOptions, globs []string) pushbroker.HeldPackOptions {
		o.CIConfigGuard, o.CIConfigPaths = true, globs
		return o
	}

	t.Run("guarded pack touching a floor path is refused", func(t *testing.T) {
		f, url, tip, pack := ciTip(t, ".gitlab-ci.yml")
		_, err := pushbroker.PrepareHeldPack(ctx, guarded(heldOpts(url, tip, pack), ciFloor))
		if !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
		}
		if got := f.originRef(heldRefName()); got != "" {
			t.Fatalf("held ref created at %s", got)
		}
	})

	t.Run("guarded pack touching only a server-added path is refused", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, "deploy/prod.yaml")
		_, err := pushbroker.PrepareHeldPack(ctx, guarded(heldOpts(url, tip, pack), []string{"deploy/**"}))
		if !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("clean guarded pack is prepared", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, "b.txt")
		p, err := pushbroker.PrepareHeldPack(ctx, guarded(heldOpts(url, tip, pack), ciFloor))
		if err != nil || p.Tip() != tip {
			t.Fatalf("PrepareHeldPack = %v, %v; want prepared at %s", p, err, tip)
		}
	})

	t.Run("missing default ref fails closed", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, "b.txt")
		o := guarded(heldOpts(url, tip, pack), ciFloor)
		o.DefaultBranch = "no-such-branch"
		if _, err := pushbroker.PrepareHeldPack(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("unguarded pack touching a floor path is unaffected", func(t *testing.T) {
		_, url, tip, pack := ciTip(t, ".github/workflows/ci.yml")
		o := heldOpts(url, tip, pack)
		o.CIConfigPaths = ciFloor // paths without the flag change nothing
		if _, err := pushbroker.PrepareHeldPack(ctx, o); err != nil {
			t.Fatalf("PrepareHeldPack: %v", err)
		}
	})
}

// ciMovedDefault builds the shape the worker ships: the base holds docs/a.md and a.txt, the run
// adds path on top, and the pack is `tip ^base` (trees shared with the base are NOT in it). The
// default branch then moves, changing docs/a.md.
func ciMovedDefault(t *testing.T, path string) (f *gitFixture, url, tip string, pack []byte) {
	t.Helper()
	f = newGitFixture(t)
	if err := os.MkdirAll(filepath.Join(f.work, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.commit("docs/a.md", "doc\n", "docs")
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.work, path)), 0o750); err != nil {
		t.Fatal(err)
	}
	tip = f.commit(path, "changed\n", "tip")
	pack = f.pack(tip, base)
	f.git("checkout", "-q", "-b", "moved", base)
	f.commit("docs/a.md", "doc moved\n", "default moves")
	f.git("push", "origin", "moved:main")
	return f, f.cloneURL(), tip, pack
}

// runBranchOpts publishes on a run branch (not the default branch the origin moved on).
func runBranchOpts(url, tip string, pack []byte) pushbroker.Options {
	o := publishOpts(url, tip, pack, true, ciFloor)
	o.Branch = "agent/issue-1"
	return o
}

func TestCIConfigGuardDefaultMovedSinceBase(t *testing.T) {
	ctx := context.Background()

	// The candidate's docs/ tree is the base's, absent from the pack, and differs from the moved
	// default: the broker cannot prove it unchanged, so it refuses (the accepted strictness).
	t.Run("a clean tip whose inherited subtree is not in the pack is refused", func(t *testing.T) {
		f, url, tip, pack := ciMovedDefault(t, "b.txt")
		if _, err := pushbroker.Publish(ctx, runBranchOpts(url, tip, pack)); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
		if got := f.originRef("refs/uzi-checkpoints/agent/issue-1"); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
	})

	t.Run("a clean held pack whose inherited subtree is not in the pack is refused", func(t *testing.T) {
		_, url, tip, pack := ciMovedDefault(t, "b.txt")
		o := heldOpts(url, tip, pack)
		o.CIConfigGuard, o.CIConfigPaths = true, ciFloor
		if _, err := pushbroker.PrepareHeldPack(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("a workflow the run added is still refused", func(t *testing.T) {
		f, url, tip, pack := ciMovedDefault(t, ".github/workflows/x.yml")
		_, err := pushbroker.Publish(ctx, runBranchOpts(url, tip, pack))
		if !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
		if got := f.originRef("refs/uzi-checkpoints/agent/issue-1"); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
		o := heldOpts(url, tip, pack)
		o.CIConfigGuard, o.CIConfigPaths = true, ciFloor
		if _, err := pushbroker.PrepareHeldPack(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
		}
	})
}

// commitIn writes a file (creating its directory) and commits it.
func (f *gitFixture) commitIn(path, content, msg string) string {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(f.work, path)), 0o750); err != nil {
		f.t.Fatal(err)
	}
	return f.commit(path, content, msg)
}

// Once a protected path changed on the default branch after the run's base, every guarded
// publication of the run refuses: the first because its inherited .gitlab tree is not in the pack
// (unknown, so refused), and none reaches origin. Accepted strictness: a non-fatal checkpoint
// skip; the worker's merge-base check decides the final branch push.
func TestCIConfigGuardRefusesAfterProtectedChangeOnDefault(t *testing.T) {
	ctx := context.Background()
	f := newGitFixture(t)
	f.commitIn(".gitlab/ci.yml", "v1\n", "ci v1")
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	c1 := f.commit("b.txt", "one\n", "c1")
	pack1 := f.pack(c1, base)
	c2 := f.commit("c.txt", "two\n", "c2")
	pack2 := f.pack(c2, base)
	f.git("checkout", "-q", "-b", "moved", base)
	f.commitIn(".gitlab/ci.yml", "v2\n", "default changes ci")
	f.git("push", "origin", "moved:main")
	url := f.cloneURL()

	for name, c := range map[string]struct {
		tip  string
		pack []byte
	}{"C1": {c1, pack1}, "C2": {c2, pack2}} {
		if _, err := pushbroker.Publish(ctx, runBranchOpts(url, c.tip, c.pack)); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish %s err = %v, want ErrCIConfigGuard", name, err)
		}
	}
	if got := f.originRef("refs/uzi-checkpoints/agent/issue-1"); got != "" {
		t.Fatalf("checkpoint ref created at %s", got)
	}
}

// An agent-authored merge whose extra parent is outside the pack leaves the changed protected
// tree out of the broker's store. The broker cannot prove the subtree unchanged, so it refuses
// both the checkpoint publish and the held preparation (nothing is sent).
func TestCIConfigGuardRefusesMergeParentOutsidePack(t *testing.T) {
	ctx := context.Background()
	f := newGitFixture(t)
	f.commitIn(".gitlab/ci.yml", "v1\n", "ci v1")
	base := f.commit("a.txt", "base\n", "base")
	f.pushMain()
	f.git("checkout", "-q", "-b", "side", base)
	side := f.commitIn(".gitlab/ci.yml", "evil\n", "side changes ci")
	f.git("push", "origin", "side:side") // the extra parent lives on origin, but the broker never fetches it
	f.git("checkout", "-q", "main")
	f.commit("b.txt", "one\n", "c1")
	f.git("merge", "--no-ff", "-m", "merge side", "side")
	tip := strings.TrimSpace(f.git("rev-parse", "HEAD"))
	// The pack excludes the side parent's objects, so the evil .gitlab tree is not in it.
	revs := f.git("rev-list", tip, "^"+base, "^"+side, "--objects")
	cmd := exec.Command("git", "-C", f.work, "pack-objects", "--stdout") //nolint:gosec // G204: test helper against a test-owned temp repo.
	cmd.Stdin = strings.NewReader(revs)
	pack, err := cmd.Output()
	if err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	if _, err := pushbroker.Publish(ctx, runBranchOpts(f.cloneURL(), tip, pack)); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
		t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
	}
	if got := f.originRef("refs/uzi-checkpoints/agent/issue-1"); got != "" {
		t.Fatalf("checkpoint ref created at %s", got)
	}
	o := heldOpts(f.cloneURL(), tip, pack)
	o.CIConfigGuard, o.CIConfigPaths = true, ciFloor
	if _, err := pushbroker.PrepareHeldPack(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
		t.Fatalf("PrepareHeldPack err = %v, want ErrCIConfigGuard", err)
	}
	if got := f.originRef(heldRefName()); got != "" {
		t.Fatalf("held ref created at %s", got)
	}
}
