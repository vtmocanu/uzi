package autoselect

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #217 M2 — Select's `exclude` parameter, exercised against hand-built
// Candidate fixtures (the package is pure). These own the RANKING half of the
// exclusion; autoChoice's fallback half is tested in workersvc.

// TestSelectExcludeDropsTheCandidate: excluding a real candidate id removes it from
// the ranking entirely — it is neither picked nor allowed to set the anchor.
//
// The dead token carries MORE headroom than the survivor, so without the exclusion
// it wins outright; the exclusion is the only thing that hands the pick to the other
// token, which is what makes this fixture discriminating.
func TestSelectExcludeDropsTheCandidate(t *testing.T) {
	dead := cand(1, 90, at(time.Hour)) // most headroom ⇒ would be picked AND would anchor
	alt := cand(2, 80, at(time.Hour))
	cands := []Candidate{dead, alt}

	// Baseline: with nothing excluded the dead token wins.
	if got := Select(cands, uuid.Nil, testPolicy(), now); !got.Picked || got.SecretID != id(1) {
		t.Fatalf("baseline picked %v (reason %q), want the 90-headroom token %v", got.SecretID, got.Reason, id(1))
	}

	got := Select(cands, id(1), testPolicy(), now)
	if !got.Picked || got.SecretID != id(2) {
		t.Fatalf("with %v excluded, picked %v (reason %q), want the surviving token %v",
			id(1), got.SecretID, got.Reason, id(2))
	}
	if got.Reason != ReasonAuto {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonAuto)
	}
	// The excluded token must not set the anchor either: the reported headroom is the
	// survivor's 80, never the dead 90.
	if got.Headroom != 80 {
		t.Fatalf("headroom = %d, want 80 — the excluded 90-headroom token must neither be picked "+
			"nor set H*", got.Headroom)
	}
}

// TestSelectExcludeTheSoleCandidateYieldsPoolEmpty: excluding the user's ONLY
// auto-eligible token yields ReasonPoolEmpty with Picked=false, not ReasonPoolStale.
//
// The exclusion is applied BEFORE `pooled` is set, so the dead token being the only
// one reads as "no pool" — and the caller then resolves the worker's non-auto
// binding rather than being handed a stale-pool answer. This pins the "Skipped BEFORE
// pooled is set" invariant in Select.
func TestSelectExcludeTheSoleCandidateYieldsPoolEmpty(t *testing.T) {
	only := cand(1, 90, at(time.Hour)) // fresh, eligible, and about to be excluded
	got := Select([]Candidate{only}, id(1), testPolicy(), now)
	if got.Picked {
		t.Fatalf("excluding the only pooled token still picked %v", got.SecretID)
	}
	if got.Reason != ReasonPoolEmpty {
		t.Fatalf("reason = %q, want %q — the skip is before `pooled` is set, so the sole token being "+
			"the dead one is an empty pool, not a stale one", got.Reason, ReasonPoolEmpty)
	}
}

// TestSelectExcludeNilAndAbsentAreNoOps: uuid.Nil is the common no-op path (every
// non-resume claim passes it), and an exclude that matches no live candidate leaves
// the outcome byte-identical to it. This is the "identical to today's 3-arg
// behaviour" property the PRD calls for, plus the non-present / non-eligible case.
func TestSelectExcludeNilAndAbsentAreNoOps(t *testing.T) {
	cands := []Candidate{cand(1, 90, at(time.Hour)), cand(2, 80, at(time.Hour))}
	base := Select(cands, uuid.Nil, testPolicy(), now)
	if !base.Picked || base.SecretID != id(1) {
		t.Fatalf("baseline picked %v, want %v", base.SecretID, id(1))
	}

	// An id that matches no candidate at all.
	if got := Select(cands, id(200), testPolicy(), now); got != base {
		t.Fatalf("excluding an absent id changed the outcome: %+v != %+v", got, base)
	}

	// An id that IS present but is not auto-eligible: it was never in the ranking, so
	// naming it changes nothing.
	unpooled := cand(3, 99, at(time.Minute))
	unpooled.AutoEligible = false
	withUnpooled := append([]Candidate{unpooled}, cands...)
	a := Select(withUnpooled, uuid.Nil, testPolicy(), now)
	if b := Select(withUnpooled, id(3), testPolicy(), now); a != b {
		t.Fatalf("excluding a non-eligible id changed the outcome: %+v != %+v", b, a)
	}
	if a.SecretID != id(1) {
		t.Fatalf("the non-eligible token perturbed the pick: got %v, want %v", a.SecretID, id(1))
	}
}

// TestSelectBumpingSyncedAtWouldRepickTheDeadToken is D3, expressed as the mutation
// the PRD names: "bump it and watch best_of_pool pick the dead token". It is the
// Select-level demonstration of WHY M1's gauge write must not touch synced_at.
//
// A dead token at 100% consumed (headroom 0 — exactly what M1's pct write records)
// sits beside a healthy-but-stale token. The single variable is the dead token's own
// synced_at:
//
//   - freshened ⇒ it classifies Measured (below-threshold), becomes the only
//     rankable token, and best_of_pool PICKS it. That is the re-pick this PRD exists
//     to prevent, and bumping synced_at would create it.
//   - left stale (M1's actual behaviour) ⇒ it is not Measured, drops out of the
//     ranking set, and nothing is picked.
func TestSelectBumpingSyncedAtWouldRepickTheDeadToken(t *testing.T) {
	// The "beside a stale one": a healthy token whose reading has aged out, so it can
	// never rescue the ranking regardless of the dead token's fate.
	stale := cand(2, 80, at(time.Hour))
	stale.SyncedAt = at(-99 * time.Hour)

	// WITH synced_at bumped: the dead row is fresh.
	freshDead := cand(1, 0, at(time.Hour)) // headroom 0; cand() leaves SyncedAt fresh (-1m)
	if e := Classify(freshDead, testPolicy(), now); !e.Measured {
		t.Fatalf("the freshened dead token is not Measured (%+v); the fixture is wrong", e)
	}
	got := Select([]Candidate{freshDead, stale}, uuid.Nil, testPolicy(), now)
	if !got.Picked || got.SecretID != id(1) || got.Reason != ReasonBestOfPool {
		t.Fatalf("with synced_at bumped, picked %v (reason %q), want the DEAD token %v at "+
			"best_of_pool — this is exactly why M1 must NOT bump synced_at (D3)",
			got.SecretID, got.Reason, id(1))
	}

	// WITHOUT the bump: the same dead token, its pre-existing stale synced_at left
	// alone. No longer Measured, so it leaves the ranking set and nothing is picked.
	staleDead := freshDead
	staleDead.SyncedAt = at(-99 * time.Hour)
	got = Select([]Candidate{staleDead, stale}, uuid.Nil, testPolicy(), now)
	if got.Picked {
		t.Fatalf("with synced_at left stale, the dead token was still picked (%v, reason %q); "+
			"leaving synced_at alone is what keeps it out of the ranking", got.SecretID, got.Reason)
	}
	if got.Reason != ReasonPoolStale {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonPoolStale)
	}
}
