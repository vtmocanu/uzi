package handler

import (
	"math/big"
	"slices"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_epoch_usage.go computes each credential epoch's own spend from a run's raw usage legs
// (issue #4201). A run's usage rows are attributed to an epoch by (claim_generation,
// credential_epoch_ordinal): a NULL ordinal is the claim's base epoch (0), and -1 is the permanent
// unattributed marker the append path stores for a frame that named an epoch the server had not
// committed. The totals view run_usage_totals (migration 00244, ADR-1562) is not additive over an
// arbitrary partition of the legs, because a resumed session's session_cumulative reading is
// high-water folded against the per_leg legs before it. Each epoch's usage is therefore computed by
// telescoping: take the view's algebra over every attributed leg at or before the epoch, and subtract
// the same total over the legs before it. The deltas then sum exactly to the attributed total, and
// the unattributed bucket is whatever the full-run total holds beyond that, so
//
//	sum(epoch usage) + unattributed == the run's usage total
//
// holds by construction. Every delta is non-negative because the algebra is monotone in the leg set.

// epochKey orders credential epochs: claim generation first, then the in-generation ordinal.
type epochKey struct {
	generation int64
	ordinal    int32
}

func (k epochKey) less(o epochKey) bool {
	if k.generation != o.generation {
		return k.generation < o.generation
	}
	return k.ordinal < o.ordinal
}

// usageVec is a usage total; cost is in microdollars so telescoping stays exact.
type usageVec struct {
	input, output, cacheRead, cacheCreation int64
	costMicros                              int64
}

func (a usageVec) minus(b usageVec) usageVec {
	return usageVec{a.input - b.input, a.output - b.output, a.cacheRead - b.cacheRead, a.cacheCreation - b.cacheCreation, a.costMicros - b.costMicros}
}

func (a usageVec) plus(b usageVec) usageVec {
	return usageVec{a.input + b.input, a.output + b.output, a.cacheRead + b.cacheRead, a.cacheCreation + b.cacheCreation, a.costMicros + b.costMicros}
}

func (a usageVec) maxWith(b usageVec) usageVec {
	return usageVec{max(a.input, b.input), max(a.output, b.output), max(a.cacheRead, b.cacheRead), max(a.cacheCreation, b.cacheCreation), max(a.costMicros, b.costMicros)}
}

func (a usageVec) floorZero() usageVec { return usageVec{}.maxWith(a) }

const usageBasisSessionCumulative = "session_cumulative"

// legRow is one (model, lineage_epoch) leg after merging the rows that share it, the unit the totals
// view's algebra runs over.
type legRow struct {
	lineageIndex int32
	cumulative   bool
	usage        usageVec
}

func rowKey(r store.ListRunUsageByCredentialEpochRow) epochKey {
	k := epochKey{}
	if r.ClaimGeneration.Valid {
		k.generation = r.ClaimGeneration.Int64
	}
	if r.CredentialEpochOrdinal.Valid {
		k.ordinal = r.CredentialEpochOrdinal.Int32
	}
	return k
}

func rowUsage(r store.ListRunUsageByCredentialEpochRow) usageVec {
	return usageVec{r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheCreationTokens, numericToMicros(r.CostUsd)}
}

// numericToMicros converts a numeric dollar amount to whole microdollars (rounding half away from
// zero below that scale). An invalid numeric is zero.
func numericToMicros(n pgtype.Numeric) int64 {
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite || n.Int == nil {
		return 0
	}
	v := new(big.Int).Set(n.Int)
	shift := int(n.Exp) + 6
	switch {
	case shift > 0:
		v.Mul(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shift)), nil))
	case shift < 0:
		div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-shift)), nil)
		q, m := new(big.Int).QuoRem(v, div, new(big.Int))
		if new(big.Int).Abs(new(big.Int).Mul(m, big.NewInt(2))).Cmp(div) >= 0 {
			if v.Sign() >= 0 {
				q.Add(q, big.NewInt(1))
			} else {
				q.Sub(q, big.NewInt(1))
			}
		}
		v = q
	}
	if !v.IsInt64() {
		return 0
	}
	return v.Int64()
}

