package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// gitlabCommitRef is the ONLY part of a GitLab commit-bearing response the ancestry
// surface reads: `id` (client-go's Commit.ID, json "id"). The branch endpoint nests it
// under `commit`; the merge_base endpoint returns the commit object itself.
type gitlabCommitRef struct {
	ID *string `json:"id"`
}

type gitlabBranchHeadBody struct {
	Name   *string          `json:"name"`
	Commit *gitlabCommitRef `json:"commit"`
}

// gitlabGetBounded issues a raw authenticated GET against {baseURL}/api/v4{path}
// through g.logClient (the redirect-refusing client JobLogTail uses, so a cross-host
// redirect cannot replay the PRIVATE-TOKEN header) and reads the body under a hard
// ceiling. It bypasses client-go on purpose: client-go's retryable transport SLEEPS and
// retries on a 429, and the ancestry surface must answer a rate limit as "unknown"
// immediately rather than stall the settle request. A non-2xx status is an error
// (returned with the status so a caller can recognise a 404). Every error is
// PAT-redacted.
func (g *gitLab) gitlabGetBounded(ctx context.Context, op, pathAndQuery string, limit int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+"/api/v4"+pathAndQuery, nil)
	if err != nil {
		return 0, nil, g.wrapErr(op, err)
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	resp, err := g.logClient.Do(req)
	if err != nil {
		return 0, nil, g.wrapErr(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, nil, g.wrapErr(op, fmt.Errorf("status %d", resp.StatusCode))
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		return resp.StatusCode, nil, g.wrapErr(op, err)
	}
	return resp.StatusCode, body, nil
}

// BranchHead implements Forge (issue #1582 M1). GET
// /projects/:id/repository/branches/:branch → commit.id. The branch is a single
// URL-encoded path segment (a "/" in it is %2F, as client-go's GetBranch sends it).
// A 404 is ErrRefNotFound.
func (g *gitLab) BranchHead(ctx context.Context, projectID int64, branch string) (string, error) {
	if branch == "" {
		return "", errors.New("gitlab: branch head: empty branch name")
	}
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/repository/branches/" + url.PathEscape(branch)
	status, body, err := g.gitlabGetBounded(ctx, "branch head", path, ancestryBodyLimit)
	if err != nil {
		if status == http.StatusNotFound {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var b gitlabBranchHeadBody
	if err := json.Unmarshal(body, &b); err != nil {
		return "", g.wrapErr("branch head: decode", err)
	}
	if b.Name != nil && *b.Name != branch {
		return "", errors.New("gitlab: branch head: response names a different branch")
	}
	if b.Commit == nil || b.Commit.ID == nil || !isCommitSHA(*b.Commit.ID) {
		return "", errors.New("gitlab: branch head: response carries no 40-hex commit id")
	}
	return *b.Commit.ID, nil
}

// CompareAncestry implements Forge (issue #1582 M1). GET
// /projects/:id/repository/merge_base?refs[]={head}&refs[]={candidate}: the merge base
// of head and candidate IS candidate exactly when candidate is an ancestor of head. So
// id == candidate is AncestryAncestor; a valid 40-hex id != candidate is
// AncestryNotAncestor; a missing/malformed id, 400/404 (unknown ref, no common
// history), 429, 5xx, oversize or any transport error is AncestryUnknown.
func (g *gitLab) CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (Ancestry, error) {
	if a, done, err := validateAncestryArgs("gitlab", head, candidate); done {
		return a, err
	}
	q := url.Values{"refs[]": []string{head, candidate}}
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/repository/merge_base?" + q.Encode()
	_, body, err := g.gitlabGetBounded(ctx, "compare ancestry", path, ancestryBodyLimit)
	if err != nil {
		return AncestryUnknown, err
	}
	var c gitlabCommitRef
	if err := json.Unmarshal(body, &c); err != nil {
		return AncestryUnknown, g.wrapErr("compare ancestry: decode", err)
	}
	if c.ID == nil || !isCommitSHA(*c.ID) {
		return AncestryUnknown, errors.New("gitlab: compare ancestry: merge base carries no 40-hex commit id")
	}
	if *c.ID == candidate {
		return AncestryAncestor, nil
	}
	return AncestryNotAncestor, nil
}

// RefHead implements Forge (issue #1751 M2). GET /projects/:id/repository/commits/{ref} → id,
// with the FULL ref sent as a single URL-encoded path segment (every "/" is %2F), through the
// same redirect-refusing, bounded, never-retrying read as BranchHead. A 404 (no such ref) is
// ErrRefNotFound. GitLab's commit object does not echo the ref it resolved, so the answer is
// bound only by the exact full ref name the request carried.
func (g *gitLab) RefHead(ctx context.Context, projectID int64, ref string) (string, error) {
	if err := validateFullRef("gitlab", ref); err != nil {
		return "", err
	}
	path := "/projects/" + strconv.FormatInt(projectID, 10) + "/repository/commits/" + url.PathEscape(ref)
	status, body, err := g.gitlabGetBounded(ctx, "ref head", path, ancestryBodyLimit)
	if err != nil {
		if status == http.StatusNotFound {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var c gitlabCommitRef
	if err := json.Unmarshal(body, &c); err != nil {
		return "", g.wrapErr("ref head: decode", err)
	}
	if c.ID == nil || !isCommitSHA(*c.ID) {
		return "", errors.New("gitlab: ref head: response carries no 40-hex commit id")
	}
	return *c.ID, nil
}
