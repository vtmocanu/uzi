package workersvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrCrossCheckRefused = errors.New("plan cross-check refused")
var ErrCrossCheckInterrupted = fmt.Errorf("%w: interrupted", ErrCrossCheckRefused)
var ErrCrossCheckNoRow = fmt.Errorf("%w: no candidate", ErrCrossCheckRefused)
var ErrCrossCheckRevisionsExhausted = fmt.Errorf("%w: revisions exhausted", ErrCrossCheckRefused)
var ErrCrossCheckUnavailable = fmt.Errorf("%w: checker unavailable", ErrCrossCheckRefused)

// PlanCrossCheckCandidate contains only the fields the checker approves. The caller
// scrubs untrusted text before this value is stored and digested.
type PlanCrossCheckCandidate struct {
	CandidateIdentityVersion string                      `json:"candidate_identity_version,omitempty"`
	SemanticCandidate        *apitypes.SemanticCandidate `json:"semantic_candidate,omitempty"`
	PlanMd                   string                      `json:"plan_md"`
	Milestones               json.RawMessage             `json:"milestones"`
	RequiredCapabilities     []string                    `json:"required_capabilities"`
	RequiredTools            []string                    `json:"required_tools"`
	SizeClass                string                      `json:"size_class"`
	BaseCommit               string                      `json:"base_commit"`
	PlanningDiff             string                      `json:"planning_diff"`
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

// oppositeHarness is the family a lead's plan checker must run on: claude for a codex
// lead, codex for a claude lead (PRD #2460). The second result is false for any other
// harness, so an unknown lead can never be cross-checked.
func oppositeHarness(lead string) (Harness, bool) {
	switch lead {
	case string(HarnessClaude):
		return HarnessCodex, true
	case string(HarnessCodex):
		return HarnessClaude, true
	}
	return "", false
}

// leadCrossCheckCapable reports whether a lead of this harness, on a worker advertising
// caps, may take another cross-check round: any known family, and for a Codex lead only a
// worker with cross_check_codex_lead_v1.
func leadCrossCheckCapable(lead string, caps []string) bool {
	if !isCrossCheckLeadHarness(lead) {
		return false
	}
	return lead != string(HarnessCodex) || slices.Contains(caps, capability.CrossCheckCodexLeadV1)
}

// planCrossCheckLoopEnrolled reports whether lead's checked plan loop may run: a required
// autopilot lead, or (PRD #3708) a gated lead that stays on the human approval path. The two
// modes are mutually exclusive on the row (runs_plan_cross_check_mode_check), so this is the
// same predicate CreatePlanCrossCheckChild enforces in SQL.
func planCrossCheckLoopEnrolled(lead store.Run) bool {
	return (lead.PlanCrossCheckRequired && lead.AutoApprove) || (lead.PlanCrossCheckGated && !lead.AutoApprove)
}

func isCrossCheckLeadHarness(lead string) bool {
	_, ok := oppositeHarness(lead)
	return ok
}

func (s *Service) SubmitPlanCrossCheck(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, candidate PlanCrossCheckCandidate, requestedRounds ...int32) (store.CrossCheck, error) {
	round := int32(1)
	if len(requestedRounds) > 1 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if len(requestedRounds) == 1 {
		round = requestedRounds[0]
	}
	if round < 1 || round > config.MaxPlanCrossCheckMaxRevisions+1 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	lead, err := s.runOwnedByWorker(ctx, leadID, worker)
	if err != nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	childHarness, leadKnown := oppositeHarness(lead.Harness)
	if !leadKnown || !planCrossCheckLoopEnrolled(lead) ||
		(lead.Status != "claimed" && lead.Status != "running") {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	if err := validateCrossCheckContext(lead.IssueTitle, lead.IssueDescription); err != nil {
		return store.CrossCheck{}, err
	}
	var normalizeErr error
	candidate, normalizeErr = NormalizePlanCrossCheckCandidate(candidate)
	if normalizeErr != nil {
		return store.CrossCheck{}, normalizeErr
	}
	if len(candidate.PlanMd) == 0 || len(candidate.PlanMd) > 256*1024 || len(candidate.PlanningDiff) > 512*1024 ||
		len(candidate.Milestones) > 256*1024 || len(candidate.BaseCommit) != 40 || strings.Trim(candidate.BaseCommit, "0123456789abcdefABCDEF") != "" ||
		(candidate.SizeClass != "s" && candidate.SizeClass != "m" && candidate.SizeClass != "l") || len(candidate.RequiredCapabilities) > 64 || len(candidate.RequiredTools) > 64 {
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
	// Identity is selected under the run lock from frozen enrollment below.
	var digest []byte
	// Recover the immutable attempt before resolving checker credentials. A lost
	// ACK does not authorize another round, even if the checker already decided.
	if s.txBeginner == nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	retryQ := store.New(tx)
	locked, err := retryQ.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: leadID, WorkerID: pgconv.UUID(worker.ID)})
	if err != nil || locked.UserID != worker.UserID || locked.ClaimGeneration != generation || locked.ClaimReleasedAt.Valid ||
		locked.Harness != lead.Harness || !planCrossCheckLoopEnrolled(locked) ||
		(locked.Status != "claimed" && locked.Status != "running") ||
		validateCrossCheckContext(locked.IssueTitle, locked.IssueDescription) != nil {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	candidate.RequiredCapabilities = applyInferredCapabilityPolicy(locked, candidate.RequiredCapabilities)
	candidate, digest, err = bindSemanticCheckerCandidate(ctx, retryQ, leadID, candidate)
	if err != nil {
		return store.CrossCheck{}, err
	}
	prior, err := retryQ.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: leadID, Round: round})
	if err == nil {
		if prior.LeadClaimGeneration != generation || !bytes.Equal(prior.CandidateDigest, digest) {
			return store.CrossCheck{}, ErrCrossCheckInterrupted
		}
		if err := tx.Commit(ctx); err != nil {
			return store.CrossCheck{}, err
		}
		return prior, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, err
	}
	_, _, err = s.preparePlanCrossCheckRound(ctx, retryQ, locked, worker, round)
	if err != nil {
		return store.CrossCheck{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CrossCheck{}, err
	}

	timeout := s.p.PlanCrossCheckTimeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	if timeout <= 0 || timeout > 2*time.Hour || (s.p.RunTimeout > 0 && timeout >= s.p.RunTimeout) {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	budgetSeconds := int64((timeout + time.Second - 1) / time.Second)
	// Keep the checked bound adjacent to the narrowing conversion (including
	// fractional-second ceilings), rather than relying on the duration guard.
	if budgetSeconds < 1 || budgetSeconds > 7200 {
		return store.CrossCheck{}, ErrCrossCheckRefused
	}
	budgetWallSeconds := int32(budgetSeconds)
	// Creation rechecks the lead and attempt under lock after harness resolution.
	if worker.IsolatedLane {
		return store.CrossCheck{}, ErrCrossCheckUnavailable
	}
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
		if locked.UserID != worker.UserID || locked.ClaimGeneration != generation || locked.Harness != lead.Harness ||
			!planCrossCheckLoopEnrolled(locked) || locked.ClaimReleasedAt.Valid ||
			(locked.Status != "claimed" && locked.Status != "running") ||
			validateCrossCheckContext(locked.IssueTitle, locked.IssueDescription) != nil {
			return store.Run{}, ErrCrossCheckRefused
		}
		candidate.RequiredCapabilities = applyInferredCapabilityPolicy(locked, candidate.RequiredCapabilities)
		rebound, reboundDigest, e := bindSemanticCheckerCandidate(ctx, txq, leadID, candidate)
		if e != nil || !bytes.Equal(reboundDigest, digest) {
			return store.Run{}, ErrCrossCheckInterrupted
		}
		candidate = rebound
		prior, e := txq.GetExactPlanCrossCheck(ctx, store.GetExactPlanCrossCheckParams{LeadRunID: leadID, Round: round})
		if e == nil {
			if prior.LeadClaimGeneration != generation || !bytes.Equal(prior.CandidateDigest, digest) {
				return store.Run{}, ErrCrossCheckInterrupted
			}
			existing = prior
			return store.Run{}, errRetry
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return store.Run{}, e
		}
		enabled, limit, e := s.preparePlanCrossCheckRound(ctx, txq, locked, worker, round)
		if e != nil {
			return store.Run{}, e
		}
		if resolved.Harness != childHarness {
			return store.Run{}, ErrCrossCheckRefused
		}
		childID := uuid.New()
		child, e := txq.CreatePlanCrossCheckChild(ctx, store.CreatePlanCrossCheckChildParams{
			ChildID: childID, ChildHarness: string(childHarness), LeadRunID: leadID, UserID: lead.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, BudgetWallSeconds: budgetWallSeconds,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			// The guarded write found no lead of the opposite family in this state.
			return store.Run{}, ErrCrossCheckRefused
		}
		if e != nil {
			return store.Run{}, e
		}
		existing, e = txq.InsertPlanCrossCheck(ctx, store.InsertPlanCrossCheckParams{
			LeadRunID: leadID, LeadClaimGeneration: generation, Round: round, AutomaticRoundsEnabled: enabled, AutomaticRevisionLimit: limit, PlanMd: pgtype.Text{String: candidate.PlanMd, Valid: true},
			Milestones: candidate.Milestones, RequiredCapabilities: candidate.RequiredCapabilities, RequiredTools: candidate.RequiredTools,
			SizeClass: pgtype.Text{String: candidate.SizeClass, Valid: true}, BaseCommit: pgtype.Text{String: candidate.BaseCommit, Valid: true},
			PlanningDiff: pgtype.Text{String: candidate.PlanningDiff, Valid: true}, CandidateDigest: digest,
			CheckerRunID: child.ID, DeadlineAt: pgtype.Timestamptz{Time: s.now().Add(timeout), Valid: true},
		})
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				// checker_harness comes from the child row and must differ from the lead's.
				return store.Run{}, ErrCrossCheckRefused
			}
			return store.Run{}, e
		}
		return child, nil
	})
	if errors.Is(err, errRetry) {
		return existing, nil
	}
	if errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrHarnessCredentialDisabled) {
		return store.CrossCheck{}, ErrCrossCheckUnavailable
	}
	if err != nil {
		return store.CrossCheck{}, fmt.Errorf("create plan cross-check: %w", err)
	}
	s.notify(existing.CheckerRunID.Bytes, "queued")
	return existing, nil
}

