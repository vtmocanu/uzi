package autoselect

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

// resetFirstPolicy is testPolicy pinned to the #3844 ordering explicitly, so these
// tests do not silently depend on the zero-value default (which one test checks).
func resetFirstPolicy() Policy {
	p := testPolicy()
	p.Order = OrderResetFirst
	return p
}

// weekly is cand with the reset_first ranking key set. cand's own FiveResetsAt is the
// binding reset and is deliberately left nil here: reset_first must not read it.
func weekly(n byte, headroom int, reset *time.Time) Candidate {
	c := cand(n, headroom, nil)
	c.WeeklyResetsAt = reset
	return c
}

func pick(t *testing.T, cands []Candidate, p Policy) Outcome {
	t.Helper()
	got := Select(cands, uuid.Nil, p, now)
	if !got.Picked {
		t.Fatalf("picked nothing (reason %q)", got.Reason)
	}
	return got
}

// The soonest weekly reset wins even with far less headroom: the point of the order
// is spending allowance about to expire, not preserving the emptiest token.
func TestResetFirstSoonestWeeklyResetWinsOverHeadroom(t *testing.T) {
	got := pick(t, []Candidate{
		weekly(1, 90, at(48*time.Hour)),
		weekly(2, 30, at(2*time.Hour)),
		weekly(3, 60, at(24*time.Hour)),
	}, resetFirstPolicy())
	if got.SecretID != id(2) || got.Reason != ReasonAuto {
		t.Fatalf("picked %v (reason %q), want %v auto", got.SecretID, got.Reason, id(2))
	}
	if got.Headroom != 30 {
		t.Fatalf("Headroom = %d, want the raw 30", got.Headroom)
	}
}

// A below-floor token with the soonest reset is not eligible and so not considered.
func TestResetFirstBelowFloorIsSkipped(t *testing.T) {
	got := pick(t, []Candidate{
		weekly(1, 10, at(time.Hour)), // headroom 10 < MinHeadroom 15
		weekly(2, 50, at(72*time.Hour)),
	}, resetFirstPolicy())
	if got.SecretID != id(2) || got.Reason != ReasonAuto {
		t.Fatalf("picked %v (reason %q), want %v auto", got.SecretID, got.Reason, id(2))
	}
}

// nil and already-passed (or exactly-now) weekly resets sort after every known future one.
func TestResetFirstUnknownAndPastResetsSortLast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset *time.Time
	}{
		{"nil", nil},
		{"past", at(-time.Hour)},
		{"exactly now", at(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pick(t, []Candidate{
				weekly(1, 95, tc.reset), // best headroom and lowest id, but no usable reset
				weekly(2, 20, at(150*time.Hour)),
			}, resetFirstPolicy())
			if got.SecretID != id(2) {
				t.Fatalf("picked %v, want the token with a known future reset %v", got.SecretID, id(2))
			}
		})
	}
	// Among only unknown/past candidates, the order falls through to headroom, then id.
	got := pick(t, []Candidate{
		weekly(1, 40, nil),
		weekly(2, 70, at(-time.Hour)),
	}, resetFirstPolicy())
	if got.SecretID != id(2) {
		t.Fatalf("all-unknown picked %v, want the higher-headroom %v", got.SecretID, id(2))
	}
}

// An exact reset tie goes to the higher penalised headroom, then to the lowest id.
func TestResetFirstExactTieBreaksOnRankThenID(t *testing.T) {
	same := at(5 * time.Hour)
	got := pick(t, []Candidate{
		weekly(1, 50, same),
		weekly(2, 70, same),
		weekly(3, 70, same),
	}, resetFirstPolicy())
	if got.SecretID != id(2) {
		t.Fatalf("picked %v, want %v (highest headroom, lowest id)", got.SecretID, id(2))
	}

	// The in-flight penalty only matters inside an exact tie: 70 - 3*10 = 40 < 50.
	busy := weekly(2, 70, same)
	busy.InFlight = 10
	got = pick(t, []Candidate{weekly(1, 50, same), busy}, resetFirstPolicy())
	if got.SecretID != id(1) || got.Ranked != 50 || got.Headroom != 50 {
		t.Fatalf("picked %v ranked %d, want %v 50: the busy token's penalised rank is 40", got.SecretID, got.Ranked, id(1))
	}
}

// In-flight runs on the soonest-reset token do not move the pick off it.
func TestResetFirstInflightDoesNotMovePickAcrossResets(t *testing.T) {
	soonest := weekly(1, 40, at(time.Hour))
	soonest.InFlight = 9
	got := pick(t, []Candidate{soonest, weekly(2, 90, at(2*time.Hour))}, resetFirstPolicy())
	if got.SecretID != id(1) {
		t.Fatalf("picked %v, want the soonest-reset token %v despite 9 in-flight runs", got.SecretID, id(1))
	}
	if got.Headroom != 40 || got.Ranked != 40-3*9 {
		t.Fatalf("Headroom/Ranked = %d/%d, want raw 40 and penalised %d", got.Headroom, got.Ranked, 40-3*9)
	}
}

