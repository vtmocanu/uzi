package pushbroker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage"
	"github.com/go-git/go-git/v5/storage/memory"
)

type ancestryStore struct {
	storage.Storer
	reads []plumbing.Hash
	hook  func(plumbing.Hash) error
}

func (s *ancestryStore) EncodedObject(kind plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	s.reads = append(s.reads, h)
	if s.hook != nil {
		if err := s.hook(h); err != nil {
			return nil, err
		}
	}
	return s.Storer.EncodedObject(kind, h)
}

func TestAncestryBudgets(t *testing.T) {
	for _, tc := range []struct {
		name         string
		parents      []int // 0=base, 1/2=missing, 3=shared stored node
		nodes, edges int
		want         bool
		loads        int // base load plus queued nodes; declared already decoded
	}{
		{"base_no_slot", []int{0}, 1, 1, true, 1},
		{"exact_edges", []int{1, 0}, 2, 2, true, 1},
		{"over_edges", []int{1, 0}, 2, 1, false, 1},
		{"exact_nodes", []int{1, 0}, 2, 9, true, 1},
		{"over_nodes", []int{1, 2, 0}, 2, 9, false, 1},
		{"duplicates_charge_edges", []int{1, 1, 0}, 2, 2, false, 1},
		{"duplicates_dedup_slots", []int{1, 1, 0}, 2, 3, true, 1},
		{"all_missing_dedup_load", []int{1, 1, 2}, 3, 3, false, 3},
		{"missing_consumes_slot", []int{1, 2, 0}, 3, 3, true, 1},
		{"wide_refuses_before_late_base", []int{1, 2, 3, 0}, 3, 10, false, 1},
		{"shared_repeated", []int{3, 3}, 2, 3, true, 2},
		{"shared_edge_limit", []int{3, 3}, 2, 2, false, 2},
		{"unreachable", nil, 1, 1, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &ancestryStore{Storer: memory.NewStorage()}
			repo, err := git.Init(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			tree := storeTree(t, repo)
			base := storeCommit(t, repo, tree, "base")
			shared := storeCommit(t, repo, tree, "shared", base)
			hashes := []plumbing.Hash{base, plumbing.NewHash("1111111111111111111111111111111111111111"), plumbing.NewHash("2222222222222222222222222222222222222222"), shared}
			var parents []plumbing.Hash
			for _, i := range tc.parents {
				parents = append(parents, hashes[i])
			}
			declared := getCommit(t, repo, storeCommit(t, repo, tree, "declared", parents...))
			s.reads = nil
			got, err := ancestry(context.Background(), repo, base, declared, false, tc.nodes, tc.edges)
			if err != nil || got != tc.want {
				t.Fatalf("got %t,%v want %t", got, err, tc.want)
			}
			if len(s.reads) != tc.loads {
				t.Fatalf("reads=%v want %d", s.reads, tc.loads)
			}
		})
	}
}