func (s *Service) PlanCrossCheckStatus(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, round int32) (store.CrossCheck, int32, error) {
	return s.planCrossCheckStatus(ctx, worker, leadID, generation, round, nil)
}

func (s *Service) planCrossCheckStatus(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64, round int32, result *PlanCrossCheckStatusResult) (store.CrossCheck, int32, error) {
	if round < 1 || round > config.MaxPlanCrossCheckMaxRevisions+1 || s.txBeginner == nil {
		return store.CrossCheck{}, 0, ErrCrossCheckRefused
	}
	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.CrossCheck{}, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	lead, err := q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{
		ID: leadID, WorkerID: pgconv.UUID(worker.ID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, 0, ErrCrossCheckRefused
	}
	if err != nil {
		return store.CrossCheck{}, 0, err
	}
	if lead.UserID != worker.UserID || !(lead.PlanCrossCheckRequired || lead.PlanCrossCheckGated) || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid ||
		(lead.Status != "claimed" && lead.Status != "running" && lead.Status != "awaiting_approval") {
		return store.CrossCheck{}, 0, ErrCrossCheckRefused
	}
	if lead.Status == "awaiting_approval" {
		// Read only under the owning lead lock. No expiry, approval, or child write.
		cc, readErr := q.GetPlanCrossCheck(ctx, leadID)
		if readErr != nil && !errors.Is(readErr, pgx.ErrNoRows) {
			return store.CrossCheck{}, 0, readErr
		}
		if readErr == nil && cc.LeadClaimGeneration != generation {
			cc.Verdict = "failed"
			cc.ReasonClass = pgtype.Text{String: "interrupted", Valid: true}
		}
		proof, err := captureLeadReconciliation(ctx, q, worker, leadID, generation)
		if err != nil {
			return store.CrossCheck{}, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return store.CrossCheck{}, 0, err
		}
		if result != nil {
			result.Parked = true
			result.Reconciliation = proof
		}
		if errors.Is(readErr, pgx.ErrNoRows) {
			return cc, proof.LeadLastSeq, ErrCrossCheckNoRow
		}
		return cc, proof.LeadLastSeq, nil
	}
	expired, err := q.ExpirePlanCrossCheck(ctx, store.ExpirePlanCrossCheckParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation,
	})
	var eventPayload []byte
	if err == nil {
		lead.LastSeq, eventPayload, err = appendPlanCrossCheckEvent(ctx, q, lead, store.CrossCheck(expired), "server")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return store.CrossCheck{}, 0, err
	}
	cc, err := q.GetOwnedPlanCrossCheck(ctx, store.GetOwnedPlanCrossCheckParams{
		LeadRunID: leadID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: generation, Round: round,
	})
	noRow := errors.Is(err, pgx.ErrNoRows)
	if noRow && result == nil {
		return cc, lead.LastSeq, ErrCrossCheckNoRow
	}
	if err != nil && !noRow {
		return cc, 0, err
	}
	if !noRow && cc.LeadClaimGeneration != generation && cc.Verdict != "block" &&
		(cc.Verdict != "failed" || cc.ReasonClass.String == "superseded" || cc.ReasonClass.String == "approved_not_stored") {
		// Stale approval and recoverable evidence confer no current approval.
		// Decided BLOCK and checker failures retain their recorded fallback.
		cc.Verdict = "failed"
		cc.ReasonClass = pgtype.Text{String: "interrupted", Valid: true}
	}
	var proof *LeadReconciliation
	if !noRow && result != nil {
		semantic, resolveErr := rehydrateSemanticChecker(ctx, q, cc)
		if resolveErr != nil {
			return store.CrossCheck{}, 0, resolveErr
		}
		// Legacy status rendering keeps its existing validation path. Only an
		// enrolled semantic attempt needs immutable-history reconstruction here.
		if semantic != nil {
			input, resolveErr := crossCheckClaimInput(cc, semantic)
			if resolveErr != nil {
				return store.CrossCheck{}, 0, resolveErr
			}
			result.Candidate = &input.PlanCrossCheckCandidate
		}
	}
	if result != nil {
		proof, err = captureLeadReconciliation(ctx, q, worker, leadID, generation)
		if err != nil {
			return store.CrossCheck{}, 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CrossCheck{}, 0, err
	}
	if result != nil {
		result.Reconciliation = proof
	}
	if eventPayload != nil && s.bcast != nil {
		s.bcast.PublishMessage(lead.ID, lead.LastSeq, "cross_check", "", "", "", eventPayload, s.now())
	}
	if noRow {
		return cc, lead.LastSeq, ErrCrossCheckNoRow
	}
	return cc, lead.LastSeq, nil
}

func (s *Service) DecidePlanCrossCheck(ctx context.Context, worker store.Worker, childID uuid.UUID, generation int64, verdict, reason string, findings []byte) (store.CrossCheck, error) {
	var err error
	findings, err = NormalizeCrossCheckFindings(verdict, reason, findings)
	if err != nil {
		return store.CrossCheck{}, err
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
	if lead.UserID != worker.UserID {
		return store.CrossCheck{}, ErrCrossCheckRefused
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
	seq, payload, err := appendPlanCrossCheckEvent(ctx, q, lead, cc, "model")
	if err != nil {
		return cc, err
	}
	if err = tx.Commit(ctx); err != nil {
		return cc, err
	}
	if s.bcast != nil {
		s.bcast.PublishMessage(lead.ID, seq, "cross_check", "", "", "", payload, s.now())
	}
	return cc, nil
}

// appendPlanCrossCheckEvent is used by current-generation poll expiry and verdict
// transactions holding the lead lock. It tries at most 32 sequence collisions;
// any query or generation-fence failure aborts the transaction. Queries use ctx.
// Worker ingestion continues to reject this reserved message kind.
func appendPlanCrossCheckEvent(ctx context.Context, q *store.Queries, lead store.Run, cc store.CrossCheck, author string) (int32, []byte, error) {
	if cc.LeadRunID != lead.ID || cc.LeadClaimGeneration != lead.ClaimGeneration || cc.Verdict == "pending" {
		return 0, nil, ErrCrossCheckRefused
	}
	var childID *uuid.UUID
	if cc.CheckerRunID.Valid {
		id := uuid.UUID(cc.CheckerRunID.Bytes)
		childID = &id
	}
	payload, err := json.Marshal(map[string]any{"stage": "plan", "round": cc.Round, "candidate_generation": cc.LeadClaimGeneration, "verdict": cc.Verdict, "reason_class": cc.ReasonClass.String, "findings": json.RawMessage(cc.Findings), "checker_run_id": childID, "findings_author": author})
	if err != nil {
		return 0, nil, err
	}
	inserted := false
	var seq int32
	for attempt := 0; attempt < 32; attempt++ {
		next, nextErr := q.NextPlanCrossCheckMessageSeq(ctx, store.NextPlanCrossCheckMessageSeqParams{
			LeadRunID: lead.ID, ClaimGeneration: lead.ClaimGeneration,
		})
		if nextErr != nil {
			return 0, nil, nextErr
		}
		seq = next
		result, insertErr := q.InsertRunMessage(ctx, store.InsertRunMessageParams{
			RunID: lead.ID, Seq: seq, Kind: "cross_check", Payload: payload,
			ClaimGeneration: pgconv.Int8Ptr(&lead.ClaimGeneration),
		})
		if insertErr != nil {
			return 0, nil, insertErr
		}
		if !result.GenerationLive {
			return 0, nil, ErrCrossCheckRefused
		}
		if result.Inserted {
			inserted = true
			break
		}
	}
	if !inserted {
		return 0, nil, ErrCrossCheckRefused
	}
	advanced, err := q.UpdateRunLastSeq(ctx, store.UpdateRunLastSeqParams{
		ID: lead.ID, Seq: seq, ClaimGeneration: pgconv.Int8Ptr(&lead.ClaimGeneration),
	})
	if err != nil {
		return 0, nil, err
	}
	if advanced != 1 {
		return 0, nil, ErrCrossCheckRefused
	}
	return seq, payload, nil
}

// preparePlanCrossCheckRound runs under the owning lead lock, before and after
// credential resolution. Only exact-round retries bypass fresh-round admission.
func (s *Service) preparePlanCrossCheckRound(ctx context.Context, q *store.Queries, lead store.Run, worker store.Worker, round int32) (bool, int32, error) {
	caps, err := q.GetPlanCrossCheckClaimingWorkerCaps(ctx, store.GetPlanCrossCheckClaimingWorkerCapsParams{LeadRunID: lead.ID, WorkerID: worker.ID, UserID: worker.UserID, ClaimGeneration: lead.ClaimGeneration})
	if err != nil {
		return false, 0, ErrCrossCheckRefused
	}
	// A Codex lead's checked gate and its Claude checker need a worker that advertises
	// cross_check_codex_lead_v1 (PRD #2460); a Claude lead's flow predates it.
	if lead.Harness == string(HarnessCodex) && !slices.Contains(caps, capability.CrossCheckCodexLeadV1) {
		return false, 0, ErrCrossCheckRefused
	}
	// PRD #3708: the claim field is the only way a worker learns to run a gated check, so a
	// submit from a worker that does not advertise cross_check_gated_v1 is a stale or forged
	// caller. Defense in depth: nothing is created for it.
	if lead.PlanCrossCheckGated && !slices.Contains(caps, capability.CrossCheckGatedV1) {
		return false, 0, ErrCrossCheckRefused
	}
	capable := slices.Contains(caps, capability.CrossCheckRoundsV1)
	prior, err := q.GetPlanCrossCheck(ctx, lead.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		if round != 1 || lead.PlanMd.Valid || lead.GateRevision > 0 {
			return false, 0, ErrCrossCheckRefused
		}
		limit := s.p.PlanCrossCheckMaxRevisions
		if limit < 0 || limit > config.MaxPlanCrossCheckMaxRevisions {
			return false, 0, ErrCrossCheckRefused
		}
		if !capable {
			limit = 0
		}
		return capable, limit, nil
	}
	if err != nil {
		return false, 0, err
	}
	// PRD #3708 (gated recovery policy): only a genuinely decided REVISE allows another round.
	// Anything else, notably a check that lost custody while pending or an APPROVE that was
	// never presented, ends the check; it must not reach SupersedeUnstoredPlanApproval, which
	// would make a stale or forged round-N+1 submit rewrite the recorded evidence.
	if lead.PlanCrossCheckGated && !eligibleGatedPlanRound(prior) {
		return false, 0, ErrCrossCheckInterrupted
	}
	if prior.AutomaticRoundsEnabled && prior.Verdict == "approve" && prior.LeadClaimGeneration != lead.ClaimGeneration && !lead.PlanMd.Valid && lead.GateRevision == 0 {
		prior, err = q.SupersedeUnstoredPlanApproval(ctx, lead.ID)
		if err != nil {
			return false, 0, err
		}
	}
	if round != prior.Round+1 || !capable || !prior.AutomaticRoundsEnabled ||
		lead.PlanMd.Valid || lead.GateRevision > 0 || !eligibleAutomaticPlanRound(prior) {
		return false, 0, ErrCrossCheckInterrupted
	}
	if prior.Round >= prior.AutomaticRevisionLimit+1 {
		return false, 0, ErrCrossCheckRevisionsExhausted
	}
	return prior.AutomaticRoundsEnabled, prior.AutomaticRevisionLimit, nil
}

func eligibleAutomaticPlanRound(cc store.CrossCheck) bool {
	switch cc.Verdict {
	case "revise":
		return true
	case "failed":
		return cc.DecidedAt.Valid && cc.DeadlineAt.Valid && cc.DecidedAt.Time.Before(cc.DeadlineAt.Time) &&
			(cc.ReasonClass.String == "superseded" || cc.ReasonClass.String == "approved_not_stored")
	}
	return false
}

// eligibleGatedPlanRound is the gated-only recovery policy: unlike a required autopilot
// lead, a gated lead gets a fresh round only after the checker genuinely decided REVISE.
func eligibleGatedPlanRound(cc store.CrossCheck) bool {
	return cc.Verdict == "revise"
}

// gatedSettledFallback is the narrow gated override of LatestPlanCrossCheck. A gated check
// that lost custody (a pending or APPROVE row of an older claim generation, or a row the settle
// trigger closed as superseded/approved_not_stored) ends the check: the second return is true,
// the round is not eligible, and the first return is the gate reason SetRunAwaitingApproval
// derives for the same row (timed_out past the deadline rules, otherwise interrupted). Every
// other row shape keeps today's metadata, including a decided BLOCK or checker failure and a
// decided REVISE at the limit.
func gatedSettledFallback(row store.GetPlanCrossCheckMetadataRow, generation int64) (string, bool) {
	older := row.LeadClaimGeneration != generation
	decidedInTime := row.DecidedAt.Valid && row.DeadlineAt.Valid && row.DecidedAt.Time.Before(row.DeadlineAt.Time)
	switch {
	case row.Verdict == "pending" && older:
		if row.InterruptedAt.Valid && row.DeadlineAt.Valid && row.InterruptedAt.Time.Before(row.DeadlineAt.Time) {
			return "interrupted", true
		}
		return "timed_out", true
	case row.Verdict == "approve" && older:
		if row.AutomaticRoundsEnabled && !decidedInTime {
			return "timed_out", true
		}
		return "interrupted", true
	case row.Verdict == "failed" && (row.ReasonClass.String == "superseded" || row.ReasonClass.String == "approved_not_stored"):
		if !decidedInTime {
			return "timed_out", true
		}
		return "interrupted", true
	}
	return "", false
}

// PlanCrossCheckLatestMetadata has no candidate, findings or approval proof.
type PlanCrossCheckLatestMetadata struct {
	Result                 string `json:"result"`
	Round                  int32  `json:"round"`
	CandidateGeneration    int64  `json:"candidate_generation"`
	AutomaticRevisionLimit int32  `json:"automatic_revision_limit"`
	AutomaticRoundsEnabled bool   `json:"automatic_rounds_enabled"`
	NextRound              *int32 `json:"next_round"`
	NextRoundEligible      bool   `json:"next_round_eligible"`
	FallbackReason         string `json:"fallback_reason"`
}

// LatestPlanCrossCheck is side-effect-free, including for expired or old claims.
func (s *Service) LatestPlanCrossCheck(ctx context.Context, worker store.Worker, leadID uuid.UUID, generation int64) (PlanCrossCheckLatestMetadata, error) {
	lead, err := s.runOwnedByWorker(ctx, leadID, worker)
	if err != nil || lead.UserID != worker.UserID || lead.ClaimGeneration != generation || lead.ClaimReleasedAt.Valid ||
		!(lead.PlanCrossCheckRequired || lead.PlanCrossCheckGated) || (lead.Status != "claimed" && lead.Status != "running" && lead.Status != "awaiting_approval") {
		return PlanCrossCheckLatestMetadata{}, ErrCrossCheckRefused
	}
	q, ok := s.q.(interface {
		GetPlanCrossCheckMetadata(context.Context, uuid.UUID) (store.GetPlanCrossCheckMetadataRow, error)
		GetPlanCrossCheckClaimingWorkerCaps(context.Context, store.GetPlanCrossCheckClaimingWorkerCapsParams) ([]string, error)
	})
	if !ok {
		return PlanCrossCheckLatestMetadata{}, ErrCrossCheckRefused
	}
	caps, err := q.GetPlanCrossCheckClaimingWorkerCaps(ctx, store.GetPlanCrossCheckClaimingWorkerCapsParams{LeadRunID: leadID, WorkerID: worker.ID, UserID: worker.UserID, ClaimGeneration: generation})
	if err != nil {
		return PlanCrossCheckLatestMetadata{}, ErrCrossCheckRefused
	}
	// A gated lead may only run the loop on a worker that advertises cross_check_gated_v1.
	gatedCapable := !lead.PlanCrossCheckGated || slices.Contains(caps, capability.CrossCheckGatedV1)
	row, err := q.GetPlanCrossCheckMetadata(ctx, leadID)
	result := PlanCrossCheckLatestMetadata{Result: "no_row"}
	if errors.Is(err, pgx.ErrNoRows) {
		if (lead.AutoApprove || lead.PlanCrossCheckGated) && gatedCapable && leadCrossCheckCapable(lead.Harness, caps) && lead.Status != "awaiting_approval" && !lead.PlanMd.Valid && lead.GateRevision == 0 {
			next := int32(1)
			result.NextRound = &next
			result.NextRoundEligible = true
		}
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Result = "latest"
	result.Round = row.Round
	result.CandidateGeneration = row.LeadClaimGeneration
	result.AutomaticRevisionLimit = row.AutomaticRevisionLimit
	result.AutomaticRoundsEnabled = row.AutomaticRoundsEnabled
	cc := store.CrossCheck{Verdict: row.Verdict, ReasonClass: row.ReasonClass, DecidedAt: row.DecidedAt, DeadlineAt: row.DeadlineAt}
	eligible := eligibleAutomaticPlanRound(cc)
	if row.Verdict == "approve" && row.LeadClaimGeneration != generation && !lead.PlanMd.Valid {
		eligible = row.DecidedAt.Valid && row.DeadlineAt.Valid && row.DecidedAt.Time.Before(row.DeadlineAt.Time)
	}
	result.FallbackReason = row.ReasonClass.String
	if !row.AutomaticRoundsEnabled && row.Verdict == "approve" && row.LeadClaimGeneration != generation {
		result.FallbackReason = "interrupted"
	}
	if (row.Verdict == "failed" && (row.ReasonClass.String == "superseded" || row.ReasonClass.String == "approved_not_stored") ||
		row.AutomaticRoundsEnabled && row.Verdict == "approve" && row.LeadClaimGeneration != generation && !lead.PlanMd.Valid) && !eligible {
		result.FallbackReason = "timed_out"
	}
	if row.Verdict == "pending" && !s.now().Before(row.DeadlineAt.Time) {
		result.FallbackReason = "timed_out"
	}
	if row.Verdict == "pending" && row.LeadClaimGeneration != generation {
		eligible = row.InterruptedAt.Valid && row.DeadlineAt.Valid && row.InterruptedAt.Time.Before(row.DeadlineAt.Time)
		if !eligible {
			result.FallbackReason = "timed_out"
		}
	}
	if lead.PlanCrossCheckGated {
		if reason, settled := gatedSettledFallback(row, generation); settled {
			eligible = false
			result.FallbackReason = reason
		}
	}
	if eligible && row.AutomaticRoundsEnabled && row.Round >= row.AutomaticRevisionLimit+1 {
		result.FallbackReason = "revisions_exhausted"
	}
	if eligible && row.AutomaticRoundsEnabled && row.Round < row.AutomaticRevisionLimit+1 &&
		slices.Contains(caps, capability.CrossCheckRoundsV1) && gatedCapable &&
		(lead.AutoApprove || (lead.PlanCrossCheckGated && row.Verdict == "revise")) && leadCrossCheckCapable(lead.Harness, caps) &&
		lead.Status != "awaiting_approval" && !lead.PlanMd.Valid && lead.GateRevision == 0 {
		next := row.Round + 1
		result.NextRound = &next
		result.NextRoundEligible = true
		result.FallbackReason = ""
	}
	if lead.Status == "awaiting_approval" {
		result.FallbackReason = lead.PlanCrossCheckGateReason.String
	}
	return result, nil
}
