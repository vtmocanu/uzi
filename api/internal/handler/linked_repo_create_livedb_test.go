package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
)

// PRD #4173 M5 (D12, D14): an issue uzi creates on a repo linked to a YouTrack project is
// created in that project. The configured uzi label becomes the stored eligibility tag,
// addressed by id (never the same-named tag the connection's account owns), the autopilot
// label is dropped, and an issue filed without send-to-uzi carries no eligibility tag.

// trustYouTrack makes the process-wide default transport trust srv's certificate for the
// test, as the linked fixture does, so the issue tracker driver built by issuetrackersvc
// reaches the fake over TLS.
func trustYouTrack(t *testing.T, srv *youtracktest.Server) {
	t.Helper()
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })
}

// linkNeverEnabledFixture links the fixture's repo to a fresh YouTrack fake and wires the
// real issue tracker driver source into its forgesvc.
func linkNeverEnabledFixture(t *testing.T, f *neverEnabledFixture) *youtracktest.Server {
	t.Helper()
	ctx := context.Background()
	srv := youtracktest.NewServer(t)
	trustYouTrack(t, srv)
	itSvc := issuetrackersvc.New(f.h.q, f.h.box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	f.h.svc.SetIssueTrackerDrivers(itSvc)
	yt, err := itSvc.Connect(ctx, f.admin.ID, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	mustExecT(ctx, t, f.pool, `UPDATE repos SET enabled = true, awaiting_first_enable = false,
	        issue_tracker_connection_id = $2, issue_tracker_project_id = $3, issue_tracker_project_short = $4,
	        eligibility_tag_id = $5, eligibility_tag_name = $6, issue_tracker_linked_at = now() WHERE id = $1`,
		f.repoID, yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	srv.ResetRequests()
	return srv
}

// createdIID returns the first "iid" in a JSON response body, however deeply nested.
func createdIID(t *testing.T, body []byte) int64 {
	t.Helper()
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	var find func(any) (int64, bool)
	find = func(v any) (int64, bool) {
		switch x := v.(type) {
		case map[string]any:
			if n, ok := x["iid"].(float64); ok {
				return int64(n), true
			}
			for _, c := range x {
				if n, ok := find(c); ok {
					return n, true
				}
			}
		case []any:
			for _, c := range x {
				if n, ok := find(c); ok {
					return n, true
				}
			}
		}
		return 0, false
	}
	n, ok := find(v)
	if !ok {
		t.Fatalf("no iid in %s", body)
	}
	return n
}

// issueTags returns the tag ids and the tag names on YouTrack issue n.
func issueTags(srv *youtracktest.Server, n int64) (ids, names []string) {
	ids = srv.IssueTagIDs(n)
	for _, id := range ids {
		names = append(names, srv.TagByID(id).Name)
	}
	slices.Sort(names)
	return ids, names
}

func wantTags(t *testing.T, srv *youtracktest.Server, n int64, wantEligibility bool, wantNames ...string) {
	t.Helper()
	ids, names := issueTags(srv, n)
	if got := slices.Contains(ids, youtracktest.EligibilityTagID); got != wantEligibility {
		t.Errorf("eligibility tag on the issue = %v, want %v (tags %v)", got, wantEligibility, ids)
	}
	if slices.Contains(ids, youtracktest.SameNameTagID) {
		t.Errorf("the same-named tag the connection's account owns was applied: %v", ids)
	}
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Errorf("tag names = %v, want %v", names, wantNames)
	}
	for _, stray := range []string{"uzi", "autopilot"} {
		if len(srv.TagsNamed(stray)) != 0 {
			t.Errorf("a %q tag was created on the issue tracker", stray)
		}
	}
}

func TestLinkedRepoFindingFilingCreatesInTheLinkedProjectLiveDB(t *testing.T) {
	cases := []struct {
		name            string
		sendToUzi       bool
		wantEligibility bool
		wantNames       []string
	}{
		{"send to uzi carries the eligibility tag only", true, true, []string{"agent-found", youtracktest.EligibilityTagName}},
		{"without send to uzi carries no eligibility tag", false, false, []string{"agent-found"}},
	}
	for _, tc := range cases {
		t.Run("FileFinding/"+tc.name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			rr := httptest.NewRecorder()
			f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{SendToUzi: tc.sendToUzi}, map[string]string{"id": f.findID.String()}))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
			}
			wantTags(t, srv, createdIID(t, rr.Body.Bytes()), tc.wantEligibility, tc.wantNames...)
			if n := f.hits.Load(); n != 0 {
				t.Errorf("the forge saw %d request(s), want 0", n)
			}
		})
		t.Run("FileFindingGroup/"+tc.name, func(t *testing.T) {
			f := seedNeverEnabledFixture(t)
			srv := linkNeverEnabledFixture(t, f)
			rr := httptest.NewRecorder()
			f.h.FileFindingGroup(rr, f.request(http.MethodPost, apitypes.FindingGroupFileRequest{IDs: []string{f.dispID.String()}, SendToUzi: tc.sendToUzi}, nil))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
			}
			wantTags(t, srv, createdIID(t, rr.Body.Bytes()), tc.wantEligibility, tc.wantNames...)
			if n := f.hits.Load(); n != 0 {
				t.Errorf("the forge saw %d request(s), want 0", n)
			}
		})
	}
}

