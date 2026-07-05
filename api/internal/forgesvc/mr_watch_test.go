package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
		IssueIid: iid,
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
