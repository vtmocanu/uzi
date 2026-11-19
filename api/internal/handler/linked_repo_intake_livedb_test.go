package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 intake through the handlers: the linked sync caches the eligibility
// tag's issues with their issue key, the board returns it, a manual run starts on a
// cached tagged issue, and eviction follows the full form only.

func (f *linkedHandlerFixture) issueRow(t *testing.T, n int64) (store.Issue, bool) {
	t.Helper()
	row, err := f.h.q.GetIssueByIID(context.Background(), store.GetIssueByIIDParams{RepoID: f.repo, ForgeIssueIid: n})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Issue{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func (f *linkedHandlerFixture) tracker(t *testing.T) forge.IssueTracker {
	t.Helper()
	tr, err := f.h.svc.IssueTrackerForRepo(context.Background(), f.user.ID, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func (f *linkedHandlerFixture) syncOK(t *testing.T) {
	t.Helper()
	if rr := f.call(f.h.SyncRepo, http.MethodPost, nil); rr.Code != http.StatusOK {
		t.Fatalf("sync status = %d body = %s", rr.Code, rr.Body.String())
	}
}

// Acceptance 1: a tagged issue is cached with its key and the board returns it; a
// manual run starts on it with the issue's title and description; an issue carrying
// only a same-named tag of another user is neither cached nor startable.
func TestLinkedRepoTaggedIssueIsCachedKeyedAndStartableLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	ctx := context.Background()
	foreign := f.srv.SeedIssue("foreign tag", "no", youtracktest.SameNameTagID)
	f.syncOK(t)

	row, ok := f.issueRow(t, f.issueN)
	if !ok || row.IssueKey.String != youtracktest.ProjectShortName+"-"+strconv.FormatInt(f.issueN, 10) {
		t.Fatalf("cached row = %+v (found %v), want issue_key %s-%d", row, ok, youtracktest.ProjectShortName, f.issueN)
	}
	if _, ok := f.issueRow(t, foreign); ok {
		t.Fatal("an issue carrying only a same-named tag of another user was cached")
	}

	rr := f.call(f.h.GetBoard, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("board status = %d body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Board struct {
			Cards []struct {
				IID      int64  `json:"iid"`
				IssueKey string `json:"issue_key"`
			} `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	keys := map[int64]string{}
	for _, c := range body.Board.Cards {
		keys[c.IID] = c.IssueKey
	}
	if want := youtracktest.ProjectShortName + "-" + strconv.FormatInt(f.issueN, 10); keys[f.issueN] != want {
		t.Fatalf("board cards = %v, want %d keyed %q", keys, f.issueN, want)
	}
	if _, listed := keys[foreign]; listed {
		t.Fatalf("the board lists the foreign-tag issue: %v", keys)
	}

	f.h.wsvc.SetForges(f.h.svc)
	f.h.wsvc.SetIssueSources(f.h.svc)
	f.seedRunCredentials(t)
	run := f.call(f.h.CreateRun, http.MethodPost, map[string]any{"issue_iid": f.issueN})
	if run.Code != http.StatusCreated {
		t.Fatalf("manual run on the tagged issue: status = %d body = %s", run.Code, run.Body.String())
	}
	var title, desc string
	if err := f.pool.QueryRow(ctx, `SELECT issue_title, issue_description FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, f.issueN).Scan(&title, &desc); err != nil {
		t.Fatal(err)
	}
	if title != "a youtrack card" || desc != "yt body" {
		t.Fatalf("run title/description = %q/%q, want the issue's own", title, desc)
	}

	// The foreign-tag issue is not cached, so starting a run on its number is refused.
	refused := f.call(f.h.CreateRun, http.MethodPost, map[string]any{"issue_iid": foreign})
	if refused.Code == http.StatusCreated {
		t.Fatalf("a run started on an issue with only a foreign same-named tag: %s", refused.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, foreign).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runs on the foreign issue = %d (%v)", n, err)
	}
}

// Acceptance 8 (intake half): an issue moved to another project is evicted by the next
// FULL linked sync, and only by it.
func TestLinkedRepoMovedIssueIsEvictedByTheNextFullSyncLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.syncOK(t)
	if _, ok := f.issueRow(t, f.issueN); !ok {
		t.Fatal("the tagged issue was not cached")
	}
	f.srv.MoveIssue(f.issueN, youtracktest.OtherProjectID)
	f.syncOK(t)
	if _, ok := f.issueRow(t, f.issueN); ok {
		t.Fatal("the moved issue is still cached after the next full sync")
	}
}

// The incremental form walks the same listing, caches an issue whose updated time is
// older than the marks, and never evicts.
func TestLinkedRepoIncrementalSyncCachesBelowTheMarksAndNeverEvictsLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	ctx := context.Background()
	late := forgesvc.Marks{PRD: time.Now().Add(24 * time.Hour), Open: time.Now().Add(24 * time.Hour)}
	if _, err := f.h.svc.SyncIssuesIncremental(ctx, f.user.ID, f.repo, 77, f.tracker(t), late); err != nil {
		t.Fatal(err)
	}
	if row, ok := f.issueRow(t, f.issueN); !ok || row.IssueKey.String == "" {
		t.Fatalf("an issue older than the marks was not cached by the incremental sync: %+v %v", row, ok)
	}
	f.srv.MoveIssue(f.issueN, youtracktest.OtherProjectID)
	got, err := f.h.svc.SyncIssuesIncremental(ctx, f.user.ID, f.repo, 77, f.tracker(t), late)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PRD.Equal(late.PRD) {
		t.Fatalf("marks = %+v, want %+v unchanged", got, late)
	}
	if _, ok := f.issueRow(t, f.issueN); !ok {
		t.Fatal("an incremental sync evicted the moved issue")
	}
}

// Acceptance 9: an incremental sync whose marks are earlier than the issue's updated time
// advances them to the newest updated time, and a later incremental sync after the issue
// moved to another project evicts nothing.
func TestLinkedRepoIncrementalSyncAdvancesEarlierMarksLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	ctx := context.Background()
	early := forgesvc.Marks{PRD: time.Unix(1000, 0), Open: time.Unix(1000, 0)}
	got, err := f.h.svc.SyncIssuesIncremental(ctx, f.user.ID, f.repo, 77, f.tracker(t), early)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PRD.After(early.PRD) {
		t.Fatalf("marks = %+v, want PRD advanced past %v", got, early.PRD)
	}
	// The PRD mark advances to exactly the newest updated time the walk returned.
	row, ok := f.issueRow(t, f.issueN)
	if !ok {
		t.Fatal("the incremental sync did not cache the issue")
	}
	if !got.PRD.Equal(row.ForgeUpdatedAt.Time) {
		t.Fatalf("PRD mark = %v, want the issue's updated time %v", got.PRD, row.ForgeUpdatedAt.Time)
	}
	f.srv.MoveIssue(f.issueN, youtracktest.OtherProjectID)
	if _, err := f.h.svc.SyncIssuesIncremental(ctx, f.user.ID, f.repo, 77, f.tracker(t), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.issueRow(t, f.issueN); !ok {
		t.Fatal("an incremental sync evicted the moved issue")
	}
}

// Acceptance (board contract): an unlinked repo's board cards carry no issue_key member.
func TestUnlinkedBoardCardsCarryNoIssueKeyLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardWriterFixture(ctx, t, &boardWriterStub{})
	mustExecT(ctx, t, f.pool, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, 5, 'plain card', 'opened', $2, 'https://forge.example/i/5', now(), now())`, f.repoID, `["`+wantUziLabel+`"]`)
	rr := httptest.NewRecorder()
	f.h.GetBoard(rr, boardWriterReq(f.user, f.repoID, "", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("board status = %d body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Board struct {
			Cards []map[string]any `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Board.Cards) == 0 {
		t.Fatalf("the board has no cards, the check would be vacuous: %s", rr.Body.String())
	}
	for _, c := range body.Board.Cards {
		if _, has := c["issue_key"]; has {
			t.Fatalf("an unlinked repo's card carries issue_key: %v", c)
		}
	}
}

// A failed listing evicts nothing: the cached row survives a full sync whose listing
// fails (the issue tracker is gone), and the endpoint reports the failure.
func TestLinkedRepoFailedListingEvictsNothingLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.syncOK(t)
	f.srv.Close()
	rr := f.call(f.h.SyncRepo, http.MethodPost, nil)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "sync failed") {
		t.Fatalf("status = %d body = %s, want the 502 sync failure", rr.Code, rr.Body.String())
	}
	if _, ok := f.issueRow(t, f.issueN); !ok {
		t.Fatal("a failed listing evicted a cached issue")
	}
}

// seedRunCredentials gives the fixture's owner the default Anthropic credential a run
// creation requires.
func (f *linkedHandlerFixture) seedRunCredentials(t *testing.T) {
	t.Helper()
	mustExecT(context.Background(), t, f.pool,
		`INSERT INTO user_secrets(user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,'anthropic_token','default',true,$2,'master')`,
		f.user.ID, []byte{2})
}
