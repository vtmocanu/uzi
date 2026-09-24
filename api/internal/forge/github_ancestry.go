package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	gh "github.com/google/go-github/v91/github"
)

// githubBranchHeadBody is the ONLY part of GET /repos/{o}/{r}/branches/{branch} that
// BranchHead reads: the branch name (to refuse a rename redirect that resolved a
// DIFFERENT branch) and commit.sha (go-github's Branch.Commit.SHA, json "sha").
type githubBranchHeadBody struct {
	Name   *string `json:"name"`
	Commit *struct {
		SHA *string `json:"sha"`
	} `json:"commit"`
}

// githubCompareBody is the ONLY part of GET /repos/{o}/{r}/compare/{base}...{head}
// CompareAncestry reads: the top-level `status` (go-github's CommitsComparison.Status).
// The `commits` array is deliberately NOT modelled — GitHub truncates it, so it can
// never be evidence of anything.
type githubCompareBody struct {
	Status *string `json:"status"`
}

// githubGetBounded issues an authenticated GET for the ancestry surface and reads the
// body under a hard ceiling. The request is BUILT by go-github (base URL, Accept,
// User-Agent, API-version headers) but SENT through g.ancClient, which refuses every
// redirect: go-github's own client re-adds the Bearer PAT on each hop through its auth
// transport, so a followed redirect would replay the PAT to the redirect target and
// read that host's answer as proof. Any 3xx is therefore an error. Other statuses are
// classified by gh.CheckResponse, the classifier BareDo uses, so a
// 403-with-remaining-0 or a 429 still surfaces as the neutral *RateLimitError via
// wrapErr. It returns the HTTP status alongside any error so the caller can recognise
// a 404 before redaction.
func (g *github) githubGetBounded(ctx context.Context, op, path string, limit int64) (int, []byte, error) {
	req, err := g.client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, nil, g.wrapErr(op, err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	resp, err := g.ancClient.Do(req)
	if err != nil {
		return 0, nil, g.wrapErr(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 == 3 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil, g.wrapErr(op, fmt.Errorf("refusing to follow a redirect (status %d)", resp.StatusCode))
	}
	if err := gh.CheckResponse(resp); err != nil {
		return resp.StatusCode, nil, g.wrapErr(op, err)
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		return resp.StatusCode, nil, g.wrapErr(op, err)
	}
	return resp.StatusCode, body, nil
}

// BranchHead implements Forge (issue #1582 M1). GET /repos/{o}/{r}/branches/{branch},
// reader-gated like DefaultBranchProtection's call. A 404 is ErrRefNotFound. A redirect
// (GitHub answers a renamed branch with a 301 to the NEW name) is never followed and is an
// error; the returned `name` must still equal the requested branch, so an answer naming
// another branch is an error too, never another branch's head.
func (g *github) BranchHead(ctx context.Context, projectID int64, branch string) (string, error) {
	if branch == "" {
		return "", errors.New("github: branch head: empty branch name")
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("repos/%s/%s/branches/%s", url.PathEscape(slug.owner), url.PathEscape(slug.repo), url.PathEscape(branch))
	status, body, err := g.githubGetBounded(ctx, "branch head", path, ancestryBodyLimit)
	if err != nil {
		if status == http.StatusNotFound {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var b githubBranchHeadBody
	if err := json.Unmarshal(body, &b); err != nil {
		return "", g.wrapErr("branch head: decode", err)
	}
	if b.Name == nil || *b.Name != branch {
		return "", errors.New("github: branch head: response names a different branch (renamed?)")
	}
	if b.Commit == nil || b.Commit.SHA == nil || !isCommitSHA(*b.Commit.SHA) {
		return "", errors.New("github: branch head: response carries no 40-hex commit sha")
	}
	return *b.Commit.SHA, nil
}

// CompareAncestry implements Forge (issue #1582 M1). GET
// /repos/{o}/{r}/compare/{head}...{candidate} and read ONLY `status`: "identical" or
// "behind" (the candidate is reachable from head) is AncestryAncestor; "ahead" or
// "diverged" is AncestryNotAncestor; a missing or unrecognized status, any error
// (404/422/5xx/timeout), a rate limit, or an oversize body is AncestryUnknown.
func (g *github) CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (Ancestry, error) {
	if a, done, err := validateAncestryArgs("github", head, candidate); done {
		return a, err
	}
	slug, err := g.repoSlugFor(ctx, projectID)
	if err != nil {
		return AncestryUnknown, err
	}
	path := fmt.Sprintf("repos/%s/%s/compare/%s...%s", url.PathEscape(slug.owner), url.PathEscape(slug.repo), head, candidate)
	_, body, err := g.githubGetBounded(ctx, "compare ancestry", path, ancestryBodyLimit)
	if err != nil {
		return AncestryUnknown, err
	}
	var c githubCompareBody
	if err := json.Unmarshal(body, &c); err != nil {
		return AncestryUnknown, g.wrapErr("compare ancestry: decode", err)
	}
	if c.Status == nil {
		return AncestryUnknown, errors.New("github: compare ancestry: response carries no status")
	}
	switch *c.Status {
	case "identical", "behind":
		return AncestryAncestor, nil
	case "ahead", "diverged":
		return AncestryNotAncestor, nil
	default:
		return AncestryUnknown, fmt.Errorf("github: compare ancestry: unrecognized status %q", truncateForError(g.redact.string(*c.Status)))
	}
}

// truncateForError bounds forge-supplied text before it is folded into an error.
func truncateForError(s string) string {
	const limit = 64
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
