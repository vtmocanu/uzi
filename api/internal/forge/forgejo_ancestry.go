package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// forgejoBranchHeadBody is the ONLY part of GET /repos/{o}/{r}/branches/{branch}
// BranchHead reads: `name` and commit.id (the gitea SDK's Branch.Commit, a
// PayloadCommit whose ID is json "id").
type forgejoBranchHeadBody struct {
	Name   *string `json:"name"`
	Commit *struct {
		ID *string `json:"id"`
	} `json:"commit"`
}

// forgejoCompareBody is the ONLY part of GET /repos/{o}/{r}/compare/{base}...{head}
// CompareAncestry reads: `total_commits` (the gitea SDK's Compare.TotalCommits). A
// pointer so a MISSING field is distinguishable from an explicit 0.
type forgejoCompareBody struct {
	TotalCommits *int64 `json:"total_commits"`
}

// forgejoGetBounded is the ancestry surface's raw GET: rawGetLimited's auth, but sent
// through f.ancClient, which REFUSES redirects (the SDK's shared client follows them,
// re-sending the token header and reading the redirect target's answer as if it were
// this forge's). Any non-2xx, a 3xx included, is an error. It returns the status (so a
// 404 is recognisable before redaction), treats a body past the ceiling as an error
// rather than silently truncating it, and never folds the error body into the message.
// Every error is PAT-redacted.
func (f *forgejo) forgejoGetBounded(ctx context.Context, op, path string, limit int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/api/v1"+path, nil)
	if err != nil {
		return 0, nil, f.wrapErr(op, err)
	}
	req.Header.Set("Authorization", "token "+f.token)
	resp, err := f.ancClient.Do(req)
	if err != nil {
		return 0, nil, f.wrapErr(op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode, nil, f.wrapErr(op, fmt.Errorf("status %d", resp.StatusCode))
	}
	body, err := readBounded(resp.Body, limit)
	if err != nil {
		return resp.StatusCode, nil, f.wrapErr(op, err)
	}
	return resp.StatusCode, body, nil
}

// forgejoSlug resolves the repo slug through the SDK client the rest of the driver
// uses (repoSlugFor caches it for the driver's lifetime).
func (f *forgejo) forgejoSlug(ctx context.Context, projectID int64) (repoSlug, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return repoSlug{}, err
	}
	return f.repoSlugFor(c, projectID)
}

