package autoselect

import (
	"time"

	"github.com/google/uuid"
)

// Reason is WHY a claim spent the credential it spent, persisted in
// runs.anthropic_select_reason and rendered by M5. These five are the auto lane's
// share of a CLOSED vocabulary; workersvc owns the other three (default, pinned,
// judge), and migration 00089's CHECK is the union of both halves. The two homes
// are kept honest by TestSelectReasonVocabularyMatchesCheck in workersvc, which
// parses that CHECK — because a value added in Go and forgotten in SQL is a
// constraint violation at claim time, i.e. a failed run, and one removed from Go
// but left in SQL is a promise nothing keeps.
type Reason string

const (
	// ReasonAuto: picked from the eligible set — the feature working as designed.
	ReasonAuto Reason = "auto"
	// ReasonBestOfPool: every measurable pooled token was below MinHeadroom, so the
	// emptiest of THEM was picked anyway (D10). Not a failure: "least consumed" still
	// has a best answer, and falling to the owner default instead could pick a
	// MORE-throttled token that simply is not in the pool.
	ReasonBestOfPool Reason = "best_of_pool"
	// ReasonPoolEmpty: the user opted no token in. Select picks nothing and the
	// caller resolves the worker's non-auto binding (D7 — auto never fails a run).
	ReasonPoolEmpty Reason = "pool_empty"
	// ReasonPoolStale: tokens ARE pooled but not one of them is measurable — no
	// gauge row, a NULL window, or a reading that aged out. This is also what a
	// disabled poller produces for every token (R2), which is why it is a distinct
	// reason from pool_empty: "you pooled nothing" and "your poller is not running"
	// send a user to entirely different places.
	ReasonPoolStale Reason = "pool_stale"
	// ReasonOpenFailed is produced by the CALLER, never by Select: the selector's
	// pick was fine on paper and then would not decrypt (or vanished between the
	// ranking query and the open), so the claim retried once on the non-auto binding
	// (D14). It lives in this vocabulary rather than workersvc's because it can only
	// ever arise on the auto lane — no other mode has a second credential to fall
	// back to.
	ReasonOpenFailed Reason = "open_failed"
)

// AllReasons is the auto lane's half of the reason vocabulary, in a form a guard
// can enumerate. Returns a fresh slice: a package-level var would let one caller's
// append corrupt every other reader's view of a CLOSED set.
func AllReasons() []Reason {
	return []Reason{ReasonAuto, ReasonBestOfPool, ReasonPoolEmpty, ReasonPoolStale, ReasonOpenFailed}
}

// Outcome is the ranker's answer.
//
// Picked is false for ReasonPoolEmpty and ReasonPoolStale, and the caller then
// resolves the worker's non-auto binding — recording the reason anyway, so "auto
// was on and you still got the default" is a fact a user can read rather than
// infer (D7).
//
// Headroom and Ranked are deliberately BOTH kept. Headroom is the raw
// min(100-five, 100-seven) — what the eligibility gate tested and what M5 renders
// as "N% headroom", because that is the number the user's own meters show. Ranked
// is the same value after the in-flight penalty — what actually decided the pick,
// and diagnostics only. Rendering Ranked would show a percentage that appears
// nowhere else in the product.
type Outcome struct {
	Picked   bool
	SecretID uuid.UUID
	Label    string
	Headroom int
	Ranked   int
	Reason   Reason
}

