package forge

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
)

// GitHub conditional-request layer (PRD #1255 D4).
//
// go-github has no ETag API and surfaces a 304 as an error, so conditional GETs
// cannot be done per-call above go-github; they live BELOW it as an
// http.RoundTripper wrapping the driver's HTTP transport. On an allowlisted read
// GET the transport sends If-None-Match from a cached ETag and, when GitHub
// answers 304, replays the cached body as a synthesized 200 so go-github sees a
// normal response instead of erroring. GitHub's documented rule: a 304 does not
// count against the primary rate limit, so the new read routes' polling does not
// spend the poller/worker rate budget.
//
// The cache is PROCESS-WIDE (package-level): ForgeForConnection rebuilds the
// driver per request, so a per-driver cache would never survive to serve a
// second poll. It is a bounded LRU keyed by (sha256(token), method+URL); bodies
// over 512 KiB are not cached. Only GET, and only for the allowlisted read
// paths, is touched — every other request passes through unchanged, so the
// poller and worker lanes see zero behavior difference.

const (
	etagCacheMaxEntries = 256
	etagCacheMaxBytes   = 512 * 1024
)

// githubETagCache is the process-wide conditional-GET cache. It outlives any one
// driver instance because the driver is rebuilt per ForgeForConnection call.
var githubETagCache = newETagCache()

// etagEntry is one cached conditional-GET response: the ETag to echo back as
// If-None-Match, the buffered body to replay on 304, and a copy of the response
// headers (so the replayed 200 carries the original ETag and content type).
type etagEntry struct {
	key    string
	etag   string
	body   []byte
	header http.Header
}

// etagCache is a bounded, mutex-guarded LRU of etagEntry keyed by an opaque
// string. Most-recently-used at the front of ll, least at the back.
type etagCache struct {
	mu  sync.Mutex
	ll  *list.List
	m   map[string]*list.Element
	max int
}

func newETagCache() *etagCache {
	return &etagCache{ll: list.New(), m: make(map[string]*list.Element), max: etagCacheMaxEntries}
}

// get returns the cached entry for key and marks it most-recently-used.
func (c *etagCache) get(key string) (*etagEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*etagEntry), true
}

// put stores e, replacing any existing entry for its key, and evicts the
// least-recently-used entry when the insert would exceed the bound.
func (c *etagCache) put(e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[e.key]; ok {
		el.Value = e
		c.ll.MoveToFront(el)
		return
	}
	c.m[e.key] = c.ll.PushFront(e)
	for c.ll.Len() > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.ll.Remove(back)
		delete(c.m, back.Value.(*etagEntry).key)
	}
}

// etagTransport wraps a base RoundTripper, adding If-None-Match on allowlisted
// read GETs and replaying cached bodies on 304. salt = sha256(token) hex
// namespaces the cache per PAT so two connections never share an entry.
type etagTransport struct {
	base  http.RoundTripper
	salt  string
	cache *etagCache
}

// newETagTransport wraps base with the process-wide conditional-GET cache. base
// nil means http.DefaultTransport.
func newETagTransport(base http.RoundTripper, token string) *etagTransport {
	return newETagTransportWithCache(base, token, githubETagCache)
}

// newETagTransportWithCache is newETagTransport with an explicit cache, used by
// tests for isolation from the process-wide cache.
func newETagTransportWithCache(base http.RoundTripper, token string, cache *etagCache) *etagTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	sum := sha256.Sum256([]byte(token))
	return &etagTransport{base: base, salt: hex.EncodeToString(sum[:]), cache: cache}
}

// etagAllowlisted reports whether path is one of the new forge-view read paths
// the conditional cache is scoped to (D4). Matching is on the path shape, not
// the host, so it covers both api.github.com (prod) and the /api/v3 test base:
//
//	/repos/{o}/{r}/pulls...                        (list, detail, reviews)
//	/repos/{o}/{r}/commits/{sha}/check-runs
//	/repos/{o}/{r}/commits/{sha}/status            (combined status)
//	/repos/{o}/{r}/actions/runs...                 (runs list, run, jobs)
func etagAllowlisted(path string) bool {
	switch {
	case strings.Contains(path, "/pulls"):
		return true
	case strings.Contains(path, "/check-runs"):
		return true
	case strings.Contains(path, "/actions/runs"):
		return true
	case strings.Contains(path, "/commits/") && strings.HasSuffix(path, "/status"):
		return true
	default:
		return false
	}
}

