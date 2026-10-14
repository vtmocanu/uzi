package pushbroker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Guarded runs (PRD #2865): the checkpoint and held paths refuse a tip that touches a protected
// CI-config path before anything is sent. The origin is a local bare repo over file://; a refusal
// is proven by the checkpoint or held ref staying absent on it.

var ciFloor = []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", ".github/workflows/**", ".forgejo/workflows/**"}

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
