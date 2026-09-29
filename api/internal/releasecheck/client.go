package releasecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// defaultBaseURL is the GitHub REST API base. The fetch endpoint is a compile-time
// CONSTANT (baseURL + releasePath) — an instance-global, unauthenticated-by-default
// check against a host uzi controls, so the user-supplied-URL SSRF allowlist
// deliberately does NOT apply (PRD #836). baseURL indirects the constant only so a
// test can point the fetch at an httptest server; production never rewrites it.
const defaultBaseURL = "https://api.github.com"

// releasePath is the constant "latest release" path for vtmocanu/uzi. The endpoint
// excludes drafts and prereleases itself, so a prerelease-ahead reads as up to date.
const releasePath = "/repos/vtmocanu/uzi/releases/latest"
const releasesPath = "/repos/vtmocanu/uzi/releases?per_page=100"

// baseURL is the fetch base; overridable by tests only (see defaultBaseURL).
var baseURL = defaultBaseURL

const (
	// releaseCheckTimeout is the hard per-call ceiling so a poll can never hang.
	releaseCheckTimeout = 15 * time.Second
	// maxReleaseBodyBytes bounds the JSON read (a latest-release payload is a few KB;
	// this caps a hostile/oversized response, mirroring the agent-source wire cap).
	maxReleaseBodyBytes = 1 << 20 // 1 MiB
)

// githubRelease is the subset of the releases/latest payload the check reads.
type githubRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
}

// newHTTPClient builds the dedicated guarded client for the release check (PRD #836):
// a hard Timeout and a redirect refusal (github.com/api.github.com never legitimately
// 3xx-redirects this GET; returning ErrUseLastResponse hands the redirect response
// back UNFOLLOWED, which fetchLatest then rejects as a non-200 status). It is NOT the
// per-user forge driver (newGitHub), which carries a user PAT — the wrong trust
// context for an instance-global check. The response body is separately bounded with
// io.LimitReader in fetchLatest.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: releaseCheckTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// fetchLatest GETs the constant releases/latest endpoint and parses the fields the
// derivation needs. It sends Accept: application/vnd.github+json; when token is
// non-empty it adds Authorization: Bearer <token> and scrubs the token from any
// returned error (a token must never appear in a message/log). The JSON read is
// capped with io.LimitReader. A non-200 status or a decode failure is a plain error
// with no token material.
func fetchLatest(ctx context.Context, client *http.Client, token string) (githubRelease, error) {
	var rel githubRelease
	err := fetchJSON(ctx, client, token, releasePath, &rel)
	return rel, err
}

// fetchLatestRC scans one bounded page, choosing the highest exact RC tag among
// published, non-draft releases. GitHub's ordering is not a version ordering.
func fetchLatestRC(ctx context.Context, client *http.Client, token string) (githubRelease, error) {
	var releases []githubRelease
	if err := fetchJSON(ctx, client, token, releasesPath, &releases); err != nil {
		return githubRelease{}, err
	}
	var best githubRelease
	for _, rel := range releases {
		if rel.Draft || !exactRCTag(rel.TagName) {
			continue
		}
		if best.TagName == "" || semver.Compare(rel.TagName, best.TagName) > 0 {
			best = rel
		}
	}
	return best, nil
}

func exactRCTag(tag string) bool {
	if !semver.IsValid(tag) || semver.Canonical(tag) != tag {
		return false
	}
	base, n, ok := strings.Cut(tag, "-rc.")
	if !ok || strings.Contains(base, "-") || strings.Contains(n, ".") || n == "" || (len(n) > 1 && n[0] == '0') {
		return false
	}
	for _, ch := range n {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func fetchJSON(ctx context.Context, client *http.Client, token, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return scrubToken(err, token)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release check: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBodyBytes+1))
	if err != nil {
		return fmt.Errorf("release check: read response: %w", scrubToken(err, token))
	}
	if len(body) > maxReleaseBodyBytes {
		return errors.New("release check: response exceeds 1 MiB")
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("release check: decode response: %w", err)
	}
	return nil
}

// scrubToken removes the token from an error message defensively. A transport error
// carries a URL, not a header, so the token normally never appears — but if it ever
// does, redact it so the error is safe to store in the status/log.
func scrubToken(err error, token string) error {
	if err == nil {
		return nil
	}
	if token == "" {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		return errors.New(strings.ReplaceAll(msg, token, "REDACTED"))
	}
	return err
}
