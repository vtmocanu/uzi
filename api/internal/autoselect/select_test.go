package autoselect

import (
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"
)

// id builds a deterministic uuid whose byte order is the DIGIT order, so a test can
// say "the lowest id" and mean something a reader can check by eye. compareUUID
// orders by raw bytes and these differ in byte 0.
func id(n byte) uuid.UUID {
	var u uuid.UUID
	u[0] = n
	return u
}

// cand is a measurable, pooled candidate with an explicit headroom and binding
// window. seven=0 makes the FIVE-hour window bind (headroom = 100-five), so a case
// that does not care about D22 can set one number and read the headroom off it.
func cand(n byte, headroom int, reset *time.Time) Candidate {
	return Candidate{
		SecretID:     id(n),
		Label:        string(rune('a'+n-1)) + "-token",
		AutoEligible: true,
		HasReading:   true,
		FiveHourPct:  i16(int16(100 - headroom)),
		SevenDayPct:  i16(0),
		FiveResetsAt: reset,
		SyncedAt:     at(-time.Minute),
	}
}

// --- the anchor, and the intransitivity it exists to avoid ----------------------

// TestSelectPrefersMostHeadroom is the primary key: least consumed first, when the
// gap is wider than the tie tolerance.
//
// MUTATION THIS CATCHES: flipping `m.rank > hstar` to `<` in the anchor loop, which
// makes H* the WORST rank and hands every claim to the most-throttled token.
func TestSelectPrefersMostHeadroom(t *testing.T) {
	got := Select([]Candidate{
		cand(1, 20, at(time.Minute)), // soonest reset, but 60 points behind
		cand(2, 80, at(99*time.Hour)),
	}, testPolicy(), now)
	if !got.Picked || got.SecretID != id(2) {
		t.Fatalf("picked %v (reason %q), want the 80-headroom token %v", got.SecretID, got.Reason, id(2))
	}
	if got.Reason != ReasonAuto {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonAuto)
	}
}

// TestSelectAnchorClusterExcludesOutsiders is R1, the whole reason the ranking is
// specified rather than left to the implementer. With T=5 and A=80, B=75, C=70 the
// naive pairwise rule "within T ⇒ compare by reset" is INTRANSITIVE: A ties B and B
// ties C, but A does not tie C. Fed to a sort, the answer depends on the input's
// order and can be C.
//
// The fixture is built so the broken and the correct implementation DISAGREE, which
// is the only kind worth writing: C carries by far the soonest reset, so any
// implementation that lets it into the tie cluster picks it.
//
// MUTATION THIS CATCHES: deleting the `hstar-rank > tie` guard (cluster = the whole
// eligible set) → C wins. Measured.
func TestSelectAnchorClusterExcludesOutsiders(t *testing.T) {
	a := cand(1, 80, at(2*time.Hour))
	b := cand(2, 75, at(time.Hour))
	c := cand(3, 70, at(time.Minute)) // soonest by far — the bait

	got := Select([]Candidate{a, b, c}, testPolicy(), now) // T = 5
	if got.SecretID == c.SecretID {
		t.Fatalf("picked the 70-headroom token: it is 10 points outside a 5-point tie window, " +
			"and only an intransitive pairwise comparator lets it in")
	}
	if got.SecretID != b.SecretID {
		t.Fatalf("picked %v, want %v — the cluster is {80,75} and 75 resets sooner", got.SecretID, b.SecretID)
	}
}

