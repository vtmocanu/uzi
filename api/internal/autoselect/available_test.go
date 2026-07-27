package autoselect

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// dead is the credential that just exhausted its window. Every case below excludes
// it, because the whole question NextAvailable answers is about the OTHERS.
//
// It is deliberately built as a perfectly ELIGIBLE candidate: a gauge polled minutes
// before the death still reads healthy, which is exactly the reading that would make
// a missing exclusion promote the run straight back into the exhausted window. A
// fixture whose dead credential looked unusable would pass against an implementation
// that never excludes anything.
func dead() Candidate { return cand(9, 90, at(5*time.Hour)) }

func deadID() uuid.UUID { return id(9) }

// belowThreshold is measurable, fresh and pooled, but under MinHeadroom (15): it
// contributes its BINDING-window reset rather than `now`.
//
// five and seven are set explicitly rather than through cand(), because which of
// them binds is the whole point of the D22 case below and cand() pins seven to 0.
func belowThreshold(n byte, fivePct, sevenPct int16, five, seven *time.Time) Candidate {
	return Candidate{
		SecretID:      id(n),
		Label:         "low",
		AutoEligible:  true,
		HasReading:    true,
		FiveHourPct:   i16(fivePct),
		SevenDayPct:   i16(sevenPct),
		FiveResetsAt:  five,
		SevenResetsAt: seven,
		SyncedAt:      at(-time.Minute),
	}
}

// --- the degenerate cases, which are the ones that must not regress -------------

// TestNextAvailableSingleTokenUserDoesNotRegress is the property that makes
// Decision 6e safe to ship: a user with exactly one credential — the one that just
// died — gets no pool leg at all, so the park stamp stays the dead credential's own
// cross-checked reset, which is today's behaviour exactly.
func TestNextAvailableSingleTokenUserDoesNotRegress(t *testing.T) {
	if _, ok := NextAvailable([]Candidate{dead()}, deadID(), testPolicy(), now); ok {
		t.Fatal("the dead credential voted for its own replacement; with one token in the " +
			"pool there is nothing else to spend and the caller must keep its base")
	}
}

// TestNextAvailableNilExclusionRefuses covers a pre-PRD-#111 run, or one whose
// credential recording failed: runs.anthropic_secret_id is NULL, so there is no id
// to exclude.
//
// The refusal lives inside the function rather than in the caller because the
// failure it prevents is severe and silent: without the exclusion, leg 1 fires on the
// dead credential's own stale-but-eligible reading and the run promotes instantly
// into the window it just exhausted.
func TestNextAvailableNilExclusionRefuses(t *testing.T) {
	// The pool here is ONE eligible candidate that would otherwise contribute `now`.
	if got, ok := NextAvailable([]Candidate{cand(1, 90, at(time.Hour))}, uuid.Nil, testPolicy(), now); ok {
		t.Fatalf("a nil exclusion yielded %v; with no id to exclude, the dead credential's own "+
			"reading is indistinguishable from an alternative's and the run would promote "+
			"straight back into the exhausted window", got)
	}
}

func TestNextAvailableEmptyPool(t *testing.T) {
	if _, ok := NextAvailable(nil, deadID(), testPolicy(), now); ok {
		t.Fatal("a nil candidate slice contributed something")
	}
}

// --- the three contributing classes ---------------------------------------------

// TestNextAvailableEligibleContributesNow is leg 1: a second credential with
// headroom means the run can resume on the very next sweeper tick, regardless of
// when the dead credential's window reopens.
func TestNextAvailableEligibleContributesNow(t *testing.T) {
	got, ok := NextAvailable([]Candidate{
		dead(),
		cand(1, 90, at(99*time.Hour)), // eligible; its own reset is days out and irrelevant
	}, deadID(), testPolicy(), now)
	if !ok {
		t.Fatal("an eligible alternative contributed nothing")
	}
	if !got.Equal(now) {
		t.Fatalf("floor = %v, want %v — an ELIGIBLE token is spendable right now, so its own "+
			"reset must not enter the answer", got, now)
	}
}

// TestNextAvailableBelowThresholdContributesItsBindingReset is leg 2. A
// below-threshold token is measurably low, not unusable (D10's best-of-pool picks
// exactly these), so the moment its binding window replenishes is a real lower bound.
func TestNextAvailableBelowThresholdContributesItsBindingReset(t *testing.T) {
	// five=95 binds (headroom 5 < MinHeadroom 15), and its reset is 2h out.
	want := now.Add(2 * time.Hour)
	got, ok := NextAvailable([]Candidate{
		dead(),
		belowThreshold(1, 95, 10, at(2*time.Hour), at(70*time.Hour)),
	}, deadID(), testPolicy(), now)
	if !ok {
		t.Fatal("a below-threshold alternative contributed nothing")
	}
	if !got.Equal(want) {
		t.Fatalf("floor = %v, want %v", got, want)
	}
}

