package forgesvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSyncFiledIssueClosesWiring covers what a fake CAN honestly cover for the PRD #98 M6
// pass: that each enumerated edge is applied exactly once, with the right coordinate and a
// rationale hash re-stamped from the CURRENT rationale, that the enumeration is bounded per
// tick, and that a failure is contained rather than aborting the repo's remaining edges.
//
// It does NOT model the edge semantics — a fake replaying the poller's snapshot as
// transition events would test the model rather than the mechanism, which is the very
// failure the edge marker exists to prevent. Once-only, Undo-sticks, dismissed-not-
// overwritten, reopen-does-not-reopen and the repo-scoped join all live in
// handler/judge_issue_close_livedb_test.go against real Postgres.
func TestSyncFiledIssueClosesWiring(t *testing.T) {
	repoID := uuid.New()
	edge := func(rationale string) store.ListFiledIssueCloseEdgesRow {
		return store.ListFiledIssueCloseEdgesRow{
			FiledID: uuid.New(), ReviewID: uuid.New(),
			Category: "improve_uzi", Target: "docs", RationaleMd: rationale,
			FiledIssueIid: pgtype.Int8{Int64: 7, Valid: true},
		}
	}

	t.Run("applies every enumerated edge once, with the resolved coordinate", func(t *testing.T) {
		st := &fakeStore{closeEdges: []store.ListFiledIssueCloseEdgesRow{edge("first"), edge("second")}}
		st.closeApplyRows = store.ApplyFiledIssueCloseEdgeRow{Disposed: 1, Stamped: 1}
		if err := newTestService(st).SyncFiledIssueCloses(context.Background(), repoID); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if len(st.closeApplied) != 2 {
			t.Fatalf("applied %d edges, want 2 (one statement each)", len(st.closeApplied))
		}
		for i, got := range st.closeApplied {
			want := st.closeEdges[i]
			if got.ReviewID != want.ReviewID || got.Category != want.Category || got.Target != want.Target {
				t.Errorf("edge %d coordinate = %+v, want the enumerated row's", i, got)
			}
			if got.FiledID != want.FiledID {
				t.Errorf("edge %d stamps filed id %v, want %v", i, got.FiledID, want.FiledID)
			}
			// The hash is re-stamped from the CURRENT rationale (#94 Decision 3), so an
			// auto-done is not born stale.
			if got.RationaleHash != workersvc.RationaleHash(want.RationaleMd) {
				t.Errorf("edge %d hash = %q, want the hash of the current rationale_md", i, got.RationaleHash)
			}
		}
	})

	t.Run("the per-tick enumeration is bounded", func(t *testing.T) {
		st := &fakeStore{}
		if err := newTestService(st).SyncFiledIssueCloses(context.Background(), repoID); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if len(st.closeEdgeArgs) != 1 {
			t.Fatalf("want one enumeration, got %d", len(st.closeEdgeArgs))
		}
		if st.closeEdgeArgs[0].RepoID != repoID {
			t.Errorf("enumerated repo %v, want %v", st.closeEdgeArgs[0].RepoID, repoID)
		}
		if st.closeEdgeArgs[0].Lim != FiledIssueCloseBatch {
			t.Errorf("limit = %d, want the per-tick batch bound %d", st.closeEdgeArgs[0].Lim, FiledIssueCloseBatch)
		}
	})

	t.Run("one failing edge does not abort the rest", func(t *testing.T) {
		st := &fakeStore{closeEdges: []store.ListFiledIssueCloseEdgesRow{edge("a"), edge("b")}}
		st.closeApplyErr = errors.New("boom")
		// A per-edge failure is logged and skipped; the repo's sync still reports success,
		// so the poller carries on and the unconsumed edges retry next tick.
		if err := newTestService(st).SyncFiledIssueCloses(context.Background(), repoID); err != nil {
			t.Fatalf("a per-edge failure must not fail the repo's sync, got %v", err)
		}
		if len(st.closeApplied) != 2 {
			t.Fatalf("both edges must be attempted, got %d", len(st.closeApplied))
		}
	})

	t.Run("an enumeration failure surfaces to the poller", func(t *testing.T) {
		st := &fakeStore{closeEdgesErr: errors.New("db down")}
		if err := newTestService(st).SyncFiledIssueCloses(context.Background(), repoID); err == nil {
			t.Fatal("an enumeration failure must be returned so the poller logs it")
		}
		if len(st.closeApplied) != 0 {
			t.Fatal("nothing may be applied when the enumeration failed")
		}
	})
}