// TestSelectAnchorIsOverTheFilteredSet pins that H* is computed AFTER the
// eligible/best-of-pool choice, not before. A below-threshold token is still
// MEASURED, and the in-flight penalty can push an eligible token's rank below an
// idle below-threshold one — so anchoring on the measured set would let a token the
// gate rejected set the reference and pull the tie window off the eligible tokens.
//
// MUTATION THIS CATCHES: computing hstar over `measured` instead of `set`. Measured:
// the anchor moves 10 → 14, which evicts the 6-rank token from the cluster and
// changes the pick.
func TestSelectAnchorIsOverTheFilteredSet(t *testing.T) {
	p := Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: time.Hour, InflightPenalty: 3}

	f := cand(1, 16, at(9*time.Hour))
	f.InFlight = 2 // rank 10 — the eligible maximum
	e := cand(2, 15, at(8*time.Hour))
	e.InFlight = 2 // rank 9
	e2 := cand(3, 15, at(time.Minute))
	e2.InFlight = 3 // rank 6, and the soonest reset
	g := cand(4, 14, at(time.Second))
	g.InFlight = 0 // rank 14 — below MinHeadroom, so NOT in the set, but the highest rank overall

	got := Select([]Candidate{f, e, e2, g}, p, now)
	if got.SecretID == g.SecretID {
		t.Fatalf("picked the below-threshold token outright; the gate rejected it")
	}
	if got.SecretID != e2.SecretID {
		t.Fatalf("picked %v, want %v — with H* = 10 the cluster is {10,9,6} and 6 resets soonest; "+
			"anchoring on the measured set (H* = 14) would evict it", got.SecretID, e2.SecretID)
	}
}

