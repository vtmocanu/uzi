package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// stateIssue builds a cached issue with a given state and labels, for the
// close/reopen service tests.
func stateIssue(t *testing.T, state string, labels ...string) store.Issue {
	t.Helper()
	raw, err := json.Marshal(labels)
	if err != nil {
		t.Fatalf("marshal labels: %v", err)
	}
	return store.Issue{RepoID: uuid.New(), ForgeIssueIid: 42, Title: "t", State: state, Labels: raw}
}

// TestCloseIssue covers the bare-close path (PRD #1034 M2): forge-first close, then
// the narrow UpdateIssueState cache flip, with idempotency and snap-back.
func TestCloseIssue(t *testing.T) {
	t.Run("flips cache to closed forge-first", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{}
		issue := stateIssue(t, "opened", "Planned")

		got, err := svc.CloseIssue(context.Background(), f, 7, issue)
		if err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
		// Exactly one forge close, for THIS issue, to StateClosed.
		if len(f.setStateCalls) != 1 || f.setStateCalls[0].issueIID != 42 || f.setStateCalls[0].state != forge.StateClosed {
			t.Fatalf("setState calls = %+v, want one {42 closed}", f.setStateCalls)
		}
		// Exactly one cache flip, via the narrow state-only query, to "closed".
		if len(st.stateUpdates) != 1 || st.stateUpdates[0].State != string(forge.StateClosed) {
			t.Fatalf("stateUpdates = %+v, want one State=closed", st.stateUpdates)
		}
		if st.stateUpdates[0].ForgeIssueIid != 42 {
			t.Fatalf("stateUpdate iid = %d, want 42", st.stateUpdates[0].ForgeIssueIid)
		}
		// Close must NOT route through the label-write query (it touches only state).
		if len(st.labelUpserts) != 0 {
			t.Fatalf("close wrote labels: %+v — close is state-only", st.labelUpserts)
		}
		if got.State != string(forge.StateClosed) {
			t.Fatalf("returned row state = %q, want closed", got.State)
		}
	})

	t.Run("idempotent: already closed makes no forge call and no cache write", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{}
		issue := stateIssue(t, "closed", "Planned")

		got, err := svc.CloseIssue(context.Background(), f, 7, issue)
		if err != nil {
			t.Fatalf("CloseIssue: %v", err)
		}
		if len(f.setStateCalls) != 0 {
			t.Fatalf("forge was called for an already-closed issue: %+v (success criterion 5)", f.setStateCalls)
		}
		if len(st.stateUpdates) != 0 {
			t.Fatalf("cache was written for an already-closed issue: %+v", st.stateUpdates)
		}
		if got.State != "closed" {
			t.Fatalf("returned row state = %q, want the input unchanged (closed)", got.State)
		}
	})

	t.Run("snap-back: forge error leaves the cache untouched", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{setStateErr: errors.New("forge down")}
		issue := stateIssue(t, "opened", "Planned")

		if _, err := svc.CloseIssue(context.Background(), f, 7, issue); err == nil {
			t.Fatal("expected CloseIssue to propagate the forge error")
		}
		if len(st.stateUpdates) != 0 {
			t.Fatalf("cache flip ran despite the forge error: %+v", st.stateUpdates)
		}
	})
}