func TestResetFirstOrderIndependent(t *testing.T) {
	cands := []Candidate{
		weekly(1, 80, at(time.Hour)),
		weekly(2, 85, at(time.Hour)),
		weekly(3, 85, at(time.Hour)),
		weekly(4, 99, nil),
		weekly(5, 90, at(-time.Minute)),
		weekly(6, 20, at(30*time.Minute)),
		weekly(7, 5, at(time.Minute)), // below the floor
	}
	want := pick(t, cands, resetFirstPolicy())
	if want.SecretID != id(6) {
		t.Fatalf("baseline picked %v, want %v", want.SecretID, id(6))
	}
	rng := rand.New(rand.NewSource(3844)) //nolint:gosec // G404: deterministic seed; this asserts order independence, not security
	for i := 0; i < 200; i++ {
		shuffled := append([]Candidate(nil), cands...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Select(shuffled, uuid.Nil, resetFirstPolicy(), now); got != want {
			t.Fatalf("permutation %d changed the outcome: %+v != %+v", i, got, want)
		}
	}
}

// The zero-value Order and any unknown value mean reset_first; only the exact
// headroom_first constant selects the old path.
func TestResetFirstIsTheDefaultOrder(t *testing.T) {
	cands := []Candidate{weekly(1, 90, at(48*time.Hour)), weekly(2, 30, at(time.Hour))}
	for _, o := range []Order{"", OrderResetFirst, "bogus"} {
		p := testPolicy()
		p.Order = o
		if got := pick(t, cands, p); got.SecretID != id(2) {
			t.Fatalf("Order %q picked %v, want the sooner reset %v", o, got.SecretID, id(2))
		}
	}
	if got := pick(t, cands, headroomFirstPolicy()); got.SecretID != id(1) {
		t.Fatalf("headroom_first picked %v, want the 90-headroom token %v", got.SecretID, id(1))
	}
}

// best_of_pool, pool_empty, pool_stale, exclusion and the floor are order-independent:
// both orders return the same outcome.
func TestResetFirstFallbacksMatchHeadroomFirst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cands   []Candidate
		exclude uuid.UUID
	}{
		{"best_of_pool", []Candidate{weekly(1, 10, at(time.Hour)), weekly(2, 12, at(72*time.Hour))}, uuid.Nil},
		{"pool_empty", nil, uuid.Nil},
		{"sole excluded", []Candidate{weekly(1, 80, at(time.Hour))}, id(1)},
		{"exclusion skips the soonest", []Candidate{weekly(1, 80, at(time.Hour)), weekly(2, 60, at(2*time.Hour)), weekly(3, 90, at(3*time.Hour))}, id(1)},
		{"stale", []Candidate{func() Candidate { c := weekly(1, 80, at(time.Hour)); c.SyncedAt = at(-time.Hour); return c }()}, uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := Select(tc.cands, tc.exclude, resetFirstPolicy(), now)
			hf := Select(tc.cands, tc.exclude, headroomFirstPolicy(), now)
			if tc.name == "exclusion skips the soonest" {
				// An auto pick, so the orders differ by design: reset_first takes the
				// soonest survivor, headroom_first the emptiest.
				if rf.SecretID != id(2) || hf.SecretID != id(3) {
					t.Fatalf("reset_first %v / headroom_first %v, want %v / %v", rf.SecretID, hf.SecretID, id(2), id(3))
				}
				return
			}
			if rf != hf {
				t.Fatalf("outcomes differ: reset_first %+v, headroom_first %+v", rf, hf)
			}
		})
	}

	// Floor ignores the order entirely (it takes no Policy).
	cands := []Candidate{weekly(1, 10, nil), weekly(2, 10, nil)}
	if got, ok := Floor(cands, uuid.Nil, now); !ok || got != id(1) {
		t.Fatalf("Floor = %v %v, want %v", got, ok, id(1))
	}
}

// A reported binding 5-hour window still gates eligibility (a near-full 5h window
// puts the token below the floor) but never reorders eligible tokens: the weekly
// reset alone is the key.
func TestResetFirstFiveHourWindowGatesButDoesNotReorder(t *testing.T) {
	// Five-hour window 95% used: headroom 5 < 15, so the token is below the floor
	// even though its weekly reset is the soonest.
	full := weekly(1, 80, at(time.Hour))
	full.FiveHourPct = i16(95)
	full.FiveResetsAt = at(30 * time.Minute)
	got := pick(t, []Candidate{full, weekly(2, 50, at(72*time.Hour))}, resetFirstPolicy())
	if got.SecretID != id(2) || got.Reason != ReasonAuto {
		t.Fatalf("picked %v (reason %q), want %v: a near-full 5h window makes #1 ineligible", got.SecretID, got.Reason, id(2))
	}

	// Both eligible: A's 5h window resets soon but its weekly reset is later, so B wins.
	a := weekly(1, 70, at(100*time.Hour))
	a.FiveResetsAt = at(time.Minute)
	b := weekly(2, 40, at(10*time.Hour))
	b.FiveResetsAt = at(50 * time.Hour)
	if got := pick(t, []Candidate{a, b}, resetFirstPolicy()); got.SecretID != id(2) {
		t.Fatalf("picked %v, want %v: the 5h reset must not reorder", got.SecretID, id(2))
	}
}
