package forgememo

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTTLLiveHitThenReload proves a value is served from cache within its TTL
// (load not re-run), and load runs again once the clock advances past expiry.
// The clock is injected so the boundary is crossed by advancing a variable, not
// by sleeping.
func TestTTLLiveHitThenReload(t *testing.T) {
	c := New(16, 1024)
	nowT := time.Unix(1000, 0)
	c.now = func() time.Time { return nowT }

	var calls int32
	load := func() (any, int, error) {
		atomic.AddInt32(&calls, 1)
		return "v", 3, nil
	}

	if v, err := c.Do("k", 5*time.Second, load); err != nil || v != "v" {
		t.Fatalf("first Do = (%v, %v), want (v, nil)", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after first Do, calls = %d, want 1", got)
	}

	// Within TTL: cached, no reload.
	nowT = nowT.Add(4 * time.Second)
	if v, err := c.Do("k", 5*time.Second, load); err != nil || v != "v" {
		t.Fatalf("within-TTL Do = (%v, %v), want (v, nil)", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("within TTL, calls = %d, want 1 (must not reload)", got)
	}

	// Past TTL: load runs again.
	nowT = nowT.Add(2 * time.Second) // +6s total, past the 5s expiry
	if v, err := c.Do("k", 5*time.Second, load); err != nil || v != "v" {
		t.Fatalf("past-TTL Do = (%v, %v), want (v, nil)", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("past TTL, calls = %d, want 2 (must reload)", got)
	}
}

// TestSingleflightCollapsesConcurrentMiss proves N concurrent Do(sameKey) with a
// live miss call load exactly once and all share the result (the "hit exactly
// once" precedent from oidc/provider_test.go). The single load holds the
// in-flight call open until every goroutine has piled onto the group.
func TestSingleflightCollapsesConcurrentMiss(t *testing.T) {
	c := New(16, 1024)

	const n = 20
	var calls int32
	release := make(chan struct{})
	load := func() (any, int, error) {
		atomic.AddInt32(&calls, 1)
		<-release // keep the leader in-flight so the others collapse onto it
		return "shared", 6, nil
	}

	start := make(chan struct{})
	results := make([]any, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = c.Do("k", time.Minute, load)
		}(i)
	}
	close(start)
	// Give every goroutine time to reach the singleflight group before the leader
	// completes; then release it so all n share the one result.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("load ran %d times, want exactly 1 (singleflight must collapse)", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil || results[i] != "shared" {
			t.Fatalf("goroutine %d got (%v, %v), want (shared, nil)", i, results[i], errs[i])
		}
	}
}

// TestLRUEviction fills the cache to maxEntries, touches the oldest key so it is
// no longer least-recently-used, then inserts one more — asserting the genuine
// LRU key was evicted (a later Do re-runs its load) and a recently-used key
// survived (no reload).
func TestLRUEviction(t *testing.T) {
	c := New(3, 1024)
	fixedNow := time.Unix(1000, 0)
	c.now = func() time.Time { return fixedNow } // freeze the clock: TTL never fires here

	var counts [5]int32
	loader := func(i int, val string) func() (any, int, error) {
		return func() (any, int, error) {
			atomic.AddInt32(&counts[i], 1)
			return val, 1, nil
		}
	}

	mustDo := func(key string, i int, val string) {
		t.Helper()
		if v, err := c.Do(key, time.Hour, loader(i, val)); err != nil || v != val {
			t.Fatalf("Do(%q) = (%v, %v), want (%q, nil)", key, v, err, val)
		}
	}

	mustDo("k1", 1, "a") // MRU order: k1
	mustDo("k2", 2, "b") // k2, k1
	mustDo("k3", 3, "c") // k3, k2, k1

	// Touch k1 (read hit): recency order becomes k1, k3, k2 — k2 is now the LRU.
	mustDo("k1", 1, "a")
	if got := atomic.LoadInt32(&counts[1]); got != 1 {
		t.Fatalf("k1 reloaded on a hit: counts[1] = %d, want 1", got)
	}

	// Insert k4: exceeds maxEntries=3, evicts the LRU (k2).
	mustDo("k4", 4, "d") // k4, k1, k3

	// k2 was evicted: its load runs again.
	mustDo("k2", 2, "b")
	if got := atomic.LoadInt32(&counts[2]); got != 2 {
		t.Fatalf("k2 was not evicted: counts[2] = %d, want 2", got)
	}
	// k1 survived: no reload.
	mustDo("k1", 1, "a")
	if got := atomic.LoadInt32(&counts[1]); got != 1 {
		t.Fatalf("k1 did not survive: counts[1] = %d, want 1", got)
	}
}

