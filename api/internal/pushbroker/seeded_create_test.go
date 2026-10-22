package pushbroker_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// Issue #3284: a run seeded from an origin branch that already holds the tip, with no checkpoint
// ref, publishes that tip before its first new commit. Strict descent binds only an EXISTING
// checkpoint ref; with none, the declared tip must descend or equal the published branch tip and
// the broker creates the ref (Old = zero hash).

const (
	seededBranch = "feat"
	seededCkpt   = "refs/uzi-checkpoints/feat"
)

// seededOrigin builds an origin whose main holds a base commit and whose refs/heads/feat is the
// seed (one commit ahead of base), with no checkpoint ref. It returns the base and the seed.
func seededOrigin(t *testing.T) (f *gitFixture, base, seed string) {
	t.Helper()
	f = newGitFixture(t)
	base = f.commit("a.txt", "base\n", "base")
	f.pushMain()
	seed = f.commit("seed.txt", "seed\n", "seed")
	f.git("push", "origin", seed+":refs/heads/"+seededBranch)
	return f, base, seed
}

func seededOpts(url, tip string, pack []byte) pushbroker.Options {
	return pushbroker.Options{
		CloneURL: url, Username: "uzi-bot", PAT: redirectTestPAT(),
		Branch: seededBranch, DefaultBranch: "main", DeclaredTip: tip, Pack: pack,
	}
}

func TestPublishSeededFirstCheckpoint(t *testing.T) {
	ctx := context.Background()

	t.Run("creates the ref at the published branch tip", func(t *testing.T) {
		backend := requireGitHTTPBackend(t)
		trustLoopbackTLS(t)
		f, _, seed := seededOrigin(t)
		url := newGitHTTPRemote(t, f, backend).cloneURL(f)
		res, err := pushbroker.Publish(ctx, seededOpts(url, seed, f.pack(seed, seed)))
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if res.AlreadyCurrent || res.Disposition != pushbroker.PublishAdvanced {
			t.Fatalf("result = %+v, want a real advance (AlreadyCurrent=false, PublishAdvanced)", res)
		}
		if got := f.originRef(seededCkpt); got != seed {
			t.Fatalf("checkpoint = %q, want seed %s", got, seed)
		}
		if got := f.originRef("refs/heads/" + seededBranch); got != seed {
			t.Fatalf("branch moved to %q, want %s", got, seed)
		}
	})

	t.Run("a tip ahead of the published branch still creates", func(t *testing.T) {
		f, _, seed := seededOrigin(t)
		tip := f.commit("n.txt", "new\n", "new")
		if _, err := pushbroker.Publish(ctx, seededOpts(f.cloneURL(), tip, f.pack(tip, seed))); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := f.originRef(seededCkpt); got != tip {
			t.Fatalf("checkpoint = %q, want %s", got, tip)
		}
	})

	t.Run("an existing checkpoint ref equal to the tip stays AlreadyCurrent", func(t *testing.T) {
		f, _, seed := seededOrigin(t)
		f.git("push", "origin", seed+":"+seededCkpt)
		res, err := pushbroker.Publish(ctx, seededOpts(f.cloneURL(), seed, f.pack(seed, seed)))
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if !res.AlreadyCurrent {
			t.Fatalf("result = %+v, want AlreadyCurrent", res)
		}
	})

	t.Run("a tip that does not descend an existing checkpoint ref is refused", func(t *testing.T) {
		f, _, seed := seededOrigin(t)
		ahead := f.commit("n.txt", "new\n", "new")
		f.git("push", "origin", ahead+":"+seededCkpt)
		_, err := pushbroker.Publish(ctx, seededOpts(f.cloneURL(), seed, f.pack(seed, seed)))
		if !isNotDescendant(err) {
			t.Fatalf("Publish err = %v, want ErrNotDescendant", err)
		}
		if got := f.originRef(seededCkpt); got != ahead {
			t.Fatalf("checkpoint moved to %q, want %s", got, ahead)
		}
	})

	t.Run("with no checkpoint ref a tip behind the published branch is refused", func(t *testing.T) {
		f, base, seed := seededOrigin(t)
		// The published branch moves ahead of the seed; the declared seed is now a strict ancestor.
		ahead := f.commit("n.txt", "new\n", "new")
		f.git("push", "origin", ahead+":refs/heads/"+seededBranch)
		_, err := pushbroker.Publish(ctx, seededOpts(f.cloneURL(), seed, f.pack(seed, base)))
		if !isNotDescendant(err) {
			t.Fatalf("Publish err = %v, want ErrNotDescendant", err)
		}
		if got := f.originRef(seededCkpt); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
	})

	t.Run("with no checkpoint ref a tip diverged from the published branch is refused", func(t *testing.T) {
		f, base, _ := seededOrigin(t)
		f.git("checkout", "-q", "-b", "fork", base)
		fork := f.commit("fork.txt", "fork\n", "fork")
		_, err := pushbroker.Publish(ctx, seededOpts(f.cloneURL(), fork, f.pack(fork, base)))
		if !isNotDescendant(err) {
			t.Fatalf("Publish err = %v, want ErrNotDescendant", err)
		}
		if got := f.originRef(seededCkpt); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
	})

	t.Run("a guarded tip behind a workflow change on main is refused", func(t *testing.T) {
		f, _, seed := seededOrigin(t)
		f.git("checkout", "-q", "main")
		if err := os.MkdirAll(filepath.Join(f.work, ".github", "workflows"), 0o750); err != nil {
			t.Fatal(err)
		}
		f.commit(".github/workflows/ci.yml", "name: ci\n", "workflow")
		f.pushMain()
		o := seededOpts(f.cloneURL(), seed, f.pack(seed, seed))
		o.CIConfigGuard, o.CIConfigPaths = true, ciFloor
		if _, err := pushbroker.Publish(ctx, o); !errors.Is(err, pushbroker.ErrCIConfigGuard) {
			t.Fatalf("Publish err = %v, want ErrCIConfigGuard", err)
		}
		if got := f.originRef(seededCkpt); got != "" {
			t.Fatalf("checkpoint ref created at %s", got)
		}
	})

	t.Run("a checkpoint ref created after the fetch is refused and left alone", func(t *testing.T) {
		backend := requireGitHTTPBackend(t)
		trustLoopbackTLS(t)
		f, base, seed := seededOrigin(t)
		remote := newGitHTTPRemote(t, f, backend)
		var once sync.Once
		inner := remote.srv.Config.Handler
		remote.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				// The racer lands after the broker's fetch and before its receive-pack POST.
				once.Do(func() { run(t, "", "git", "-C", f.bare, "update-ref", seededCkpt, base) })
			}
			inner.ServeHTTP(w, r)
		})
		_, err := pushbroker.Publish(ctx, seededOpts(remote.cloneURL(f), seed, f.pack(seed, seed)))
		if err == nil {
			t.Fatal("Publish succeeded over a checkpoint ref created after its fetch")
		}
		if got := f.originRef(seededCkpt); got != base {
			t.Fatalf("checkpoint = %q, want the racer's %s", got, base)
		}
		if got := f.originRef("refs/heads/" + seededBranch); got != seed {
			t.Fatalf("branch moved to %q, want %s", got, seed)
		}
	})
}