// BranchHead implements Forge (issue #1582 M1). GET /repos/{o}/{r}/branches/{branch}
// → commit.id, the reader-gated endpoint DefaultBranchProtection already reads. The
// branch is escaped per segment exactly as the gitea SDK's GetRepoBranch does (Forgejo
// routes a slash-bearing branch name by its raw segments). A 404 is ErrRefNotFound; a
// redirect is an error. The response's `name` is REQUIRED and must equal the requested
// branch (missing or different is an error), as on GitHub.
func (f *forgejo) BranchHead(ctx context.Context, projectID int64, branch string) (string, error) {
	if branch == "" {
		return "", errors.New("forgejo: branch head: empty branch name")
	}
	slug, err := f.forgejoSlug(ctx, projectID)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/repos/%s/%s/branches/%s", url.PathEscape(slug.owner), url.PathEscape(slug.repo), escapePathSegments(branch))
	status, body, err := f.forgejoGetBounded(ctx, "branch head", path, ancestryBodyLimit)
	if err != nil {
		if status == http.StatusNotFound {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var b forgejoBranchHeadBody
	if err := json.Unmarshal(body, &b); err != nil {
		return "", f.wrapErr("branch head: decode", err)
	}
	if b.Name == nil || *b.Name != branch {
		return "", errors.New("forgejo: branch head: response does not name the requested branch")
	}
	if b.Commit == nil || b.Commit.ID == nil || !isCommitSHA(*b.Commit.ID) {
		return "", errors.New("forgejo: branch head: response carries no 40-hex commit id")
	}
	return *b.Commit.ID, nil
}

// CompareAncestry implements Forgejo's two-call proof (issue #1582 M1). Forgejo's
// compare EMPTIES its commit list when the merge base cannot be computed, and
// total_commits is derived from that list, so a lone total_commits == 0 is NOT proof
// of ancestry (unrelated histories also read 0). The driver therefore asks both ways:
//
//	A = compare/{head}...{candidate}  (commits the candidate adds on top of head)
//	B = compare/{candidate}...{head}  (commits head adds on top of the candidate)
//
// and answers AncestryAncestor ONLY when A.total_commits == 0 AND B.total_commits > 0:
// the candidate adds nothing AND a real merge base exists from which head moved on.
// EVERY other combination is AncestryUnknown — a missing field on either side, any
// error/404/429, both 0 (unrelated histories, or a merge base Forgejo failed to find),
// or A > 0 (Forgejo cannot distinguish "ahead" from "diverged" here). Forgejo never
// answers AncestryNotAncestor.
func (f *forgejo) CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (Ancestry, error) {
	if a, done, err := validateAncestryArgs("forgejo", head, candidate); done {
		return a, err
	}
	slug, err := f.forgejoSlug(ctx, projectID)
	if err != nil {
		return AncestryUnknown, err
	}
	a, err := f.compareTotal(ctx, slug, head, candidate)
	if err != nil {
		return AncestryUnknown, err
	}
	if a != 0 {
		return AncestryUnknown, fmt.Errorf("forgejo: compare ancestry: candidate adds %d commit(s) on top of head; inconclusive", a)
	}
	b, err := f.compareTotal(ctx, slug, candidate, head)
	if err != nil {
		return AncestryUnknown, err
	}
	if b <= 0 {
		return AncestryUnknown, errors.New("forgejo: compare ancestry: no commits in either direction (unrelated histories or no merge base); inconclusive")
	}
	return AncestryAncestor, nil
}

// compareTotal reads total_commits for compare/{base}...{head}; a missing field is an
// error (never read as 0).
func (f *forgejo) compareTotal(ctx context.Context, slug repoSlug, base, head string) (int64, error) {
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", url.PathEscape(slug.owner), url.PathEscape(slug.repo), base, head)
	_, body, err := f.forgejoGetBounded(ctx, "compare ancestry", path, forgejoCompareBodyLimit)
	if err != nil {
		return 0, err
	}
	var c forgejoCompareBody
	if err := json.Unmarshal(body, &c); err != nil {
		return 0, f.wrapErr("compare ancestry: decode", err)
	}
	if c.TotalCommits == nil {
		return 0, errors.New("forgejo: compare ancestry: response carries no total_commits")
	}
	return *c.TotalCommits, nil
}

// escapePathSegments escapes each "/"-separated segment of a ref name with
// url.PathEscape and rejoins them, mirroring the gitea SDK's pathEscapeSegments.
func escapePathSegments(p string) string {
	segs := strings.Split(p, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return strings.Join(segs, "/")
}

// RefHead implements Forge (issue #1751 M2). GET /repos/{o}/{r}/git/refs/{ref without
// "refs/"}, each ref segment escaped as BranchHead does, through the same redirect-refusing,
// bounded read. Forgejo matches that path as a PREFIX and answers a JSON array (or, for a
// single match, one object), so the driver selects the entry whose `ref` equals the requested
// full ref EXACTLY and requires it to point at a commit object. A 404 is ErrRefNotFound; an
// answer with no exact entry (only a longer ref sharing the prefix) is an error, never another
// ref's head.
func (f *forgejo) RefHead(ctx context.Context, projectID int64, ref string) (string, error) {
	if err := validateFullRef("forgejo", ref); err != nil {
		return "", err
	}
	slug, err := f.forgejoSlug(ctx, projectID)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/repos/%s/%s/git/refs/%s", url.PathEscape(slug.owner), url.PathEscape(slug.repo),
		escapePathSegments(strings.TrimPrefix(ref, "refs/")))
	status, body, err := f.forgejoGetBounded(ctx, "ref head", path, ancestryBodyLimit)
	if err != nil {
		if status == http.StatusNotFound {
			return "", ErrRefNotFound
		}
		return "", err
	}
	var entries []refObjectBody
	if err := json.Unmarshal(body, &entries); err != nil {
		var one refObjectBody
		if err2 := json.Unmarshal(body, &one); err2 != nil {
			return "", f.wrapErr("ref head: decode", err2)
		}
		entries = []refObjectBody{one}
	}
	for _, e := range entries {
		if e.Ref != nil && *e.Ref == ref {
			return e.commitSHAOf("forgejo", ref)
		}
	}
	return "", errors.New("forgejo: ref head: response does not name the requested ref")
}
