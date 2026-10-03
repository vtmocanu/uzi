package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrCrossCheckRefused = errors.New("plan cross-check refused")

// PlanCrossCheckCandidate contains only the fields the checker approves. The caller
// scrubs untrusted text before this value is stored and digested.
type PlanCrossCheckCandidate struct {
	PlanMd               string          `json:"plan_md"`
	Milestones           json.RawMessage `json:"milestones"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	RequiredTools        []string        `json:"required_tools"`
	SizeClass            string          `json:"size_class"`
	BaseCommit           string          `json:"base_commit"`
	PlanningDiff         string          `json:"planning_diff"`
}

func (c PlanCrossCheckCandidate) Digest() ([]byte, error) {
	milestones, err := canonicalRawJSON(c.Milestones)
	if err != nil {
		return nil, err
	}
	payload := struct {
		PlanMd               string          `json:"plan_md"`
		Milestones           json.RawMessage `json:"milestones"`
		RequiredCapabilities []string        `json:"required_capabilities"`
		RequiredTools        []string        `json:"required_tools"`
		SizeClass            string          `json:"size_class"`
		BaseCommit           string          `json:"base_commit"`
		PlanningDiff         string          `json:"planning_diff"`
	}{c.PlanMd, milestones, sortedSet(c.RequiredCapabilities), sortedSet(c.RequiredTools), c.SizeClass, c.BaseCommit, c.PlanningDiff}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func (s *Service) SubmitPlanCrossCheck(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, candidate PlanCrossCheckCandidate) (store.CrossCheck, error) {
	if len(candidate.PlanMd) == 0 || len(candidate.PlanMd) > 256*1024 || len(candidate.PlanningDiff) > 512*1024 ||
		len(candidate.Milestones) > 256*1024 || len(candidate.BaseCommit) != 40 || strings.Trim(candidate.BaseCommit, "0123456789abcdefABCDEF") != "" ||
		candidate.SizeClass == "" || len(candidate.RequiredCapabilities) > 64 || len(candidate.RequiredTools) > 64 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	var milestones []json.RawMessage
	if err := json.Unmarshal(candidate.Milestones, &milestones); err != nil || len(milestones) > 64 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	candidate.RequiredCapabilities = sortedSet(candidate.RequiredCapabilities)
	candidate.RequiredTools = sortedSet(candidate.RequiredTools)
	canonical, err := canonicalRawJSON(candidate.Milestones)
	if err != nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	candidate.Milestones = canonical
	digest, err := candidate.Digest()
	if err != nil {
		return store.CrossCheck{}, err
	}
	lead, err := s.runOwnedByWorker(ctx, leadID, worker)
	if err != nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if lead.Harness != string(HarnessClaude) || !lead.PlanCrossCheckRequired || !lead.AutoApprove ||
		(lead.Status != "claimed" && lead.Status != "running") {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	// A retry is read under the lead lock in the creation closure below. This
	// availability read is repeated inside createRunAtomic by the explicit resolver.
	childHarness := HarnessCodex
	var existing store.CrossCheck
	errRetry := errors.New("existing plan cross-check")
	_, err = s.createRunResolved(ctx, lead.UserID, &childHarness, func(q Store, resolved resolvedHarness) (store.Run, error) {
		txq, ok := q.(*store.Queries)
		if !ok {
			return store.Run{}, ErrCrossCheckRefused
		}
		locked, e := txq.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
		if e != nil {
			return store.Run{}, ErrCrossCheckRefused
		}
		if locked.UserID != worker.UserID || locked.ClaimGeneration != generation || locked.Harness != string(HarnessClaude) ||
			!locked.PlanCrossCheckRequired || !locked.AutoApprove || locked.ClaimReleasedAt.Valid ||
			(locked.Status != "claimed" && locked.Status != "running") {
			return store.Run{}, ErrCrossCheckRefused
		}
		prior, e := txq.GetPlanCrossCheck(ctx, leadID)
		if e == nil {
			if prior.Verdict != "pending" || prior.LeadClaimGeneration != generation || !bytes.Equal(prior.CandidateDigest, digest) {
				return store.Run{}, ErrCrossCheckRefused
			}
			existing = prior
			return store.Run{}, errRetry
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.Run{}, e
		}
		if resolved.Harness != HarnessCodex {
			return store.Run{}, ErrCrossCheckRefused
		}
		childID := uuid.New()
		child, e := txq.CreatePlanCrossCheckChild(ctx, store.CreatePlanCrossCheckChildParams{
			ChildID: childID, LeadRunID: leadID, UserID: lead.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation,
		})
		if e != nil {
			return store.Run{}, e
		}
		existing, e = txq.InsertPlanCrossCheck(ctx, store.InsertPlanCrossCheckParams{
			LeadRunID: leadID, LeadClaimGeneration: generation, PlanMd: pgtype.Text{String: candidate.PlanMd, Valid: true},
			Milestones: candidate.Milestones, RequiredCapabilities: candidate.RequiredCapabilities, RequiredTools: candidate.RequiredTools,
			SizeClass: pgtype.Text{String: candidate.SizeClass, Valid: true}, BaseCommit: pgtype.Text{String: candidate.BaseCommit, Valid: true},
			PlanningDiff: pgtype.Text{String: candidate.PlanningDiff, Valid: true}, CandidateDigest: digest,
			CheckerRunID: pgconv.UUID(child.ID),
		})
		if e != nil {
			return store.Run{}, e
		}
		return child, nil
	})
	if errors.Is(err, errRetry) {
		return existing, nil
	}
	if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if err != nil {
		return store.CrossCheck{}, fmt.Errorf("create plan cross-check: %w", err)
	}
	s.notify(existing.CheckerRunID.Bytes, "queued")
	return existing, nil
}

func (s *Service) PlanCrossCheckStatus(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, round int32) (store.CrossCheck, error) {
	if round != 1 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: leadID, WorkerID: pgconv.UUID(worker.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if err != nil {
		return store.CrossCheck{}, err
	}
	if lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid ||
		(lead.Status != "claimed" && lead.Status != "running") {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	_, err = q.ExpirePlanCrossCheck(ctx, store.ExpirePlanCrossCheckParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, err
	}
	cc, err := q.GetOwnedPlanCrossCheck(ctx, store.GetOwnedPlanCrossCheckParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, Round: round,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return cc, ErrCrossCheckRefused
	}
	if err != nil {
		return cc, err
	}
	if err := tx.Commit(ctx); err != nil {
		return cc, err
	}
	return cc, nil
}

func (s *Service) DecidePlanCrossCheck(ctx context.Context, worker store.Worker, childID uuid.UUID, generation int64, verdict, reason string, findings []byte) (store.CrossCheck, error) {
	if verdict != "approve" && verdict != "revise" && verdict != "block" && verdict != "failed" {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.LockPlanCrossCheckLeadForVerdict(ctx, store.LockPlanCrossCheckLeadForVerdictParams{
		ChildID: childID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if err != nil {
		return store.CrossCheck{}, err
	}
	cc, err := q.DecidePlanCrossCheck(ctx, store.DecidePlanCrossCheckParams{
		Verdict: verdict, ReasonClass: pgtype.Text{String: reason, Valid: true}, Findings: findings,
		ChildID: childID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return cc, ErrCrossCheckRefused
	}
	if err != nil {
		return cc, err
	}
	banked, err := q.BankPlanCrossCheckWait(ctx, cc.ID)
	if err != nil {
		return cc, err
	}
	if banked != 1 {
		return cc, ErrCrossCheckRefused
	}
	payload, err := json.Marshal(map[string]any{"stage": "plan", "verdict": verdict, "reason_class": reason, "findings": json.RawMessage(findings), "checker_run_id": childID})
	if err != nil {
		return cc, err
	}
	inserted := false
	var seq int32
	for attempt := 0; attempt < 32; attempt++ {
		next, nextErr := q.NextPlanCrossCheckMessageSeq(ctx, store.NextPlanCrossCheckMessageSeqParams{
			LeadRunID: lead.ID, ClaimGeneration: lead.ClaimGeneration,
		})
		if nextErr != nil {
			return cc, nextErr
		}
		seq = next + int32(attempt)
		result, insertErr := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID: lead.ID, Seq: seq, Kind: "cross_check", Payload: payload,
			ClaimGeneration: pgconv.Int8Ptr(&lead.ClaimGeneration),
		})
		if insertErr != nil {
			return cc, insertErr
		}
		if result.Inserted {
			inserted = true
			break
		}
	}
	if !inserted {
		return cc, ErrCrossCheckRefused
	}
	advanced, err := q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{
		ID: lead.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&lead.ClaimGeneration),
	})
	if err != nil {
		return cc, err
	}
	if advanced != 1 {
		return cc, ErrCrossCheckRefused
	}
	if err = tx.Commit(ctx); err != nil {
		return cc, err
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(lead.ID, seq, "cross_check", "", "", "", payload, s.now())
	}
	return cc, nil
}
