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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+releasePath, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return githubRelease{}, scrubToken(err, token)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("release check: unexpected status %d", resp.StatusCode)
	}

	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBodyBytes)).Decode(&rel); err != nil {
		return githubRelease{}, fmt.Errorf("release check: decode response: %w", err)
	}
	return rel, nil
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
