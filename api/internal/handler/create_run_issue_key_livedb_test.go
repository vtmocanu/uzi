package handler

import (
	"encoding/json"
	"net/http"
	"testing"
)

// PRD #4173 M6: the create-run response itself (not only the later list and detail reads)
// carries the issue key of an issue run on a linked repo.
func TestCreateRunResponseCarriesTheLinkedIssueKeyLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.syncOK(t)
	f.h.wsvc.SetForges(f.h.svc)
	f.h.wsvc.SetIssueSources(f.h.svc)
	f.seedRunCredentials(t)

	rr := f.call(f.h.CreateRun, http.MethodPost, map[string]any{"issue_iid": f.issueN})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Run struct {
			IssueIID int64  `json:"issue_iid"`
			IssueKey string `json:"issue_key"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Run.IssueIID != f.issueN || body.Run.IssueKey != wantKey(f.issueN) {
		t.Fatalf("created run = %+v, want issue %d keyed %q", body.Run, f.issueN, wantKey(f.issueN))
	}
}
