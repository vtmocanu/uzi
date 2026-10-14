package pushbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// floorGlobs mirrors the repair floor the guarded runs carry (see the parity fixture).
var floorGlobs = []string{".gitlab-ci.yml", ".gitlab/**", "**/*.gitlab-ci.yml", ".github/workflows/**", ".forgejo/workflows/**", ".gitea/workflows/**"}

// blobHash fabricates a blob id for content without storing the blob: the guard compares ids and
// never reads blobs.
func blobHash(content string) plumbing.Hash {
	return plumbing.ComputeHash(plumbing.BlobObject, []byte(content))
}

func file(name, content string) object.TreeEntry {
	return object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blobHash(content)}
}

type treeBuilder struct {
	t *testing.T
	s *memory.Storage
}

func (b treeBuilder) tree(entries ...object.TreeEntry) plumbing.Hash {
	b.t.Helper()
	sorted := append([]object.TreeEntry(nil), entries...)
	key := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	tr := &object.Tree{Entries: sorted}
	obj := b.s.NewEncodedObject()
	if err := tr.Encode(obj); err != nil {
		b.t.Fatalf("encode tree: %v", err)
	}
	h, err := b.s.SetEncodedObject(obj)
	if err != nil {
		b.t.Fatalf("store tree: %v", err)
	}
	return h
}

func (b treeBuilder) dir(name string, entries ...object.TreeEntry) object.TreeEntry {
	return object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: b.tree(entries...)}
}

func newTB(t *testing.T) treeBuilder { return treeBuilder{t: t, s: memory.NewStorage()} }

func guard(t *testing.T, b treeBuilder, cand, def plumbing.Hash, globs ...string) error {
	t.Helper()
	m, ok, err := newCIConfigMatcher(globs)
	if err != nil || !ok {
		t.Fatalf("matcher: ok=%v err=%v", ok, err)
	}
	return ciConfigTreeGuard(context.Background(), b.s, cand, def, m)
}

func TestCIConfigTreeGuardPathCases(t *testing.T) {
	b := newTB(t)
	base := []object.TreeEntry{file("README.md", "r"), b.dir("src", file("a.go", "a"))}
	with := func(extra ...object.TreeEntry) plumbing.Hash {
		return b.tree(append(append([]object.TreeEntry{}, base...), extra...)...)
	}
	wf := func(name, content string) object.TreeEntry {
		return b.dir(".github", b.dir("workflows", file(name, content)))
	}
	def := with(wf("ci.yml", "v1"))

	cases := []struct {
		name    string
		cand    plumbing.Hash
		globs   []string
		refused bool
	}{
		{"identical trees", def, floorGlobs, false},
		{"modified floor file", with(wf("ci.yml", "v2")), floorGlobs, true},
		{"added floor file", with(wf("ci.yml", "v1"), file(".gitlab-ci.yml", "x")), floorGlobs, true},
		{"deleted floor file", b.tree(base...), floorGlobs, true},
		{"rename out of protected dir", with(b.dir(".github", b.dir("workflows"))), floorGlobs, true},
		{"mode change only", with(b.dir(".github", b.dir("workflows", object.TreeEntry{Name: "ci.yml", Mode: filemode.Executable, Hash: blobHash("v1")}))), floorGlobs, true},
		{"trailing space path", with(wf("ci.yml", "v1"), b.dir(".github", b.dir("workflows", file("evil.yml ", "x")))), floorGlobs, true},
		{"newline path", with(b.dir(".github", b.dir("workflows", file("a\nb.yml", "x")))), floorGlobs, true},
		{"server-added non-floor glob", b.tree(file("README.md", "r"), b.dir("src", file("a.go", "changed"))), []string{"src/**"}, true},
		{"unprotected change only", b.tree(file("README.md", "changed"), b.dir("src", file("a.go", "a")), wf("ci.yml", "v1")), floorGlobs, false},
		{"file replaces protected directory", b.tree(file("README.md", "r"), b.dir("src", file("a.go", "a")), file(".github", "now a file")), floorGlobs, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// For the rename/delete cases the candidate lacks the default's file, so an
			// unrelated default (with wf) is what the guard compares against.
			err := guard(t, b, c.cand, def, c.globs...)
			switch {
			case c.refused && !errors.Is(err, ErrCIConfigGuard):
				t.Fatalf("err = %v, want ErrCIConfigGuard", err)
			case !c.refused && err != nil:
				t.Fatalf("err = %v, want nil", err)
			}
		})
	}
}

