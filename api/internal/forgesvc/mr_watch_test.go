package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ── helpers ─────────────────────────────────────────────────────────────────

const testProjectID = 77

func forgeMR(iid int64, state string) forge.MergeRequest {
	return forge.MergeRequest{IID: iid, State: state}
}

func mrCols() []store.BoardColumn {
	names := []string{board.ColumnInProgress, board.ColumnHumanReview, "Upcoming", "Later"}
	cols := make([]store.BoardColumn, len(names))
	for i, n := range names {
		cols[i] = store.BoardColumn{LabelName: n, Position: int32(i)}
	}
	return cols
}

func mrIssue(repoID uuid.UUID, iid int64, state string, labels ...string) store.Issue {
	b, _ := json.Marshal(labels)
	return store.Issue{RepoID: repoID, ForgeIssueIid: iid, State: state, Labels: b}
}

func candidate(runID uuid.UUID, iid, mrIID int64, stored pgtype.Text) store.ListMRWatchCandidatesRow {
	return store.ListMRWatchCandidatesRow{
		ID:       runID,
		IssueIid: pgtype.Int8{Int64: iid, Valid: true},
		MrIid:    pgtype.Int8{Int64: mrIID, Valid: true},
		MrState:  stored,
	}
}

func mrNull() pgtype.Text        { return pgtype.Text{} }
func mrTxt(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// run drives SyncMRStates for one candidate whose MR currently reports observed.
func run(t *testing.T, st *fakeStore, f *fakeForge) {
	t.Helper()
	svc := newTestService(st)
	if err := svc.SyncMRStates(context.Background(), uuid.New(), testProjectID, f); err != nil {
		t.Fatalf("SyncMRStates: %v", err)
	}
}

// assertRecorded asserts exactly one mr_state write with the given value.
func assertRecorded(t *testing.T, st *fakeStore, runID uuid.UUID, want string) {
	t.Helper()
	if len(st.mrStateWrites) != 1 {
		t.Fatalf("expected exactly one mr_state write, got %d: %+v", len(st.mrStateWrites), st.mrStateWrites)
	}
	w := st.mrStateWrites[0]
	if w.ID != runID || w.MrState.String != want || !w.MrState.Valid {
		t.Fatalf("mr_state write = {%v %q}, want {%v %q}", w.ID, w.MrState.String, runID, want)
	}
}

func assertNoMove(t *testing.T, f *fakeForge) {
	t.Helper()
	if len(f.updateCalls) != 0 {
		t.Fatalf("expected no card move, got UpdateIssueLabels calls %+v", f.updateCalls)
	}
}

func assertNoRecord(t *testing.T, st *fakeStore) {
	t.Helper()
	if len(st.mrStateWrites) != 0 {
		t.Fatalf("expected the edge to be LEFT (no mr_state write), got %+v", st.mrStateWrites)
	}
}

// ── bootstrap (Decision 9) ──────────────────────────────────────────────────

func TestMRBootstrapRecordsWithoutMoving(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrNull())},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "closed") // recorded, but the card is NOT moved
	if len(f.mrCalls) != 1 || f.mrCalls[0] != 13 {
		t.Fatalf("expected one GetMergeRequest(13), got %v", f.mrCalls)
	}
}

// ── steady state ────────────────────────────────────────────────────────────

func TestMRNoOpWhenStateUnchanged(t *testing.T) {
	runID := uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "opened")}

	run(t, st, f)

	assertNoMove(t, f)
	assertNoRecord(t, st) // no transition → no write at all
}

// ── close edge: opened → closed → In Progress ───────────────────────────────

func TestMRCloseEdgeMovesToInProgress(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	if len(f.updateCalls) != 1 {
		t.Fatalf("expected one card move, got %+v", f.updateCalls)
	}
	mv := f.updateCalls[0]
	if !contains(mv.add, board.ColumnInProgress) {
		t.Fatalf("close edge must ADD In Progress, got add=%v", mv.add)
	}
	if !contains(mv.remove, board.ColumnHumanReview) {
		t.Fatalf("close edge must REMOVE Human Review, got remove=%v", mv.remove)
	}
	assertRecorded(t, st, runID, "closed")
}

