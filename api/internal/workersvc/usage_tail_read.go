package workersvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/anthropicprice"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_tail_read.go is the read half of the estimated usage tail (issue #2014, ADR-2014 D4-D6):
// which recorded messages no result frame covered, whether the record is complete, and what the
// uncovered tokens would cost at the recorded public prices. Nothing here writes, and the figure
// is never merged into the metered total.

// The closed coverage vocabulary (ADR-2014 D5), in the order they are reported.
const (
	UsageTailReasonLegNotClosed        = "leg_not_closed"
	UsageTailReasonOrdinalGap          = "ordinal_gap"
	UsageTailReasonOutputNotFinal      = "output_not_final"
	UsageTailReasonSupersededUncertain = "superseded_uncertain"
	UsageTailReasonRecordsDropped      = "records_dropped"
	UsageTailReasonRecordCapReached    = "record_cap_reached"
	UsageTailReasonUnresolved          = "unresolved"
)

var usageTailReasonOrder = []string{
	UsageTailReasonLegNotClosed,
	UsageTailReasonOrdinalGap,
	UsageTailReasonOutputNotFinal,
	UsageTailReasonSupersededUncertain,
	UsageTailReasonRecordsDropped,
	UsageTailReasonRecordCapReached,
	UsageTailReasonUnresolved,
}

// Cost-status values of the tail (the metered total's "metered" is unchanged and separate).
const (
	usageTailCostEstimated = "estimated"
	usageTailCostUnpriced  = "unpriced"
)

// errUsageTailNoTx reports a Service with no transaction beginner wired: the tail is read in one
// REPEATABLE READ snapshot, which needs a transaction. Production always wires one.
var errUsageTailNoTx = errors.New("run usage tail unavailable: no tx beginner wired")

// RunUsageTail returns the run's estimated usage tail, or nil when the run has no recorded legs
// and no tail state (so the DTO field is omitted). Legs, messages and the cap state are read in
// ONE REPEATABLE READ read-only snapshot, so a post committing mid-read cannot make the coverage
// reasons and the tail disagree.
func (s *Service) RunUsageTail(ctx context.Context, runID uuid.UUID) (*apitypes.UsageTailDTO, error) {
	if s.txBeginner == nil {
		return nil, errUsageTailNoTx
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Must be the transaction's first statement.
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
		return nil, fmt.Errorf("run usage tail: set snapshot: %w", err)
	}
	q := store.New(tx)
	legs, err := q.ListRunUsageLegsForTail(ctx, runID)
	if err != nil {
		return nil, err
	}
	capReached := false
	haveState := true
	state, err := q.GetRunUsageTailState(ctx, runID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		haveState = false
	case err != nil:
		return nil, err
	default:
		capReached = state.RecordCapReached
	}
	if len(legs) == 0 && !haveState {
		return nil, nil
	}
	tail, err := q.ListRunUsageTailMessages(ctx, runID)
	if err != nil {
		return nil, err
	}
	return buildUsageTail(legs, tail, capReached), nil
}

// supersededLegs returns the legs some LATER leg of the SAME SDK session covered with a
// session-cumulative result (ADR-2014 D4 arm b). NULL-safe: a leg with no init_seq or sdk_session_id
// is never superseded and never supersedes.
func supersededLegs(legs []store.ListRunUsageLegsForTailRow) map[uuid.UUID]bool {
	out := map[uuid.UUID]bool{}
	for _, l := range legs {
		if !l.InitSeq.Valid || !l.SdkSessionID.Valid {
			continue
		}
		for _, c := range legs {
			if c.CoveredThrough.Valid && c.CoveredCumulative && c.InitSeq.Valid && c.SdkSessionID.Valid &&
				c.SdkSessionID.String == l.SdkSessionID.String && c.InitSeq.Int64 > l.InitSeq.Int64 {
				out[l.LegID] = true
				break
			}
		}
	}
	return out
}

// satAdd adds two non-negative token counts, clamping at MaxInt64 instead of wrapping negative.
// Every stored count is a bigint a worker may set arbitrarily high, so a sum can overflow.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// modelAcc accumulates one model's share of the tail.
type modelAcc struct {
	in, cacheRead, cacheCreate, out int64
	cost                            anthropicprice.Cost
	allPriced                       bool
}

