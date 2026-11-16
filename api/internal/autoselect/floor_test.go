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
	got := Select([]Candidate{only}, id(1), headroomFirstPolicy(), now)
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
			got := Select(tc.cands, uuid.Nil, headroomFirstPolicy(), now)
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
	got := Select([]Candidate{cand(1, 80, at(time.Hour))}, uuid.Nil, headroomFirstPolicy(), now)
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

	if got := Select([]Candidate{stale}, uuid.Nil, headroomFirstPolicy(), now); got.Picked {
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
	unmeasured.SevenDayPct = nil // no headroom signal ⇒ Select drops it
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
	if out := Select([]Candidate{only}, id(1), headroomFirstPolicy(), now); !out.PoolNonEmpty {
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
	unmeasured.SevenDayPct = nil
	nullReset := cand(3, 78, nil)
	dated := cand(4, 77, at(time.Hour))
	dated2 := cand(5, 40, at(time.Hour)) // shares dated's reset ⇒ decided on secret id

	cands := []Candidate{stale, unmeasured, nullReset, dated, dated2}
	want, ok := Floor(cands, uuid.Nil, now)
	if !ok {
		t.Fatalf("baseline Floor found nothing")
	}
	rng := rand.New(rand.NewSource(4242)) //nolint:gosec // G404: a deterministic seed is the point — this asserts Floor is order-independent, not a security context
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

// --- #4405: Floor demotes a known-exhausted reading -------------------------------

// staleReading is a stale pooled token whose last reading had the given window
// percentages and resets. Stale, so Select cannot rank it and only Floor decides.
func staleReading(n byte, five, seven *int16, fiveReset, sevenReset *time.Time) Candidate {
	return Candidate{
		SecretID:      id(n),
		Label:         string(rune('a'+n-1)) + "-token",
		AutoEligible:  true,
		HasReading:    true,
		FiveHourPct:   five,
		SevenDayPct:   seven,
		FiveResetsAt:  fiveReset,
		SevenResetsAt: sevenReset,
		SyncedAt:      at(-39 * time.Hour),
	}
}

// TestFloorDemotesStaleExhaustedToken is the #4405 incident: A's last reading is the
// 7-day window at 100% with its reset ~19 h out (sooner than B's), B's is 67%. Both
// are stale, so Select measures nothing and the claim floors. Before the fix tieLess
// picked A (sooner reset) and the first turn hit the weekly limit.
func TestFloorDemotesStaleExhaustedToken(t *testing.T) {
	a := staleReading(1, i16(0), i16(100), at(-time.Hour), at(19*time.Hour))
	b := staleReading(2, i16(0), i16(67), at(-time.Hour), at(90*time.Hour))
	if got := Select([]Candidate{a, b}, uuid.Nil, testPolicy(), now); got.Picked {
		t.Fatalf("Select picked %v; the fixture must be unrankable (pool_stale)", got.SecretID)
	}
	for _, cands := range [][]Candidate{{a, b}, {b, a}} {
		if sid, ok := Floor(cands, uuid.Nil, now); !ok || sid != id(2) {
			t.Fatalf("Floor = (%v, %v), want B (%v, true) — A's last reading is a 7-day window at 100%% "+
				"whose reset is still ahead", sid, ok, id(2))
		}
	}
}

// TestFloorDemotesFiveHourExhaustion: a five-hour window at 100% with its own reset
// ahead demotes the token even though its 7-day window is low and its reset is the
// soonest.
func TestFloorDemotesFiveHourExhaustion(t *testing.T) {
	a := staleReading(1, i16(100), i16(10), at(30*time.Minute), at(72*time.Hour))
	b := staleReading(2, i16(50), i16(40), at(4*time.Hour), at(96*time.Hour))
	if sid, ok := Floor([]Candidate{a, b}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want (%v, true)", sid, ok, id(2))
	}
}

// TestFloorDemotesSevenDayOnlyExhaustion: the 7-day window is at 100% with its reset
// ahead while the five-hour window (also 100%, reset already past) binds resetKey.
// Exhaustion is tested per window, not through the binding-window choice.
func TestFloorDemotesSevenDayOnlyExhaustion(t *testing.T) {
	a := staleReading(1, i16(100), i16(100), at(-time.Minute), at(10*time.Hour))
	b := staleReading(2, i16(30), i16(60), at(3*time.Hour), at(50*time.Hour))
	if sid, ok := Floor([]Candidate{a, b}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want (%v, true) — a past five-hour reset does not clear a future "+
			"weekly one", sid, ok, id(2))
	}
	// Same shape with the weekly reset only a minute out: A's binding (five-hour)
	// reset is in the past, the soonest of all, so only the demotion saves B.
	a2 := staleReading(1, i16(100), i16(100), at(-time.Minute), at(time.Minute))
	b2 := staleReading(2, i16(30), i16(60), at(3*time.Hour), at(50*time.Hour))
	if sid, ok := Floor([]Candidate{a2, b2}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want (%v, true)", sid, ok, id(2))
	}
}

// TestFloorDemotesEqualPctCase: both windows at 100%, the five-hour reset already past
// (so it binds resetKey with a past instant, the soonest of all) and the weekly reset
// still ahead — the token is still exhausted.
func TestFloorDemotesEqualPctCase(t *testing.T) {
	a := staleReading(1, i16(100), i16(100), at(-2*time.Hour), at(20*time.Hour))
	b := staleReading(2, i16(10), i16(20), at(2*time.Hour), at(30*time.Hour))
	if sid, ok := Floor([]Candidate{a, b}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want (%v, true)", sid, ok, id(2))
	}
}

// TestFloorResetBoundariesNotDemoted: a window at 100% whose reset is nil (unknown),
// already past, or exactly now may have replenished, so the token is NOT demoted and
// tieLess decides as before. A's binding reset is the soonest, so A wins iff it is
// not demoted.
func TestFloorResetBoundariesNotDemoted(t *testing.T) {
	nowCopy := now
	cases := []struct {
		name  string
		reset *time.Time
	}{
		{"nil reset", nil},
		{"past reset", at(-time.Minute)},
		{"reset exactly now", &nowCopy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Both windows exhausted on the same (non-future) reset. For the nil case the
			// binding reset is +∞, so B gets a NULL reset too and the id leg decides (A=1).
			a := staleReading(1, i16(100), i16(100), tc.reset, tc.reset)
			bReset := at(5 * time.Hour)
			if tc.reset == nil {
				bReset = nil
			}
			b := staleReading(2, i16(10), i16(20), bReset, bReset)
			if knownExhausted(a, now) {
				t.Fatalf("knownExhausted = true for a 100%% window with %s", tc.name)
			}
			if sid, ok := Floor([]Candidate{b, a}, uuid.Nil, now); !ok || sid != id(1) {
				t.Fatalf("Floor = (%v, %v), want A (%v, true) — a %s does not demote", sid, ok, id(1), tc.name)
			}
		})
	}
}