// TestSelectOrderIndependent is the property that makes every other assertion here
// meaningful, and the one a future `sort.Slice` over an intransitive Less would
// break. The fixture is deliberately full of collisions — equal headroom, equal
// resets, a NULL reset — so a wrong tie-break has somewhere to be non-deterministic.
//
// MUTATION THIS CATCHES: dropping the secret-id leg of tieLess (return false), which
// makes two identically-reset tokens resolve to whichever the input listed first.
func TestSelectOrderIndependent(t *testing.T) {
	cands := []Candidate{
		cand(1, 80, at(time.Hour)),
		cand(2, 80, at(time.Hour)), // identical to #1 except its id
		cand(3, 78, nil),           // NULL reset: +∞
		cand(4, 77, at(time.Hour)),
		cand(5, 40, at(time.Minute)), // outside the cluster
	}
	want := Select(cands, testPolicy(), now)
	if !want.Picked {
		t.Fatalf("baseline picked nothing (reason %q)", want.Reason)
	}
	rng := rand.New(rand.NewSource(1111))
	for i := 0; i < 200; i++ {
		shuffled := append([]Candidate(nil), cands...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Select(shuffled, testPolicy(), now); got != want {
			t.Fatalf("permutation %d changed the outcome: %+v != %+v", i, got, want)
		}
	}
}

// --- D22: the tie-break reads the BINDING window's reset ------------------------

// TestSelectTieBreakUsesBindingWindowReset is D22, and it is the ONLY place in this
// package where the two windows are distinguishable at all: headroom is
// min(100-five, 100-seven), min is symmetric, and so is Classify's NULL gate — so a
// five/seven swap is invisible everywhere else BY CONSTRUCTION. A fixture where both
// windows share a reset, or where only one is populated, passes against a correct
// and a broken ranker alike.
//
// Both tokens have headroom 10 (so they cluster) and their windows disagree about
// which one binds:
//
//	A: 90% five-hour / 40% seven-day → five-hour binds → its reset is +10h
//	B: 50% five-hour / 90% seven-day → seven-day binds → its reset is  +5h
//
// B replenishes sooner in the window that actually gates it, so B wins.
//
// MUTATIONS THIS CATCHES, all measured:
//   - resetKey always returns FiveResetsAt        → A (10h vs 72h)
//   - resetKey always returns SevenResetsAt       → A (1h vs 5h)
//   - resetKey returns the earlier of the two     → A (1h vs 5h)
//   - the `>=` comparison flipped to `<`          → A
//   - A's five/seven PCTs swapped in the fixture  → A, which is the swap D22 exists
//     to make observable
//   - tieLess ignoring the reset entirely         → A (lower secret id)
func TestSelectTieBreakUsesBindingWindowReset(t *testing.T) {
	p := Policy{MinHeadroom: 0, HeadroomTiePct: 5, MaxStaleness: time.Hour}

	a := Candidate{
		SecretID: id(1), Label: "a-token", AutoEligible: true, HasReading: true,
		FiveHourPct: i16(90), SevenDayPct: i16(40),
		FiveResetsAt: at(10 * time.Hour), SevenResetsAt: at(time.Hour),
		SyncedAt: at(-time.Minute),
	}
	b := Candidate{
		SecretID: id(2), Label: "b-token", AutoEligible: true, HasReading: true,
		FiveHourPct: i16(50), SevenDayPct: i16(90),
		FiveResetsAt: at(72 * time.Hour), SevenResetsAt: at(5 * time.Hour),
		SyncedAt: at(-time.Minute),
	}

	got := Select([]Candidate{a, b}, p, now)
	if got.Headroom != 10 {
		t.Fatalf("headroom = %d, want 10 — the fixture is meant to make both tokens tie", got.Headroom)
	}
	if got.SecretID != b.SecretID {
		t.Fatalf("picked %v, want %v — A's binding window (five-hour, 90%%) resets in 10h while "+
			"B's (seven-day, 90%%) resets in 5h; only the BINDING window's replenishment raises headroom",
			got.SecretID, b.SecretID)
	}

	// The same fixture with A's two windows swapped. Headroom is unchanged (min is
	// symmetric) and Classify cannot tell the difference — but A's binding window is
	// now the seven-day one, resetting in 1h, so A wins. If this half ever agrees
	// with the half above, the tie-break has stopped reading the binding window.
	swapped := a
	swapped.FiveHourPct, swapped.SevenDayPct = a.SevenDayPct, a.FiveHourPct
	if e := Classify(swapped, p, now); e != Classify(a, p, now) {
		t.Fatalf("the swap changed Classify (%+v); it must not — that is what makes this the only "+
			"place the swap is observable", e)
	}
	if got := Select([]Candidate{swapped, b}, p, now); got.SecretID != swapped.SecretID {
		t.Fatalf("after swapping A's windows the pick is %v, want %v — A now binds on its seven-day "+
			"window, which resets in 1h", got.SecretID, swapped.SecretID)
	}
}

// TestSelectTieBreakEqualPctPrefersFiveHour is D22's stated resolution of the
// equal-utilisation case: the five-hour window, because it replenishes sooner.
//
// MUTATION THIS CATCHES: `>=` → `>` in resetKey. Measured: the pick moves to the
// token whose five-hour reset is 9h out.
func TestSelectTieBreakEqualPctPrefersFiveHour(t *testing.T) {
	p := Policy{MinHeadroom: 0, HeadroomTiePct: 5, MaxStaleness: time.Hour}
	mk := func(n byte, five, seven time.Duration) Candidate {
		return Candidate{
			SecretID: id(n), AutoEligible: true, HasReading: true,
			FiveHourPct: i16(50), SevenDayPct: i16(50), // equal ⇒ five-hour binds
			FiveResetsAt: at(five), SevenResetsAt: at(seven),
			SyncedAt: at(-time.Minute),
		}
	}
	c := mk(1, 9*time.Hour, time.Hour)
	d := mk(2, 2*time.Hour, 99*time.Hour)
	if got := Select([]Candidate{c, d}, p, now); got.SecretID != d.SecretID {
		t.Fatalf("picked %v, want %v — on equal utilisation the FIVE-hour reset decides (2h vs 9h)",
			got.SecretID, d.SecretID)
	}
}

// TestSelectNullResetLosesTie: the poller writes NULL when Anthropic reports no
// reset (00080). A token that names no reset is never "about to replenish", so it is
// +∞ and loses to any token that names one.
//
// MUTATION THIS CATCHES: using the zero time.Time as the sentinel instead of a
// separate bool — a zero time is a real, orderable instant that sorts BEFORE every
// genuine reset, so the NULL token would win every tie. Measured.
func TestSelectNullResetLosesTie(t *testing.T) {
	null := cand(1, 80, nil)
	dated := cand(2, 80, at(99*time.Hour))
	if got := Select([]Candidate{null, dated}, testPolicy(), now); got.SecretID != dated.SecretID {
		t.Fatalf("picked %v, want %v — a NULL reset is +∞ and loses to a reset 99h out",
			got.SecretID, dated.SecretID)
	}
	// Both NULL ⇒ neither is "about to replenish" ⇒ fall through to the lowest id.
	null2 := cand(2, 80, nil)
	if got := Select([]Candidate{null2, null}, testPolicy(), now); got.SecretID != id(1) {
		t.Fatalf("with both resets NULL the pick is %v, want the lowest id %v", got.SecretID, id(1))
	}
}

// TestSelectTieBreakLowestSecretID: exactly-equal resets fall through to the secret
// id, a TOTAL order over values unique per row. Without it the pick is only as
// deterministic as the query's ORDER BY, which is not a property this package can
// assert about itself.
//
// MUTATION THIS CATCHES: `compareUUID(...) < 0` → `> 0`, which reverses it.
func TestSelectTieBreakLowestSecretID(t *testing.T) {
	shared := at(time.Hour)
	got := Select([]Candidate{cand(9, 80, shared), cand(2, 80, shared), cand(5, 80, shared)}, testPolicy(), now)
	if got.SecretID != id(2) {
		t.Fatalf("picked %v, want the lowest id %v", got.SecretID, id(2))
	}
}

// --- the in-flight bias --------------------------------------------------------

// TestSelectInFlightBiasSpreads is herd control. The gauge lags the poll interval,
// so several claims inside one interval read the SAME headroom and would pile onto
// the same emptiest token; the penalty breaks that toward spreading.
//
// MUTATION THIS CATCHES: dropping `- p.InflightPenalty*c.InFlight` from the rank →
// the 80-headroom token keeps winning however many runs are already on it.
func TestSelectInFlightBiasSpreads(t *testing.T) {
	p := Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: time.Hour, InflightPenalty: 3}
	busy := cand(1, 80, at(time.Minute)) // soonest reset too, so only the penalty can move the pick
	busy.InFlight = 4                    // rank 68
	idle := cand(2, 75, at(99*time.Hour))
	// 68 vs 75: the gap is 7, wider than T=5, so `idle` is alone in the cluster.
	got := Select([]Candidate{busy, idle}, p, now)
	if got.SecretID != idle.SecretID {
		t.Fatalf("picked %v, want %v — four in-flight runs cost the emptier token 12 points",
			got.SecretID, idle.SecretID)
	}
	// It is a BIAS, not a cap: one run is not enough to lose a 5-point lead.
	busy.InFlight = 1 // rank 77, still ahead of 75 and inside T of it
	if got := Select([]Candidate{busy, idle}, p, now); got.SecretID != busy.SecretID {
		t.Fatalf("picked %v, want %v — one in-flight run must not surrender the lead",
			got.SecretID, busy.SecretID)
	}
}

