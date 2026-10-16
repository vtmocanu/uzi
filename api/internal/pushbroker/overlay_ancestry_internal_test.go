package pushbroker

import (
	"context"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestStrictDescendsOverlayParentOrderDepth1 accepts either overlay parent order
// when the prior checkpoint is reachable in the available depth-1 store. Missing
// side paths cannot hide that proof. Raw tips without a chain remain refused.
func TestStrictDescendsOverlayParentOrderDepth1(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	emptyTree := storeTree(t, repo)

	// D_old: the branch-fork's parent, EXCLUDED by the broker's depth-1 fetch. Never stored,
	// so any walk that reaches it surfaces plumbing.ErrObjectNotFound — exactly the shallow
	// boundary descendsOrEqual documents.
	dOld := plumbing.NewHash("1111111111111111111111111111111111111111")

	branchBase := storeCommit(t, repo, emptyTree, "branch-base", dOld)
	realTipPrev := storeCommit(t, repo, emptyTree, "realTip-prev", branchBase)
	realTipNew := storeCommit(t, repo, emptyTree, "realTip-new", realTipPrev)

	// The prior checkpoint ref tip (a first overlay): base = parent[0], realTipPrev last.
	prevRef := storeCommit(t, repo, emptyTree, "ckpt(overlay): prev", branchBase, realTipPrev)

	// The two candidate shapes for the NEXT (second sequential) overlay.
	baseFirst := getCommit(t, repo, storeCommit(t, repo, emptyTree, "ckpt(overlay): base-first", prevRef, realTipNew))
	realTipFirst := getCommit(t, repo, storeCommit(t, repo, emptyTree, "ckpt(overlay): realtip-first", realTipNew, prevRef))

	// Base-first overlay reaches the validated prior checkpoint.
	ok, err := strictDescends(context.Background(), repo, prevRef, baseFirst)
	if err != nil {
		t.Fatalf("base-first: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("base-first overlay must be accepted as a strict descendant of the prior ckpt ref")
	}

	// Real-tip-first also proves the prior checkpoint, despite a missing side path.
	ok, err = strictDescends(context.Background(), repo, prevRef, realTipFirst)
	if err != nil || !ok {
		t.Fatalf("realtip-first must be accepted: %t, %v", ok, err)
	}

	// RAW realTipNew over the prior ref: refused (issue #2733). A tick or iteration checkpoint
	// that packs the bare real tip, with no chain over the prior overlay, is never a descendant
	// of prevRef on any store (prevRef is not among its ancestors); on this depth-1 store the
	// walk also encounters the absent D_old, but no available path proves ancestry. The
	// agent-side chain [prevRef, realTipNew] is what the base-first case above accepts.
	ok, err = strictDescends(context.Background(), repo, prevRef, getCommit(t, repo, realTipNew))
	if err != nil {
		t.Fatalf("raw realTipNew: the shallow-boundary miss must map to (false, nil), got err: %v", err)
	}
	if ok {
		t.Fatalf("a raw realTipNew must NOT be accepted over the prior overlay ref " +
			"(it is why tick and iteration checkpoints are chained over the overlay)")
	}
}

// TestStrictDescendsLostAckReconcileDepth1 requires the next overlay to chain
// from the accepted attempted tip after a lost ACK. The stale confirmed chain
// cannot reach that base, regardless of missing side paths or parent order.
func TestStrictDescendsLostAckReconcileDepth1(t *testing.T) {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		t.Fatalf("git.Init: %v", err)
	}

	emptyTree := storeTree(t, repo)

	// dOld: the branch-fork's parent, EXCLUDED by the broker's depth-1 fetch. Never stored,
	// so any walk that reaches it surfaces plumbing.ErrObjectNotFound — the shallow boundary.
	dOld := plumbing.NewHash("2222222222222222222222222222222222222222")

	// branchBase == C: the last CONFIRMED tip the agent still believes is the broker ref.
	branchBase := storeCommit(t, repo, emptyTree, "branch-base", dOld)
	realTipPrev := storeCommit(t, repo, emptyTree, "realTip-prev", branchBase)
	realTipNew := storeCommit(t, repo, emptyTree, "realTip-new", realTipPrev)

	// o1: the overlay the broker ACCEPTED (advancing its ref) but whose ACK the agent never
	// received. base-FIRST: parent[0] = branchBase (== C), realTipPrev LAST.
	o1 := storeCommit(t, repo, emptyTree, "ckpt(overlay): O1 (accepted, ACK lost)", branchBase, realTipPrev)

	// o2Stale: the BUGGY next overlay (pre-fix) chained from the STALE confirmed tip
	// (parent[0] = branchBase == C).
	o2Stale := getCommit(t, repo, storeCommit(t, repo, emptyTree, "ckpt(overlay): O2 chained from STALE confirmed", branchBase, realTipNew))

	// o2Fixed: the FIXED next overlay chained from the ATTEMPTED tip (parent[0] = o1).
	o2Fixed := getCommit(t, repo, storeCommit(t, repo, emptyTree, "ckpt(overlay): O2 chained from ATTEMPTED", o1, realTipNew))

	// Stale chaining never reaches the accepted attempted tip.
	ok, err := strictDescends(context.Background(), repo, o1, o2Stale)
	if err != nil {
		t.Fatalf("lost-ACK stale-chained: the shallow-boundary miss must map to (false, nil), got err: %v", err)
	}
	if ok {
		t.Fatalf("lost-ACK stale-chained overlay must NOT be accepted: chaining the next overlay " +
			"from the STALE confirmed tip after a lost ACK has no path to " +
			"the attempted tip — this is the not_descendant stall the agent-side two-tip fix avoids")
	}

	// ATTEMPTED-chained: accepted. The walk hits parent[0] == o1 == base immediately and
	// stops before the shallow boundary. This is the fix's chaining choice.
	ok, err = strictDescends(context.Background(), repo, o1, o2Fixed)
	if err != nil {
		t.Fatalf("lost-ACK attempted-chained: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("lost-ACK attempted-chained overlay must be accepted as a strict descendant of the " +
			"ATTEMPTED (accepted-but-unconfirmed) tip — this is why the agent chains the next overlay " +
			"from the attempted tip, not the stale confirmed tip")
	}
}

func storeTree(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	tree := &object.Tree{}
	obj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		t.Fatalf("encode tree: %v", err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store tree: %v", err)
	}
	return h
}

// storeCommit builds and stores a commit with the given tree and parent hashes (parent
// ORDER preserved — the whole point of the test) and returns its hash. A parent hash need
// not be present in the storer: an absent parent models the depth-1 shallow boundary.
func storeCommit(t *testing.T, repo *git.Repository, tree plumbing.Hash, msg string, parents ...plumbing.Hash) plumbing.Hash {
	t.Helper()
	when := time.Unix(1_700_000_000, 0).UTC()
	c := &object.Commit{
		Author:       object.Signature{Name: "t", Email: "t@t", When: when},
		Committer:    object.Signature{Name: "t", Email: "t@t", When: when},
		Message:      msg,
		TreeHash:     tree,
		ParentHashes: parents,
	}
	obj := repo.Storer.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatalf("encode commit %q: %v", msg, err)
	}
	h, err := repo.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit %q: %v", msg, err)
	}
	return h
}

func getCommit(t *testing.T, repo *git.Repository, h plumbing.Hash) *object.Commit {
	t.Helper()
	c, err := object.GetCommit(repo.Storer, h)
	if err != nil {
		t.Fatalf("get commit %s: %v", h, err)
	}
	return c
}
