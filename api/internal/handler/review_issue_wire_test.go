package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestReviewFileClientWireRoundtrip pins the hand-mirrored json tags across the
// handler-local fileIssueResponse/createdIssueDTO and the client-local
// uzicli.ReviewIssueFileResult/ReviewFiledIssueDTO. It marshals a fully-populated
// handler response (with a non-empty warning, which the happy path never emits) and
// decodes it into the CLIENT type, so a server-side tag rename (e.g. web_url -> url)
// breaks this test instead of silently breaking `uzi review file`.
func TestReviewFileClientWireRoundtrip(t *testing.T) {
	want := fileIssueResponse{
		Issue:   createdIssueDTO{IID: 4242, WebURL: "https://forge.example/g/ra/-/issues/4242", Title: "Improve the reviewer"},
		Warning: "The issue was created on the forge, but linking it in uzi failed; the next sync will reconcile it.",
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal handler response: %v", err)
	}
	var got uzicli.ReviewIssueFileResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode into client type: %v", err)
	}
	if got.Issue.IID != want.Issue.IID {
		t.Errorf("iid tag drift: got %d, want %d", got.Issue.IID, want.Issue.IID)
	}
	if got.Issue.WebURL != want.Issue.WebURL {
		t.Errorf("web_url tag drift: got %q, want %q", got.Issue.WebURL, want.Issue.WebURL)
	}
	if got.Issue.Title != want.Issue.Title {
		t.Errorf("title tag drift: got %q, want %q", got.Issue.Title, want.Issue.Title)
	}
	if got.Warning != want.Warning {
		t.Errorf("warning tag drift: got %q, want %q", got.Warning, want.Warning)
	}
}

// TestFileIssueClientWireRoundtripLiveDB is the live-DB roundtrip the client.go comment
// names: it files a happy-path issue through the real handler and decodes the REAL server
// fileIssueResponse body into the CLI's own uzicli.ReviewIssueFileResult, proving the
// hand-mirrored client type stays in sync with what the handler actually writes. The
// LiveDB suffix is REQUIRED — that is how CI's store-it job selects it; do not rename it.
func TestFileIssueClientWireRoundtripLiveDB(t *testing.T) {
	h, pool, _, box, fs := fileIssueLiveDB(t)
	ctx := context.Background()
	f := seedFileFixture(ctx, t, pool, store.New(pool), box, fs.server.URL)
	rr := httptest.NewRecorder()
	h.FileIssue(rr, fileIssueReq(f.owner, f.runID, f.recID, f.ownerRepo, "Improve the reviewer", "make it report skipped checks"))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got uzicli.ReviewIssueFileResult
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode into client type: %v", err)
	}
	if got.Issue.IID == 0 {
		t.Errorf("iid did not carry through the real server body: got 0")
	}
	if got.Issue.Title != "Improve the reviewer" {
		t.Errorf("title = %q, want %q", got.Issue.Title, "Improve the reviewer")
	}
	// Prove web_url mapped (not merely non-empty): the stub emits this exact URL for the iid.
	wantURL := fmt.Sprintf("https://forge.example/g/ra/-/issues/%d", got.Issue.IID)
	if got.Issue.WebURL == "" || got.Issue.WebURL != wantURL {
		t.Errorf("web_url = %q, want %q", got.Issue.WebURL, wantURL)
	}
	// The warning tag is exercised by TestReviewFileClientWireRoundtrip; the happy path never warns.
	if got.Warning != "" {
		t.Errorf("warning = %q, want empty on the happy path", got.Warning)
	}
}