// foldUsageRows applies the run_usage_totals algebra to a set of usage rows: merge the rows of one
// (model, lineage_epoch) leg (max of each metric, cumulative when any row is session_cumulative),
// then per (model, lineage_index) sum the per_leg legs and add the high-water of each cumulative
// leg's reading over the per_leg legs that precede it (floored at zero).
func foldUsageRows(rows []store.ListRunUsageByCredentialEpochRow) usageVec {
	type legKey struct {
		model string
		epoch int32
	}
	legs := map[legKey]*legRow{}
	for _, r := range rows {
		k := legKey{r.Model, r.LineageEpoch}
		u := rowUsage(r)
		cum := r.UsageBasis == usageBasisSessionCumulative
		l, ok := legs[k]
		if !ok {
			legs[k] = &legRow{lineageIndex: r.LineageIndex, cumulative: cum, usage: u}
			continue
		}
		l.lineageIndex = max(l.lineageIndex, r.LineageIndex)
		l.cumulative = l.cumulative || cum
		l.usage = l.usage.maxWith(u)
	}
	type groupKey struct {
		model        string
		lineageIndex int32
	}
	type orderedLeg struct {
		epoch int32
		leg   legRow
	}
	groups := map[groupKey][]orderedLeg{}
	for k, l := range legs {
		gk := groupKey{k.model, l.lineageIndex}
		groups[gk] = append(groups[gk], orderedLeg{k.epoch, *l})
	}
	var total usageVec
	for _, g := range groups {
		slices.SortFunc(g, func(a, b orderedLeg) int { return int(a.epoch) - int(b.epoch) })
		var perLeg, best usageVec
		haveBest := false
		for _, ol := range g {
			if ol.leg.cumulative {
				// The reading's excess over the per_leg legs up to and including this leg's epoch
				// (a cumulative leg contributes none of its own to that running sum).
				d := ol.leg.usage.minus(perLeg)
				if haveBest {
					best = best.maxWith(d)
				} else {
					best, haveBest = d, true
				}
				continue
			}
			perLeg = perLeg.plus(ol.leg.usage)
		}
		total = total.plus(perLeg)
		if haveBest {
			total = total.plus(best.floorZero())
		}
	}
	return total
}

// telescopeCredentialEpochUsage returns each attributed epoch's own usage and the unattributed
// remainder. Rows with a negative ordinal are the unattributed bucket. known is the set of epochs the
// caller has a journal row for: usage attributed to any other key (a legacy NULL-generation leg, a
// generation with no recorded epoch) cannot be shown on an epoch and is added to the unattributed
// bucket, so the returned parts always sum to the run's total.
func telescopeCredentialEpochUsage(rows []store.ListRunUsageByCredentialEpochRow, known map[epochKey]bool) (map[epochKey]usageVec, usageVec) {
	var attributed []store.ListRunUsageByCredentialEpochRow
	for _, r := range rows {
		if r.CredentialEpochOrdinal.Valid && r.CredentialEpochOrdinal.Int32 < 0 {
			continue
		}
		attributed = append(attributed, r)
	}
	var keys []epochKey
	seen := map[epochKey]bool{}
	for _, r := range attributed {
		if k := rowKey(r); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b epochKey) int {
		switch {
		case a.less(b):
			return -1
		case b.less(a):
			return 1
		}
		return 0
	})
	out := map[epochKey]usageVec{}
	var prev, orphans usageVec
	for _, k := range keys {
		var prefix []store.ListRunUsageByCredentialEpochRow
		for _, r := range attributed {
			if !k.less(rowKey(r)) {
				prefix = append(prefix, r)
			}
		}
		cur := foldUsageRows(prefix)
		delta := cur.minus(prev)
		prev = cur
		if known[k] {
			out[k] = delta
		} else {
			orphans = orphans.plus(delta)
		}
	}
	return out, foldUsageRows(rows).minus(prev).plus(orphans)
}
