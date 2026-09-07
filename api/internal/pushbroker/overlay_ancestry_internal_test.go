package pushbroker

import (
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestStrictDescendsOverlayParentOrderDepth1 pins the LOAD-BEARING property behind PRD
// #1062 M2 (#1036): a `.github/workflows` checkpoint overlay must be built base-FIRST
// (the prior checkpoint tip as parent[0], the real tip LAST), NOT realTip-first. The
// broker's strict-descendant gate runs go-git `IsAncestor`, a parent[0]-first preorder
// DFS, over its DEPTH-1 object store. This test reproduces that depth-1 store (the
// branch-fork's parent `D_old` is absent) and asserts:
//
//   - base-FIRST overlay  → accepted (IsAncestor finds base at parent[0] immediately and
//     stops, never walking below base into the missing object).
//   - realTip-FIRST overlay → rejected as a BENIGN skip (the walk dives into the real
//     tip's chain first, runs off the depth-1 pack into the absent `D_old`, surfaces
//     plumbing.ErrObjectNotFound, and descendsOrEqual maps it to (false, nil)).
//
// The original PRD draft specified realTip-first; that shipped broken (every second and
// later sequential overlay skipped `not_descendant`, so a behind-on-workflows branch
// checkpointed once then went dark). This test is the executable guard against a
// regression that flips the parent order back — the agent-side shape tests use a
// FULL-graph merge-base that is parent-order-independent and would NOT catch it. It lives
// here (internal test) because strictDescends is unexported and the depth-1 property is
// only reproducible against the broker's own store.
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

	// base-FIRST: accepted. IsAncestor hits base at parent[0] and stops before the shallow
	// boundary.
	ok, err := strictDescends(repo, prevRef, baseFirst)
	if err != nil {
		t.Fatalf("base-first: unexpected error: %v", err)
	}
	if !ok {
		t.Fatalf("base-first overlay must be accepted as a strict descendant of the prior ckpt ref")
	}

	// realTip-FIRST: rejected as a benign skip. The parent[0]-first walk runs off the
	// depth-1 pack into the absent D_old → ErrObjectNotFound → (false, nil). This is the
	// exact failure the parent-flip fixes; a regression that flips the order back reddens
	// here.
	ok, err = strictDescends(repo, prevRef, realTipFirst)
	if err != nil {
		t.Fatalf("realtip-first: the shallow-boundary miss must map to (false, nil), got err: %v", err)
	}
	if ok {
		t.Fatalf("realtip-first overlay must NOT be accepted on a depth-1 store " +
			"(the parent[0]-first walk runs off the pack before reaching the base) — " +
			"this is the not_descendant regression the base-first parent order fixes")
	}
}

// TestStrictDescendsLostAckReconcileDepth1 pins the depth-1 broker property that makes the
// agent-side two-tip checkpoint reconciliation (issue #1086, F2 from #1036) correct. When
// the broker ACCEPTS an overlay but its HTTP ACK is lost in flight, the broker's ref has
// already advanced to that accepted-but-unconfirmed overlay (the ATTEMPTED tip), while the
// agent still believes the last CONFIRMED tip is the ref. The agent's fix chains the NEXT
// overlay from the ATTEMPTED tip, not the stale confirmed one; this test proves WHY that is
// the only chaining choice the broker's strict-descendant gate accepts.
//
// The gate runs go-git `IsAncestor`, a parent[0]-first preorder DFS, over the broker's
// DEPTH-1 object store (the branch-fork's parent `dOld` is absent). With the broker's ref
// now at the accepted overlay `o1` (base = `o1`):
//
//   - O2 chained from the STALE confirmed tip (parent[0] = branchBase == C) → REJECTED as a
//     BENIGN non-descendant. The parent[0]-first walk goes o2Stale → branchBase → dOld
//     (absent) and runs off the depth-1 pack before ever reaching `o1`, surfacing
//     plumbing.ErrObjectNotFound, which descendsOrEqual maps to (false, nil). This is the
//     BUG the agent-side fix avoids: a next overlay chained from the confirmed tip would be
//     silently skipped not_descendant, stalling checkpoints after a lost ACK.
//   - O2 chained from the ATTEMPTED tip (parent[0] = o1) → ACCEPTED. The walk hits
//     parent[0] == base immediately and stops before the shallow boundary. This is the FIX's
//     chaining choice.
//
// The broker PRODUCTION code is UNCHANGED: this is a property/guard test documenting why the
// agent chains from the attempted tip, complementary to the agent-side regression test in
// agent/test/runner-checkpoint.test.ts. It lives here (internal test) because strictDescends
// is unexported and the depth-1 shallow-boundary property is only reproducible against the
// broker's own store. Failure messages name the lost-ACK scenario so a regression reads
// clearly.
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

	// STALE-chained: rejected as a benign non-descendant. The parent[0]-first walk runs off
	// the depth-1 pack (o2Stale → branchBase → dOld absent) before reaching o1 →
	// ErrObjectNotFound → (false, nil). This is the lost-ACK bug the agent-side fix avoids.
	ok, err := strictDescends(repo, o1, o2Stale)
	if err != nil {
		t.Fatalf("lost-ACK stale-chained: the shallow-boundary miss must map to (false, nil), got err: %v", err)
	}
	if ok {
		t.Fatalf("lost-ACK stale-chained overlay must NOT be accepted: chaining the next overlay " +
			"from the STALE confirmed tip after a lost ACK walks off the depth-1 pack before " +
			"reaching the attempted tip — this is the not_descendant stall the agent-side two-tip fix avoids")
	}

	// ATTEMPTED-chained: accepted. The walk hits parent[0] == o1 == base immediately and
	// stops before the shallow boundary. This is the fix's chaining choice.
	ok, err = strictDescends(repo, o1, o2Fixed)
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
