package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5: the linked board. A drag writes the mapped State to the YouTrack fake and
// caches the column; a refused or failed move leaves the cache unchanged so the card snaps
// back; close and reopen use the stored States; a sync keeps what a move wrote.

// boardMapping saves the mapping the board tests use: Open, Fixed, and the two board columns.
func (f *linkedHandlerFixture) boardMapping(t *testing.T) {
	t.Helper()
	if rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"}, [2]string{"Human Review", "In Review"})); rr.Code != http.StatusOK {
		t.Fatalf("PUT mapping = %d\n%s", rr.Code, rr.Body.String())
	}
	f.srv.ResetRequests()
}

func (f *linkedHandlerFixture) move(to string) (int, string) {
	rr := f.call(f.h.MoveIssue, http.MethodPost, map[string]string{"to_column": to})
	return rr.Code, rr.Body.String()
}

// cached reads the card's cached state and column.
func (f *linkedHandlerFixture) cached(t *testing.T) (state string, column pgtype.Text) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT state, issue_tracker_column FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, f.issueN).Scan(&state, &column); err != nil {
		t.Fatal(err)
	}
	return state, column
}

func (f *linkedHandlerFixture) assertCache(t *testing.T, wantState, wantColumn string) {
	t.Helper()
	state, col := f.cached(t)
	if state != wantState || col.String != wantColumn || col.Valid != (wantColumn != "") {
		t.Fatalf("cache = state %q column %+v, want state %q column %q (NULL when empty)", state, col, wantState, wantColumn)
	}
}

// labelsAndPosition reads the card's labels and board_position.
func (f *linkedHandlerFixture) labelsAndPosition(t *testing.T) (labels string, position pgtype.Int8) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT labels::text, board_position FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, f.issueN).Scan(&labels, &position); err != nil {
		t.Fatal(err)
	}
	return labels, position
}

// assertOnlyColumnChanged fails when labels or board_position differ from the values read
// before the move.
func (f *linkedHandlerFixture) assertOnlyColumnChanged(t *testing.T, wantLabels string, wantPosition pgtype.Int8) {
	t.Helper()
	labels, position := f.labelsAndPosition(t)
	if labels != wantLabels || position != wantPosition || !position.Valid {
		t.Fatalf("labels %s position %+v after the move, want %s %+v (a move writes only the column)", labels, position, wantLabels, wantPosition)
	}
}

// boardCard reads the card of the fixture's issue off the board the handler serves.
func (f *linkedHandlerFixture) boardCard(t *testing.T) (column string, closed bool) {
	t.Helper()
	rr := f.call(f.h.GetBoard, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("board = %d\n%s", rr.Code, rr.Body.String())
	}
	var b struct {
		Board struct {
			Cards []struct {
				IID    int64  `json:"iid"`
				Column string `json:"column"`
				Closed bool   `json:"closed"`
			} `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Board.Cards {
		if c.IID == f.issueN {
			return c.Column, c.Closed
		}
	}
	t.Fatalf("the board has no card %d: %s", f.issueN, rr.Body.String())
	return "", false
}

func (f *linkedHandlerFixture) assertNoYouTrackWrites(t *testing.T) {
	t.Helper()
	for _, r := range f.srv.Requests() {
		if r.Method != http.MethodGet {
			t.Errorf("the issue tracker saw a write %s %s", r.Method, r.Path)
		}
	}
}

func TestLinkedBoardMoveWritesTheStateAndCachesTheColumnLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	// A move writes only the column: give the row labels and a position to prove they survive.
	if _, err := f.pool.Exec(context.Background(), `UPDATE issues SET labels = '["uzi-ready","keep-me"]'::jsonb, board_position = 7 WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, f.issueN); err != nil {
		t.Fatal(err)
	}
	labels, position := f.labelsAndPosition(t)

	code, body := f.move("In Progress")
	if code != http.StatusOK {
		t.Fatalf("move = %d %s", code, body)
	}
	if got := f.srv.IssueState(f.issueN); got != "Doing" {
		t.Fatalf("YouTrack State = %q, want Doing", got)
	}
	f.assertCache(t, "opened", "In Progress")
	f.assertOnlyColumnChanged(t, labels, position)
	if col, closed := f.boardCard(t); col != "In Progress" || closed {
		t.Fatalf("board card in %q closed=%v, want In Progress", col, closed)
	}

	// Moving to Open writes the stored open State and caches NULL (the Open lane).
	if code, body := f.move(""); code != http.StatusOK {
		t.Fatalf("move to Open = %d %s", code, body)
	}
	if got := f.srv.IssueState(f.issueN); got != "Open" {
		t.Fatalf("YouTrack State = %q, want Open", got)
	}
	f.assertCache(t, "opened", "")
	if col, _ := f.boardCard(t); col != "" {
		t.Fatalf("board card in %q, want the Open lane", col)
	}
}

func TestLinkedBoardMoveToAnUnmappedColumnIsRefusedLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	// Only In Progress is mapped; Human Review is a board column with no State.
	if rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"})); rr.Code != http.StatusOK {
		t.Fatalf("PUT mapping = %d\n%s", rr.Code, rr.Body.String())
	}
	if code, _ := f.move("In Progress"); code != http.StatusOK {
		t.Fatalf("move = %d", code)
	}
	f.srv.ResetRequests()

	code, body := f.move("Human Review")
	if code != http.StatusConflict || !strings.Contains(body, "map this column to an issue tracker State first") {
		t.Fatalf("move = %d %s, want 409 with the mapping hint", code, body)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got != "Doing" {
		t.Fatalf("YouTrack State = %q, want it unchanged", got)
	}
	f.assertCache(t, "opened", "In Progress")
	if col, _ := f.boardCard(t); col != "In Progress" {
		t.Fatalf("the card snapped to %q, want it to stay in In Progress", col)
	}
}

