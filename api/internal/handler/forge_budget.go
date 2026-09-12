package handler

// forge_budget.go holds the forge-agnostic half of PRD #1255 D4's outbound budget: a
// process-wide, per-connection token bucket over INTERACTIVE forge reads. The inbound
// forgeLimiter caps HTTP requests per user/route, NOT forge SPEND, and one forge-view
// list request fans a single inbound request out into up to ~61 forge calls (D4), so a
// per-connection budget over the forge CALLS themselves is the primary, forge-agnostic
// protection that keeps an interactive reader from starving the poller (both ride the
// same PAT). It is charged AT THE FORGE-CALL SITE (inside forgeview.go's memoized load
// closures), so the enrichment fan-out is charged too and a memo HIT costs nothing;
// when a connection has exhausted its budget the load returns a *forge.RateLimitError
// BEFORE the forge call, which writeForgeError maps to 429 + Retry-After — a shed LOAD
// makes zero forge calls (a request that exhausts its budget mid-fan-out may complete a
// few real calls before a later load sheds, but still returns 429). This token bucket
// is one of D4's two shedding rules; the other, the GitHub Rate.Remaining reserve, is
// implemented in the forge driver (see forge/github_rate.go shedIfReserved).

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// forgeBudget is the per-connection token bucket over interactive forge reads. It keys
// by connection id (uuid): all of a connection's interactive reads share one budget,
// because they all spend the same PAT's forge quota. The bucket refills at max tokens
// per minute with a burst of max, so the "window" is a fixed one minute — no separate
// window knob. max <= 0 means UNLIMITED: Take always succeeds and never sheds (this is
// how a struct-literal Handler with no configured budget, and an operator who sets
// FORGE_INTERACTIVE_RATE_MAX to a non-positive value — which parseInt floors back to
// the 120 default anyway — behave). The clock is injected (now) so the refill is
// testable without sleeping.
type forgeBudget struct {
	// max is BOTH the refill rate (tokens per minute) and the burst ceiling. <= 0 is
	// unlimited. Kept as float64 so the refill arithmetic needs no per-op conversion.
	max float64
	now func() time.Time
	mu  sync.Mutex
	// buckets is the per-connection state, created lazily on first Take for a connID.
	// It grows one entry per connection ever seen; the connection set is small and
	// bounded by the forge_connections table, so no eviction is needed.
	buckets map[uuid.UUID]*tokenBucket
}

// tokenBucket is one connection's refilling bucket: tokens available now, and the last
// time they were refilled.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// newForgeBudget builds a budget refilling at max tokens/minute with burst max. A nil
// clock defaults to time.Now, so New and the lazy accessor can pass h.now (which may be
// nil on a struct-literal test Handler) without a guard at the call site.
func newForgeBudget(max int, now func() time.Time) *forgeBudget {
	if now == nil {
		now = time.Now
	}
	return &forgeBudget{
		max:     float64(max),
		now:     now,
		buckets: make(map[uuid.UUID]*tokenBucket),
	}
}

// Take charges one interactive forge read against connID's bucket. It returns ok=true
// (token spent) or ok=false with a positive retryAfter — the time until the next whole
// token accrues — when the bucket is empty. An unlimited budget (max <= 0), or a nil
// receiver, always returns ok=true with no retry. Concurrency-safe: the whole
// refill-and-take is under one lock, so N goroutines fanning out enrichment loads for
// one connection charge exactly N tokens.
func (b *forgeBudget) Take(connID uuid.UUID) (ok bool, retryAfter time.Duration) {
	if b == nil || b.max <= 0 {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	bk := b.buckets[connID]
	if bk == nil {
		// A fresh connection starts full (burst = max), then refills from there.
		bk = &tokenBucket{tokens: b.max, last: now}
		b.buckets[connID] = bk
	}
	// Refill by the elapsed fraction of a minute, capped at the burst ceiling. Only
	// advance on forward time so an injected clock that does not move (or a rare
	// non-monotonic wall clock) never drains a bucket.
	if elapsed := now.Sub(bk.last); elapsed > 0 {
		bk.tokens += elapsed.Seconds() * b.max / 60.0
		if bk.tokens > b.max {
			bk.tokens = b.max
		}
		bk.last = now
	}
	if bk.tokens >= 1 {
		bk.tokens--
		return true, 0
	}
	// Empty: time to accrue the shortfall to one whole token, at max tokens/minute.
	needed := 1 - bk.tokens
	retry := time.Duration(needed * (60.0 / b.max) * float64(time.Second))
	if retry <= 0 {
		retry = time.Second
	}
	return false, retry
}
