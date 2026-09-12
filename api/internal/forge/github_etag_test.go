package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// getBody issues a GET through client and returns the status and body.
func getBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// TestETagTransportReplaysCachedBodyOn304 is the D4/M2b core: the mock serves an
// ETag on the first 200, then ASSERTS the inbound If-None-Match equals that ETag
// before answering 304 — and the RoundTripper must replay the cached body as a
// synthesized 200.
func TestETagTransportReplaysCachedBodyOn304(t *testing.T) {
	const etag = `"pulls-v1"`
	const body = `[{"number":1}]`
	var hits, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			conditional++
			if inm != etag {
				t.Errorf("If-None-Match = %q, want the previously served %q", inm, etag)
			}
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newETagTransportWithCache(nil, "ghp_token", newETagCache())}
	url := srv.URL + "/repos/o/r/pulls"

	// First request: 200 + ETag + body, cached; no conditional header sent.
	if code, got := getBody(t, client, url); code != http.StatusOK || got != body {
		t.Fatalf("first GET = (%d, %q), want (200, %q)", code, got, body)
	}

	// Second request: RoundTripper sends If-None-Match, server 304, replayed 200.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET status = %d, want a replayed 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != body {
		t.Fatalf("replayed body = %q, want %q", string(b), body)
	}
	if resp.Header.Get("ETag") != etag {
		t.Errorf("replayed ETag header = %q, want %q (cached headers must be copied)", resp.Header.Get("ETag"), etag)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("replayed Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
	}
	if conditional != 1 {
		t.Fatalf("server saw %d conditional requests, want 1 (If-None-Match must be sent on the repeat)", conditional)
	}
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2", hits)
	}
}

// TestETagTransportPassesThroughNonAllowlistedGET proves a GET on a
// non-allowlisted path is not conditioned and not cached: two requests both
// reach the server as plain GETs with no If-None-Match.
func TestETagTransportPassesThroughNonAllowlistedGET(t *testing.T) {
	const etag = `"issues-v1"`
	var hits, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") != "" {
			conditional++
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "issues")
	}))
	defer srv.Close()

	client := &http.Client{Transport: newETagTransportWithCache(nil, "ghp_token", newETagCache())}
	url := srv.URL + "/repos/o/r/issues"

	if code, _ := getBody(t, client, url); code != http.StatusOK {
		t.Fatalf("first GET status = %d, want 200", code)
	}
	if code, _ := getBody(t, client, url); code != http.StatusOK {
		t.Fatalf("second GET status = %d, want 200", code)
	}
	if conditional != 0 {
		t.Fatalf("server saw %d conditional requests on a non-allowlisted path, want 0", conditional)
	}
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2 (no caching for non-allowlisted paths)", hits)
	}
}

// TestETagTransportPassesThroughNonGET proves a POST to an allowlisted path is
// untouched: no If-None-Match, not cached.
func TestETagTransportPassesThroughNonGET(t *testing.T) {
	var posts, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		if r.Header.Get("If-None-Match") != "" {
			conditional++
		}
		w.Header().Set("ETag", `"x"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	client := &http.Client{Transport: newETagTransportWithCache(nil, "ghp_token", newETagCache())}
	url := srv.URL + "/repos/o/r/pulls"

	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if conditional != 0 {
		t.Fatalf("server saw %d conditional POSTs, want 0", conditional)
	}
	if posts != 2 {
		t.Fatalf("server saw %d POSTs, want 2 (POST must never be cached/replayed)", posts)
	}
}

// TestETagTransportSkipsOversizedBody proves a 200 body over the 512 KiB ceiling
// is not cached AND is still handed to the caller in FULL (Fix 2): the transport
// buffers only cap+1 bytes and streams the rest, so the caller reads every byte,
// and the second request sends no If-None-Match and re-fetches. The body is a few
// KiB over the cap so the streamed remainder (past the cap+1 prefix) is exercised.
func TestETagTransportSkipsOversizedBody(t *testing.T) {
	const etag = `"big-v1"`
	big := strings.Repeat("x", etagCacheMaxBytes+1024)
	var hits, conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("If-None-Match") != "" {
			conditional++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, big)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newETagTransportWithCache(nil, "ghp_token", newETagCache())}
	url := srv.URL + "/repos/o/r/actions/runs"

	if code, got := getBody(t, client, url); code != http.StatusOK || len(got) != len(big) {
		t.Fatalf("first GET = (%d, %d bytes), want (200, %d bytes)", code, len(got), len(big))
	}
	// Not cached: no If-None-Match, a full 200 re-fetch.
	if code, got := getBody(t, client, url); code != http.StatusOK || len(got) != len(big) {
		t.Fatalf("second GET = (%d, %d bytes), want a full 200 re-fetch", code, len(got))
	}
	if conditional != 0 {
		t.Fatalf("server saw %d conditional requests, want 0 (oversized body must not be cached)", conditional)
	}
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2", hits)
	}
}

// TestETagTransport304CarriesLiveRateLimitHeaders is Fix 1 at the go-github-client
// level, the way the finding was demonstrated: a real driver built via newGitHub
// points at an httptest server. The first PullRequests.List gets 200 + ETag +
// X-RateLimit-Remaining: 4999; the second is answered 304 with the LIVE
// X-RateLimit-Remaining: 10. The synthesized 200 the transport replays must carry
// the LIVE rate headers (not the stale cached 4999), so go-github's rate tracker
// updates from them and the returned *github.Response.Rate.Remaining reads 10.
func TestETagTransport304CarriesLiveRateLimitHeaders(t *testing.T) {
	const etag = `"pulls-rate-v1"`
	const body = `[]`                              // an empty PR list decodes cleanly on both the 200 and the replayed 200
	const token = "ghp_rateFixtureToken1234567890" //nolint:gosec // G101: fake fixture token, never a real secret //gitleaks:allow
	var conditional int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			t.Errorf("unexpected path %q, want a /pulls list", r.URL.Path)
		}
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			conditional++
			if inm != etag {
				t.Errorf("If-None-Match = %q, want the previously served %q", inm, etag)
			}
			// LIVE rate state reported on the 304: the budget has dropped to 10.
			w.Header().Set("ETag", etag)
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "10")
			w.Header().Set("X-RateLimit-Used", "4990")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Used", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	d, err := newGitHub(srv.URL, token, 5*time.Second)
	if err != nil {
		t.Fatalf("newGitHub: %v", err)
	}
	ctx := context.Background()

	// First List: 200 + ETag, live rate 4999, cached.
	_, resp1, err := d.client.PullRequests.List(ctx, "o", "r", nil)
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if resp1.Rate.Remaining != 4999 {
		t.Fatalf("first List Rate.Remaining = %d, want 4999", resp1.Rate.Remaining)
	}

	// Second List: the transport sends If-None-Match, the server answers 304 with the
	// LIVE remaining (10); the replayed 200 must surface that through go-github.
	_, resp2, err := d.client.PullRequests.List(ctx, "o", "r", nil)
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if conditional != 1 {
		t.Fatalf("server saw %d conditional requests, want 1 (If-None-Match must be sent on the repeat)", conditional)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second List status = %d, want a replayed 200", resp2.StatusCode)
	}
	if resp2.Rate.Remaining != 10 {
		t.Fatalf("after 304 replay Rate.Remaining = %d, want the LIVE 10, not the stale cached 4999", resp2.Rate.Remaining)
	}
}