func TestMRCloseEdgeForgeFailureLeavesEdge(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed"), updateErr: errors.New("forge unreachable")}

	run(t, st, f)

	if len(f.updateCalls) != 1 {
		t.Fatalf("the move must be ATTEMPTED, got %+v", f.updateCalls)
	}
	// The whole point: a forge-move failure must NOT advance mr_state, so the next
	// tick re-observes the opened→closed edge and retries. Consuming it here would
	// re-create the stuck-card bug this PRD fixes.
	assertNoRecord(t, st)
}

func TestMRCloseEdgeManualDragConsumesEdge(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		// A human already dragged the card out of Human Review to Upcoming.
		issue:   mrIssue(repoID, 9, "opened", "PRD", "Upcoming"),
		columns: mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f) // manual drag wins — never re-fight a human
	assertRecorded(t, st, runID, "closed")
}

func TestMRCloseEdgeClosedIssueConsumesEdge(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f) // Closed is a terminal placement, never a workflow column
	assertRecorded(t, st, runID, "closed")
}

func TestMRCloseEdgeMissingCachedIssueLeavesEdge(t *testing.T) {
	runID := uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issueErr:   pgx.ErrNoRows, // evicted since the candidate scan
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f)
	assertNoRecord(t, st) // deferred: no cache row to move, retry next tick
}

// ── reopen edge: closed → opened → Human Review (Decision 6) ────────────────

func TestMRReopenEdgeMovesToHumanReview(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("closed"))},
		// The close edge left the card in In Progress; reopening restores it.
		issue:   mrIssue(repoID, 9, "opened", "PRD", board.ColumnInProgress),
		columns: mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "opened")}

	run(t, st, f)

	if len(f.updateCalls) != 1 {
		t.Fatalf("expected one card move, got %+v", f.updateCalls)
	}
	mv := f.updateCalls[0]
	if !contains(mv.add, board.ColumnHumanReview) {
		t.Fatalf("reopen edge must ADD Human Review, got add=%v", mv.add)
	}
	if !contains(mv.remove, board.ColumnInProgress) {
		t.Fatalf("reopen edge must REMOVE In Progress, got remove=%v", mv.remove)
	}
	assertRecorded(t, st, runID, "opened")
}

func TestMRReopenEdgeManualDragConsumesEdge(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("closed"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", "Later"), // human moved it elsewhere
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "opened")}

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "opened")
}

func TestMRReopenEdgeForgeFailureLeavesEdge(t *testing.T) {
	// Symmetry with the close-edge forge-failure case: a failed reopen move must
	// NOT advance mr_state, so it stays 'closed' and the next tick retries the
	// closed→opened edge. The poller cadence is the retry loop in both directions.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("closed"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnInProgress),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "opened"), updateErr: errors.New("forge unreachable")}

	run(t, st, f)

	if len(f.updateCalls) != 1 {
		t.Fatalf("the reopen move must be ATTEMPTED, got %+v", f.updateCalls)
	}
	assertNoRecord(t, st) // edge left unconsumed — mr_state stays 'closed'
}

// ── merged / locked no-ops (Risk: locked churn) ─────────────────────────────

func TestMRMergedIsRecordedNeverMoves(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "merged")}

	run(t, st, f)

	assertNoMove(t, f) // merge → issue closes via `Closes #N`; the issue sync owns it
	assertRecorded(t, st, runID, "merged")
}

func TestMRLockedIsRecordedNeverMoves(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "locked")} // transient during merge processing

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "locked")
}