// buildUsageTail turns the snapshot rows into the DTO: the coverage reasons (ADR-2014 D5), the
// token aggregates, and the per-message pricing roll-up.
func buildUsageTail(legs []store.ListRunUsageLegsForTailRow, tail []store.ListRunUsageTailMessagesRow, capReached bool) *apitypes.UsageTailDTO {
	reasons := map[string]bool{}
	superseded := supersededLegs(legs)
	for _, l := range legs {
		if superseded[l.LegID] {
			// A leg a resumed cumulative total absorbed is only trusted when we saw it end
			// and every message's final usage: the "cumulative includes the interrupted turn"
			// premise cannot be verified, so otherwise say so.
			if !l.ClosedThrough.Valid || l.AnyNotFinal {
				reasons[UsageTailReasonSupersededUncertain] = true
			}
		} else {
			if !l.ClosedThrough.Valid {
				reasons[UsageTailReasonLegNotClosed] = true
			}
			maxOrd := int64(l.MaxOrdinal)
			if l.ClosedThrough.Valid && int64(l.ClosedThrough.Int32) > maxOrd {
				maxOrd = int64(l.ClosedThrough.Int32)
			}
			if l.CoveredThrough.Valid && int64(l.CoveredThrough.Int32) > maxOrd {
				maxOrd = int64(l.CoveredThrough.Int32)
			}
			if l.DistinctOrdinals < maxOrd {
				reasons[UsageTailReasonOrdinalGap] = true
			}
		}
		if l.DroppedRecords > 0 {
			reasons[UsageTailReasonRecordsDropped] = true
		}
		// Messages the tail excludes: a conflicting re-post, or (in a leg with no init_seq or
		// sdk_session_id) a message its own leg's result did not cover.
		if l.NConflict > 0 || ((!l.InitSeq.Valid || !l.SdkSessionID.Valid) && l.NUncoveredOwn > 0) {
			reasons[UsageTailReasonUnresolved] = true
		}
	}
	if capReached {
		reasons[UsageTailReasonRecordCapReached] = true
	}

	dto := &apitypes.UsageTailDTO{
		PriceTableVersion: anthropicprice.AnthropicPriceTableVersion,
		CoverageReasons:   []string{},
		Models:            []apitypes.UsageTailModelDTO{},
	}
	byModel := map[string]*modelAcc{}
	var total anthropicprice.Cost
	allPriced := true
	for _, m := range tail {
		if !m.OutputFinal {
			reasons[UsageTailReasonOutputNotFinal] = true
		}
		dto.InputTokens = satAdd(dto.InputTokens, m.InputTokens)
		dto.CacheReadTokens = satAdd(dto.CacheReadTokens, m.CacheReadInputTokens)
		dto.CacheCreationTokens = satAdd(dto.CacheCreationTokens, m.CacheCreationInputTokens)
		dto.OutputTokens = satAdd(dto.OutputTokens, m.OutputTokens)
		acc := byModel[m.Model]
		if acc == nil {
			acc = &modelAcc{allPriced: true}
			byModel[m.Model] = acc
		}
		acc.in = satAdd(acc.in, m.InputTokens)
		acc.cacheRead = satAdd(acc.cacheRead, m.CacheReadInputTokens)
		acc.cacheCreate = satAdd(acc.cacheCreate, m.CacheCreationInputTokens)
		acc.out = satAdd(acc.out, m.OutputTokens)
		u := anthropicprice.Usage{
			Model:                    m.Model,
			InputTokens:              m.InputTokens,
			CacheReadInputTokens:     m.CacheReadInputTokens,
			CacheCreationInputTokens: m.CacheCreationInputTokens,
			OutputTokens:             m.OutputTokens,
			ServiceTier:              m.ServiceTier.String,
			Speed:                    m.Speed.String,
			InferenceGeo:             m.InferenceGeo.String,
		}
		if m.CacheCreation5mInputTokens.Valid {
			v := m.CacheCreation5mInputTokens.Int64
			u.CacheCreation5mTokens = &v
		}
		if m.CacheCreation1hInputTokens.Valid {
			v := m.CacheCreation1hInputTokens.Int64
			u.CacheCreation1hTokens = &v
		}
		c, ok := anthropicprice.Price(u)
		if !ok {
			allPriced = false
			acc.allPriced = false
			continue
		}
		total.Add(c)
		acc.cost.Add(c)
	}

	for _, r := range usageTailReasonOrder {
		if reasons[r] {
			dto.CoverageReasons = append(dto.CoverageReasons, r)
		}
	}
	dto.Coverage = "complete"
	if len(dto.CoverageReasons) > 0 {
		dto.Coverage = "partial"
	}

	// Unpriced is never zero: a tail with any unpriced message carries a null cost.
	dto.CostStatus = usageTailCostUnpriced
	if allPriced {
		dto.CostStatus = usageTailCostEstimated
		usd := total.USD()
		dto.CostUSD = &usd
	}
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, name := range models {
		acc := byModel[name]
		row := apitypes.UsageTailModelDTO{
			Model:               name,
			InputTokens:         acc.in,
			CacheReadTokens:     acc.cacheRead,
			CacheCreationTokens: acc.cacheCreate,
			OutputTokens:        acc.out,
			CostStatus:          usageTailCostUnpriced,
		}
		if acc.allPriced {
			row.CostStatus = usageTailCostEstimated
			usd := acc.cost.USD()
			row.CostUSD = &usd
		}
		dto.Models = append(dto.Models, row)
	}
	return dto
}