// RoundTrip implements http.RoundTripper. Non-GET requests and GETs outside the
// allowlist pass straight through the wrapped transport untouched.
func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || !etagAllowlisted(req.URL.Path) {
		return t.base.RoundTrip(req)
	}

	key := t.salt + "\x00" + req.Method + " " + req.URL.String()
	cached, hasCached := t.cache.get(key)

	// Clone so the caller's request is never mutated; add If-None-Match if we
	// hold an ETag for this key.
	outReq := req.Clone(req.Context())
	if hasCached && cached.etag != "" {
		outReq.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := t.base.RoundTrip(outReq)
	if err != nil {
		return resp, err
	}

	// 304 Not Modified: GitHub confirms our cached copy is current. Replay the
	// cached body as a synthesized 200 so go-github never sees the 304 it would
	// treat as an error, but carry the LIVE rate-limit headers the 304 reported
	// (see replay) so go-github's client-side rate tracker updates from current
	// numbers, not the stale ones frozen in the cached entry. Drain and close the
	// empty 304 body only after we have copied its headers.
	if resp.StatusCode == http.StatusNotModified && hasCached {
		replayed := t.replay(req, cached, resp)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return replayed, nil
	}

	// 200 with an ETag: buffer the body so we can both cache it and hand the caller
	// a fresh reader. Read at most etagCacheMaxBytes+1 bytes up front so an
	// oversized allowlisted response is never fully buffered just to be discarded
	// for the cache — cap+1 is enough to tell "fits" from "there is more".
	if resp.StatusCode == http.StatusOK {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return resp, nil
		}
		buf, rerr := io.ReadAll(io.LimitReader(resp.Body, etagCacheMaxBytes+1))
		if rerr != nil {
			_ = resp.Body.Close()
			return nil, rerr
		}
		if len(buf) <= etagCacheMaxBytes {
			// Whole body fit: cache it and hand the caller a fresh reader over the
			// buffered bytes; the original stream is drained, so close it.
			_ = resp.Body.Close()
			t.cache.put(&etagEntry{
				key:    key,
				etag:   etag,
				body:   buf,
				header: resp.Header.Clone(),
			})
			resp.Body = io.NopCloser(bytes.NewReader(buf))
			resp.ContentLength = int64(len(buf))
			return resp, nil
		}
		// Oversized: do NOT cache. Hand the caller the already-read prefix followed
		// by the rest of the still-open stream, closing the original on Close, so at
		// most cap+1 bytes are ever buffered while the caller still gets the full
		// body. ContentLength stays as the server reported it (the full length).
		resp.Body = &multiReadCloser{r: io.MultiReader(bytes.NewReader(buf), resp.Body), c: resp.Body}
		return resp, nil
	}

	// Any other status (or a 304 with no cache entry, which should not occur):
	// pass through untouched, cache nothing.
	return resp, nil
}

// etagLiveRateHeaders are the response headers whose values reflect the CURRENT
// rate-limit state and so must come from the live 304 (not the cached 200) when a
// replay is synthesized. GitHub sends fresh X-RateLimit-* on every 304, and
// go-github ingests them into its client-side tracker for any response lacking an
// X-From-Cache header — so if the replay carried the stale cached numbers the
// tracker (and the returned *github.Response.Rate) would read a stale-high
// Remaining after a 304. Retry-After is included for the secondary-limit case.
var etagLiveRateHeaders = []string{
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Used",
	"X-RateLimit-Reset",
	"X-RateLimit-Resource",
	"Retry-After",
}

// replay builds a synthesized 200 response from a cached entry: the buffered body
// over a fresh reader and a copy of the cached headers (so Content-Type / ETag are
// the ones from the original 200), with req attached. The rate-limit-relevant
// headers are then OVERWRITTEN from the live 304 (live), so the synthesized 200
// carries the current rate numbers the 304 reported rather than the stale ones
// frozen in the cache. It deliberately does NOT set X-From-Cache: we WANT
// go-github to update its rate tracker from these live headers.
func (t *etagTransport) replay(req *http.Request, e *etagEntry, live *http.Response) *http.Response {
	h := e.header.Clone()
	if h == nil {
		h = make(http.Header)
	}
	if live != nil {
		for _, name := range etagLiveRateHeaders {
			if v := live.Header.Get(name); v != "" {
				h.Set(name, v)
			}
		}
	}
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(e.body)),
		ContentLength: int64(len(e.body)),
		Request:       req,
	}
}

// multiReadCloser hands back an already-buffered prefix (r's first reader)
// followed by the rest of an underlying stream, and closes that underlying body
// on Close. Used on the oversized 200 path so an allowlisted body over the cache
// cap is streamed to the caller in full without being fully buffered.
type multiReadCloser struct {
	r io.Reader
	c io.Closer
}

func (m *multiReadCloser) Read(p []byte) (int, error) { return m.r.Read(p) }
func (m *multiReadCloser) Close() error               { return m.c.Close() }
