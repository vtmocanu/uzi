package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Optional to preserve Store implementations used by ordinary run reads.
type planCrossCheckSummaryReader interface {
	GetLatestPlanCrossCheckSummary(context.Context, store.GetLatestPlanCrossCheckSummaryParams) (store.GetLatestPlanCrossCheckSummaryRow, error)
}

// PlanCrossCheckSummary reads owner-only HUD metadata, without approval authority
// or lifecycle writes. Ownership and child eligibility are enforced by the query.
func (s *Service) PlanCrossCheckSummary(ctx context.Context, userID, leadID uuid.UUID) (*apitypes.PlanCrossCheckSummaryDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q, ok := s.q.(planCrossCheckSummaryReader)
	if !ok {
		return nil, nil
	}
	row, err := q.GetLatestPlanCrossCheckSummary(ctx, store.GetLatestPlanCrossCheckSummaryParams{LeadRunID: leadID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dto := &apitypes.PlanCrossCheckSummaryDTO{
		CheckerModelSource:  textPtr(row.CheckerModelSource),
		CheckerEffortSource: textPtr(row.CheckerEffortSource),
		Round:               row.Round, Verdict: scrubThenBound(row.Verdict, 512),
		ReasonClass:   planCrossCheckSummaryLabel(row.ReasonClass),
		CheckerModel:  planCrossCheckSummaryLabel(row.CheckerModel),
		CheckerEffort: planCrossCheckSummaryLabel(row.CheckerEffort),
		Historical:    row.Historical,
	}
	// JSONB adds insignificant whitespace. Compact before the writer's 32 KiB
	// validation, keeping a separate 64 KiB input limit for legacy/corrupt rows.
	if len(row.Findings) <= 64*1024 {
		var compact bytes.Buffer
		if json.Compact(&compact, row.Findings) == nil {
			if clean, err := NormalizeCrossCheckFindings(row.Verdict, row.ReasonClass.String, compact.Bytes()); err == nil {
				var findings apitypes.PlanCrossCheckFindingsDTO
				if json.Unmarshal(clean, &findings) == nil {
					dto.Findings = &findings
				}
			}
		}
	}
	if row.HasChild && row.CheckerRunID.Valid {
		id := uuid.UUID(row.CheckerRunID.Bytes).String()
		dto.CheckerRunID = &id
		if row.HasUsage {
			status, cost := costStatusUnreported, float64(0)
			switch row.CostStatus.String {
			case costStatusSubscription:
				if row.CostStatus.Valid {
					status = costStatusSubscription
				}
			case costStatusMetered:
				value, err := row.CostUsd.Float64Value()
				if row.CostStatus.Valid && err == nil && value.Valid && !math.IsNaN(value.Float64) && !math.IsInf(value.Float64, 0) && value.Float64 >= 0 {
					status, cost = costStatusMetered, value.Float64
				}
			}
			dto.Usage = &apitypes.UsageDTO{
				InputTokens: row.InputTokens, CacheReadTokens: row.CacheReadTokens,
				CacheCreationTokens: row.CacheCreationTokens, OutputTokens: row.OutputTokens,
				CostUSD: cost, CostStatus: status,
			}
		}
	}
	return dto, nil
}

func planCrossCheckSummaryLabel(text pgtype.Text) *string {
	if !text.Valid {
		return nil
	}
	value := scrubThenBound(text.String, 512)
	// SanitizeBounded appends whole runes before checking its cap. Remove any
	// overflow after scrubbing; at most the final four-byte rune can cross it.
	if len(value) > 512 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return &value
}
