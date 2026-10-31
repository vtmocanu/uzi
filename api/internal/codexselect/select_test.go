package codexselect

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
)

func id(n byte) uuid.UUID { return uuid.UUID{15: n} }

var policy = autoselect.Policy{MinHeadroom: 10, HeadroomTiePct: 5, MaxStaleness: 0, InflightPenalty: 5}

func normal(n byte, score float64) Candidate {
	return Candidate{AccountKey: id(n), AliasID: id(100 + n), Structural: true, Score: score, ScoreKnown: true}
}

func unknownScore(n byte) Candidate {
	return Candidate{AccountKey: id(n), AliasID: id(100 + n), Structural: true}
}

func exhausted(n byte, release *time.Time) Candidate {
	return Candidate{AccountKey: id(n), AliasID: id(100 + n), Structural: true, CapacityExcluded: true,
		ReleaseAt: release, ReleaseKnown: release != nil}
}

func mustSelect(t *testing.T, cands []Candidate) Outcome {
	t.Helper()
	out, err := Select(cands, policy, t0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSelectAutoPicksMostHeadroom(t *testing.T) {
	out := mustSelect(t, []Candidate{normal(1, 40), normal(2, 80), normal(3, 20)})
	if !out.Picked || out.AccountKey != id(2) || out.AliasID != id(102) || out.Reason != ReasonAuto || out.Headroom != 80 {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectHeadroomIsFloorOfScore(t *testing.T) {
	out := mustSelect(t, []Candidate{normal(1, 80.4)})
	if out.Headroom != 80 {
		t.Fatalf("headroom=%d want 80", out.Headroom)
	}
}

func TestSelectBestOfPoolWhenAllBelowMin(t *testing.T) {
	out := mustSelect(t, []Candidate{normal(1, 2), normal(2, 9), normal(3, 3)})
	if out.AccountKey != id(2) || out.Reason != ReasonBestOfPool || out.Headroom != 9 {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectClampedScoreStillPicks(t *testing.T) {
	out := mustSelect(t, []Candidate{normal(1, 0), normal(2, 0)})
	if !out.Picked || out.Reason != ReasonBestOfPool || out.AccountKey != id(1) {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectUnknownScoreIsNotRanked(t *testing.T) {
	out := mustSelect(t, []Candidate{unknownScore(1), normal(2, 15)})
	if out.AccountKey != id(2) || out.Reason != ReasonAuto {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectPoolStale(t *testing.T) {
	out := mustSelect(t, []Candidate{unknownScore(7), unknownScore(3), unknownScore(5)})
	if !out.Picked || out.AccountKey != id(3) || out.AliasID != id(103) || out.Reason != ReasonPoolStale {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectInFlightPenaltyShiftsPick(t *testing.T) {
	a, b := normal(1, 80), normal(2, 70)
	if out := mustSelect(t, []Candidate{a, b}); out.AccountKey != id(1) {
		t.Fatalf("without load got %+v", out)
	}
	a.InFlight = 4 // 80 - 20 = 60 < 70, outside the 5-point tie window
	if out := mustSelect(t, []Candidate{a, b}); out.AccountKey != id(2) {
		t.Fatalf("with load got %+v", out)
	}
}

func TestSelectTieBreaksOnSoonerResetThenKey(t *testing.T) {
	early, late := t0.Add(time.Hour), t0.Add(2*time.Hour)
	a, b := normal(1, 50), normal(2, 52)
	a.ResetAt, b.ResetAt = &late, &early
	if out := mustSelect(t, []Candidate{a, b}); out.AccountKey != id(2) {
		t.Fatalf("sooner reset should win inside the tie window: %+v", out)
	}
	a.ResetAt, b.ResetAt = nil, nil
	if out := mustSelect(t, []Candidate{b, a}); out.AccountKey != id(1) {
		t.Fatalf("lowest key should win exact tie: %+v", out)
	}
}

func TestSelectImmuneToPolicyMaxStaleness(t *testing.T) {
	for _, ms := range []time.Duration{0, -1, time.Nanosecond, time.Hour} {
		p := policy
		p.MaxStaleness = ms
		out, err := Select([]Candidate{normal(1, 60)}, p, t0)
		if err != nil || out.Reason != ReasonAuto {
			t.Fatalf("MaxStaleness=%v got %+v err=%v", ms, out, err)
		}
	}
}

func TestSelectPoolExhaustedOrdering(t *testing.T) {
	soon, later := t0.Add(time.Minute), t0.Add(time.Hour)
	out := mustSelect(t, []Candidate{exhausted(1, &later), exhausted(2, nil), exhausted(3, &soon)})
	if out.AccountKey != id(3) || out.Reason != ReasonPoolExhausted {
		t.Fatalf("earliest known release should win: %+v", out)
	}
	out = mustSelect(t, []Candidate{exhausted(4, nil), exhausted(2, nil)})
	if out.AccountKey != id(2) {
		t.Fatalf("all unknown: lowest key should win: %+v", out)
	}
	out = mustSelect(t, []Candidate{exhausted(5, &soon), exhausted(4, &soon)})
	if out.AccountKey != id(4) {
		t.Fatalf("equal release: lowest key should win: %+v", out)
	}
	// ReleaseKnown=false with a time set (some cause unknown) sorts last.
	partial := exhausted(1, &soon)
	partial.ReleaseKnown = false
	out = mustSelect(t, []Candidate{partial, exhausted(9, &later)})
	if out.AccountKey != id(9) {
		t.Fatalf("partially unknown release must sort last: %+v", out)
	}
}

func TestSelectCapacityExcludedOnlyViaPoolExhausted(t *testing.T) {
	cx := normal(1, 99)
	cx.CapacityExcluded = true
	out := mustSelect(t, []Candidate{cx, normal(2, 5)})
	if out.AccountKey != id(2) || out.Reason == ReasonPoolExhausted {
		t.Fatalf("a normal account must beat a capacity-excluded one: %+v", out)
	}
	out = mustSelect(t, []Candidate{cx})
	if out.AccountKey != id(1) || out.Reason != ReasonPoolExhausted {
		t.Fatalf("got %+v", out)
	}
}

func TestSelectStructuralNeverPicked(t *testing.T) {
	bad := normal(1, 100)
	bad.Structural = false
	out := mustSelect(t, []Candidate{bad, normal(2, 1)})
	if out.AccountKey != id(2) {
		t.Fatalf("got %+v", out)
	}
	badEx := exhausted(1, nil)
	badEx.Structural = false
	if out := mustSelect(t, []Candidate{bad, badEx}); out.Picked {
		t.Fatalf("no structural account must not pick: %+v", out)
	}
	if out := mustSelect(t, nil); out.Picked {
		t.Fatalf("empty must not pick: %+v", out)
	}
	nilKey := normal(1, 90)
	nilKey.AccountKey = uuid.Nil
	if out := mustSelect(t, []Candidate{nilKey}); out.Picked {
		t.Fatalf("uuid.Nil account key must be skipped: %+v", out)
	}
}

// permutations calls fn with every ordering of set (Heap's algorithm), so the order-independence
// check is exhaustive and needs no random source.
func permutations(set []Candidate, fn func([]Candidate)) {
	a := append([]Candidate(nil), set...)
	var gen func(k int)
	gen = func(k int) {
		if k <= 1 {
			fn(a)
			return
		}
		for i := 0; i < k; i++ {
			gen(k - 1)
			if k%2 == 0 {
				a[i], a[k-1] = a[k-1], a[i]
			} else {
				a[0], a[k-1] = a[k-1], a[0]
			}
		}
	}
	gen(len(a))
}

func TestSelectDeterministicUnderShuffle(t *testing.T) {
	soon := t0.Add(time.Minute)
	dupLow, dupHigh := exhausted(4, &soon), exhausted(4, &soon)
	dupLow.AliasID, dupHigh.AliasID = id(140), id(150)
	sets := [][]Candidate{
		{normal(1, 50), normal(2, 50), normal(3, 52), normal(4, 20), unknownScore(5)},
		{unknownScore(9), unknownScore(4), unknownScore(6)},
		{exhausted(3, nil), exhausted(1, nil), exhausted(2, nil)},
		{normal(1, 2), normal(2, 2), normal(3, 7)},
		// Duplicate account key in pool_exhausted: the lowest alias id wins in every order.
		{dupHigh, dupLow, exhausted(5, &soon)},
	}
	for i, set := range sets {
		want := mustSelect(t, set)
		permutations(set, func(p []Candidate) {
			if got := mustSelect(t, p); got != want {
				t.Fatalf("set %d: permutation %+v != %+v", i, got, want)
			}
		})
	}
	if out := mustSelect(t, sets[4]); out.AccountKey != id(4) || out.AliasID != id(140) {
		t.Fatalf("duplicate key should resolve to the lowest alias id: %+v", out)
	}
}