// A rename out of a protected path: the candidate moved the file elsewhere. The old path is
// absent on the candidate side, which is itself a difference at a protected path.
func TestCIConfigTreeGuardRenameOut(t *testing.T) {
	b := newTB(t)
	def := b.tree(b.dir(".github", b.dir("workflows", file("a.yml", "x"))))
	cand := b.tree(file("moved.yml", "x"))
	if err := guard(t, b, cand, def, floorGlobs...); !errors.Is(err, ErrCIConfigGuard) {
		t.Fatalf("err = %v, want ErrCIConfigGuard", err)
	}
}

func filesTree(b treeBuilder, n int, content string) []object.TreeEntry {
	es := make([]object.TreeEntry, 0, n)
	for i := 0; i < n; i++ {
		es = append(es, file(fmt.Sprintf("f%07d", i), content))
	}
	return es
}

func TestCIConfigTreeGuardWalkCap(t *testing.T) {
	protected := []string{"**/ci.yml"}

	t.Run("flat trees over the cap are refused", func(t *testing.T) {
		b := newTB(t)
		// 51,000 identical entries per side: 102,000 visited.
		cand := b.tree(append(filesTree(b, 51_000, "x"), file("z", "1"))...)
		def := b.tree(append(filesTree(b, 51_000, "x"), file("z", "2"))...)
		err := guard(t, b, cand, def, protected...)
		if !errors.Is(err, ErrCIConfigGuard) || !strings.Contains(err.Error(), "reached") {
			t.Fatalf("err = %v, want cap refusal", err)
		}
	})

	t.Run("flat trees just under the cap get a verdict", func(t *testing.T) {
		b := newTB(t)
		clean := guard(t, b,
			b.tree(append(filesTree(b, 49_000, "x"), file("z", "1"))...),
			b.tree(append(filesTree(b, 49_000, "x"), file("z", "2"))...), protected...)
		if clean != nil {
			t.Fatalf("unprotected change under cap: err = %v, want nil", clean)
		}
		refused := guard(t, b,
			b.tree(append(filesTree(b, 49_000, "x"), file("ci.yml", "1"))...),
			b.tree(append(filesTree(b, 49_000, "x"), file("ci.yml", "2"))...), protected...)
		if !errors.Is(refused, ErrCIConfigGuard) || strings.Contains(refused.Error(), "reached") {
			t.Fatalf("protected change under cap: err = %v, want a protected-path refusal", refused)
		}
	})

	t.Run("nested subtree entries count toward the cap", func(t *testing.T) {
		b := newTB(t)
		mk := func(per int, tail string) plumbing.Hash {
			inner := b.dir("b", append(filesTree(b, per, "x"), file("tail", tail))...)
			outer := b.dir("a", append(filesTree(b, per, "x"), inner, file("tail", tail))...)
			return b.tree(outer)
		}
		// Each level is under the cap on its own; together they are over.
		err := guard(t, b, mk(30_000, "1"), mk(30_000, "2"), protected...)
		if !errors.Is(err, ErrCIConfigGuard) || !strings.Contains(err.Error(), "reached") {
			t.Fatalf("err = %v, want cap refusal across nested trees", err)
		}
		if err := guard(t, b, mk(20_000, "1"), mk(20_000, "2"), protected...); err != nil {
			t.Fatalf("nested under cap, unprotected: err = %v, want nil", err)
		}
	})

	t.Run("a one-sided subtree counts every entry", func(t *testing.T) {
		b := newTB(t)
		cand := b.tree(b.dir("big", filesTree(b, 110_000, "x")...))
		def := b.tree(file("k", "k"))
		err := guard(t, b, cand, def, protected...)
		if !errors.Is(err, ErrCIConfigGuard) || !strings.Contains(err.Error(), "reached") {
			t.Fatalf("err = %v, want cap refusal", err)
		}
	})
}

