package handler

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestForgeBudgetTakeDrainsAndRefills pins the token-bucket contract (PRD #1255 D4): a
// fresh bucket starts full (burst == max), Take succeeds max times then sheds with a
// positive Retry-After, and advancing the injected clock by one refill interval
// (60s/max) restores exactly one token. No sleeping — the clock is a closure.
func TestForgeBudgetTakeDrainsAndRefills(t *testing.T) {
	const max = 3
	now := time.Unix(0, 0)
	b := newForgeBudget(max, func() time.Time { return now })
	conn := uuid.New()

	// The first max takes succeed against the initial burst.
	for i := 0; i < max; i++ {
		ok, ra := b.Take(conn)
		if !ok {
			t.Fatalf("take %d: ok=false, want true (burst not yet spent)", i)
		}
		if ra != 0 {
			t.Fatalf("take %d: retryAfter=%v, want 0 on a granted take", i, ra)
		}
	}

	// The next take sheds with a positive Retry-After (clock has not advanced).
	ok, ra := b.Take(conn)
	if ok {
		t.Fatalf("take after burst: ok=true, want false (bucket empty)")
	}
	if ra <= 0 {
		t.Fatalf("take after burst: retryAfter=%v, want > 0", ra)
	}
	// The bucket is FULLY drained (exactly max takes, clock frozen so no refill), so the
	// hint is EXACTLY the time to accrue one whole token: 60s/max = 20s. Asserting the
	// value (not just an upper bound) catches a formula regression a bound check would
	// miss — a constant 1s or a factor error would slip past ra <= 20s.
	if want := time.Duration(60/max) * time.Second; ra != want {
		t.Fatalf("retryAfter=%v, want exactly %v (time for one token at %d/min)", ra, want, max)
	}

	// Advancing by exactly one refill interval restores exactly one token: one take
	// succeeds, the next sheds again.
	now = now.Add(time.Duration(60/max) * time.Second)
	if ok, _ := b.Take(conn); !ok {
		t.Fatalf("take after +20s: ok=false, want true (one token refilled)")
	}
	if ok, _ := b.Take(conn); ok {
		t.Fatalf("second take after +20s: ok=true, want false (only one token refilled)")
	}
}

// TestForgeBudgetPerConnectionIndependent pins that two connection ids have independent
// buckets: draining one never sheds the other (D4 keys the budget by ConnectionID).
func TestForgeBudgetPerConnectionIndependent(t *testing.T) {
	now := time.Unix(0, 0)
	b := newForgeBudget(2, func() time.Time { return now })
	a, c := uuid.New(), uuid.New()

	// Drain a completely.
	for i := 0; i < 2; i++ {
		if ok, _ := b.Take(a); !ok {
			t.Fatalf("drain a take %d: ok=false, want true", i)
		}
	}
	if ok, _ := b.Take(a); ok {
		t.Fatalf("a after drain: ok=true, want false (a is empty)")
	}
	// c is untouched: its full burst is still available.
	for i := 0; i < 2; i++ {
		if ok, _ := b.Take(c); !ok {
			t.Fatalf("c take %d: ok=false, want true (c has its own bucket)", i)
		}
	}
}

// TestForgeBudgetUnlimited pins that max <= 0 (and a nil receiver) never sheds — the
// documented "unlimited" mode a struct-literal Handler / non-positive config lands in.
func TestForgeBudgetUnlimited(t *testing.T) {
	conn := uuid.New()
	for _, max := range []int{0, -1} {
		b := newForgeBudget(max, nil) // nil clock defaults to time.Now
		for i := 0; i < 1000; i++ {
			if ok, ra := b.Take(conn); !ok || ra != 0 {
				t.Fatalf("max=%d take %d: ok=%v ra=%v, want ok=true ra=0 (unlimited)", max, i, ok, ra)
			}
		}
	}
	var nilBudget *forgeBudget
	if ok, ra := nilBudget.Take(conn); !ok || ra != 0 {
		t.Fatalf("nil budget: ok=%v ra=%v, want ok=true ra=0", ok, ra)
	}
}
