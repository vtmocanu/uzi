package uzicli

import (
	"context"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// client_review.go holds the review + findings verbs (uzi review / uzi findings)
// of the Client/HTTPClient split out of client.go (PRD #1017).

// dispositionPath builds the disposition endpoint path for a (run, rec) pair,
// escaping both id segments.
func dispositionPath(runID, recID string) string {
	return "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/disposition"
}

func (c *HTTPClient) SetDisposition(ctx context.Context, runID, recID, status, reason string) error {
	reqBody := map[string]string{"status": status}
	// reason is required iff dismissed; omit it otherwise so a "done" PUT never
	// carries a stray (and server-rejected) reason field.
	if reason != "" {
		reqBody["reason"] = reason
	}
	return c.put(ctx, dispositionPath(runID, recID), reqBody, nil)
}

func (c *HTTPClient) DeleteDisposition(ctx context.Context, runID, recID string) error {
	// The command resolves recID against the current review before calling, so the
	// run and recommendation exist; a 404 here therefore means "no disposition on
	// this coordinate" — softened to ErrNoDisposition (a plain error) so undo can
	// report "already undone" and exit 0, per Decision 6. Any other non-2xx keeps
	// its real exit code.
	resp, body, err := c.doJSONRead(ctx, http.MethodDelete, dispositionPath(runID, recID), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoDisposition
	}
	return decode2xx(resp, body, dispositionPath(runID, recID), nil)
}

func (c *HTTPClient) JudgeStats(ctx context.Context) (apitypes.TriageDTO, error) {
	var out apitypes.TriageDTO
	if err := c.get(ctx, "/api/me/judge/stats", &out); err != nil {
		return apitypes.TriageDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) JudgeBacklog(ctx context.Context, bucket, runAnchor, category string) (apitypes.JudgeBacklogDTO, error) {
	path := "/api/me/judge/recommendations"
	// All omitted rather than sent empty when unset: the handler's `== ""` branches are what
	// apply its defaults, and an explicit empty value would take the same branch only by
	// coincidence. Escaped because all are user input off a flag.
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if runAnchor != "" {
		q.Set("run", runAnchor)
	}
	if category != "" {
		// Forwarded verbatim as the comma-separated `?category=a,b` list the server splits
		// and normalizes; the CLI never parses it into labels of its own.
		q.Set("category", category)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.JudgeBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.JudgeBacklogDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) BulkSetDispositions(ctx context.Context, items []apitypes.JudgeDispositionCoordDTO, status, reason string) (apitypes.JudgeDispositionResultDTO, error) {
	// Scope is left zero on purpose — see the interface doc. Reason is likewise sent as
	// "" for a `done`, which is what the shared validator requires (done carries no
	// reason); this is a struct, not the omit-when-empty map SetDisposition builds, and
	// the two agree because "" IS the legal value there.
	reqBody := apitypes.JudgeBulkDispositionRequest{Items: items, Status: status, Reason: reason}
	var out apitypes.JudgeDispositionResultDTO
	if err := c.put(ctx, "/api/me/judge/recommendations/disposition", reqBody, &out); err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) ListFindings(ctx context.Context, bucket, repo, run string) (apitypes.IncidentalFindingBacklogDTO, error) {
	path := "/api/findings"
	// Each omitted rather than sent empty when unset: the handler's `== ""` branches are what
	// apply its defaults, and an explicit empty value would take the same branch only by
	// coincidence. Escaped because all are user input off a flag.
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if repo != "" {
		q.Set("repo", repo)
	}
	if run != "" {
		q.Set("run", run)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out apitypes.IncidentalFindingBacklogDTO
	if err := c.get(ctx, path, &out); err != nil {
		return apitypes.IncidentalFindingBacklogDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) FileFinding(ctx context.Context, id string) (apitypes.IncidentalFindingFileResultDTO, error) {
	// An empty JSON object body: the file endpoint resolves title/description from the STORED,
	// already-sanitised finding row (D4) and assembles labels server-side (D5), so the CLI sends
	// no edits — the web is the rich editor, the CLI files defaults. postJSON routes a non-2xx
	// through statusError, so 404→ExitNotFound and 409→ExitConflict come for free.
	var out apitypes.IncidentalFindingFileResultDTO
	if err := c.postJSON(ctx, "/api/findings/"+url.PathEscape(id)+"/issue", struct{}{}, &out); err != nil {
		return apitypes.IncidentalFindingFileResultDTO{}, err
	}
	return out, nil
}

func (c *HTTPClient) DismissFinding(ctx context.Context, id, reason string) error {
	// reason is the wire enum, already mapped and validated by the command. postJSON discards
	// the (200) body via a nil out and maps a non-2xx through statusError (404→4, 409→5).
	return c.postJSON(ctx, "/api/findings/"+url.PathEscape(id)+"/dismiss", map[string]string{"reason": reason}, nil)
}

// ReviewFiledIssueDTO / ReviewIssueFileResult mirror the review file handler's wire shape
// (POST .../review/recommendations/{recID}/issue): the created forge issue plus an optional
// created-with-warning note. Defined here (not in apitypes) because the handler's response is
// a handler-local type; the shape is pinned by TestFileIssueClientWireRoundtripLiveDB (decodes
// a real server fileIssueResponse into this type) and TestReviewFileClientWireRoundtrip
// (marshal/unmarshal json-tag parity incl. warning), both in api/internal/handler.
type ReviewFiledIssueDTO struct {
	IID    int64  `json:"iid"`
	WebURL string `json:"web_url"`
	Title  string `json:"title"`
}

type ReviewIssueFileResult struct {
	Issue   ReviewFiledIssueDTO `json:"issue"`
	Warning string              `json:"warning,omitempty"`
}

func (c *HTTPClient) GetReviewIssueDraft(ctx context.Context, runID, recID string) (apitypes.IssueDraftDTO, error) {
	// The handler wraps the DTO in a {"draft": ...} envelope, so decode into a local envelope
	// and hand back the unwrapped draft. Both ids are escaped — they are user input off the
	// positionals. A non-2xx (404 for a foreign/unknown run or rec) maps through statusError.
	var env struct {
		Draft apitypes.IssueDraftDTO `json:"draft"`
	}
	path := "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/issue-draft"
	if err := c.get(ctx, path, &env); err != nil {
		return apitypes.IssueDraftDTO{}, err
	}
	return env.Draft, nil
}

func (c *HTTPClient) FileReviewIssue(ctx context.Context, runID, recID, repoID, title, description string) (ReviewIssueFileResult, error) {
	// Unlike FileFinding's empty body, the review file endpoint decodes fileIssueRequest
	// {repo_id,title,description} — all three required — so the CLI sends the draft defaults it
	// just fetched (the web is the rich editor). postJSON routes a non-2xx through statusError,
	// so 404→ExitNotFound and 409→ExitConflict come for free.
	body := map[string]string{"repo_id": repoID, "title": title, "description": description}
	path := "/api/runs/" + url.PathEscape(runID) + "/review/recommendations/" + url.PathEscape(recID) + "/issue"
	var out ReviewIssueFileResult
	if err := c.postJSON(ctx, path, body, &out); err != nil {
		return ReviewIssueFileResult{}, err
	}
	return out, nil
}
