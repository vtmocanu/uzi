package codexselect

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
)

// stalenessSentinel is the MaxStaleness handed to autoselect.Select. Score already decided
// freshness against the Codex poll interval; autoselect must not decide it again, and a
// Claude poll interval of 0 (which makes Claude's own MaxStaleness 0, "nothing is ever
// fresh") must not mark a Codex reading stale. Readings are stamped at now, so any positive
// value is never exceeded.
const stalenessSentinel = time.Hour

// Candidate is one Codex account (not one alias) offered to Select. The caller computes
// every field; Select reads no store.
type Candidate struct {
	// AccountKey is codex_provider_account.id, unique per user. It is the SecretID the
	// ranker sees, so ties break on the stable account key.
	AccountKey uuid.UUID
	// AliasID is the alias to bind: the lowest user_secrets.id among the account's
	// pooled, enabled, linked aliases.
	AliasID uuid.UUID
	// Structural is true when the account may ever be bound: alias enabled and linked,
	// account not quarantined, no reauth required. A false account is never picked.
	Structural bool
	// CapacityExcluded is true when the account is at capacity right now (limit reached,
	// admission cooldown, weekly reserve hold). Such an account is reachable only through
	// pool_exhausted.
	CapacityExcluded bool
	// Score, ScoreKnown and ResetAt are Score's outputs (ResetAt is the binding reset).
	Score      float64
	ScoreKnown bool
	ResetAt    *time.Time
	// ReleaseAt and ReleaseKnown order pool_exhausted: the maximum of the account's known
	// per-cause release times, computed by the caller. ReleaseKnown is false when any
	// cause's time is unknown.
	ReleaseAt    *time.Time
	ReleaseKnown bool
	// InFlight is the account's nonterminal Codex run count.
	InFlight int
}

// Outcome is Select's answer. Picked is false only when no candidate is structural, which
// the caller treats as unusable (the usability rule prevents creation from reaching it).
type Outcome struct {
	Picked     bool
	AccountKey uuid.UUID
	AliasID    uuid.UUID
	Reason     Reason
	// Headroom is the raw points of capacity left (floor of the score) for auto and
	// best_of_pool; zero for the other reasons.
	Headroom int
}

// Select picks the account a new run binds.
//
//  1. Normal accounts (structural, not capacity-excluded) with a known score are ranked by
//     autoselect.Select (reasons auto or best_of_pool) through an adapter.
//  2. Normal accounts exist but none has a known score: the lowest account key wins
//     (pool_stale).
//  3. No normal account but at least one structural one: the earliest known release time
//     wins, unknown last, ties on the lowest key (pool_exhausted).
//  4. No structural account: Picked is false.
//
// Deterministic: shuffling cands cannot change the result. p is read, never modified.
func Select(cands []Candidate, p autoselect.Policy, now time.Time) (Outcome, error) {
	var normal, exhausted []Candidate
	for _, c := range cands {
		if c.AccountKey == uuid.Nil || !c.Structural {
			continue
		}
		if c.CapacityExcluded {
			exhausted = append(exhausted, c)
		} else {
			normal = append(normal, c)
		}
	}
	if len(normal) > 0 {
		return selectNormal(normal, p, now)
	}
	if len(exhausted) > 0 {
		best := exhausted[0]
		for _, c := range exhausted[1:] {
			if releaseLess(c, best) {
				best = c
			}
		}
		return Outcome{Picked: true, AccountKey: best.AccountKey, AliasID: best.AliasID, Reason: ReasonPoolExhausted}, nil
	}
	return Outcome{}, nil
}

func selectNormal(normal []Candidate, p autoselect.Policy, now time.Time) (Outcome, error) {
	// alias maps the ranker's SecretID back to the alias; on a duplicated account key the
	// lowest alias id wins, so even a malformed input stays deterministic.
	alias := make(map[uuid.UUID]uuid.UUID, len(normal))
	var known []autoselect.Candidate
	lowest := normal[0]
	for _, c := range normal {
		if prev, ok := alias[c.AccountKey]; !ok || bytes.Compare(c.AliasID[:], prev[:]) < 0 {
			alias[c.AccountKey] = c.AliasID
		}
		if bytes.Compare(c.AccountKey[:], lowest.AccountKey[:]) < 0 {
			lowest = c
		}
		if !c.ScoreKnown {
			continue
		}
		pct := utilPct(c.Score)
		synced := now
		known = append(known, autoselect.Candidate{
			SecretID:      c.AccountKey,
			AutoEligible:  true,
			HasReading:    true,
			FiveHourPct:   &pct,
			SevenDayPct:   &pct,
			FiveResetsAt:  c.ResetAt,
			SevenResetsAt: c.ResetAt,
			SyncedAt:      &synced,
			InFlight:      c.InFlight,
		})
	}
	if len(known) == 0 {
		return Outcome{Picked: true, AccountKey: lowest.AccountKey, AliasID: alias[lowest.AccountKey], Reason: ReasonPoolStale}, nil
	}
	policy := p
	policy.MaxStaleness = stalenessSentinel
	out := autoselect.Select(known, uuid.Nil, policy, now)
	if !out.Picked {
		return Outcome{}, fmt.Errorf("codexselect: ranker picked nothing from %d scored accounts (reason %q)", len(known), out.Reason)
	}
	var reason Reason
	switch out.Reason {
	case autoselect.ReasonAuto:
		reason = ReasonAuto
	case autoselect.ReasonBestOfPool:
		reason = ReasonBestOfPool
	default:
		return Outcome{}, errors.New("codexselect: ranker returned unexpected reason " + string(out.Reason))
	}
	a, ok := alias[out.SecretID]
	if !ok {
		return Outcome{}, errors.New("codexselect: ranker returned an unknown account key")
	}
	return Outcome{Picked: true, AccountKey: out.SecretID, AliasID: a, Reason: reason, Headroom: out.Headroom}, nil
}

// utilPct is ceil(utilization) as the whole-percent the ranker works in, so the ranker's
// headroom, 100 - pct, equals floor(score).
func utilPct(score float64) int16 {
	u := math.Ceil(100 - score)
	if u < 0 {
		u = 0
	}
	if u > 100 {
		u = 100
	}
	return int16(u)
}

// releaseLess orders pool_exhausted candidates: a known release time beats an unknown one,
// the earlier known time wins, and the lowest account key, then the lowest alias id, breaks the rest.
func releaseLess(a, b Candidate) bool {
	ak := a.ReleaseKnown && a.ReleaseAt != nil
	bk := b.ReleaseKnown && b.ReleaseAt != nil
	switch {
	case ak != bk:
		return ak
	case ak && !a.ReleaseAt.Equal(*b.ReleaseAt):
		return a.ReleaseAt.Before(*b.ReleaseAt)
	}
	if c := bytes.Compare(a.AccountKey[:], b.AccountKey[:]); c != 0 {
		return c < 0
	}
	return bytes.Compare(a.AliasID[:], b.AliasID[:]) < 0
}
