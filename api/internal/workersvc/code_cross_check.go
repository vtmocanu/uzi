package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func validCodeSHA(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}

// SubmitCodeCrossCheck atomically freezes one local snapshot and creates its child.
// A lost ACK returns the exact attempt before resolving credentials again.
func (s *Service) SubmitCodeCrossCheck(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, base, head string) (store.CrossCheck, error) {
	if !validCodeSHA(base) || !validCodeSHA(head) || s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	timeout := s.p.CodeCrossCheckTimeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if timeout <= 0 || timeout > 2*time.Hour {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	var result store.CrossCheck
	retry := errors.New("existing code cross-check")
	lead, err := s.runOwnedByWorker(ctx, leadID, worker)
	if err != nil {
		return result, ErrCrossCheckRefused
	}
	harness, known := oppositeHarness(lead.Harness)
	if !known {
		return result, ErrCrossCheckRefused
	}
	// Check immutable retries before credential resolution, while holding the lead lock.
	tx, e := s.txBeginner.Begin(ctx)
	if e != nil {
		return result, e
	}
	retryQ := store.New(tx)
	owned, e := retryQ.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if e != nil || owned.UserID != worker.UserID || owned.ClaimGeneration != generation || owned.ClaimReleasedAt.Valid ||
		(owned.Status != "claimed" && owned.Status != "running") || !owned.CodeCrossCheckRequired {
		_ = tx.Rollback(ctx)
		return result, ErrCrossCheckRefused
	}
	prior, e := retryQ.GetCodeCrossCheck(ctx, leadID)
	if e == nil {
		_ = tx.Rollback(ctx)
		if prior.LeadClaimGeneration != generation || prior.InterruptedAt.Valid || prior.BaseCommit.String != base || prior.HeadCommit.String != head {
			return result, ErrCrossCheckInterrupted
		}
		return prior, nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return result, e
	}
	if e = tx.Commit(ctx); e != nil {
		return result, e
	}
	_, err = s.createRunResolved(ctx, lead.UserID, &harness, func(q Store, resolved resolvedHarness) (store.Run, error) {
		txq, ok := q.(*store.Queries)
		if !ok {
			return store.Run{}, ErrCrossCheckRefused
		}
		locked, e := txq.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
		if e != nil || locked.UserID != worker.UserID || locked.ClaimGeneration != generation || locked.ClaimReleasedAt.Valid ||
			(locked.Status != "claimed" && locked.Status != "running") || !locked.CodeCrossCheckRequired || !runkind.CodeCrossCheckable(locked.Kind) ||
			locked.Harness != lead.Harness || validateCrossCheckContext(locked.IssueTitle, locked.IssueDescription) != nil {
			return store.Run{}, ErrCrossCheckRefused
		}
		prior, e := txq.GetCodeCrossCheck(ctx, leadID)
		if e == nil {
			if prior.LeadClaimGeneration != generation || prior.InterruptedAt.Valid || prior.BaseCommit.String != base || prior.HeadCommit.String != head {
				return store.Run{}, ErrCrossCheckInterrupted
			}
			result = prior
			return store.Run{}, retry
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.Run{}, e
		}
		if !slices.Contains(worker.ProtocolCapabilities, capability.CrossCheckCodeV1) ||
			!slices.Contains(worker.ProtocolCapabilities, capability.CrossCheckLaneV1) || worker.IsolatedLane ||
			(harness == HarnessCodex && !slices.Contains(worker.ProtocolCapabilities, capability.CodexHarnessV1)) {
			return store.Run{}, ErrCrossCheckUnavailable
		}
		if resolved.Harness != harness {
			return store.Run{}, ErrCrossCheckUnavailable
		}
		contextBytes, e := json.Marshal(map[string]any{"issue_title": locked.IssueTitle, "issue_description": locked.IssueDescription})
		if e != nil {
			return store.Run{}, e
		}
		digest, e := codeCandidateDigest(store.CrossCheck{BaseCommit: pgtype.Text{String: base, Valid: true}, HeadCommit: pgtype.Text{String: head, Valid: true}, CodeContext: contextBytes, PlanMd: locked.PlanMd, Milestones: locked.MilestonesFrozen, RequiredCapabilities: locked.RequiredCapabilities, RequiredTools: locked.RequiredTools, SizeClass: pgtype.Text{String: locked.SizeClass, Valid: true}, GuidanceSnapshot: pgtype.Text{Valid: true}})
		if e != nil {
			return store.Run{}, e
		}
		child, e := txq.CreateCodeCrossCheckChild(ctx, store.CreateCodeCrossCheckChildParams{ChildID: uuid.New(), ChildHarness: string(harness), LeadRunID: leadID, UserID: locked.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, BudgetWallSeconds: int32((timeout + time.Second - 1) / time.Second)})
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				e = ErrCrossCheckUnavailable
			}
			return store.Run{}, e
		}
		result, e = txq.InsertCodeCrossCheck(ctx, store.InsertCodeCrossCheckParams{LeadRunID: leadID, CheckerRunID: child.ID, BaseCommit: base, HeadCommit: head, CandidateDigest: digest, CodeContext: contextBytes, DeadlineAt: pgtype.Timestamptz{Time: s.now().Add(timeout), Valid: true}})
		return child, e
	})
	if errors.Is(err, retry) {
		return result, nil
	}
	if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) || errors.Is(err, ErrCrossCheckUnavailable) {
		return s.RecordCodeCrossCheckFailure(ctx, worker, leadID, generation, base, head, "checker_unavailable")
	}
	if err != nil {
		return result, err
	}
	s.notify(result.CheckerRunID.Bytes, "queued")
	return result, nil
}

