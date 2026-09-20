package store_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestPruneUserSidebarCodexAccountsLiveDB pins the atomic stale-id prune (PRD #1209 M1): it
// removes ONLY genuinely stale ids (those with no linked alias) while preserving the stored
// order, and — because it is a single atomic UPDATE, never a read-merge-write — it cannot
// lose a concurrent SetUserSidebarCodexAccounts write.
func TestPruneUserSidebarCodexAccountsLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)

	// Two genuinely-linked accounts and one stale id (a uuid with no linked alias).
	accA, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "keepA", false)
	accB, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "keepB", false)
	stale := uuid.New()

	set := func(ids []uuid.UUID) {
		t.Helper()
		if _, err := q.SetUserSidebarCodexAccounts(ctx, store.SetUserSidebarCodexAccountsParams{
			ID: user, SidebarCodexAccountIds: ids,
		}); err != nil {
			t.Fatalf("SetUserSidebarCodexAccounts: %v", err)
		}
	}
	prune := func() []uuid.UUID {
		t.Helper()
		got, err := q.PruneUserSidebarCodexAccounts(ctx, user)
		if err != nil {
			t.Fatalf("PruneUserSidebarCodexAccounts: %v", err)
		}
		return got
	}

	// Store [A, stale, B]; prune drops the stale id and preserves [A, B] in order.
	set([]uuid.UUID{accA, stale, accB})
	if got := prune(); len(got) != 2 || got[0] != accA || got[1] != accB {
		t.Fatalf("prune = %v, want [%s %s] (stale removed, order preserved)", got, accA, accB)
	}

	// A no-stale set is returned unchanged (order preserved), so prune never disturbs a
	// clean selection.
	set([]uuid.UUID{accB, accA})
	if got := prune(); len(got) != 2 || got[0] != accB || got[1] != accA {
		t.Fatalf("prune of a clean set = %v, want [%s %s] unchanged", got, accB, accA)
	}

	// Concurrency / barrier: a Set (a PUT-equivalent) and a prune (a GET) race. Every id in
	// the Set's target is linked, so prune removes NONE of them — whichever order the two
	// UPDATEs serialize on the row lock, the Set's write must survive IN FULL. A
	// read-merge-write prune could drop the concurrent Set; a single atomic UPDATE cannot,
	// and this asserts that invariant.
	accC, _ := mkLinkedCodexAccount(ctx, t, pool, q, user, "keepC", false)
	target := []uuid.UUID{accB, accA, accC}
	set([]uuid.UUID{accA}) // a different baseline, so a lost Set would be visible
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := q.SetUserSidebarCodexAccounts(ctx, store.SetUserSidebarCodexAccountsParams{
			ID: user, SidebarCodexAccountIds: target,
		})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		_, err := q.PruneUserSidebarCodexAccounts(ctx, user)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent set/prune: %v", err)
		}
	}
	// Final state: the Set's target, intact (all ids linked, so prune is a no-op on it).
	final := prune()
	if len(final) != len(target) {
		t.Fatalf("after concurrent set/prune, selection = %v, want the Set's target %v intact", final, target)
	}
	want := map[uuid.UUID]bool{accA: true, accB: true, accC: true}
	for _, id := range final {
		if !want[id] {
			t.Fatalf("final selection %v contains an id not in the concurrent Set's target %v", final, target)
		}
	}
}