// TestReopenIssue covers the reopen-then-move path (PRD #1034 M2): forge-first reopen,
// the ReopenIssueState cache flip (state opened + board_position nulled), then AutoMove
// to the drop-target column — including the clobber-regression, Backlog-clear,
// snap-back, and move-half-failure paths.
func TestReopenIssue(t *testing.T) {
	t.Run("flips state then moves to the target column, cache stays opened (clobber-regression)", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{}
		issue := stateIssue(t, "closed") // closed, no column label

		got, err := svc.ReopenIssue(context.Background(), f, 7, issue, boardColumns(), "Planned")
		if err != nil {
			t.Fatalf("ReopenIssue: %v", err)
		}
		// Forge reopened, once, to StateOpened.
		if len(f.setStateCalls) != 1 || f.setStateCalls[0].state != forge.StateOpened {
			t.Fatalf("setState calls = %+v, want one {opened}", f.setStateCalls)
		}
		// Cache flipped via the reopen query (nulls board_position), once.
		if len(st.reopens) != 1 || st.reopens[0].ForgeIssueIid != 42 {
			t.Fatalf("reopens = %+v, want one for iid 42", st.reopens)
		}
		// The move ran: EnsureLabels + UpdateIssueLabels for Planned, then the cache upsert.
		if len(f.updateCalls) != 1 || len(f.updateCalls[0].add) != 1 || f.updateCalls[0].add[0] != "Planned" {
			t.Fatalf("update add = %+v, want [Planned]", f.updateCalls)
		}
		if len(st.labelUpserts) != 1 {
			t.Fatalf("expected exactly one label upsert from the move, got %+v", st.labelUpserts)
		}
		// THE CLOBBER-REGRESSION: the move's cache write MUST carry State "opened". If
		// reopen passed the stale closed struct to AutoMove, this is "closed" and the
		// EXCLUDED.state write re-closes the cache — forge opened, cache closed.
		if st.labelUpserts[0].State != string(forge.StateOpened) {
			t.Fatalf("label upsert State = %q, want %q — the move must not clobber the reopened state back to closed",
				st.labelUpserts[0].State, forge.StateOpened)
		}
		if got.State != string(forge.StateOpened) {
			t.Fatalf("returned row state = %q, want opened", got.State)
		}
	})

	t.Run("reopen to Backlog clears column labels, state opened", func(t *testing.T) {
		st := &fakeStore{reopenLabels: []byte(`["Planned"]`)} // reopened row still carries its old column
		svc := newTestService(st)
		f := &fakeForge{}
		issue := stateIssue(t, "closed", "Planned")

		if _, err := svc.ReopenIssue(context.Background(), f, 7, issue, boardColumns(), ""); err != nil {
			t.Fatalf("ReopenIssue: %v", err)
		}
		if len(st.reopens) != 1 {
			t.Fatalf("reopens = %+v, want one", st.reopens)
		}
		// Backlog target strips the column: remove carries Planned, add is empty.
		if len(f.updateCalls) != 1 || len(f.updateCalls[0].remove) != 1 || f.updateCalls[0].remove[0] != "Planned" {
			t.Fatalf("update remove = %+v, want [Planned] (Backlog clears the column)", f.updateCalls)
		}
		if len(f.updateCalls[0].add) != 0 {
			t.Fatalf("update add = %+v, want empty for a Backlog move", f.updateCalls[0].add)
		}
		if len(st.labelUpserts) != 1 || st.labelUpserts[0].State != string(forge.StateOpened) {
			t.Fatalf("label upsert = %+v, want one with State opened", st.labelUpserts)
		}
		var gotLabels []string
		if err := json.Unmarshal(st.labelUpserts[0].Labels, &gotLabels); err != nil {
			t.Fatalf("label upsert Labels not valid json: %q (%v)", st.labelUpserts[0].Labels, err)
		}
		if len(gotLabels) != 0 {
			t.Fatalf("label upsert Labels = %v, want [] — Backlog clears all column labels", gotLabels)
		}
	})

	t.Run("snap-back: forge reopen error leaves the cache untouched entirely", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		f := &fakeForge{setStateErr: errors.New("forge down")}
		issue := stateIssue(t, "closed")

		if _, err := svc.ReopenIssue(context.Background(), f, 7, issue, boardColumns(), "Planned"); err == nil {
			t.Fatal("expected ReopenIssue to propagate the forge error")
		}
		// Neither the state flip nor the move touched the cache.
		if len(st.reopens) != 0 {
			t.Fatalf("reopen cache flip ran despite the forge error: %+v", st.reopens)
		}
		if len(st.labelUpserts) != 0 {
			t.Fatalf("move cache write ran despite the forge error: %+v", st.labelUpserts)
		}
		if len(f.updateCalls) != 0 {
			t.Fatalf("forge label move ran despite the reopen error: %+v", f.updateCalls)
		}
	})

	t.Run("move-half failure returns the reopened-but-unmoved row with the error", func(t *testing.T) {
		st := &fakeStore{}
		svc := newTestService(st)
		// Forge reopen succeeds, ReopenIssueState succeeds, but the label move fails.
		f := &fakeForge{updateErr: errors.New("move failed")}
		issue := stateIssue(t, "closed")

		got, err := svc.ReopenIssue(context.Background(), f, 7, issue, boardColumns(), "Planned")
		if err == nil {
			t.Fatal("expected ReopenIssue to surface the move-half error")
		}
		// The reopen half DID land (forge + cache), so the card renders open.
		if len(f.setStateCalls) != 1 || f.setStateCalls[0].state != forge.StateOpened {
			t.Fatalf("setState calls = %+v, want one {opened}", f.setStateCalls)
		}
		if len(st.reopens) != 1 {
			t.Fatalf("reopens = %+v, want one — the state flip must persist even when the move fails", st.reopens)
		}
		// The returned row is the reopened row (state opened), not a zero value.
		if got.State != string(forge.StateOpened) {
			t.Fatalf("returned row state = %q, want opened (reopened-but-unmoved)", got.State)
		}
		// The move's cache write never ran (forge move failed first).
		if len(st.labelUpserts) != 0 {
			t.Fatalf("label upsert ran despite the forge move failure: %+v", st.labelUpserts)
		}
	})
}