// 🔴 TestNextAvailableUsesTheBindingWindowNotTheEarlier is the case that proves the
// rule is D22's and not the plausible-looking min(five, seven).
//
// The fixture is built so the two readings DISAGREE, which is the only kind worth
// writing: seven=95 binds (headroom 5, against the five-hour window's 40), and the
// seven-day reset is 70h out while the five-hour one is 2h out. min(five, seven)
// answers 2h; the binding-window rule answers 70h.
//
// And the SWAP is the control: exchanging the two percentages (five=95, seven=60)
// moves the binding window to the five-hour one and the answer to 2h, from the same
// two reset timestamps. An implementation returning "the earlier reset" gives 2h for
// BOTH halves and fails the first; one returning "the later reset" gives 70h for both
// and fails the second. Only the binding-window rule passes the pair.
func TestNextAvailableUsesTheBindingWindowNotTheEarlier(t *testing.T) {
	five, seven := at(2*time.Hour), at(70*time.Hour)

	t.Run("seven-day binds", func(t *testing.T) {
		got, ok := NextAvailable([]Candidate{
			dead(),
			belowThreshold(1, 60, 95, five, seven),
		}, deadID(), testPolicy(), now)
		if !ok {
			t.Fatal("contributed nothing")
		}
		if !got.Equal(*seven) {
			t.Fatalf("floor = %v, want the SEVEN-day reset %v. seven=95 is the binding window "+
				"(headroom 5, vs the five-hour window's 40), and only the binding window's "+
				"replenishment raises headroom — the five-hour reset 2h out says nothing about "+
				"when this token becomes usable", got, *seven)
		}
	})

	t.Run("five-hour binds after the swap", func(t *testing.T) {
		got, ok := NextAvailable([]Candidate{
			dead(),
			belowThreshold(1, 95, 60, five, seven),
		}, deadID(), testPolicy(), now)
		if !ok {
			t.Fatal("contributed nothing")
		}
		if !got.Equal(*five) {
			t.Fatalf("floor = %v, want the FIVE-hour reset %v. Swapping the two percentages "+
				"moves the binding window, and the answer must move with it — this half is what "+
				"separates the binding-window rule from 'always the later reset'", got, *five)
		}
	})
}

// TestNextAvailableUnknownsContributeNothing is the load-bearing asymmetry, and each
// sub-case is a deployment that really exists.
//
// If an unknown contributed `now`, the poller-disabled case would promote every
// parked run instantly and thrash it back into the exhausted window. If it pushed the
// floor out, one never-polled token would delay every park for a user with a
// perfectly good second credential. Contributing nothing is the only reading under
// which a gauge-less deployment behaves exactly as it does today.
func TestNextAvailableUnknownsContributeNothing(t *testing.T) {
	alt := cand(1, 90, at(time.Hour)) // eligible in every case except the one broken below

	notPooled := alt
	notPooled.AutoEligible = false

	noReading := alt
	noReading.HasReading = false
	noReading.SyncedAt = nil

	unmeasured := alt
	unmeasured.SevenDayPct = nil

	stale := alt
	stale.SyncedAt = at(-time.Hour) // MaxStaleness is 15m

	for name, c := range map[string]Candidate{
		"not_pooled": notPooled,
		"no_reading": noReading,
		"unmeasured": unmeasured,
		"stale":      stale,
	} {
		if got, ok := NextAvailable([]Candidate{dead(), c}, deadID(), testPolicy(), now); ok {
			t.Fatalf("%s contributed %v; an UNKNOWN must neither pull the floor to now nor push "+
				"it out — %q is a statement about our data, not about the user's capacity",
				name, got, name)
		}
	}
}

// TestNextAvailablePollerDisabledDoesNotRegress is the deployment
// TestNextAvailableUnknownsContributeNothing's `stale` case exists for, asserted end
// to end: MaxStaleness <= 0 is what UZI_USAGE_POLL_INTERVAL=0 produces, and it makes
// EVERY candidate stale at once rather than one at a time.
func TestNextAvailablePollerDisabledDoesNotRegress(t *testing.T) {
	p := testPolicy()
	p.MaxStaleness = 0
	pool := []Candidate{dead(), cand(1, 90, at(time.Hour)), cand(2, 80, at(2*time.Hour))}
	if got, ok := NextAvailable(pool, deadID(), p, now); ok {
		t.Fatalf("floor = %v with the poller disabled; every token classifies stale, so nothing "+
			"can contribute and the caller must fall back to the worker's reported reset — "+
			"today's behaviour exactly", got)
	}
}

