package autoselect

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

// #754 M1 — the empty-vs-excluded split (Outcome.PoolNonEmpty) and the last-resort
// Floor pick, exercised against hand-built Candidate fixtures (the package is pure).

// --- Change 1: PoolNonEmpty ------------------------------------------------------

// TestSelectPoolNonEmptyWhenSoleTokenExcluded is the split this milestone exists for:
// excluding the user's ONLY pooled token still yields ReasonPoolEmpty (Picked=false,
// unchanged), but PoolNonEmpty is true — the pool is not empty, its one member is the
// credential we must not re-pick. This is the fact a later milestone reads to tell
// "pooled nothing" from "pooled one token, and it is excluded".
func TestSelectPoolNonEmptyWhenSoleTokenExcluded(t *testing.T) {
	only := cand(1, 90, at(time.Hour)) // fresh, eligible, about to be excluded
	got := Select([]Candidate{only}, id(1), testPolicy(), now)
	if got.Picked {
		t.Fatalf("excluding the only pooled token still picked %v", got.SecretID)
	}
	if got.Reason != ReasonPoolEmpty {
		t.Fatalf("reason = %q, want %q — the pooled variable is set after the exclude skip, so this "+
			"stays ReasonPoolEmpty", got.Reason, ReasonPoolEmpty)
	}
	if !got.PoolNonEmpty {
		t.Fatalf("PoolNonEmpty = false, want true — an AutoEligible token exists even though it is " +
			"excluded; PoolNonEmpty must be ORed BEFORE the exclude skip")
	}
}

// TestSelectPoolNonEmptyFalseWhenNothingPooled: a genuinely empty pool (no AutoEligible
// candidate at all) reports PoolNonEmpty=false. This is the case that must stay
// distinguishable from the excluded-sole-token case above.
func TestSelectPoolNonEmptyFalseWhenNothingPooled(t *testing.T) {
	unpooled := cand(1, 80, at(time.Hour))
	unpooled.AutoEligible = false
	for _, tc := range []struct {
		name  string
		cands []Candidate
	}{
		{"no candidates at all", nil},
		{"a candidate that is not pooled", []Candidate{unpooled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.cands, uuid.Nil, testPolicy(), now)
			if got.Reason != ReasonPoolEmpty {
				t.Fatalf("reason = %q, want %q", got.Reason, ReasonPoolEmpty)
			}
			if got.PoolNonEmpty {
				t.Fatalf("PoolNonEmpty = true, want false — nothing here is AutoEligible")
			}
		})
	}
}

// TestSelectPoolNonEmptyOnSuccessfulPick: an ordinary pick reports PoolNonEmpty=true,
// so the field is populated on the Picked return too, not only the fallback returns.
func TestSelectPoolNonEmptyOnSuccessfulPick(t *testing.T) {
	got := Select([]Candidate{cand(1, 80, at(time.Hour))}, uuid.Nil, testPolicy(), now)
	if !got.Picked {
		t.Fatalf("picked nothing (reason %q); the fixture is a healthy eligible token", got.Reason)
	}
	if !got.PoolNonEmpty {
		t.Fatalf("PoolNonEmpty = false, want true on a successful pick")
	}
}

// --- Change 2: Floor -------------------------------------------------------------

// TestFloorReturnsSoleStaleToken: where Select returns nothing because the only token
// is stale (ReasonPoolStale), Floor still returns it — that is the whole point of a
// last resort, which ranks the pooled set regardless of measurability.
func TestFloorReturnsSoleStaleToken(t *testing.T) {
	stale := cand(1, 80, at(time.Hour))
	stale.SyncedAt = at(-99 * time.Hour) // aged out ⇒ Select cannot pick it

	if got := Select([]Candidate{stale}, uuid.Nil, testPolicy(), now); got.Picked {
		t.Fatalf("Select picked %v; the fixture is meant to be unrankable", got.SecretID)
	}
	sid, ok := Floor([]Candidate{stale}, uuid.Nil, now)
	if !ok || sid != id(1) {
		t.Fatalf("Floor = (%v, %v), want (%v, true) — a stale pooled token is still a valid last resort",
			sid, ok, id(1))
	}
}

// TestFloorReturnsUnmeasuredToken: Floor includes a token whose window is NULL
// (unmeasured), which Select excludes. resetKey returns +∞ for it, so it is chosen on
// the secret-id leg rather than dereferencing a nil pct.
func TestFloorReturnsUnmeasuredToken(t *testing.T) {
	unmeasured := cand(1, 80, at(time.Hour))
	unmeasured.FiveHourPct = nil // no headroom signal ⇒ Select drops it
	sid, ok := Floor([]Candidate{unmeasured}, uuid.Nil, now)
	if !ok || sid != id(1) {
		t.Fatalf("Floor = (%v, %v), want (%v, true)", sid, ok, id(1))
	}
}