// A drag on a repo whose project's State field a state machine runs (set up after the
// mapping was saved) is a 409 with the state-machine reason; the YouTrack fake sees no write
// and the cache is unchanged so the card snaps back.
func TestLinkedBoardDragOnAStateMachineProjectIs409LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.SetStateMachine(youtracktest.ProjectID, true)
	f.srv.ResetRequests()

	code, body := f.move("In Progress")
	if code != http.StatusConflict || !strings.Contains(body, issuetrackersvc.StateMachineRefusal(youtracktest.ProjectShortName)) {
		t.Fatalf("move = %d %s, want 409 with the state-machine reason", code, body)
	}
	f.assertNoYouTrackWrites(t)
	f.assertCache(t, "opened", "")
	if col, _ := f.boardCard(t); col == "In Progress" {
		t.Fatalf("the card moved to %q, want it to snap back", col)
	}
}

// A drag or a close on a repo whose issue's State field uzi cannot read as a plain one (set up
// after the mapping was saved) is a 409 naming the project and the issue; the YouTrack fake
// sees no write and the cache is unchanged so the card snaps back.
func TestLinkedBoardDragOnAnUnreadableStateFieldIs409LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.SeedStateFieldShape(f.issueN, youtracktest.StateFieldAbsent)
	before := f.srv.IssueState(f.issueN)
	f.srv.ResetRequests()

	code, body := f.move("In Progress")
	key := wantKey(f.issueN)
	if code != http.StatusConflict || !strings.Contains(body, "could not read project") || !strings.Contains(body, key) {
		t.Fatalf("move = %d %s, want 409 naming the project and %s", code, body, key)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got != before {
		t.Fatalf("YouTrack State = %q, want %q unchanged", got, before)
	}
	f.assertCache(t, "opened", "")
	if col, _ := f.boardCard(t); col == "In Progress" {
		t.Fatalf("the card moved to %q, want it to snap back", col)
	}
}

func TestLinkedBoardCloseOnAnUnreadableStateFieldIs409LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.SeedStateFieldShape(f.issueN, youtracktest.StateFieldUntyped)
	before := f.srv.IssueState(f.issueN)
	f.srv.ResetRequests()

	code, body := f.move("closed")
	key := wantKey(f.issueN)
	if code != http.StatusConflict || !strings.Contains(body, "could not read project") || !strings.Contains(body, key) {
		t.Fatalf("close = %d %s, want 409 naming the project and %s", code, body, key)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got != before {
		t.Fatalf("YouTrack State = %q, want %q unchanged", got, before)
	}
	f.assertCache(t, "opened", "")
	if _, closed := f.boardCard(t); closed {
		t.Fatal("the board must not show the card closed")
	}
}

// A card a person resolved in YouTrack while the cache still shows it open is not moved by a
// drag: 409, no State write, the cache unchanged so the card snaps back.
func TestLinkedBoardDragOfAnIssueResolvedInYouTrackIs409LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	// Resolve the issue in YouTrack, then put the cache back as it was before the next sync.
	if code, body := f.move("closed"); code != http.StatusOK {
		t.Fatalf("close = %d %s", code, body)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE issues SET state = 'opened' WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, f.issueN); err != nil {
		t.Fatal(err)
	}
	f.srv.ResetRequests()

	code, body := f.move("In Progress")
	if code != http.StatusConflict || !strings.Contains(body, "resolved in the issue tracker") {
		t.Fatalf("move = %d %s, want 409 naming the resolved issue", code, body)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got != "Fixed" {
		t.Fatalf("YouTrack State = %q, want Fixed untouched", got)
	}
	f.assertCache(t, "opened", "")
}