// TestNextAvailableBelowThresholdWithNoResetContributesNothing: 00080 writes NULL
// when Anthropic reports no reset. A token that names no reset is not "available
// now"; it is unknown, and unknowns contribute nothing.
func TestNextAvailableBelowThresholdWithNoResetContributesNothing(t *testing.T) {
	c := belowThreshold(1, 95, 10, nil, at(70*time.Hour)) // five binds, and its reset is NULL
	if got, ok := NextAvailable([]Candidate{dead(), c}, deadID(), testPolicy(), now); ok {
		t.Fatalf("floor = %v; the BINDING window names no reset, so this token's availability "+
			"is unknown and must not be inferred from the other window", got)
	}
}

// --- the fold over a real pool ---------------------------------------------------

// TestNextAvailableTakesTheMinimumOverContributors: the answer is a floor, so a
// single eligible token beats any number of distant below-threshold resets.
func TestNextAvailableTakesTheMinimumOverContributors(t *testing.T) {
	got, ok := NextAvailable([]Candidate{
		belowThreshold(1, 99, 10, at(50*time.Hour), at(80*time.Hour)),
		dead(),
		cand(2, 90, at(99*time.Hour)), // eligible ⇒ now
		belowThreshold(3, 98, 10, at(30*time.Hour), at(80*time.Hour)),
	}, deadID(), testPolicy(), now)
	if !ok {
		t.Fatal("a pool with three contributors yielded nothing")
	}
	if !got.Equal(now) {
		t.Fatalf("floor = %v, want %v — the minimum over {now, 50h, 30h} is now", got, now)
	}
}

// TestNextAvailableMinimumAcrossBelowThresholdOnly is the same fold with leg 1 absent,
// so the min is genuinely computed rather than short-circuited by an eligible token.
func TestNextAvailableMinimumAcrossBelowThresholdOnly(t *testing.T) {
	want := now.Add(30 * time.Hour)
	got, ok := NextAvailable([]Candidate{
		belowThreshold(1, 99, 10, at(50*time.Hour), at(80*time.Hour)),
		dead(),
		belowThreshold(3, 98, 10, at(30*time.Hour), at(80*time.Hour)),
	}, deadID(), testPolicy(), now)
	if !ok {
		t.Fatal("two below-threshold contributors yielded nothing")
	}
	if !got.Equal(want) {
		t.Fatalf("floor = %v, want %v (min of 50h and 30h)", got, want)
	}
}

// TestNextAvailableIgnoresOrder: the fold must be order-independent, like Select.
// Cheap, and it is what lets the query's ORDER BY stay a readability choice.
func TestNextAvailableIgnoresOrder(t *testing.T) {
	a := belowThreshold(1, 99, 10, at(50*time.Hour), at(80*time.Hour))
	b := belowThreshold(3, 98, 10, at(30*time.Hour), at(80*time.Hour))
	first, ok1 := NextAvailable([]Candidate{a, dead(), b}, deadID(), testPolicy(), now)
	second, ok2 := NextAvailable([]Candidate{b, a, dead()}, deadID(), testPolicy(), now)
	if ok1 != ok2 || !first.Equal(second) {
		t.Fatalf("order changed the answer: %v/%v vs %v/%v", first, ok1, second, ok2)
	}
}

// TestBindingWindowResetIsResetKey pins the export to the implementation rather than
// to a restatement of it. If someone replaces the wrapper with a copy, this stays
// green — which is the point of it being a wrapper, and why the comment on it carries
// the argument that a test cannot.
func TestBindingWindowResetIsResetKey(t *testing.T) {
	for _, c := range []Candidate{
		cand(1, 90, at(time.Hour)),
		belowThreshold(2, 60, 95, at(2*time.Hour), at(70*time.Hour)),
		{SecretID: id(3)}, // unmeasured ⇒ +∞
	} {
		wantT, wantOK := resetKey(c)
		gotT, gotOK := BindingWindowReset(c)
		if gotOK != wantOK || !gotT.Equal(wantT) {
			t.Fatalf("BindingWindowReset(%v) = %v/%v, resetKey = %v/%v", c.SecretID, gotT, gotOK, wantT, wantOK)
		}
	}
}