// Select ranks a user's candidates and returns the pick (PRD #111 M4).
//
// Pure and TOTAL: any input, including nil, yields an Outcome. ORDER-INDEPENDENT:
// shuffling cands cannot change the result, which is not a nicety but the property
// that makes the ranking testable at all.
//
// 🔴 THE ONE THING NOT TO REWRITE AS A COMPARATOR. The PRD's rule reads like a
// pairwise `less`: "within T points of each other, prefer the sooner reset;
// otherwise prefer more headroom". Written that way it is INTRANSITIVE — with
// T=5 and A=80, B=75, C=70, A ties B and B ties C but A does not tie C — so it is
// not a strict weak ordering, and sort.Slice over it gives order-dependent,
// undefined results (the standard library is explicit that a bad Less makes the
// output arbitrary, and it will not tell you). The fix is structural, not a
// stricter comparator: anchor the tolerance to ONE fixed value, H*, so every
// membership test is against the same reference and no chain exists. Inside the
// cluster the tie-break IS a total order (reset, then secret id), which is exactly
// what a comparator may be.
//
// The gate reads RAW headroom while the cluster reads the PENALIZED rank, and the
// asymmetry is deliberate (D-note 1): gating on the penalized value would let a
// busy-but-empty token drop under MinHeadroom and spuriously trigger best-of-pool,
// spending a fallback account because a token was popular rather than because it
// was full.
func Select(cands []Candidate, p Policy, now time.Time) Outcome {
	// scored is one pooled, measurable candidate with the two numbers that decide
	// its fate: the gate's raw headroom and the ranker's penalized key.
	type scored struct {
		c    Candidate
		e    Eligibility
		rank int
	}

	var pooled bool
	var measured []scored
	var anyEligible bool
	for _, c := range cands {
		// The pool membership test is here rather than in SQL on purpose: the query
		// returns every anthropic_token the user holds so the ranker can tell "you
		// pooled nothing" from "you pooled tokens that are all stale", which are
		// different reasons a user reads differently.
		if !c.AutoEligible {
			continue
		}
		pooled = true
		e := Classify(c, p, now)
		if !e.Measured {
			continue
		}
		// In-flight bias (herd control). The gauge lags the poll interval, so several
		// claims inside one interval read the SAME headroom and would pile onto the
		// same emptiest token. Subtracting per concurrent run breaks that tie toward
		// spreading. It may go negative; that is fine, the anchor is relative.
		measured = append(measured, scored{c: c, e: e, rank: e.Headroom - p.InflightPenalty*c.InFlight})
		if e.Status == StatusEligible {
			anyEligible = true
		}
	}
	if !pooled {
		return Outcome{Reason: ReasonPoolEmpty}
	}
	if len(measured) == 0 {
		return Outcome{Reason: ReasonPoolStale}
	}

	// D10's best-of-pool. When nothing clears MinHeadroom the candidate set becomes
	// every MEASURED token rather than none — a below-threshold token still has a
	// perfectly good headroom number, it is just a low one, and Eligibility.Measured
	// is true for exactly that case so the fallback has something to rank.
	reason := ReasonAuto
	set := measured
	if !anyEligible {
		reason = ReasonBestOfPool
	} else {
		set = make([]scored, 0, len(measured))
		for _, m := range measured {
			if m.e.Status == StatusEligible {
				set = append(set, m)
			}
		}
	}

	// H* is the anchor, computed over the FILTERED set and never before it (D-note
	// 2): anchoring on the whole measured set would let a below-threshold token set
	// the reference and pull the tie window down around it.
	hstar := set[0].rank
	for _, m := range set[1:] {
		if m.rank > hstar {
			hstar = m.rank
		}
	}

	// A negative tolerance is floored at 0 rather than handled downstream. hstar is
	// itself one of the ranks, so a non-negative tolerance always admits at least the
	// anchor and the cluster is never empty; a negative one would exclude even that,
	// and every way of recovering from an empty cluster ("take the first at hstar")
	// reintroduces exactly the input-order dependence this whole function is shaped
	// to avoid. Config clamps the knob non-negative already — this is the guard for
	// the direct callers config does not stand in front of.
	tie := p.HeadroomTiePct
	if tie < 0 {
		tie = 0
	}

	// One pass over the cluster keeping the best under a TOTAL order. No sort, so
	// there is no Less for a future refactor to make intransitive, and the result
	// cannot depend on the input's order.
	best := 0
	for i := range set {
		if hstar-set[i].rank > tie {
			continue
		}
		if hstar-set[best].rank > tie || tieLess(set[i].c, set[best].c) {
			best = i
		}
	}

	pick := set[best]
	return Outcome{
		Picked:   true,
		SecretID: pick.c.SecretID,
		Label:    pick.c.Label,
		Headroom: pick.e.Headroom,
		Ranked:   pick.rank,
		Reason:   reason,
	}
}

// tieLess is the within-cluster order: soonest reset of the BINDING window, then
// lowest secret id. Both legs are total, so the pick is deterministic — which is
// what lets a test assert anything at all about it.
func tieLess(a, b Candidate) bool {
	ta, oka := resetKey(a)
	tb, okb := resetKey(b)
	switch {
	case oka != okb:
		// A NULL reset is +∞ (the poller writes NULL when Anthropic reports no reset,
		// 00080). A token that names no reset is never "about to replenish", so it
		// loses to any token that does.
		return oka
	case oka && !ta.Equal(tb):
		return ta.Before(tb)
	}
	// Exactly-equal resets and the all-NULL case fall through to the secret id — a
	// total order over values that are unique per row, so there is always exactly one
	// answer.
	return compareUUID(a.SecretID, b.SecretID) < 0
}

// resetKey is the reset that MATTERS for this candidate: the BINDING window's, i.e.
// whichever window produced headroom's min (D22).
//
// Not min(five, seven), and the difference is the whole decision. The tie-break
// asks which token is about to replenish, and only the binding window's
// replenishment raises headroom: a token at 90% five-hour and 40% seven-day has
// headroom 10, and its seven-day reset three days out says nothing about when it
// becomes usable again. Using the other window's reset would prefer a token whose
// headroom does not move.
//
// D22 also settles the equal-pct case as the FIVE-hour window (it replenishes
// sooner), which is why the test is `>=` and not `>`. An earlier draft of this rule
// said "the earlier non-nil of the two" for that case; D22 supersedes it, and the
// simpler form is also the one that makes a five/seven SWAP observable — the swap
// is invisible through Classify by construction, because min is symmetric and so is
// the NULL gate, so this function is the only place in the package that can catch
// it.
//
// The bool is false for +∞. A zero time.Time is NOT used as the sentinel: it is a
// real, orderable instant that would sort BEFORE every genuine reset and make a
// NULL-reset token win every tie — the exact inversion of the rule.
func resetKey(c Candidate) (time.Time, bool) {
	// Callers reach here only for MEASURED candidates, where Classify has already
	// established both pcts are non-nil. The guard is kept anyway because resetKey is
	// one refactor away from being called on an unmeasured row, and the failure mode
	// there is a nil dereference in the claim path.
	if c.FiveHourPct == nil || c.SevenDayPct == nil {
		return time.Time{}, false
	}
	r := c.SevenResetsAt
	if *c.FiveHourPct >= *c.SevenDayPct {
		r = c.FiveResetsAt
	}
	if r == nil {
		return time.Time{}, false
	}
	return *r, true
}

// compareUUID orders two ids by their raw bytes, the honest total order over a
// uuid.UUID ([16]byte, which Go compares for equality but not for order).
//
// Hand-rolled rather than bytes.Compare, and that is not stubbornness: this package
// is pure by design and TestPackageImportsStayPure allows exactly `time` and `uuid`.
// Widening that allowlist for a six-line loop would weaken the one guard standing
// between this file and an `import "context"`.
//
// Bytes, not the string form: equivalent for canonical lowercase hex, but the byte
// order does not depend on formatting.
func compareUUID(a, b uuid.UUID) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