// A failing issue tracker answers 502 and leaves the cache as it was.
func TestLinkedBoardMoveWhenYouTrackFailsIs502LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.Close() // every request now fails to connect

	code, body := f.move("In Progress")
	if code != http.StatusBadGateway {
		t.Fatalf("move = %d %s, want 502", code, body)
	}
	f.assertCache(t, "opened", "")
}

func TestLinkedBoardCloseAndReopenLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)

	if code, body := f.move("closed"); code != http.StatusOK {
		t.Fatalf("close = %d %s", code, body)
	}
	if got := f.srv.IssueState(f.issueN); got != "Fixed" {
		t.Fatalf("YouTrack State after close = %q, want Fixed", got)
	}
	f.assertCache(t, "closed", "")
	if _, closed := f.boardCard(t); !closed {
		t.Fatal("the board must show the card closed")
	}

	// Reopen onto a mapped column: one State write, the cache opened and in the column.
	f.srv.ResetRequests()
	if code, body := f.move("In Progress"); code != http.StatusOK {
		t.Fatalf("reopen = %d %s", code, body)
	}
	if got := f.srv.IssueState(f.issueN); got != "Doing" {
		t.Fatalf("YouTrack State after reopen = %q, want Doing", got)
	}
	posts := 0
	for _, r := range f.srv.Requests() {
		if r.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("a linked reopen made %d write requests, want exactly 1", posts)
	}
	f.assertCache(t, "opened", "In Progress")
	if col, closed := f.boardCard(t); col != "In Progress" || closed {
		t.Fatalf("board card in %q closed=%v after the reopen", col, closed)
	}
}

func TestLinkedBoardCloseWithNoMappingIs409LiveDB(t *testing.T) {
	f := newStateMappingFixture(t) // no mapping saved
	code, body := f.move("closed")
	if code != http.StatusConflict {
		t.Fatalf("close = %d %s, want 409", code, body)
	}
	if !strings.Contains(body, "open and closed issue tracker States are not set") {
		t.Fatalf("close body = %s, want the unset open/closed States message", body)
	}
	f.assertNoYouTrackWrites(t)
	f.assertCache(t, "opened", "")
}

// A closed State renamed to Won't fix after the mapping was saved is refused with no write.
func TestLinkedBoardCloseOntoARenamedWontFixIsRefusedLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.RenameState("Fixed", "Won't fix")
	f.srv.ResetRequests()

	code, body := f.move("closed")
	if code != http.StatusConflict {
		t.Fatalf("close = %d %s, want 409", code, body)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got == "Won't fix" {
		t.Fatalf("the issue was closed as %q", got)
	}
	f.assertCache(t, "opened", "")
}

// A reopen onto a column with no State changes neither YouTrack nor the cache.
func TestLinkedBoardReopenOntoAnUnmappedColumnChangesNothingLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	if rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"})); rr.Code != http.StatusOK {
		t.Fatalf("PUT mapping = %d\n%s", rr.Code, rr.Body.String())
	}
	if code, body := f.move("closed"); code != http.StatusOK {
		t.Fatalf("close = %d %s", code, body)
	}
	f.srv.ResetRequests()

	code, body := f.move("Human Review")
	if code != http.StatusConflict {
		t.Fatalf("reopen = %d %s, want 409", code, body)
	}
	f.assertNoYouTrackWrites(t)
	if got := f.srv.IssueState(f.issueN); got != "Fixed" {
		t.Fatalf("YouTrack State = %q, want it still Fixed", got)
	}
	f.assertCache(t, "closed", "")
}

// A card dragged after its issue left the linked project is a 404, and the cache is unchanged.
func TestLinkedBoardMoveOfAMovedAwayIssueIs404LiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	f.srv.MoveIssue(f.issueN, youtracktest.OtherProjectID)
	f.srv.ResetRequests()

	code, body := f.move("In Progress")
	if code != http.StatusNotFound || !strings.Contains(body, "no longer in the linked project") {
		t.Fatalf("move = %d %s, want 404", code, body)
	}
	f.assertNoYouTrackWrites(t)
	f.assertCache(t, "opened", "")
}

