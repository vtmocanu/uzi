// Package forgememo is a generic, process-local, tenant-agnostic short-TTL memo
// with singleflight collapsing (PRD #1255 D4). The forge-view read routes use it
// to fold duplicate forge reads: N terminals polling one PR cost one forge
// round-trip per TTL window instead of N.
//
// The memo is deliberately dumb about its keys. Callers build an opaque string
// key (the handlers prefix it with the tenant scope
// connection_id | sha256(token_ciphertext) | repo_id | route | query); this
// package never interprets it, so it stays tenant-agnostic and reusable.
//
// It imports NO forge SDK on purpose: forgememo is not under
// **/internal/forge/**, so the depguard forge-sdk-isolation rule (.golangci.yml)
// denies go-github / client-go / gitea here. Only stdlib +
// golang.org/x/sync/singleflight (already a dependency; see
// api/internal/oidc/provider.go) are used.
package forgememo

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// entry is one cached value with its approximate byte size and absolute expiry.
type entry struct {
	key    string
	val    any
	size   int
	expiry time.Time
}

// Cache is a bounded, process-local LRU memo guarded by a mutex, with a
// singleflight.Group collapsing concurrent misses for one key onto a single
// load. The zero value is not usable; construct with New. Safe for concurrent
// use.
type Cache struct {
	maxEntries int
	maxBytes   int

	// now is the clock, injectable for deterministic TTL tests. Defaults to
	// time.Now; tests in this package override it so a TTL boundary is crossed by
	// advancing a variable instead of sleeping.
	now func() time.Time

	mu sync.Mutex
	// ll orders entries by recency: most-recently-used at the front, least at the
	// back. Each element's Value is an *entry. m indexes the same elements by key.
	ll *list.List
	m  map[string]*list.Element

	// sf collapses concurrent Do(key) calls with a live miss onto one load.
	sf singleflight.Group
}

// New builds a bounded memo. maxEntries caps the LRU (the forge-view routes use
// 256); maxBytes is the per-entry ceiling (512 KiB there) above which a value is
// returned to the caller but never stored. maxEntries is floored at 1 so the
// cache is always usable.
func New(maxEntries, maxBytes int) *Cache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Cache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		now:        time.Now,
		ll:         list.New(),
		m:          make(map[string]*list.Element),
	}
}

// Do returns the value for key. On a live cached entry (now before its expiry)
// it returns immediately without calling load. Otherwise it runs load exactly
// once across all concurrent Do(key) callers (via singleflight) and shares the
// result with every waiter.
//
// load returns the value, its approximate byte size, and an error.
//   - On error: nothing is cached and the error is returned. Concurrent callers
//     of that one load still share it, but the NEXT Do after it returns retries
//     load (a failed load leaves no entry).
//   - On success: the value is stored with expiry = now + ttl, UNLESS its size
//     exceeds maxBytes, in which case it is returned to this caller but not
//     stored.
//
// Storing enforces the LRU bound: an insert past maxEntries evicts the
// least-recently-used entry. A read hit updates recency.
func (c *Cache) Do(key string, ttl time.Duration, load func() (val any, size int, err error)) (any, error) {
	// Fast path: a live cached entry short-circuits without touching singleflight
	// or load.
	if v, ok := c.get(key); ok {
		return v, nil
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		// A concurrent leader may have populated the entry while we queued for the
		// group; re-check before spending a load.
		if v, ok := c.get(key); ok {
			return v, nil
		}
		val, size, lerr := load()
		if lerr != nil {
			// Errors are never cached: singleflight already collapsed the concurrent
			// callers of this one load, and the next Do retries.
			return nil, lerr
		}
		c.store(key, val, size, ttl)
		return val, nil
	})
	return v, err
}

// get returns a live cached value and updates its recency. An expired entry is
// evicted and reported as a miss.
func (c *Cache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*entry)
	if !c.now().Before(e.expiry) {
		c.ll.Remove(el)
		delete(c.m, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

// store caches val under key with expiry = now + ttl, skipping values larger
// than maxBytes, and evicts the least-recently-used entry when the insert would
// exceed maxEntries.
func (c *Cache) store(key string, val any, size int, ttl time.Duration) {
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(ttl)
	if el, ok := c.m[key]; ok {
		e := el.Value.(*entry)
		e.val = val
		e.size = size
		e.expiry = exp
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&entry{key: key, val: val, size: size, expiry: exp})
	c.m[key] = el
	for c.ll.Len() > c.maxEntries {
		back := c.ll.Back()
		if back == nil {
			break
		}
		be := back.Value.(*entry)
		c.ll.Remove(back)
		delete(c.m, be.key)
	}
}