// TestSelectGateReadsRawHeadroomNotRank is the asymmetry stated as a test: the
// MIN_HEADROOM gate reads RAW headroom, the cluster reads the PENALIZED rank.
// Gating on the penalized value would let a busy-but-empty token fall below the
// floor and spuriously trigger best-of-pool — spending a fallback account because a
// token was popular rather than because it was full.
//
// MUTATION THIS CATCHES: classifying on the penalized value (or filtering the set on
// `rank >= MinHeadroom`) → the reason turns best_of_pool. Measured.
func TestSelectGateReadsRawHeadroomNotRank(t *testing.T) {
	p := Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: time.Hour, InflightPenalty: 3}
	busy := cand(1, 20, at(time.Hour))
	busy.InFlight = 5 // raw headroom 20 (eligible), rank 5 (would be below the floor)
	got := Select([]Candidate{busy}, p, now)
	if got.Reason != ReasonAuto {
		t.Fatalf("reason = %q, want %q — raw headroom 20 clears a floor of 15; only the RANK is 5",
			got.Reason, ReasonAuto)
	}
	if got.Headroom != 20 || got.Ranked != 5 {
		t.Fatalf("headroom/ranked = %d/%d, want 20/5", got.Headroom, got.Ranked)
	}
}

// --- the fallback chain --------------------------------------------------------