func TestMRUnknownStateInTransitionIsIgnored(t *testing.T) {
	// Reviewer hardening (post-audit): an unrecognized state is ignored ENTIRELY —
	// no move AND no write — so the prior baseline ("opened") is preserved and a
	// later real "closed" still fires the edge. Recording garbage would instead
	// mask the next close until a full reopen cycle re-synced the baseline.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "some_new_state")}

	run(t, st, f)

	assertNoMove(t, f)
	assertNoRecord(t, st) // baseline "opened" left intact — the glitch self-heals
}

func TestMRUnknownOrEmptyStateBootstrapIsIgnored(t *testing.T) {
	// Same rule on the bootstrap path: an empty/unknown first observation writes
	// nothing, so mr_state stays NULL and the next KNOWN state bootstraps cleanly.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrNull())},
		issue:      mrIssue(repoID, 9, "opened", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "")} // empty state is not a known value

	run(t, st, f)

	assertNoMove(t, f)
	assertNoRecord(t, st) // NULL baseline left intact
}

// ── read failure leaves the edge ────────────────────────────────────────────

func TestMRReadErrorLeavesEdge(t *testing.T) {
	runID := uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		columns:    mrCols(),
	}
	f := &fakeForge{mrErr: errors.New("forge down")}

	run(t, st, f)

	assertNoMove(t, f)
	assertNoRecord(t, st) // a read failure is not an edge; leave mr_state, retry next tick
	if len(f.mrCalls) != 1 {
		t.Fatalf("expected the MR read to be attempted once, got %v", f.mrCalls)
	}
}

// ── candidate enumeration ───────────────────────────────────────────────────

func TestMRReworkSuppressionYieldsNoCandidates(t *testing.T) {
	// Decision 4: a non-completed latest run means the SQL yields NO candidate, so
	// the watch is suppressed entirely. At this layer that is an empty candidate
	// set — the watcher must then do nothing (no MR read, no move, no write). The
	// SQL that produces the suppression is exercised by the M5 e2e.
	st := &fakeStore{candidates: nil}
	f := &fakeForge{}

	run(t, st, f)

	if len(f.mrCalls) != 0 {
		t.Fatalf("no candidates must mean no MR reads, got %v", f.mrCalls)
	}
	assertNoMove(t, f)
	assertNoRecord(t, st)
}

func TestMRMultiCandidateIsolation(t *testing.T) {
	// One bad candidate must never stall the rest of the loop (poller log-and-skip
	// convention). Candidate A's MR read fails; candidate B still gets read, moved,
	// and recorded. Both fakeStore.GetIssueByIID lookups return the same in-review
	// issue — fine, the point is that B is processed despite A failing.
	runA, runB := uuid.New(), uuid.New()
	repoID := uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{
			candidate(runA, 9, 13, mrTxt("opened")),
			candidate(runB, 10, 14, mrTxt("opened")),
		},
		issue:   mrIssue(repoID, 10, "opened", "PRD", board.ColumnHumanReview),
		columns: mrCols(),
	}
	f := &fakeForge{
		mrErrByIID: map[int64]error{13: errors.New("forge down for this MR")},
		mrByIID:    map[int64]forge.MergeRequest{14: forgeMR(14, "closed")},
	}

	run(t, st, f)

	if len(f.mrCalls) != 2 {
		t.Fatalf("both candidates must be read (A's failure must not stall the loop), got %v", f.mrCalls)
	}
	if len(f.updateCalls) != 1 {
		t.Fatalf("only candidate B moves; A failed its read, got %+v", f.updateCalls)
	}
	// The single mr_state write belongs to B (A wrote nothing — its edge is left).
	if len(st.mrStateWrites) != 1 || st.mrStateWrites[0].ID != runB || st.mrStateWrites[0].MrState.String != "closed" {
		t.Fatalf("expected exactly B's 'closed' write, got %+v", st.mrStateWrites)
	}
}

