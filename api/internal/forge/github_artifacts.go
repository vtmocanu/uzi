package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	artifactArchiveBytes  int64 = 64 << 20
	artifactPageBytes     int64 = 1 << 20
	artifactMetadataBytes int64 = 4 << 20
	artifactMaxCount            = 100
	artifactMaxPages            = 10
)

// These per-driver hooks are inaccessible through configuration. They let tests
// exercise DNS validation and pinned dialing without contacting public hosts.
type artifactTestHooks struct {
	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
	blob   http.RoundTripper
}

func artifactIdentityEncoding(resp *http.Response) bool {
	values := resp.Header.Values("Content-Encoding")
	return !resp.Uncompressed && (len(values) == 0 || (len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "identity")))
}

// artifactRead consumes at most limit+1 bytes, including the overflow probe.
// Closing the original body does not perform a caller-side cleanup drain.
func artifactRead(resp *http.Response, limit int64) ([]byte, error) {
	if !artifactIdentityEncoding(resp) {
		return nil, errArtifactResponse
	}
	if resp.ContentLength > limit {
		return nil, ErrArtifactBudget
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return nil, ErrArtifactBudget
	}
	if err != nil {
		return nil, errArtifactRequest
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return nil, errArtifactResponse
	}
	return data, nil
}

// Error bodies are never read or included in returned errors.
func artifactStatus(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound:
		return ErrArtifactMissing
	case http.StatusGone:
		return ErrArtifactExpired
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			e := &RateLimitError{Err: errorsArtifactRate}
			if n, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); err == nil && n > 0 {
				e.Retry = time.Duration(min(n, 3600)) * time.Second
			}
			if n, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && n > 0 {
				e.Reset = time.Unix(n, 0)
			}
			return e
		}
		return ErrArtifactDenied
	case http.StatusUnauthorized:
		return ErrArtifactDenied
	default:
		return errArtifactResponse
	}
}

var errorsArtifactRate = fmt.Errorf("artifact: rate limited")

func (g *github) artifactGET(ctx context.Context, path string) (*http.Response, error) {
	req, err := g.client.NewRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, errArtifactRequest
	}
	// NewRequest resolves only generated numeric paths; keep an explicit origin
	// check before attaching credentials, in addition to the transport origin pin.
	base, err := url.Parse(g.client.BaseURL())
	if err != nil {
		return nil, errArtifactDestination
	}
	if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host || req.URL.User != nil {
		return nil, errArtifactDestination
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := g.artifactAPI.Do(req)
	if err != nil {
		return nil, errArtifactRequest
	}
	return resp, nil
}

type githubArtifactPage struct {
	TotalCount *int64 `json:"total_count"`
	Artifacts  []struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Size    int64  `json:"size_in_bytes"`
		Expired bool   `json:"expired"`
		Run     struct {
			ID           int64  `json:"id"`
			RepositoryID int64  `json:"repository_id"`
			SHA          string `json:"head_sha"`
		} `json:"workflow_run"`
	} `json:"artifacts"`
}

// artifactRepoPath uses the same cached ID-to-slug resolution as other GitHub
// operations, but bounds a cache miss before SDK decoding. Slugs select only
// the API path; they never supply blob-download authority.
func (g *github) artifactRepoPath(ctx context.Context, projectID int64) (string, error) {
	if ciBudget(ctx) == nil {
		ctx = WithCIRepairReadLimits(ctx)
	}
	slug, err := g.repoSlugFor(markCIJSON(ctx), projectID)
	if err != nil {
		return "", errArtifactRequest
	}
	for _, component := range []string{slug.owner, slug.repo} {
		if component == "" || component == "." || component == ".." || len(component) > 255 {
			return "", errArtifactResponse
		}
		for _, r := range component {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
				return "", errArtifactResponse
			}
		}
	}
	return "repos/" + slug.owner + "/" + slug.repo, nil
}