// TestFloorHonoursExclude: Floor skips the excluded id exactly as Select does, handing
// the pick to the surviving pooled token.
func TestFloorHonoursExclude(t *testing.T) {
	a := cand(1, 90, at(time.Minute)) // soonest reset ⇒ would win without exclusion
	b := cand(2, 80, at(time.Hour))

	// Baseline: with nothing excluded the soonest-reset token wins.
	if sid, ok := Floor([]Candidate{a, b}, uuid.Nil, now); !ok || sid != id(1) {
		t.Fatalf("baseline Floor = (%v, %v), want (%v, true)", sid, ok, id(1))
	}
	sid, ok := Floor([]Candidate{a, b}, id(1), now)
	if !ok || sid != id(2) {
		t.Fatalf("with %v excluded Floor = (%v, %v), want the survivor (%v, true)", id(1), sid, ok, id(2))
	}
}

// TestFloorEmptyPool: ok is false when no pooled AutoEligible candidate remains. Both
// the no-candidate and the none-eligible cases, plus the excluded-sole-token case —
// which is exactly where Floor.ok and Select.PoolNonEmpty DIVERGE (PoolNonEmpty is
// counted before the exclude skip, Floor.ok after), so a caller must read Floor's ok
// directly rather than infer it from PoolNonEmpty.
func TestFloorEmptyPool(t *testing.T) {
	unpooled := cand(1, 80, at(time.Hour))
	unpooled.AutoEligible = false

	if _, ok := Floor(nil, uuid.Nil, now); ok {
		t.Fatalf("Floor(nil) ok = true, want false")
	}
	if _, ok := Floor([]Candidate{unpooled}, uuid.Nil, now); ok {
		t.Fatalf("Floor over a non-pooled candidate ok = true, want false")
	}

	// The excluded-sole-token case: Floor.ok and Select.PoolNonEmpty DIVERGE here.
	// The pool has one AutoEligible member, so Select.PoolNonEmpty=true; Floor honours
	// the exclude and drops that member, so Floor.ok=false. This is the intended
	// distinct-signal design — PoolNonEmpty says "the user pooled something", Floor.ok
	// says "there is a token I may spend right now".
	only := cand(1, 90, at(time.Hour))
	if _, ok := Floor([]Candidate{only}, id(1), now); ok {
		t.Fatalf("Floor over the excluded sole token ok = true, want false")
	}
	if out := Select([]Candidate{only}, id(1), testPolicy(), now); !out.PoolNonEmpty {
		t.Fatalf("Select PoolNonEmpty = false, want true — the pool has one member, it is just excluded")
	}
}

// TestFloorOrderIndependent: Floor over any permutation of a mixed fixture returns the
// same id. The fixture is full of collisions — a stale token, an unmeasured token, a
// NULL reset, equal resets — so a wrong tie-break has somewhere to be non-deterministic.
func TestFloorOrderIndependent(t *testing.T) {
	stale := cand(1, 80, at(time.Hour))
	stale.SyncedAt = at(-99 * time.Hour)
	unmeasured := cand(2, 80, at(time.Hour))
	unmeasured.FiveHourPct = nil
	nullReset := cand(3, 78, nil)
	dated := cand(4, 77, at(time.Hour))
	dated2 := cand(5, 40, at(time.Hour)) // shares dated's reset ⇒ decided on secret id

	cands := []Candidate{stale, unmeasured, nullReset, dated, dated2}
	want, ok := Floor(cands, uuid.Nil, now)
	if !ok {
		t.Fatalf("baseline Floor found nothing")
	}
	rng := rand.New(rand.NewSource(4242))
	for i := 0; i < 200; i++ {
		shuffled := append([]Candidate(nil), cands...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, gotOK := Floor(shuffled, uuid.Nil, now)
		if !gotOK || got != want {
			t.Fatalf("permutation %d changed Floor: (%v,%v) != (%v,true)", i, got, gotOK, want)
		}
	}
}

// TestFloorTieBreakSoonestResetThenID: two stale pooled tokens with different reset
// windows → Floor returns the soonest-reset one; with equal windows → the lower id.
func TestFloorTieBreakSoonestResetThenID(t *testing.T) {
	late := cand(1, 80, at(2*time.Hour))
	late.SyncedAt = at(-99 * time.Hour)
	soon := cand(2, 80, at(time.Minute))
	soon.SyncedAt = at(-99 * time.Hour)
	if sid, ok := Floor([]Candidate{late, soon}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want the soonest-reset token (%v, true)", sid, ok, id(2))
	}

	shared := at(time.Hour)
	hi := cand(9, 80, shared)
	hi.SyncedAt = at(-99 * time.Hour)
	lo := cand(2, 80, shared)
	lo.SyncedAt = at(-99 * time.Hour)
	if sid, ok := Floor([]Candidate{hi, lo}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("on equal resets Floor = (%v, %v), want the lower id (%v, true)", sid, ok, id(2))
	}
}