func TestMRCandidateListErrorPropagates(t *testing.T) {
	st := &fakeStore{candidatesErr: errors.New("db down")}
	f := &fakeForge{}
	svc := newTestService(st)

	if err := svc.SyncMRStates(context.Background(), uuid.New(), testProjectID, f); err == nil {
		t.Fatal("a candidate-enumeration failure must surface so the poller logs it once")
	}
	if len(f.mrCalls) != 0 {
		t.Fatal("no MR reads when candidates could not be listed")
	}
}

// ── closed-issue terminal recording (Lane B, #527) ──────────────────────────
//
// A merge CLOSES the issue (via `Closes #N`) before SyncMRStates runs, so a
// merged/closed terminal state is only ever observed once i.state='closed'. Lane B
// of ListMRWatchCandidates admits those closed-issue runs; the watcher must RECORD
// the terminal state but never MOVE a card (Closed is terminal, not a workflow
// column). These pin that watcher-side behaviour with a closed issue in the cache.

func TestMRClosedIssueMergedRecordsNeverMoves(t *testing.T) {
	// stored="opened" exercises the merged `default` arm (stored valid, observed
	// merged is a KNOWN non-edge transition): record, never move — and the issue is
	// closed so guardedMRMove would skip anyway.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("opened"))},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "merged")}

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "merged")
}

func TestMRClosedIssueMergedBootstrapRecords(t *testing.T) {
	// stored=NULL: the bootstrap arm records the first observation without moving.
	// This is the historical-backfill path — a merged PR whose merge closed the
	// issue before uzi ever recorded an mr_state.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrNull())},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "merged")}

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "merged")
}

func TestMRClosedIssueClosedRecordsNeverMoves(t *testing.T) {
	// stored=NULL, observed "closed": the bootstrap arm short-circuits BEFORE the
	// close-edge switch, so a closed issue never reaches guardedMRMove for the
	// close edge on the bootstrap path. Records "closed", moves nothing.
	runID, repoID := uuid.New(), uuid.New()
	st := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrNull())},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f := &fakeForge{mr: forgeMR(13, "closed")}

	run(t, st, f)

	assertNoMove(t, f)
	assertRecorded(t, st, runID, "closed")
}

// TestMRClosedIssueBackfillDecayIsIdempotent models two poller ticks over one
// historical closed-issue merged run. Tick 1 backfills the terminal state; tick 2
// (were the run still returned) must be a no-op, so re-polling a settled run costs
// nothing.
//
// In production Lane B's `l.mr_state IN ('opened','locked')` exclusion means a
// terminal run is NOT even returned by the query after tick 1 — the LiveDB
// fixtures 110/111 pin that query-side decay. This test pins the complementary
// watcher-side idempotency: even if the row WERE returned, observed==stored yields
// no write.
func TestMRClosedIssueBackfillDecayIsIdempotent(t *testing.T) {
	runID, repoID := uuid.New(), uuid.New()

	// Tick 1: fresh candidate with NULL mr_state (never recorded), closed issue,
	// MR observed merged → exactly one "merged" write and exactly one MR read.
	st1 := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrNull())},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f1 := &fakeForge{mr: forgeMR(13, "merged")}

	run(t, st1, f1)

	assertNoMove(t, f1)
	assertRecorded(t, st1, runID, "merged")
	if len(f1.mrCalls) != 1 || f1.mrCalls[0] != 13 {
		t.Fatalf("tick 1: expected one GetMergeRequest(13), got %v", f1.mrCalls)
	}

	// Tick 2: the DB now holds mr_state='merged' (what tick 1 wrote). Model the row
	// the query WOULD return if it still selected the run — observed==stored → no-op.
	st2 := &fakeStore{
		candidates: []store.ListMRWatchCandidatesRow{candidate(runID, 9, 13, mrTxt("merged"))},
		issue:      mrIssue(repoID, 9, "closed", "PRD", board.ColumnHumanReview),
		columns:    mrCols(),
	}
	f2 := &fakeForge{mr: forgeMR(13, "merged")}

	run(t, st2, f2)

	assertNoMove(t, f2)
	assertNoRecord(t, st2) // observed=="merged"==stored → idempotent, no second write
}