// TestSelectFallbackReasons walks every branch that does NOT produce a pick, plus
// D10's best-of-pool, which does. The reasons are distinct because they send a user
// to different places: "you pooled nothing" is a settings problem, "nothing is
// measurable" is a poller problem, and best-of-pool is neither.
//
// MUTATION THIS CATCHES: collapsing pool_empty and pool_stale into one reason (or
// returning pool_stale for an empty pool, which the loop's shape invites, since an
// empty pool also produces an empty measured set).
func TestSelectFallbackReasons(t *testing.T) {
	unpooled := cand(1, 80, at(time.Hour))
	unpooled.AutoEligible = false

	stale := cand(2, 80, at(time.Hour))
	stale.SyncedAt = at(-99 * time.Hour)

	noReading := cand(3, 80, at(time.Hour))
	noReading.HasReading, noReading.SyncedAt = false, nil

	unmeasured := cand(4, 80, at(time.Hour))
	unmeasured.FiveHourPct = nil

	for _, tc := range []struct {
		name  string
		cands []Candidate
		want  Reason
	}{
		{"no candidates at all", nil, ReasonPoolEmpty},
		{"tokens exist but none is pooled", []Candidate{unpooled}, ReasonPoolEmpty},
		{"pooled but the reading aged out", []Candidate{stale}, ReasonPoolStale},
		{"pooled but never polled", []Candidate{noReading}, ReasonPoolStale},
		{"pooled but a window is NULL", []Candidate{unmeasured}, ReasonPoolStale},
		{"pooled, mixed, none measurable", []Candidate{unpooled, stale, noReading, unmeasured}, ReasonPoolStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Select(tc.cands, testPolicy(), now)
			if got.Picked {
				t.Fatalf("picked %v; nothing here is rankable", got.SecretID)
			}
			if got.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.want)
			}
		})
	}
}

// TestSelectBestOfPool is D10: when every measurable pooled token is under the
// floor, the emptiest of THEM is still the least-bad answer. Falling to the owner
// default instead could pick a more-throttled token that simply is not in the pool.
//
// MUTATION THIS CATCHES: returning {picked=false} when the eligible set is empty →
// the reason becomes a fallback and the caller spends the owner default.
func TestSelectBestOfPool(t *testing.T) {
	p := testPolicy() // MinHeadroom 15
	got := Select([]Candidate{
		cand(1, 3, at(99*time.Hour)),
		cand(2, 12, at(99*time.Hour)), // the least-bad
		cand(3, 8, at(99*time.Hour)),
	}, p, now)
	if !got.Picked {
		t.Fatalf("picked nothing (reason %q); a below-threshold pool still has a best answer", got.Reason)
	}
	if got.Reason != ReasonBestOfPool {
		t.Fatalf("reason = %q, want %q — the pick did not clear MinHeadroom and the run view must say so",
			got.Reason, ReasonBestOfPool)
	}
	if got.SecretID != id(2) || got.Headroom != 12 {
		t.Fatalf("picked %v at %d%%, want %v at 12%%", got.SecretID, got.Headroom, id(2))
	}
}