func TestAncestryEqualityMissingAndErrors(t *testing.T) {
	s := &ancestryStore{Storer: memory.NewStorage()}
	repo, err := git.Init(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := storeTree(t, repo)
	base := storeCommit(t, repo, tree, "base")
	declared := getCommit(t, repo, base)
	for _, strict := range []bool{false, true} {
		got, err := ancestry(context.Background(), repo, base, declared, strict, 1, 1)
		if err != nil || got == strict {
			t.Fatalf("strict=%t got=%t,%v", strict, got, err)
		}
	}
	missing := plumbing.NewHash("3333333333333333333333333333333333333333")
	if got, err := descendsOrEqual(context.Background(), repo, missing, declared); got || err != nil {
		t.Fatalf("missing=%t,%v", got, err)
	}
	// Even equal hashes cannot validate an object absent from storage.
	phantom := &object.Commit{Hash: missing}
	if got, err := descendsOrEqual(context.Background(), repo, missing, phantom); got || err != nil {
		t.Fatalf("missing equality=%t,%v", got, err)
	}
	fault := errors.New("storage fault")
	child := getCommit(t, repo, storeCommit(t, repo, tree, "child", missing))
	for _, failHash := range []plumbing.Hash{base, missing} {
		s.hook = func(h plumbing.Hash) error {
			if h == failHash {
				return fault
			}
			return nil
		}
		if got, err := descendsOrEqual(context.Background(), repo, base, child); got || !errors.Is(err, fault) {
			t.Fatalf("fault %s=%t,%v", failHash, got, err)
		}
	}
	s.hook = nil
	bad := repo.Storer.NewEncodedObject()
	bad.SetType(plumbing.CommitObject)
	w, err := bad.Writer()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("parent not-a-hash\n")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	badHash, err := repo.Storer.SetEncodedObject(bad)
	if err != nil {
		t.Fatal(err)
	}
	broken := getCommit(t, repo, storeCommit(t, repo, tree, "broken", badHash))
	for _, h := range []plumbing.Hash{base, badHash} {
		if got, err := descendsOrEqual(context.Background(), repo, h, broken); got || err == nil {
			t.Fatalf("decode %s=%t,%v", h, got, err)
		}
	}
}

// Err calls provide deterministic cancellation during duplicate parent iteration;
// no timer or goroutine races influence which operation gets cancelled.
type ancestryCancelContext struct {
	context.Context
	remaining int
}

func (c *ancestryCancelContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}
func TestAncestryCancellation(t *testing.T) {
	for _, mode := range []string{"equal", "strict_equal", "missing_base", "success", "exhaustion", "node_budget", "edge_budget"} {
		t.Run(mode, func(t *testing.T) {
			s := &ancestryStore{Storer: memory.NewStorage()}
			repo, err := git.Init(s, nil)
			if err != nil {
				t.Fatal(err)
			}
			tree := storeTree(t, repo)
			base := storeCommit(t, repo, tree, "base")
			missing := plumbing.NewHash("4444444444444444444444444444444444444444")
			declared := getCommit(t, repo, storeCommit(t, repo, tree, "child", missing, missing, base))
			strict := mode == "strict_equal"
			nodes, edges := 10, 10
			switch mode {
			case "equal", "strict_equal":
				declared = getCommit(t, repo, base)
			case "missing_base":
				base = missing
			case "exhaustion":
				declared = getCommit(t, repo, storeCommit(t, repo, tree, "empty"))
			case "node_budget":
				nodes = 1
			case "edge_budget":
				edges = 1
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.hook = func(plumbing.Hash) error { cancel(); return nil }
			if got, err := ancestry(ctx, repo, base, declared, strict, nodes, edges); got || !errors.Is(err, context.Canceled) {
				t.Fatalf("after base load=%t,%v", got, err)
			}
			s.hook = nil
			// Sweep every context-check boundary through the normal result, including
			// success, exhaustion and budget-refusal returns.
			completed := false
			for i := 1; i < 40; i++ {
				ctx := &ancestryCancelContext{Context: context.Background(), remaining: i}
				got, err := ancestry(ctx, repo, base, declared, strict, nodes, edges)
				if err == nil {
					completed = true
					break
				}
				if got || !errors.Is(err, context.Canceled) {
					t.Fatalf("check %d: %t,%v", i, got, err)
				}
			}
			if !completed {
				t.Fatal("context sweep did not reach normal return")
			}
		})
	}
}

func TestAncestryWideMissing(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := storeTree(t, repo)
	base := storeCommit(t, repo, tree, "base")
	parents := make([]plumbing.Hash, 128)
	for i := range parents {
		parents[i] = plumbing.NewHash(fmt.Sprintf("%040x", i+1))
	}
	side := storeCommit(t, repo, tree, "side", parents...)
	tip := getCommit(t, repo, storeCommit(t, repo, tree, "tip", side, side))
	if got, err := ancestry(context.Background(), repo, base, tip, false, 130, 130); got || err != nil {
		t.Fatalf("all incomplete=%t,%v", got, err)
	}
	// Every missing sibling is scheduled before the late base, at the exact limits.
	tip = getCommit(t, repo, storeCommit(t, repo, tree, "wide", append(parents, base)...))
	if got, err := ancestry(context.Background(), repo, base, tip, false, 129, 129); !got || err != nil {
		t.Fatalf("late base=%t,%v", got, err)
	}
}

// Keep the context implementation's deadline explicit: ancestry cancellation
// tests use Err exclusively and never wait for wall time.
var _ context.Context = (*ancestryCancelContext)(nil)

func TestAncestryFIFOAndSharedParents(t *testing.T) {
	s := &ancestryStore{Storer: memory.NewStorage()}
	repo, err := git.Init(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := storeTree(t, repo)
	base := storeCommit(t, repo, tree, "base")
	missing := plumbing.NewHash("5555555555555555555555555555555555555555")
	shared := storeCommit(t, repo, tree, "shared", missing)
	left := storeCommit(t, repo, tree, "left", shared, shared)
	right := storeCommit(t, repo, tree, "right", shared)
	tip := getCommit(t, repo, storeCommit(t, repo, tree, "tip", left, right))
	s.reads = nil
	if got, err := ancestry(context.Background(), repo, base, tip, false, 5, 6); got || err != nil {
		t.Fatalf("incomplete shared paths=%t,%v", got, err)
	}
	want := []plumbing.Hash{base, left, right, shared, missing}
	if fmt.Sprint(s.reads) != fmt.Sprint(want) {
		t.Fatalf("FIFO reads=%v want=%v", s.reads, want)
	}
	// A later queue sibling proves ancestry after earlier paths encounter a missing node.
	right = storeCommit(t, repo, tree, "right-base", base)
	tip = getCommit(t, repo, storeCommit(t, repo, tree, "late", missing, right))
	if got, err := ancestry(context.Background(), repo, base, tip, false, 3, 3); !got || err != nil {
		t.Fatalf("late queued base=%t,%v", got, err)
	}
	// Cancel inside a queued storage call, rather than only on the base read.
	ctx, cancel := context.WithCancel(context.Background())
	s.hook = func(h plumbing.Hash) error {
		if h == missing {
			cancel()
		}
		return nil
	}
	if got, err := descendsOrEqual(ctx, repo, base, tip); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation=%t,%v", got, err)
	}
	s.hook = nil
	// Repeated entries still inspect context, even when no new node is scheduled.
	parents := make([]plumbing.Hash, 64)
	for i := range parents {
		parents[i] = missing
	}
	parents = append(parents, base)
	tip = getCommit(t, repo, storeCommit(t, repo, tree, "duplicates", parents...))
	countCtx := &ancestryCancelContext{Context: context.Background(), remaining: 32}
	s.reads = nil
	if got, err := descendsOrEqual(countCtx, repo, base, tip); got || !errors.Is(err, context.Canceled) {
		t.Fatalf("duplicate iteration cancellation=%t,%v", got, err)
	}
	if len(s.reads) != 1 {
		t.Fatalf("cancel should precede queue loads: %v", s.reads)
	}
}