// TestFloorAllExhaustedStillPicks: when every pooled token is known-exhausted Floor
// still returns one (ok=true, #754 liveness), ordered by tieLess within the group.
func TestFloorAllExhaustedStillPicks(t *testing.T) {
	a := staleReading(1, i16(0), i16(100), nil, at(40*time.Hour))
	b := staleReading(2, i16(0), i16(100), nil, at(20*time.Hour)) // soonest ⇒ tieLess winner
	c := staleReading(3, i16(100), i16(50), at(time.Hour), at(60*time.Hour))
	c.FiveHourPct, c.SevenDayPct = i16(100), i16(100)
	c.SevenResetsAt = at(30 * time.Hour) // binding is five-hour (equal pct ⇒ five), reset +1h ⇒ soonest
	sid, ok := Floor([]Candidate{a, b, c}, uuid.Nil, now)
	if !ok {
		t.Fatal("Floor ok = false over an all-exhausted pool; the last resort must still return a token")
	}
	if sid != id(3) {
		t.Fatalf("Floor = %v, want %v (the tieLess winner within the exhausted group)", sid, id(3))
	}
	if sid, ok := Floor([]Candidate{a, b}, uuid.Nil, now); !ok || sid != id(2) {
		t.Fatalf("Floor = (%v, %v), want the soonest-reset exhausted token (%v, true)", sid, ok, id(2))
	}
}

// TestFloorExcludeLeavesOnlyExhausted: excluding the one usable token leaves only an
// exhausted one, which Floor still returns.
func TestFloorExcludeLeavesOnlyExhausted(t *testing.T) {
	exhausted := staleReading(1, i16(0), i16(100), nil, at(10*time.Hour))
	usable := staleReading(2, i16(0), i16(40), nil, at(90*time.Hour))
	sid, ok := Floor([]Candidate{exhausted, usable}, id(2), now)
	if !ok || sid != id(1) {
		t.Fatalf("Floor = (%v, %v), want the remaining exhausted token (%v, true)", sid, ok, id(1))
	}
}

// TestFloorOrderIndependentAcrossGroups: a fixture mixing exhausted and non-exhausted
// tokens, with collisions inside each group, picks the same id under any permutation.
func TestFloorOrderIndependentAcrossGroups(t *testing.T) {
	shared := at(2 * time.Hour)
	cands := []Candidate{
		staleReading(1, i16(0), i16(100), nil, at(time.Hour)),                 // exhausted, soonest
		staleReading(2, i16(100), i16(10), shared, at(90*time.Hour)),          // exhausted (5h)
		staleReading(3, i16(0), i16(70), nil, shared),                         // usable
		staleReading(4, i16(0), i16(70), nil, shared),                         // usable, same reset ⇒ id leg
		staleReading(5, nil, nil, nil, nil),                                   // unmeasured
		staleReading(6, i16(100), i16(100), at(-time.Hour), at(-time.Minute)), // 100% but both resets past
	}
	want, ok := Floor(cands, uuid.Nil, now)
	if !ok {
		t.Fatal("baseline Floor found nothing")
	}
	if !knownExhausted(cands[0], now) || !knownExhausted(cands[1], now) {
		t.Fatal("fixture: tokens 1 and 2 must be known-exhausted")
	}
	if knownExhausted(pickOf(cands, want), now) {
		t.Fatalf("Floor picked known-exhausted %v while usable tokens exist", want)
	}
	rng := rand.New(rand.NewSource(4405)) //nolint:gosec // G404: deterministic seed; asserting order independence, not security
	for i := 0; i < 200; i++ {
		shuffled := append([]Candidate(nil), cands...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		got, gotOK := Floor(shuffled, uuid.Nil, now)
		if !gotOK || got != want {
			t.Fatalf("permutation %d changed Floor: (%v,%v) != (%v,true)", i, got, gotOK, want)
		}
	}
}

// pickOf returns the candidate with the given id.
func pickOf(cands []Candidate, sid uuid.UUID) Candidate {
	for _, c := range cands {
		if c.SecretID == sid {
			return c
		}
	}
	return Candidate{}
}
