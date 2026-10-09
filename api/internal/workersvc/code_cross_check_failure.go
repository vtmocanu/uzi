package workersvc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// RecordCodeCrossCheckFailure retains a verified pair, or records an allowlisted
// pre-snapshot failure with neither SHA. Retries cannot replace a frozen attempt.
func (s *Service) RecordCodeCrossCheckFailure(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, base, head, reason string) (store.CrossCheck, error) {
	var result store.CrossCheck
	noSnapshot := base == "" && head == ""
	if s.txBeginner == nil || (noSnapshot && reason != "snapshot_failed" && reason != "worker_unsupported") ||
		(!noSnapshot && (!validCodeSHA(base) || !validCodeSHA(head) || reason != "checker_unavailable")) {
		return result, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation ||
		lead.ClaimReleasedAt.Valid || (lead.Status != "claimed" && lead.Status != "running") ||
		!lead.CodeCrossCheckRequired || !runkind.CodeCrossCheckable(lead.Kind) ||
		lead.ReportOnly || lead.FixVerdict.String == "not_code" {
		return result, ErrCrossCheckRefused
	}
	prior, err := q.GetCodeCrossCheck(ctx, leadID)
	if err == nil {
		if prior.LeadClaimGeneration != generation || prior.InterruptedAt.Valid ||
			prior.BaseCommit.String != base || prior.HeadCommit.String != head ||
			(noSnapshot && prior.ReasonClass.String != reason) {
			return result, ErrCrossCheckInterrupted
		}
		return prior, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	params := store.InsertFailedCodeCrossCheckParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), UserID: worker.UserID,
		ClaimGeneration: generation, ReasonClass: reason, CodeContext: []byte("{}"),
	}
	if !noSnapshot {
		params.BaseCommit = pgtype.Text{String: base, Valid: true}
		params.HeadCommit = pgtype.Text{String: head, Valid: true}
		params.CodeContext, err = json.Marshal(map[string]any{"issue_title": lead.IssueTitle, "issue_description": lead.IssueDescription})
		if err != nil {
			return result, err
		}
		params.CandidateDigest, err = codeCandidateDigest(store.CrossCheck{
			BaseCommit: params.BaseCommit, HeadCommit: params.HeadCommit, CodeContext: params.CodeContext,
			PlanMd: lead.PlanMd, Milestones: lead.MilestonesFrozen, RequiredCapabilities: lead.RequiredCapabilities,
			RequiredTools: lead.RequiredTools, SizeClass: pgtype.Text{String: lead.SizeClass, Valid: true}, GuidanceSnapshot: pgtype.Text{Valid: true},
		})
		if err != nil {
			return result, err
		}
	}
	result, err = q.InsertFailedCodeCrossCheck(ctx, params)
	if err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}