func TestCIConfigTreeGuardFailClosed(t *testing.T) {
	b := newTB(t)
	def := b.tree(file("a", "1"))
	t.Run("missing subtree object", func(t *testing.T) {
		missing := object.TreeEntry{Name: "d", Mode: filemode.Dir, Hash: blobHash("nowhere")}
		cand := b.tree(missing)
		if err := guard(t, b, cand, def, "**/ci.yml"); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("missing root tree", func(t *testing.T) {
		if err := guard(t, b, blobHash("nowhere"), def, "**/ci.yml"); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		big := newTB(t)
		cand := big.tree(append(filesTree(big, 5_000, "x"), file("z", "1"))...)
		d := big.tree(append(filesTree(big, 5_000, "x"), file("z", "2"))...)
		m, _, _ := newCIConfigMatcher([]string{"**/ci.yml"})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ciConfigTreeGuard(ctx, big.s, cand, d, m); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("excessive nesting", func(t *testing.T) {
		nb := newTB(t)
		leaf := nb.tree(file("a", "1"))
		cur := leaf
		for i := 0; i < ciConfigMaxDepth+5; i++ {
			cur = nb.tree(object.TreeEntry{Name: "d", Mode: filemode.Dir, Hash: cur})
		}
		other := nb.tree(file("b", "2"))
		cur2 := other
		for i := 0; i < ciConfigMaxDepth+5; i++ {
			cur2 = nb.tree(object.TreeEntry{Name: "d", Mode: filemode.Dir, Hash: cur2})
		}
		m, _, _ := newCIConfigMatcher([]string{"**/ci.yml"})
		if err := ciConfigTreeGuard(context.Background(), nb.s, cur, cur2, m); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
}

// commitRepo returns a memory repo holding a default-branch commit (parent absent, as after a
// depth-1 fetch) at refs/remotes/origin/<branch>, and a candidate commit.
func commitRepo(t *testing.T, b treeBuilder, branch string, defTree, candTree plumbing.Hash) (*git.Repository, *object.Commit) {
	t.Helper()
	repo := &git.Repository{Storer: b.s}
	store := func(tree plumbing.Hash, parents ...plumbing.Hash) *object.Commit {
		sig := object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1, 0)}
		c := &object.Commit{Author: sig, Committer: sig, Message: "m", TreeHash: tree, ParentHashes: parents}
		obj := b.s.NewEncodedObject()
		if err := c.Encode(obj); err != nil {
			t.Fatal(err)
		}
		h, err := b.s.SetEncodedObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		got, err := object.GetCommit(b.s, h)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The default tip's parent object is NOT stored: the guard must not walk history.
	def := store(defTree, plumbing.NewHash("1111111111111111111111111111111111111111"))
	if branch != "" {
		ref := plumbing.NewHashReference(plumbing.ReferenceName("refs/remotes/origin/"+branch), def.Hash)
		if err := b.s.SetReference(ref); err != nil {
			t.Fatal(err)
		}
	}
	return repo, store(candTree)
}

func TestCIConfigGuardDepthOneAndDefaultRef(t *testing.T) {
	b := newTB(t)
	wf := func(c string) object.TreeEntry {
		return b.dir(".github", b.dir("workflows", file("ci.yml", c)))
	}
	def := b.tree(file("a", "1"), wf("v1"))
	ctx := context.Background()

	t.Run("verdict without default parent", func(t *testing.T) {
		repo, cand := commitRepo(t, b, "main", def, b.tree(file("a", "2"), wf("v1")))
		if err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs); err != nil {
			t.Fatalf("clean candidate: %v", err)
		}
		repo, cand = commitRepo(t, b, "main", def, b.tree(file("a", "1"), wf("v2")))
		if err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("protected change: %v", err)
		}
	})
	t.Run("protected file changed only on the default branch is refused", func(t *testing.T) {
		// The candidate keeps the run's old workflow; the default moved on. Documented strictness.
		repo, cand := commitRepo(t, b, "main", b.tree(file("a", "1"), wf("default-moved")), b.tree(file("a", "1"), wf("v1")))
		if err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("missing default ref fails closed", func(t *testing.T) {
		repo, cand := commitRepo(t, b, "", def, def)
		if err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("empty default branch name fails closed", func(t *testing.T) {
		repo, cand := commitRepo(t, b, "main", def, def)
		if err := ciConfigGuard(ctx, repo, cand, "", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
	t.Run("missing default tree fails closed", func(t *testing.T) {
		repo, cand := commitRepo(t, b, "main", blobHash("gone"), def)
		if err := ciConfigGuard(ctx, repo, cand, "main", floorGlobs); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
}

// TestCIConfigGlobParityFixture asserts the Go matcher agrees with the shared fixture that the
// agent's matcher is also tested against.
func TestCIConfigGlobParityFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/ci-config-globs/cases.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		Cases []struct {
			Name  string
			Globs []string
			Path  string
			Match bool
		}
		ChangeSets []struct {
			Name    string
			Globs   []string
			Changed []string
			Flagged []string
		}
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fx.Cases) == 0 || len(fx.ChangeSets) == 0 {
		t.Fatalf("fixture empty: %d cases, %d changeSets", len(fx.Cases), len(fx.ChangeSets))
	}
	matcher := func(globs []string) func(string) bool {
		m, ok, err := newCIConfigMatcher(globs)
		if err != nil {
			t.Fatalf("matcher: %v", err)
		}
		if !ok {
			return func(string) bool { return false }
		}
		return m
	}
	for _, c := range fx.Cases {
		if got := matcher(c.Globs)(c.Path); got != c.Match {
			t.Errorf("case %q: path %q match = %v, want %v", c.Name, c.Path, got, c.Match)
		}
	}
	for _, c := range fx.ChangeSets {
		m := matcher(c.Globs)
		var flagged []string
		seen := map[string]bool{}
		for _, p := range c.Changed {
			if m(p) && !seen[p] {
				seen[p] = true
				flagged = append(flagged, p)
			}
		}
		if strings.Join(flagged, "\x00") != strings.Join(c.Flagged, "\x00") {
			t.Errorf("changeSet %q: flagged %q, want %q", c.Name, flagged, c.Flagged)
		}
	}
}

func TestCIConfigTreeGuardByteCaps(t *testing.T) {
	protected := []string{"**/ci.yml"}

	t.Run("oversized entry name is refused", func(t *testing.T) {
		b := newTB(t)
		long := strings.Repeat("n", ciConfigMaxPathBytes+1)
		cand := b.tree(file(long, "1"), file("z", "1"))
		def := b.tree(file("z", "2"))
		start := time.Now()
		err := guard(t, b, cand, def, protected...)
		if !errors.Is(err, ErrCIConfigGuard) || !strings.Contains(err.Error(), "longer than") {
			t.Fatalf("err = %v, want path-length refusal", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("refusal took %v", d)
		}
	})

	t.Run("oversized default-side name is refused", func(t *testing.T) {
		b := newTB(t)
		long := strings.Repeat("n", ciConfigMaxPathBytes+1)
		cand := b.tree(file("z", "1"))
		def := b.tree(file(long, "1"), file("z", "2"))
		if err := guard(t, b, cand, def, protected...); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("joined path over the cap is refused", func(t *testing.T) {
		b := newTB(t)
		half := strings.Repeat("d", ciConfigMaxPathBytes/2+10)
		inner := func(c string) plumbing.Hash { return b.tree(file(half, c)) }
		mid := func(c string) plumbing.Hash {
			return b.tree(object.TreeEntry{Name: half, Mode: filemode.Dir, Hash: inner(c)})
		}
		cand := b.tree(object.TreeEntry{Name: "top", Mode: filemode.Dir, Hash: mid("1")})
		def := b.tree(object.TreeEntry{Name: "top", Mode: filemode.Dir, Hash: mid("2")})
		if err := guard(t, b, cand, def, protected...); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})

	t.Run("total path bytes over the cap are refused", func(t *testing.T) {
		b := newTB(t)
		cand := b.tree(append(filesTree(b, 50, "x"), file("z", "1"))...)
		def := b.tree(append(filesTree(b, 50, "x"), file("z", "2"))...)
		m, _, _ := newCIConfigMatcher(protected)
		w := &ciConfigWalk{ctx: context.Background(), s: b.s, match: m, limit: ciConfigWalkCap, maxBytes: 100}
		if err := w.diff(cand, def, "", 0); !errors.Is(err, ErrCIConfigGuard) || !strings.Contains(err.Error(), "path bytes") {
			t.Fatalf("err = %v, want total-bytes refusal", err)
		}
	})

	t.Run("cancelled context is refused on a tiny tree", func(t *testing.T) {
		b := newTB(t)
		cand := b.tree(file("a", "1"))
		def := b.tree(file("a", "2"))
		m, _, _ := newCIConfigMatcher(protected)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ciConfigTreeGuard(ctx, b.s, cand, def, m); !errors.Is(err, ErrCIConfigGuard) {
			t.Fatalf("err = %v, want ErrCIConfigGuard", err)
		}
	})
}