// TestSelectBestOfPoolNeedsEVERYTokenBelow: one eligible token is enough to keep the
// reason `auto`, and the below-threshold ones must not enter the cluster at all.
//
// MUTATION THIS CATCHES: `if !anyEligible` → `if len(set) < len(measured)`, or any
// form that lets a below-threshold token into the ranked set while an eligible one
// exists. The below-threshold token here carries the soonest reset, so it would win.
func TestSelectBestOfPoolNeedsEveryTokenBelow(t *testing.T) {
	p := testPolicy() // MinHeadroom 15, T 5
	low := cand(1, 14, at(time.Second))
	ok := cand(2, 16, at(99*time.Hour))
	got := Select([]Candidate{low, ok}, p, now)
	if got.Reason != ReasonAuto {
		t.Fatalf("reason = %q, want %q — one token clears the floor", got.Reason, ReasonAuto)
	}
	if got.SecretID != ok.SecretID {
		t.Fatalf("picked %v, want %v — a below-threshold token is out of the running while any "+
			"token clears the floor, however soon it resets", got.SecretID, ok.SecretID)
	}
}

// --- totality ------------------------------------------------------------------

// TestSelectIsTotal: any input yields an Outcome. Select runs inside claim assembly,
// where a panic is a wedged worker rather than a failed test.
func TestSelectIsTotal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cands []Candidate
		p     Policy
	}{
		{"nil candidates, zero policy", nil, Policy{}},
		{"zero candidate", []Candidate{{}}, testPolicy()},
		{"pooled zero candidate", []Candidate{{AutoEligible: true}}, testPolicy()},
		{"negative tie tolerance", []Candidate{cand(1, 80, nil), cand(2, 80, nil)}, Policy{
			MinHeadroom: 0, HeadroomTiePct: -1, MaxStaleness: time.Hour,
		}},
		{"negative penalty", []Candidate{cand(1, 80, nil)}, Policy{
			MinHeadroom: 0, HeadroomTiePct: 5, MaxStaleness: time.Hour, InflightPenalty: -100,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = Select(tc.cands, tc.p, time.Time{})
		})
	}
	// A negative tolerance is floored at 0, not allowed to empty the cluster: the
	// anchor must still be reachable, and the answer must still not depend on order.
	a, b := cand(1, 80, nil), cand(2, 80, nil)
	p := Policy{MinHeadroom: 0, HeadroomTiePct: -7, MaxStaleness: time.Hour}
	fwd, rev := Select([]Candidate{a, b}, p, now), Select([]Candidate{b, a}, p, now)
	if fwd != rev {
		t.Fatalf("a negative tie tolerance made the outcome order-dependent: %+v != %+v", fwd, rev)
	}
	if !fwd.Picked || fwd.SecretID != id(1) {
		t.Fatalf("picked %v, want the lowest id %v", fwd.SecretID, id(1))
	}
}

// TestAllReasonsIsAFreshSlice guards the CLOSED-set accessor against a caller's
// append reaching every other reader. Cheap, and the alternative (a package-level
// var) is a shared mutable slice by definition.
func TestAllReasonsIsAFreshSlice(t *testing.T) {
	// Compared against the SECOND call rather than a named constant, so reordering the
	// vocabulary cannot break this the way pinning AllReasons()[0] to a specific reason
	// did when M5 folded three more in at the front.
	before := AllReasons()
	first := AllReasons()
	first[0] = "clobbered"
	if AllReasons()[0] != before[0] {
		t.Fatal("AllReasons hands out a shared backing array; one caller's append or write " +
			"would then corrupt every other reader's view of a CLOSED set")
	}
	// Eight since M5 folded workersvc's three static reasons in here, so the whole
	// vocabulary has ONE Go home. Migration 00089's CHECK is the same eight and three
	// separate guards compare against it — workersvc's, the CLI's, and the web's — so a
	// change to this number is a change to four artefacts and must be deliberate.
	if len(AllReasons()) != 8 {
		t.Fatalf("AllReasons has %d entries, want 8", len(AllReasons()))
	}
}