// The linked sync reads the State of each issue into the column: a mapped State shows the card in
// its column, a move then a sync keeps the column, and an unmapped State is the Open lane.
func TestLinkedSyncCachesTheColumnOfTheStateLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	if code, body := f.move("In Progress"); code != http.StatusOK {
		t.Fatalf("move = %d %s", code, body)
	}

	// Forget the cached column, as a sync of an older build would have left it.
	if _, err := f.pool.Exec(context.Background(), `UPDATE issues SET issue_tracker_column = NULL WHERE repo_id = $1`, f.repo); err != nil {
		t.Fatal(err)
	}
	if rr := f.call(f.h.SyncRepo, http.MethodPost, nil); rr.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rr.Code, rr.Body.String())
	}
	f.assertCache(t, "opened", "In Progress")
	if col, _ := f.boardCard(t); col != "In Progress" {
		t.Fatalf("board card in %q after the sync, want In Progress", col)
	}

	// A sync right after a move keeps the column: the move was written to YouTrack first.
	if code, body := f.move("Human Review"); code != http.StatusOK {
		t.Fatalf("second move = %d %s", code, body)
	}
	if rr := f.call(f.h.SyncRepo, http.MethodPost, nil); rr.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rr.Code, rr.Body.String())
	}
	f.assertCache(t, "opened", "Human Review")

	// The mapping loses "Human Review": its State is now unmapped, so the card is the Open lane.
	if rr := f.putStates(mappingBody("Open", "Fixed", [2]string{"In Progress", "Doing"})); rr.Code != http.StatusOK {
		t.Fatalf("PUT mapping = %d\n%s", rr.Code, rr.Body.String())
	}
	if rr := f.call(f.h.SyncRepo, http.MethodPost, nil); rr.Code != http.StatusOK {
		t.Fatalf("sync = %d %s", rr.Code, rr.Body.String())
	}
	f.assertCache(t, "opened", "")
	if col, _ := f.boardCard(t); col != "" {
		t.Fatalf("board card in %q for an unmapped State, want the Open lane", col)
	}
}

// Removing a board column clears the cached column of its cards in the same transaction, so the
// card is back in the Open lane and no cached column names a column the board lacks.
func TestConfigureColumnsClearsTheCachedColumnOfARemovedColumnLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	if code, body := f.move("Human Review"); code != http.StatusOK {
		t.Fatalf("move = %d %s", code, body)
	}

	rr := f.call(f.h.ConfigureColumns, http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": "In Progress"}}})
	if rr.Code != http.StatusOK {
		t.Fatalf("ConfigureColumns = %d\n%s", rr.Code, rr.Body.String())
	}
	f.assertCache(t, "opened", "")
	if col, _ := f.boardCard(t); col != "" {
		t.Fatalf("board card in %q after its column was removed, want the Open lane", col)
	}

	// A card in a column that stays keeps it.
	f.boardMapping(t)
	if code, body := f.move("In Progress"); code != http.StatusOK {
		t.Fatalf("move = %d %s", code, body)
	}
	if rr := f.call(f.h.ConfigureColumns, http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": "In Progress"}, {"label_name": "Backlog"}}}); rr.Code != http.StatusOK {
		t.Fatalf("ConfigureColumns = %d\n%s", rr.Code, rr.Body.String())
	}
	f.assertCache(t, "opened", "In Progress")
}

// The unlinked upserts never touch issue_tracker_column: a forge repo's sync leaves it as it is
// (NULL on every forge row).
func TestUnlinkedUpsertsLeaveTheCachedColumnAloneLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE issues SET issue_tracker_column = 'In Progress' WHERE repo_id = $1`, f.repo); err != nil {
		t.Fatal(err)
	}
	row, err := f.h.q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: f.repo, ForgeIssueIid: f.issueN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID: row.RepoID, ForgeIssueIid: row.ForgeIssueIid, Title: "renamed", State: row.State, Labels: row.Labels,
		AssigneeIds: row.AssigneeIds, WebUrl: row.WebUrl, ForgeUpdatedAt: row.ForgeUpdatedAt,
	}); err != nil {
		t.Fatal(err)
	}
	f.assertCache(t, "opened", "In Progress")
	if _, err := f.h.q.UpsertIssueLabels(ctx, store.UpsertIssueLabelsParams{
		RepoID: row.RepoID, ForgeIssueIid: row.ForgeIssueIid, Title: row.Title, State: row.State, Labels: row.Labels,
		WebUrl: row.WebUrl, ForgeUpdatedAt: row.ForgeUpdatedAt, AddLabels: []byte(`["x"]`), RemoveLabels: []byte(`[]`),
	}); err != nil {
		t.Fatal(err)
	}
	f.assertCache(t, "opened", "In Progress")
}