func (g *github) ListRunArtifacts(ctx context.Context, projectID, runID int64) ([]Artifact, error) {
	if projectID <= 0 || runID <= 0 {
		return nil, errArtifactRequest
	}
	repoPath, err := g.artifactRepoPath(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0)
	seen := make(map[int64]bool)
	remaining := artifactMetadataBytes
	var expectedTotal int64 = -1
	// Pages are generated locally, never taken from an upstream Link destination.
	// Any failure aborts this list; no partial metadata can become evidence.
	for page := 1; page <= artifactMaxPages; page++ {
		path := fmt.Sprintf("%s/actions/runs/%d/artifacts?per_page=100&page=%d", repoPath, runID, page)
		resp, err := g.artifactGET(ctx, path)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			err := artifactStatus(resp)
			_ = resp.Body.Close()
			return nil, err
		}
		body, err := artifactRead(resp, min(artifactPageBytes, remaining))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(body))
		// Raw JSON is bounded BEFORE decoding, including unknown fields and arrays.
		var p githubArtifactPage
		if json.Unmarshal(body, &p) != nil {
			return nil, errArtifactResponse
		}
		if p.TotalCount == nil || p.Artifacts == nil {
			return nil, errArtifactResponse
		}
		if expectedTotal < 0 {
			expectedTotal = *p.TotalCount
		} else if *p.TotalCount != expectedTotal {
			return nil, errArtifactResponse
		}
		if *p.TotalCount < 0 || *p.TotalCount > artifactMaxCount || len(p.Artifacts)+len(out) > artifactMaxCount {
			return nil, ErrArtifactBudget
		}
		for _, a := range p.Artifacts {
			if a.ID <= 0 || seen[a.ID] || len(a.Name) == 0 || len(a.Name) > 1024 || a.Size < 0 ||
				a.Run.ID != runID || a.Run.RepositoryID != projectID || !isCommitSHA(a.Run.SHA) {
				return nil, errArtifactResponse
			}
			seen[a.ID] = true
			out = append(out, Artifact{ID: a.ID, Name: a.Name, SizeBytes: a.Size, Expired: a.Expired, RunID: a.Run.ID, RepositoryID: a.Run.RepositoryID, SHA: a.Run.SHA})
		}
		// total_count can change during pagination. Only a consistent complete list
		// is returned; a short/inconsistent page cannot silently hide artifacts.
		if int64(len(out)) == *p.TotalCount {
			return out, nil
		}
		if len(p.Artifacts) == 0 || int64(len(out)) > *p.TotalCount {
			return nil, errArtifactResponse
		}
	}
	return nil, ErrArtifactBudget
}

func (g *github) DownloadArtifact(ctx context.Context, projectID, artifactID, maxBytes int64) ([]byte, error) {
	if projectID <= 0 || artifactID <= 0 {
		return nil, errArtifactRequest
	}
	if maxBytes <= 0 {
		return nil, ErrArtifactBudget
	}
	limit := min(maxBytes, artifactArchiveBytes)
	repoPath, err := g.artifactRepoPath(ctx, projectID)
	if err != nil {
		return nil, err
	}
	resp, err := g.artifactGET(ctx, fmt.Sprintf("%s/actions/artifacts/%d/zip", repoPath, artifactID))
	if err != nil {
		return nil, err
	}
	// No SDK redirect handling, error decoder, or cleanup drain on this response.
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return nil, artifactStatus(resp)
	}
	locations := resp.Header.Values("Location")
	if len(locations) != 1 || len(locations[0]) > 8192 || !artifactIdentityEncoding(resp) {
		return nil, errArtifactDestination
	}
	return g.artifactBlob(ctx, locations[0], limit)
}

func artifactPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Reject special-use, documentation, benchmarking and transition networks.
	// IPv6 is restricted to currently allocated global unicast space.
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range artifactSpecialNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var artifactSpecialNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func (g *github) artifactBlob(ctx context.Context, location string, limit int64) ([]byte, error) {
	u, err := url.Parse(location)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Opaque != "" || u.Fragment != "" || strings.HasSuffix(u.Host, ":") || (u.Port() != "" && u.Port() != "443") {
		return nil, errArtifactDestination
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lookup := g.artifactHooks.lookup
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addresses, err := lookup(ctx, u.Hostname())
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errArtifactDestination
	}
	for _, ip := range addresses {
		if !artifactPublicIP(ip) {
			return nil, errArtifactDestination
		}
	}
	pinned := net.JoinHostPort(addresses[0].Unmap().String(), "443")
	dial := g.artifactHooks.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	// One connection attempt, one validated address, no proxy or second DNS lookup.
	// URL hostname remains intact for normal TLS certificate and SNI verification.
	transport := artifactBlobTransport(dial, pinned)
	defer transport.CloseIdleConnections()
	var rt http.RoundTripper = transport
	if g.artifactHooks.blob != nil {
		rt = g.artifactHooks.blob
	}
	client := &http.Client{Transport: rt, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errArtifactDestination
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errArtifactRequest
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, artifactStatus(resp)
	}
	return artifactRead(resp, limit)
}

// artifactBlobTransport is the transport used for the unauthenticated, pinned blob request.
func artifactBlobTransport(dial func(context.Context, string, string) (net.Conn, error), pinned string) *http.Transport {
	return &http.Transport{
		DisableCompression:     true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 64 << 10,
		ResponseHeaderTimeout:  10 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		DialContext:            func(ctx context.Context, network, _ string) (net.Conn, error) { return dial(ctx, network, pinned) },
	}
}
