package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// wantKey is the key a linked card or detail carries: the stored project short name, a
// dash and the issue number.
func wantKey(n int64) string {
	return youtracktest.ProjectShortName + "-" + strconv.FormatInt(n, 10)
}

// Promote on a linked repo for an untagged issue adds the stored eligibility tag, and the
// response card carries the derived issue key even though the cached row (a create path's
// write) has no issue_key of its own.
func TestLinkedRepoPromoteCardCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	untagged := f.srv.SeedIssue("untagged card", "body")
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 'untagged', 'opened', '[]', 'https://yt.test/u', now(), now())`, f.repo, untagged); err != nil {
		t.Fatal(err)
	}
	f.issueN = untagged
	rr := f.call(f.h.PromoteIssue, http.MethodPost, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	wantTags(t, f.srv, untagged, true, youtracktest.EligibilityTagName)
	var resp struct {
		Card struct {
			IssueKey string `json:"issue_key"`
		} `json:"card"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp.Card.IssueKey != wantKey(untagged) {
		t.Errorf("promoted card issue_key = %q (%v), want %q", resp.Card.IssueKey, err, wantKey(untagged))
	}
}

type detailResp struct {
	Issue struct {
		IssueKey         string `json:"issue_key"`
		EligibilityLabel string `json:"eligibility_label"`
		Column           string `json:"column"`
		Closed           bool   `json:"closed"`
	} `json:"issue"`
}

func (f *linkedHandlerFixture) detail(t *testing.T) detailResp {
	t.Helper()
	rr := f.call(f.h.GetIssueDetail, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var out detailResp
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The issue page of a linked repo carries the key and the stored eligibility name, and its
// column is the cached State column, not a label.
func TestLinkedRepoIssueDetailCarriesKeyEligibilityAndCachedColumnLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	if _, err := f.pool.Exec(context.Background(), `UPDATE issues SET issue_tracker_column = 'Human Review' WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, f.issueN); err != nil {
		t.Fatal(err)
	}
	d := f.detail(t)
	if d.Issue.IssueKey != wantKey(f.issueN) || d.Issue.EligibilityLabel != youtracktest.EligibilityTagName || d.Issue.Column != "Human Review" {
		t.Errorf("detail = %+v, want key %q, eligibility %q, column Human Review", d.Issue, wantKey(f.issueN), youtracktest.EligibilityTagName)
	}
}

// An issue the cache does not hold shows in the Open lane even when a tag it carries is
// named like a column: labels never decide the column on a linked repo.
func TestLinkedRepoUncachedIssueDetailIsOpenEvenWithAColumnNamedTagLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	tagID := f.srv.SeedOwnTag("In Progress")
	n := f.srv.SeedIssue("uncached", "body", youtracktest.EligibilityTagID, tagID)
	f.issueN = n
	d := f.detail(t)
	if d.Issue.Column != "" || d.Issue.Closed || d.Issue.IssueKey != wantKey(n) {
		t.Errorf("detail = %+v, want the Open lane and key %q", d.Issue, wantKey(n))
	}
}

// A linked board carries the stored eligibility name, and every card carries the derived
// key, including a cached row whose issue_key column is NULL.
func TestLinkedRepoBoardCardsCarryTheDerivedKeyAndEligibilityLabelLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	rr := f.call(f.h.GetBoard, http.MethodGet, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("board = %d\n%s", rr.Code, rr.Body.String())
	}
	var b struct {
		Board struct {
			EligibilityLabel string `json:"eligibility_label"`
			Cards            []struct {
				IID      int64  `json:"iid"`
				IssueKey string `json:"issue_key"`
			} `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if b.Board.EligibilityLabel != youtracktest.EligibilityTagName {
		t.Errorf("eligibility_label = %q", b.Board.EligibilityLabel)
	}
	for _, c := range b.Board.Cards {
		if c.IssueKey != wantKey(c.IID) {
			t.Errorf("card %d issue_key = %q, want %q", c.IID, c.IssueKey, wantKey(c.IID))
		}
	}
	if len(b.Board.Cards) == 0 {
		t.Fatal("the board has no cards")
	}
}

// cardKey decodes the key of the {"card": ...} body a create or move returns.
func cardKey(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Card struct {
			IssueKey string `json:"issue_key"`
		} `json:"card"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return resp.Card.IssueKey
}

// The create response card of a linked repo carries the derived key although the cached row
// (written by the create path) has no issue_key of its own.
func TestLinkedRepoCreateIssueCardCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	rr := f.call(f.h.CreateIssue, http.MethodPost, map[string]string{"title": "t", "description": "d"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	n := createdIID(t, rr.Body.Bytes())
	if got := cardKey(t, rr.Body.Bytes()); got != wantKey(n) {
		t.Errorf("created card issue_key = %q, want %q", got, wantKey(n))
	}
}

// When the cache write after a create fails the issue still exists on the issue tracker, and the
// fallback card carries the key too. A trigger that rejects inserts into issues makes the
// upsert fail; it is dropped on cleanup.
func TestLinkedRepoCreateIssueFallbackCardCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	ctx := context.Background()
	mustExecT(ctx, t, f.pool, `CREATE FUNCTION m2r_reject_issue_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'm2r reject'; END $$`)
	mustExecT(ctx, t, f.pool, `CREATE TRIGGER m2r_reject_issue_write BEFORE INSERT OR UPDATE ON issues FOR EACH ROW
		WHEN (NEW.repo_id = '`+f.repo.String()+`') EXECUTE FUNCTION m2r_reject_issue_write()`)
	t.Cleanup(func() {
		mustExecT(context.Background(), t, f.pool, `DROP TRIGGER IF EXISTS m2r_reject_issue_write ON issues`)
		mustExecT(context.Background(), t, f.pool, `DROP FUNCTION IF EXISTS m2r_reject_issue_write()`)
	})
	rr := f.call(f.h.CreateIssue, http.MethodPost, map[string]string{"title": "t", "description": "d"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	n := createdIID(t, rr.Body.Bytes())
	var cached int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, n).Scan(&cached); err != nil || cached != 0 {
		t.Fatalf("cached rows for the created issue = %d (%v), want 0 so the fallback path ran", cached, err)
	}
	if got := cardKey(t, rr.Body.Bytes()); got != wantKey(n) {
		t.Errorf("fallback card issue_key = %q, want %q", got, wantKey(n))
	}
}

// The single-card response of a drag on a linked board carries the key.
func TestLinkedBoardMoveCardCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newStateMappingFixture(t)
	f.boardMapping(t)
	rr := f.call(f.h.MoveIssue, http.MethodPost, map[string]string{"to_column": "In Progress"})
	if rr.Code != http.StatusOK {
		t.Fatalf("move = %d %s", rr.Code, rr.Body.String())
	}
	if got := cardKey(t, rr.Body.Bytes()); got != wantKey(f.issueN) {
		t.Errorf("moved card issue_key = %q, want %q", got, wantKey(f.issueN))
	}
}

// The issue tracker config route is session-only: neither no credential nor a CLI token
// header reaches it.
func TestIssueTrackerConfigRefusesNoCredentialAndACLIToken(t *testing.T) {
	router := issueTrackerTestRouter()
	for _, tc := range []struct{ name, header string }{
		{"no credential", ""},
		{"cli token", "Bearer " + "uzc_" + "whatever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/issue-trackers/config", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401\n%s", rec.Code, rec.Body.String())
			}
		})
	}
}