// TestOversizedValueNotCached proves a value whose size exceeds maxBytes is
// returned to the caller but never stored (a later Do re-runs load), while a
// value exactly at maxBytes is cached (the boundary is inclusive: skip is
// size > maxBytes).
func TestOversizedValueNotCached(t *testing.T) {
	const maxBytes = 512 * 1024
	c := New(16, maxBytes)
	c.now = func() time.Time { return time.Unix(1000, 0) }

	var bigCalls int32
	big := func() (any, int, error) {
		atomic.AddInt32(&bigCalls, 1)
		return "big", maxBytes + 1, nil // one byte over the ceiling
	}
	if v, err := c.Do("big", time.Hour, big); err != nil || v != "big" {
		t.Fatalf("Do(big) = (%v, %v), want (big, nil)", v, err)
	}
	if v, err := c.Do("big", time.Hour, big); err != nil || v != "big" {
		t.Fatalf("Do(big) #2 = (%v, %v), want (big, nil)", v, err)
	}
	if got := atomic.LoadInt32(&bigCalls); got != 2 {
		t.Fatalf("oversized value was cached: bigCalls = %d, want 2", got)
	}

	var fitCalls int32
	fit := func() (any, int, error) {
		atomic.AddInt32(&fitCalls, 1)
		return "fit", maxBytes, nil // exactly at the ceiling: cached
	}
	if _, err := c.Do("fit", time.Hour, fit); err != nil {
		t.Fatalf("Do(fit) error: %v", err)
	}
	if _, err := c.Do("fit", time.Hour, fit); err != nil {
		t.Fatalf("Do(fit) #2 error: %v", err)
	}
	if got := atomic.LoadInt32(&fitCalls); got != 1 {
		t.Fatalf("value at the ceiling was not cached: fitCalls = %d, want 1", got)
	}
}

// TestErrorNeverCached proves a failing load leaves no entry: Do returns the
// error, and a subsequent Do re-runs load (and can succeed).
func TestErrorNeverCached(t *testing.T) {
	c := New(16, 1024)
	c.now = func() time.Time { return time.Unix(1000, 0) }

	wantErr := errors.New("forge blip")
	var calls int32
	load := func() (any, int, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return nil, 0, wantErr
		}
		return "recovered", 9, nil
	}

	if v, err := c.Do("k", time.Hour, load); !errors.Is(err, wantErr) || v != nil {
		t.Fatalf("first Do = (%v, %v), want (nil, forge blip)", v, err)
	}
	// The failed load left nothing cached, so the retry re-runs load and succeeds.
	if v, err := c.Do("k", time.Hour, load); err != nil || v != "recovered" {
		t.Fatalf("retry Do = (%v, %v), want (recovered, nil)", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (error must not be cached)", got)
	}
}

// TestKeyIsolation proves different keys do not collide: each keeps its own
// value and its own load.
func TestKeyIsolation(t *testing.T) {
	c := New(16, 1024)
	c.now = func() time.Time { return time.Unix(1000, 0) }

	load := func(val string) func() (any, int, error) {
		return func() (any, int, error) { return val, len(val), nil }
	}
	if v, err := c.Do("a", time.Hour, load("A")); err != nil || v != "A" {
		t.Fatalf("Do(a) = (%v, %v), want (A, nil)", v, err)
	}
	if v, err := c.Do("b", time.Hour, load("B")); err != nil || v != "B" {
		t.Fatalf("Do(b) = (%v, %v), want (B, nil)", v, err)
	}
	// Re-reads return each key's own cached value, not the other's.
	if v, err := c.Do("a", time.Hour, load("SHOULD-NOT-RUN")); err != nil || v != "A" {
		t.Fatalf("Do(a) re-read = (%v, %v), want (A, nil)", v, err)
	}
	if v, err := c.Do("b", time.Hour, load("SHOULD-NOT-RUN")); err != nil || v != "B" {
		t.Fatalf("Do(b) re-read = (%v, %v), want (B, nil)", v, err)
	}
}
