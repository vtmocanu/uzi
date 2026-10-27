package workersvc

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

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

// codeCrossCheckCustodyStatus admits finalization of an interactive task without
// consuming an owner follow-up or changing its persisted waiting status.
func codeCrossCheckCustodyStatus(lead store.Run) bool {
	return lead.Status == "claimed" || lead.Status == "running" ||
		(lead.Status == "awaiting_followup" && lead.Kind == runkind.Task && lead.Interactive)
}

// SubmitCodeCrossCheck atomically freezes one local snapshot and creates its child.
// A lost ACK returns the exact attempt before resolving credentials again.
func (s *Service) SubmitCodeCrossCheck(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, base, head string, inputs ...CodeCrossCheckInstructions) (store.CrossCheck, error) {
	input, inputErr := codeCrossCheckInstructions(inputs)
	if inputErr != nil {
		return store.CrossCheck{}, inputErr
	}
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
	timeoutSeconds := int64((timeout + time.Second - 1) / time.Second)
	if timeoutSeconds < 1 || timeoutSeconds > 7200 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	budgetWallSeconds := int32(timeoutSeconds)
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
		!codeCrossCheckCustodyStatus(owned) || !owned.CodeCrossCheckRequired {
		_ = tx.Rollback(ctx)
		return result, ErrCrossCheckRefused
	}
	prior, e := retryQ.GetCodeCrossCheck(ctx, leadID)
	if e == nil {
		_ = tx.Rollback(ctx)
		if prior.LeadClaimGeneration != generation || prior.InterruptedAt.Valid || prior.BaseCommit.String != base || prior.HeadCommit.String != head || !input.matches(prior) {
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
			!codeCrossCheckCustodyStatus(locked) || !locked.CodeCrossCheckRequired || !runkind.CodeCrossCheckable(locked.Kind) ||
			locked.Harness != lead.Harness || validateCrossCheckContext(locked.IssueTitle, locked.IssueDescription) != nil {
			return store.Run{}, ErrCrossCheckRefused
		}
		prior, e := txq.GetCodeCrossCheck(ctx, leadID)
		if e == nil {
			if prior.LeadClaimGeneration != generation || prior.InterruptedAt.Valid || prior.BaseCommit.String != base || prior.HeadCommit.String != head || !input.matches(prior) {
				return store.Run{}, ErrCrossCheckInterrupted
			}
			result = prior
			return store.Run{}, retry
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.Run{}, e
		}
		frozen, e := freezeCodeCrossCheckInputs(ctx, txq, locked, input)
		if e != nil {
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
		frozen.BaseCommit, frozen.HeadCommit = pgtype.Text{String: base, Valid: true}, pgtype.Text{String: head, Valid: true}
		frozen.CodeContext = contextBytes
		digest, e := codeCandidateDigest(frozen)
		if e != nil {
			return store.Run{}, e
		}
		child, e := txq.CreateCodeCrossCheckChild(ctx, store.CreateCodeCrossCheckChildParams{ChildID: uuid.New(), ChildHarness: string(harness), LeadRunID: leadID, UserID: locked.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, BudgetWallSeconds: budgetWallSeconds})
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				e = ErrCrossCheckUnavailable
			}
			return store.Run{}, e
		}
		result, e = txq.InsertCodeCrossCheck(ctx, store.InsertCodeCrossCheckParams{LeadRunID: leadID, CheckerRunID: child.ID, BaseCommit: base, HeadCommit: head, CandidateDigest: digest, CodeContext: contextBytes, GuidanceText: frozen.GuidanceText.String, RepoInstructionsCommit: frozen.RepoInstructionsCommit, RepoInstructionsEnabled: frozen.RepoInstructionsEnabled, RepoInstructionsText: frozen.RepoInstructionsText.String, DeadlineAt: pgtype.Timestamptz{Time: s.now().Add(timeout), Valid: true}})
		return child, e
	})
	if errors.Is(err, retry) {
		return result, nil
	}
	if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) || errors.Is(err, ErrCrossCheckUnavailable) {
		return s.RecordCodeCrossCheckFailure(ctx, worker, leadID, generation, base, head, "checker_unavailable", input)
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
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid || !codeCrossCheckCustodyStatus(lead) {
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

func (s *Service) DecideCodeCrossCheck(ctx context.Context, worker store.Worker, childID uuid.UUID, generation int64, outcome, reason string, findings []CodeCrossCheckFinding, coverage ...json.RawMessage) (store.CrossCheck, error) {
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
	cc, err := q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{ChildID: childID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, Outcome: outcome, ReasonClass: pgconv.TextPtr(reasonPtr), Findings: raw, Coverage: NormalizeCodeCrossCheckCoverage(outcome, coverage...)})
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

// CodeCrossCheckInstructions is worker-provided text plus the consent observed
// before reading it. An absent expected consent means off for legacy callers.
type CodeCrossCheckInstructions struct {
	RepoInstructionsCommit  *string
	RepoInstructionsEnabled *bool
	RepoInstructionsText    string
}

func codeCrossCheckInstructions(inputs []CodeCrossCheckInstructions) (CodeCrossCheckInstructions, error) {
	var input CodeCrossCheckInstructions
	if len(inputs) > 1 {
		return input, ErrCrossCheckRefused
	}
	if len(inputs) == 1 {
		input = inputs[0]
	}
	if (input.RepoInstructionsCommit != nil && !validCodeSHA(*input.RepoInstructionsCommit)) || len(input.RepoInstructionsText) > 65536 || !utf8.ValidString(input.RepoInstructionsText) ||
		(!input.enabled() && input.RepoInstructionsText != "") {
		return input, ErrCrossCheckRefused
	}
	return input, nil
}
func (input CodeCrossCheckInstructions) enabled() bool {
	return input.RepoInstructionsEnabled != nil && *input.RepoInstructionsEnabled
}
func (input CodeCrossCheckInstructions) matches(cc store.CrossCheck) bool {
	return pgconv.TextPtr(input.RepoInstructionsCommit) == cc.RepoInstructionsCommit && input.enabled() == cc.RepoInstructionsEnabled && input.RepoInstructionsText == cc.RepoInstructionsText.String
}
func freezeCodeCrossCheckInputs(ctx context.Context, q *store.Queries, lead store.Run, input CodeCrossCheckInstructions) (store.CrossCheck, error) {
	var cc store.CrossCheck
	settings, err := q.GetCodeCrossCheckRepoInputsForUpdate(ctx, store.GetCodeCrossCheckRepoInputsForUpdateParams{RepoID: uuid.UUID(lead.RepoID.Bytes), UserID: lead.UserID})
	if err != nil {
		return cc, ErrCrossCheckRefused
	}
	if settings.CrossCheckRepoInstructionsEnabled != input.enabled() {
		return cc, ErrCrossCheckRefused
	}
	guidance := settings.CodeCrossCheckGuidance.String
	if len(guidance) > 8192 || !utf8.ValidString(guidance) {
		return cc, ErrCrossCheckRefused
	}
	cc.GuidanceSnapshot = pgtype.Text{String: guidance, Valid: true}
	cc.GuidanceText = cc.GuidanceSnapshot
	guidanceDigest := sha256.Sum256([]byte(guidance))
	cc.GuidanceDigest = guidanceDigest[:]
	cc.RepoInstructionsCommit = pgconv.TextPtr(input.RepoInstructionsCommit)
	cc.RepoInstructionsEnabled = input.enabled()
	cc.RepoInstructionsText = pgtype.Text{String: input.RepoInstructionsText, Valid: true}
	instructionsDigest := sha256.Sum256([]byte(input.RepoInstructionsText))
	cc.RepoInstructionsDigest = instructionsDigest[:]
	cc.PlanMd, cc.Milestones = lead.PlanMd, lead.MilestonesFrozen
	cc.RequiredCapabilities, cc.RequiredTools = lead.RequiredCapabilities, lead.RequiredTools
	cc.SizeClass = pgtype.Text{String: lead.SizeClass, Valid: true}
	return cc, nil
}

// CodeCrossCheckRepoInstructionsEnabled authorizes a fresh snapshot read using
// the same live worker ownership, custody and generation guard as code status.
func (s *Service) CodeCrossCheckRepoInstructionsEnabled(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64) (bool, error) {
	if s.txBeginner == nil {
		return false, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid || !codeCrossCheckCustodyStatus(lead) || !lead.CodeCrossCheckRequired || !runkind.CodeCrossCheckable(lead.Kind) || lead.ReportOnly || lead.FixVerdict.String == "not_code" {
		return false, ErrCrossCheckRefused
	}
	// A pending or terminal attempt cannot grant fresh file-read authority.
	if _, err = q.GetCodeCrossCheck(ctx, leadID); !errors.Is(err, pgx.ErrNoRows) {
		return false, ErrCrossCheckRefused
	}
	settings, err := q.GetCodeCrossCheckRepoInputsForUpdate(ctx, store.GetCodeCrossCheckRepoInputsForUpdateParams{RepoID: uuid.UUID(lead.RepoID.Bytes), UserID: lead.UserID})
	if err != nil {
		return false, ErrCrossCheckRefused
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return settings.CrossCheckRepoInstructionsEnabled, nil
}
