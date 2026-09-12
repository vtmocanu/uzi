package forge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
// is not cached: the second request sends no If-None-Match and re-fetches.
func TestETagTransportSkipsOversizedBody(t *testing.T) {
	const etag = `"big-v1"`
	big := strings.Repeat("x", etagCacheMaxBytes+1)
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