func TestLinkedRepoReviewAndJudgeIssuesCarryTheEligibilityTagLiveDB(t *testing.T) {
	t.Run("FileIssue", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		rr := httptest.NewRecorder()
		f.h.FileIssue(rr, f.request(http.MethodPost, map[string]string{"repo_id": f.repoID.String(), "title": "t", "description": "d"},
			map[string]string{"id": f.runID.String(), "recID": f.recID.String()}))
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
		}
		wantTags(t, srv, createdIID(t, rr.Body.Bytes()), true, youtracktest.EligibilityTagName)
	})
	t.Run("AdminFileJudgeIssue", func(t *testing.T) {
		f := seedNeverEnabledFixture(t)
		srv := linkNeverEnabledFixture(t, f)
		rr := httptest.NewRecorder()
		f.h.AdminFileJudgeIssue(rr, f.request(http.MethodPost, adminFileIssueRequest{
			Category: "improve_agent", Target: "ne-" + f.repoID.String(), RepoID: f.repoID.String(), Title: "t", Description: "d",
		}, nil))
		if rr.Code != http.StatusCreated {
			t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
		}
		wantTags(t, srv, createdIID(t, rr.Body.Bytes()), true, youtracktest.EligibilityTagName)
	})
}

// The board's new issue is created in the linked project with the stored eligibility tag
// and is cached under the eligibility name.
func TestLinkedRepoBoardCreateIssueCarriesTheEligibilityTagLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	rr := f.call(f.h.CreateIssue, http.MethodPost, map[string]string{"title": "t", "description": "d"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	n := createdIID(t, rr.Body.Bytes())
	wantTags(t, f.srv, n, true, youtracktest.EligibilityTagName)
	var labels []string
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT labels FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, f.repo, n).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &labels); err != nil || !slices.Equal(labels, []string{youtracktest.EligibilityTagName}) {
		t.Errorf("cached labels = %v (%v), want only the eligibility name", labels, err)
	}
	if c := f.rec.Calls(); len(c) != 0 {
		t.Errorf("the forge saw calls %v, want none", c)
	}
}

// Promote adds the stored eligibility tag by id, not the configured uzi label, and the
// same-named tag the connection's account owns is untouched.
func TestLinkedRepoPromoteAddsTheStoredEligibilityTagLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	untagged := f.srv.SeedIssue("untagged card", "body")
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 'untagged', 'opened', '[]', 'https://yt.test/u', now(), now())`, f.repo, untagged); err != nil {
		t.Fatal(err)
	}
	f.issueN = untagged
	f.srv.ResetRequests()
	rr := f.call(f.h.PromoteIssue, http.MethodPost, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	wantTags(t, f.srv, untagged, true, youtracktest.EligibilityTagName)
	var resp struct {
		Card struct {
			Labels []string `json:"labels"`
		} `json:"card"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || !slices.Equal(resp.Card.Labels, []string{youtracktest.EligibilityTagName}) {
		t.Errorf("promoted card labels = %v (%v), want the eligibility name", resp.Card.Labels, err)
	}
	if c := f.rec.Calls(); len(c) != 0 {
		t.Errorf("the forge saw calls %v, want none", c)
	}
}

// PRD #4173 M5: an issue uzi creates without the eligibility tag (a finding filed without
// send-to-uzi) lives in the linked project but is not on the board after the linked full
// sync, while the board's own new issue (which carries the tag) is.
func TestLinkedRepoUntaggedCreateIsNotOnTheBoardAfterTheFullSyncLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	srv := linkNeverEnabledFixture(t, f)
	params := map[string]string{"id": f.repoID.String()}

	rr := httptest.NewRecorder()
	f.h.FileFinding(rr, f.request(http.MethodPost, apitypes.FileFindingRequest{SendToUzi: false}, map[string]string{"id": f.findID.String()}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("file finding: status = %d body = %s", rr.Code, rr.Body.String())
	}
	untagged := createdIID(t, rr.Body.Bytes())
	wantTags(t, srv, untagged, false, "agent-found")

	rr = httptest.NewRecorder()
	f.h.CreateIssue(rr, f.request(http.MethodPost, map[string]string{"title": "board card", "description": "d"}, params))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create issue: status = %d body = %s", rr.Code, rr.Body.String())
	}
	tagged := createdIID(t, rr.Body.Bytes())
	wantTags(t, srv, tagged, true, youtracktest.EligibilityTagName)

	rr = httptest.NewRecorder()
	f.h.SyncRepo(rr, f.request(http.MethodPost, nil, params))
	if rr.Code != http.StatusOK {
		t.Fatalf("sync: status = %d body = %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	f.h.GetBoard(rr, f.request(http.MethodGet, nil, params))
	if rr.Code != http.StatusOK {
		t.Fatalf("board: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Board struct {
			Cards []struct {
				IID int64 `json:"iid"`
			} `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	var onBoard []int64
	for _, c := range body.Board.Cards {
		onBoard = append(onBoard, c.IID)
	}
	if slices.Contains(onBoard, untagged) {
		t.Errorf("the issue created without the eligibility tag (%d) is on the board: %v", untagged, onBoard)
	}
	if !slices.Contains(onBoard, tagged) {
		t.Errorf("the issue created with the eligibility tag (%d) is not on the board: %v", tagged, onBoard)
	}
}
