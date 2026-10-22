package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

type linkedHandoffForge struct {
	forgetest.BaseFake
	issue        forge.Issue
	err          error
	project, iid int64
	calls        int
}

func (f *linkedHandoffForge) GetIssue(_ context.Context, project, iid int64) (forge.Issue, error) {
	f.calls++
	f.project, f.iid = project, iid
	return f.issue, f.err
}

type linkedHandoffBuilder struct{ f forge.Forge }

func (b linkedHandoffBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

func TestLinkedTaskHTTPLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	cliMustExec(t, pool, `INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token','default',true,$3,'master')`, uuid.New(), user, []byte("ct"))
	repo := cliSeedOwnedRepo(t, pool, user)
	token := cliMintToken(t, pool, user, clitoken.ScopeUser)
	path := "/api/repos/" + repo.String() + "/task-runs"
	f := &linkedHandoffForge{issue: forge.Issue{IID: 42, State: "closed"}}
	h.wsvc.SetForges(linkedHandoffBuilder{f})
	for _, body := range []string{
		`{"context":"task","continued_issue_iid":0}`,
		`{"context":"task","continued_issue_iid":-1}`,
		`{"context":"task","continued_issue_iid":9223372036854775808}`,
	} {
		rec := bearerReqBody(router, http.MethodPost, path, token, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid input status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if f.calls != 0 {
		t.Fatal("invalid input reached forge")
	}
	for _, tc := range []struct {
		pr     bool
		err    error
		status int
	}{
		{true, nil, 400}, {false, &forge.IssueNotFoundError{Err: errors.New("404 missing")}, 404}, {false, errors.New("forge outage"), 502}, {false, nil, 201},
	} {
		f.issue.IsPullRequest, f.err = tc.pr, tc.err
		rec := bearerReqBody(router, http.MethodPost, path, token, `{"context":"task","continued_issue_iid":42}`)
		if rec.Code != tc.status {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
		}
		if tc.status == 201 {
			var response struct {
				Run apitypes.RunDTO `json:"run"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Run.ContinuedIssueIID == nil || *response.Run.ContinuedIssueIID != 42 || response.Run.IssueIID != nil {
				t.Fatalf("wrong task identity: %s", rec.Body.String())
			}
		}
	}
	var project int64
	if err := pool.QueryRow(context.Background(), "SELECT forge_project_id FROM repos WHERE id=$1", repo).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if f.project != project || f.iid != 42 {
		t.Fatal("forge called outside owned repo scope")
	}
	rec := bearerReqBody(router, http.MethodPost, path, token, `{"context":"task","continued_issue_iid":42}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", rec.Code, rec.Body.String())
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM runs WHERE user_id=$1", user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("refused requests inserted runs: %d", n)
	}
}
