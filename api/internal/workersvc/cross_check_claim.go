package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ClaimPlanCrossCheck carries the stored candidate only. Credentials stay in the
// existing child claim's worker-owned secret envelope, never in this model input.
type ClaimPlanCrossCheck struct {
	ModelSource     *string   `json:"model_source,omitempty"`
	EffortSource    *string   `json:"effort_source,omitempty"`
	Stage           string    `json:"stage"`
	LeadRunID       string    `json:"lead_run_id"`
	Round           int32     `json:"round"`
	CandidateDigest string    `json:"candidate_digest"`
	DeadlineAt      time.Time `json:"deadline_at"`
	PlanCrossCheckCandidate
}

func crossCheckClaimInput(cc store.CrossCheck) (*ClaimPlanCrossCheck, error) {
	c := PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones,
		RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools,
		SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}
	var milestones []json.RawMessage
	if cc.Stage != "plan" || cc.Round != 1 || !cc.PlanMd.Valid || len(c.PlanMd) == 0 || len(c.PlanMd) > 256*1024 ||
		len(c.Milestones) > 256*1024 || json.Unmarshal(c.Milestones, &milestones) != nil || milestones == nil || len(milestones) > 64 ||
		len(c.PlanningDiff) > 512*1024 || len(c.BaseCommit) != 40 || strings.Trim(c.BaseCommit, "0123456789abcdefABCDEF") != "" ||
		len(c.RequiredCapabilities) > 64 || len(c.RequiredTools) > 64 || !cc.DeadlineAt.Valid {
		return nil, ErrCrossCheckRefused
	}
	digest, err := c.Digest()
	if err != nil || !bytes.Equal(digest, cc.CandidateDigest) {
		return nil, ErrCrossCheckRefused
	}
	return &ClaimPlanCrossCheck{Stage: cc.Stage, LeadRunID: cc.LeadRunID.String(), Round: cc.Round,
		CandidateDigest: hex.EncodeToString(cc.CandidateDigest), DeadlineAt: cc.DeadlineAt.Time,
		PlanCrossCheckCandidate: c, ModelSource: textPtr(cc.CheckerModelSource), EffortSource: textPtr(cc.CheckerEffortSource)}, nil
}

func (s *Service) assemblePlanCrossCheckInput(ctx context.Context, worker store.Worker, run store.Run, payload *ClaimPayload, resolutions ...*agenttmpl.CrossCheckResolution) error {
	if validateCrossCheckContext(run.IssueTitle, run.IssueDescription) != nil ||
		validateCrossCheckContext(payload.IssueTitle, payload.IssueDescription) != nil {
		return ErrCrossCheckRefused
	}
	if s.txBeginner == nil || run.UserID != worker.UserID || run.Harness != string(HarnessCodex) || !run.ReportOnly ||
		payload.Secrets.Codex == nil || payload.Secrets.AnthropicOAuthToken != "" {
		return ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	_, err = q.LockPlanCrossCheckLeadForVerdict(ctx, store.LockPlanCrossCheckLeadForVerdictParams{
		ChildID: run.ID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: run.ClaimGeneration})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCrossCheckRefused
	}
	if err != nil {
		return err
	}
	locked, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: run.ID, WorkerID: pgconv.UUID(worker.ID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCrossCheckRefused
	}
	if err != nil {
		return err
	}
	if validateCrossCheckContext(locked.IssueTitle, locked.IssueDescription) != nil {
		return ErrCrossCheckRefused
	}
	var modelSource, effortSource *string
	if len(resolutions) > 0 && resolutions[0] != nil {
		modelSource, effortSource = &resolutions[0].ModelSource, &resolutions[0].EffortSource
	}
	cc, err := q.RecordPlanCrossCheckClaim(ctx, store.RecordPlanCrossCheckClaimParams{
		ChildID: run.ID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: run.ClaimGeneration,
		CheckerModel: pgconv.TextPtr(payload.Config.DefaultModel), CheckerEffort: pgconv.TextPtr(payload.Config.DefaultEffort),
		CheckerModelSource: pgconv.TextPtr(modelSource), CheckerEffortSource: pgconv.TextPtr(effortSource)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCrossCheckRefused
	}
	if err != nil {
		return err
	}
	input, err := crossCheckClaimInput(cc)
	if err != nil {
		return err
	}
	payload.CrossCheck = input
	if _, err := MarshalCrossCheckClaim(payload); err != nil {
		payload.CrossCheck = nil
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	payload.CrossCheck = input
	return nil
}
