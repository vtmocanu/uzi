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
	dto.Findings = planCrossCheckFindings(row.Verdict, row.ReasonClass.String, row.Findings)
	if err := s.attachPlanCrossCheckRounds(ctx, userID, leadID, dto); err != nil {
		return nil, err
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

// planCrossCheckFindings bounds and normalizes one round's stored findings. JSONB adds
// insignificant whitespace, so compact before the writer's 32 KiB validation, keeping a
// separate 64 KiB input limit for legacy/corrupt rows. It returns nil for anything invalid.
func planCrossCheckFindings(verdict, reason string, raw []byte) *apitypes.PlanCrossCheckFindingsDTO {
	if len(raw) > 64*1024 {
		return nil
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil
	}
	// An unstored approval retains its original bounded findings as history.
	// Normalize the prose using its original outcome, without allowing the
	// checker verdict route to submit this server-only reason.
	if verdict == "failed" && reason == "approved_not_stored" {
		verdict, reason = "approve", "approve"
	}
	clean, err := NormalizeCrossCheckFindings(verdict, reason, compact.Bytes())
	if err != nil {
		return nil
	}
	var findings apitypes.PlanCrossCheckFindingsDTO
	if json.Unmarshal(clean, &findings) != nil {
		return nil
	}
	return &findings
}

// planCrossCheckRoundsReader and planCrossCheckLeadReader are optional so Store
// implementations used by ordinary run reads keep compiling.
type planCrossCheckRoundsReader interface {
	ListPlanCrossCheckRounds(context.Context, store.ListPlanCrossCheckRoundsParams) ([]store.ListPlanCrossCheckRoundsRow, error)
	GetRunByID(context.Context, uuid.UUID) (store.Run, error)
}

// attachPlanCrossCheckRounds adds the newest five rounds (chronological) with each round's
// own bounded response, and the pending deadline. plan_md is read only to extract the lead's
// "Cross-check dispositions" from the FOLLOWING candidate; it never reaches the DTO.
func (s *Service) attachPlanCrossCheckRounds(ctx context.Context, userID, leadID uuid.UUID, dto *apitypes.PlanCrossCheckSummaryDTO) error {
	q, ok := s.q.(planCrossCheckRoundsReader)
	if !ok {
		return nil
	}
	rows, err := q.ListPlanCrossCheckRounds(ctx, store.ListPlanCrossCheckRoundsParams{LeadRunID: leadID, UserID: userID})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	// The query is newest-first so the cap keeps the latest rounds; present oldest-first.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	var lead *store.Run
	if run, err := q.GetRunByID(ctx, leadID); err == nil && run.UserID == userID {
		lead = &run
	}
	dto.Rounds = make([]apitypes.PlanCrossCheckRoundDTO, 0, len(rows))
	for i := range rows {
		row := rows[i]
		var following *store.ListPlanCrossCheckRoundsRow
		if i+1 < len(rows) && rows[i+1].Round == row.Round+1 {
			following = &rows[i+1]
		}
		round := apitypes.PlanCrossCheckRoundDTO{
			Round: row.Round, Verdict: scrubThenBound(row.Verdict, 512),
			ReasonClass: planCrossCheckSummaryLabel(row.ReasonClass),
			Findings:    planCrossCheckFindings(row.Verdict, row.ReasonClass.String, row.Findings),
			Response:    crossCheckRoundResponse(row, following, lead),
		}
		if row.DecidedAt.Valid {
			decided := row.DecidedAt.Time
			round.DecidedAt = &decided
		}
		dto.Rounds = append(dto.Rounds, round)
	}
	if last := rows[len(rows)-1]; last.Verdict == "pending" && last.DeadlineAt.Valid {
		deadline := last.DeadlineAt.Time
		dto.DeadlineAt = &deadline
	}
	return nil
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
