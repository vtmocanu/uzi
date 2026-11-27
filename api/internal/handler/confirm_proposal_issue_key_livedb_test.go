package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Confirming a proposal on a repo linked to an issue tracker answers with the created issue
// carrying its derived key (<project short name>-<number>) next to the number, so the web
// client and the CLI show the key the issue tracker shows.
func TestConfirmProposalOnLinkedRepoResponseCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.h.wsvc.SetForges(f.h.svc)
	f.h.wsvc.SetIssueSources(f.h.svc)
	ctx := t.Context()

	chat, err := f.h.wsvc.CreateChatRun(ctx, f.user.ID, "file it")
	if err != nil {
		t.Fatalf("CreateChatRun: %v", err)
	}
	prop, err := f.h.q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{
		RunID: chat.ID, RepoID: f.repo, Title: "proposed card", Description: "proposed body", Labels: []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("CreateIssueProposal: %v", err)
	}

	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", chat.ID.String())
	rctx.URLParams.Add("pid", prop.ID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), f.user), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	f.h.ConfirmProposal(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Issue struct {
			IID      int64  `json:"iid"`
			IssueKey string `json:"issue_key"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	if resp.Issue.IID == 0 {
		t.Fatalf("no issue number in %s", rr.Body.String())
	}
	if want := wantKey(resp.Issue.IID); resp.Issue.IssueKey != want {
		t.Errorf("issue.issue_key = %q, want %q (body %s)", resp.Issue.IssueKey, want, rr.Body.String())
	}
}
