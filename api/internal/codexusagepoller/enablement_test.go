package codexusagepoller

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1732 M3b: the engine side of the Codex enablement rules. The listings decide WHICH
// accounts are live (store live-DB tests); these tests pin what the engine does with them:
// every write carries the enablement list the listing captured (D13), and a poke runs the
// recovery pass before it polls (D7).

// TestWritesCarryListingEnablementSig: the reading and the failure write of both the tick and
// the poke carry the enablement list their listing row captured, so the store can refuse a
// poll that raced an enable/disable.
func TestWritesCarryListingEnablementSig(t *testing.T) {
	userID, okAcct, badAcct := uuid.New(), uuid.New(), uuid.New()
	st := newFakeStore()
	okRow, badRow := linkedRow(userID, okAcct, 1, 1, "idle", false, false), linkedRow(userID, badAcct, 1, 1, "idle", false, false)
	okRow.EnablementSig, badRow.EnablementSig = "ok-sig", "bad-sig"
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{okRow, badRow}
	st.forUser[userID] = []store.ListLinkedCodexAccountsForUserRow{
		{UserID: userID, ProviderAccountID: okAcct, Generation: 1, CredentialRevision: 1, CoordState: "idle", EnablementSig: "poke-ok-sig"},
		{UserID: userID, ProviderAccountID: badAcct, Generation: 1, CredentialRevision: 1, CoordState: "idle", EnablementSig: "poke-bad-sig"},
	}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{
		okAcct:  {reading: sampleReading(1, 1)},
		badAcct: {err: &workersvc.CodexUsageFailure{Kind: workersvc.CodexUsageFailTransient}},
	}}
	e := newEngineFor(st, col, &fakeReconciler{})

	e.tickAll(context.Background())
	e.pokeUser(context.Background(), userID)

	if len(st.upserts) != 2 || len(st.failures) != 2 {
		t.Fatalf("writes = %d upserts / %d failures, want 2 / 2", len(st.upserts), len(st.failures))
	}
	for i, want := range []string{"ok-sig", "poke-ok-sig"} {
		if got := st.upserts[i].EnablementSig; got != want {
			t.Fatalf("upsert #%d enablement sig = %q, want %q", i+1, got, want)
		}
	}
	for i, want := range []string{"bad-sig", "poke-bad-sig"} {
		if got := st.failures[i].EnablementSig; got != want {
			t.Fatalf("failure #%d enablement sig = %q, want %q", i+1, got, want)
		}
	}
}

// recoveringCollector is a collector that also implements Recoverer, like *workersvc.Service.
// Its recovery makes the account pollable by swapping the store's poke listing row, the way a
// promotion advances the generation and clears the slot.
type recoveringCollector struct {
	*fakeCollector
	mu        sync.Mutex
	recovered []uuid.UUID
	onRecover func(accountID uuid.UUID)
}

func (c *recoveringCollector) ReconcileUnresolvedCodexRefresh(_ context.Context, _ uuid.UUID, accountID uuid.UUID) (int, error) {
	c.mu.Lock()
	c.recovered = append(c.recovered, accountID)
	c.mu.Unlock()
	if c.onRecover != nil {
		c.onRecover(accountID)
	}
	return 0, nil
}

// TestPokeRecoversBeforePolling (D7): a poke runs the recovery pass on each live account left
// unpollable by an unfinished refresh or a protected recovery slot, re-reads the listing, and
// then polls the recovered account under its new state. A healthy account and a reauth-only
// account are not recovery targets, and the tick never runs recovery (the sweeper owns the
// periodic pass).
func TestPokeRecoversBeforePolling(t *testing.T) {
	userID := uuid.New()
	slotAcct, wedgedAcct, healthyAcct, reauthAcct := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	st := newFakeStore()
	st.forUser[userID] = []store.ListLinkedCodexAccountsForUserRow{
		{UserID: userID, ProviderAccountID: slotAcct, CoordState: "quarantined", HasRecovery: true, EnablementSig: "s0"},
		{UserID: userID, ProviderAccountID: wedgedAcct, CoordState: "in_progress", EnablementSig: "w0"},
		{UserID: userID, ProviderAccountID: healthyAcct, CoordState: "idle", EnablementSig: "h0"},
		{UserID: userID, ProviderAccountID: reauthAcct, CoordState: "idle", ReauthRequired: true, EnablementSig: "r0"},
	}
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, slotAcct, 0, 0, "quarantined", false, true)}
	col := &recoveringCollector{fakeCollector: &fakeCollector{results: map[uuid.UUID]collectResult{
		slotAcct:    {reading: sampleReading(1, 0)},
		healthyAcct: {reading: sampleReading(0, 0)},
	}}}
	col.onRecover = func(accountID uuid.UUID) {
		if accountID != slotAcct {
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		rows := st.forUser[userID]
		rows[0] = store.ListLinkedCodexAccountsForUserRow{UserID: userID, ProviderAccountID: slotAcct, Generation: 1, CoordState: "idle", EnablementSig: "s1"}
	}
	e := New(st, col, &fakeReconciler{}, defaultBackoff, quietLogger())

	e.tickAll(context.Background())
	if len(col.recovered) != 0 {
		t.Fatalf("the tick ran recovery on %v, want none", col.recovered)
	}

	e.pokeUser(context.Background(), userID)
	got := map[uuid.UUID]bool{}
	for _, a := range col.recovered {
		got[a] = true
	}
	if len(col.recovered) != 2 || !got[slotAcct] || !got[wedgedAcct] {
		t.Fatalf("recovered = %v, want exactly the slot and the wedged accounts", col.recovered)
	}
	polled := map[uuid.UUID]bool{}
	for _, a := range col.calls {
		polled[a] = true
	}
	if !polled[slotAcct] || !polled[healthyAcct] || polled[wedgedAcct] || polled[reauthAcct] {
		t.Fatalf("polled = %v, want the recovered and the healthy account only", col.calls)
	}
	for _, u := range st.upserts {
		if u.ProviderAccountID == slotAcct && (u.EnablementSig != "s1" || u.ObservedGeneration != 1) {
			t.Fatalf("recovered account written under %q gen %d, want the re-read row (s1, 1)", u.EnablementSig, u.ObservedGeneration)
		}
	}
}

// TestPokeWithoutRecovererStillPolls: a collector that does not implement Recoverer (a test
// fake) skips recovery and polls as before.
func TestPokeWithoutRecovererStillPolls(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.forUser[userID] = []store.ListLinkedCodexAccountsForUserRow{
		{UserID: userID, ProviderAccountID: acct, CoordState: "idle", EnablementSig: "x"},
	}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(0, 0)}}}
	newEngineFor(st, col, &fakeReconciler{}).pokeUser(context.Background(), userID)
	if len(col.calls) != 1 || len(st.upserts) != 1 {
		t.Fatalf("collect=%d upserts=%d, want 1/1", len(col.calls), len(st.upserts))
	}
}