// CodeCrossCheckStatus reads persisted evidence, including a prior interrupted attempt.
func (s *Service) CodeCrossCheckStatus(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64) (store.CrossCheck, error) {
	if s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid || (lead.Status != "claimed" && lead.Status != "running") {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	expired, err := q.ExpireCodeCrossCheck(ctx, store.ExpireCodeCrossCheckParams{LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation})
	if err == nil {
		_, err = q.BankCodeCrossCheckWait(ctx, expired.ID)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, err
	}
	dead, deadErr := q.FailDeadCodeCrossCheckChild(ctx, store.FailDeadCodeCrossCheckChildParams{LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation})
	if deadErr == nil {
		_, deadErr = q.BankCodeCrossCheckWait(ctx, dead.ID)
	}
	if deadErr != nil && !errors.Is(deadErr, pgx.ErrNoRows) {
		return store.CrossCheck{}, deadErr
	}
	cc, err := q.GetCodeCrossCheck(ctx, leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return cc, ErrCrossCheckNoRow
	}
	if err != nil {
		return cc, err
	}
	if err = tx.Commit(ctx); err != nil {
		return cc, err
	}
	return cc, nil
}

func (s *Service) DecideCodeCrossCheck(ctx context.Context, worker store.Worker, childID uuid.UUID, generation int64, outcome, reason string, findings []CodeCrossCheckFinding) (store.CrossCheck, error) {
	raw, err := NormalizeCodeCrossCheckFindings(outcome, reason, findings)
	if err != nil || s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.LockCodeCrossCheckLeadForVerdict(ctx, store.LockCodeCrossCheckLeadForVerdictParams{ChildID: childID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation})
	if err != nil || lead.UserID != worker.UserID {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	cc, err := q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{ChildID: childID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, Outcome: outcome, ReasonClass: pgconv.TextPtr(reasonPtr), Findings: raw})
	if err != nil {
		return cc, ErrCrossCheckRefused
	}
	if _, err = q.BankCodeCrossCheckWait(ctx, cc.ID); err != nil {
		return cc, err
	}
	if err = tx.Commit(ctx); err != nil {
		return cc, err
	}
	return cc, nil
}
