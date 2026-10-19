package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var ErrSemanticGateMismatch = errors.New("semantic gate binding mismatch")
var ErrSemanticResetUnsupported = errors.New("semantic milestone reset is not supported")

func sameSemanticJSON(a, b any) bool {
	x, xe := json.Marshal(a)
	y, ye := json.Marshal(b)
	return xe == nil && ye == nil && bytes.Equal(x, y)
}

// loadSemanticGate is read-only. Mutating callers hold the run lock.
// At most three reviews, six invocations and ten blobs are loaded. Any
// corrupt sibling aborts the operation rather than supplying partial authority.
func loadSemanticGate(ctx context.Context, q *store.Queries, run store.Run) (*apitypes.SemanticGatePresentation, *semanticCoverageTx, error) {
	planning, err := q.GetSemanticPlanning(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var enrollment apitypes.SemanticEnrollment
	if strictSemanticJSON(planning.Enrollment, &enrollment) != nil || enrollment.Profile != "semantic" || !validCoveragePolicy(enrollment.Policy) || !run.CompletionContractVersion.Valid || run.CompletionContractVersion.Int32 != 1 {
		return nil, nil, ErrSemanticGateMismatch
	}
	c := &semanticCoverageTx{q: q, policy: enrollment.Policy, ledger: semanticCoverageLedger{Version: 1}}
	if strictSemanticJSON(planning.Candidate, &c.candidate) != nil || c.candidate.Revision != planning.Revision || c.candidate.Revision < 1 || c.candidate.Digest == "" {
		return nil, nil, ErrSemanticGateMismatch
	}
	if _, err = buildSemanticCompletionContract(c.candidate); err != nil {
		return nil, nil, err
	}
	raw, err := q.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{RunID: run.ID, Column2: c.candidate.Digest})
	if err != nil {
		return nil, nil, err
	}
	var immutable apitypes.SemanticCandidate
	if strictSemanticJSON(raw, &immutable) != nil || !sameSemanticJSON(immutable, c.candidate) {
		return nil, nil, ErrSemanticGateMismatch
	}
	if len(planning.ReviewState) > 0 {
		if strictSemanticJSON(planning.ReviewState, &c.ledger) != nil || c.ledger.Version != 1 || len(c.ledger.Reviews) > 3 {
			return nil, nil, ErrSemanticGateMismatch
		}
	}
	p := semanticGateProjection(c, run)
	return &p, c, nil
}

func semanticGateProjection(c *semanticCoverageTx, run store.Run) apitypes.SemanticGatePresentation {
	binding := SemanticCoverageBinding(c.candidate)
	p := apitypes.SemanticGatePresentation{Candidate: c.candidate, Binding: binding, ClaimGeneration: run.ClaimGeneration, Maintainers: []apitypes.SemanticMaintainerApproval{}}
	p.Coverage = apitypes.SemanticCoverageStatus{Outcome: "unavailable", SemanticCoverageBinding: binding, Reviews: len(c.ledger.Reviews), ChargedTokens: c.charged(), RemainingTokens: max(0, c.policy.MaxTokens-c.charged())}
	for _, r := range c.ledger.Reviews {
		p.ConsumedSeconds += int(r.Deadline.Sub(r.Started) / time.Second)
		p.Coverage.Invocations += len(r.Invocations)
		if r.Binding != binding || r.Report == nil {
			continue
		}
		var invocation *semanticCoverageInvocation
		for i := range r.Invocations {
			v := &r.Invocations[i]
			if v.Generation == run.ClaimGeneration && run.WorkerID.Valid && v.WorkerID == uuid.UUID(run.WorkerID.Bytes) && v.Status == "success" {
				invocation = v
			}
		}
		if invocation == nil {
			continue
		}
		p.ReviewID, p.InvocationID = r.ID, invocation.ID
		raw, _ := json.Marshal(r.Report)
		p.ReportDigest = semanticHash(raw)
		p.Coverage.Report = r.Report
		p.Coverage.Deadline = r.Deadline
		p.Coverage.Outcome = "complete"
		for _, v := range r.Report.Requirements {
			if v.Status != "represented" {
				p.Coverage.Outcome = "incomplete"
			}
		}
		for _, v := range r.Report.Criteria {
			if !v.Atomic {
				p.Coverage.Outcome = "incomplete"
			}
		}
		if c.candidate.SourceOutcome != "complete" {
			p.Coverage.Outcome = "uncertain"
		}
	}
	p.RemainingSeconds = max(0, c.policy.MaxSeconds-p.ConsumedSeconds)
	p.RemainingReviews = max(0, c.policy.MaxReviews-len(c.ledger.Reviews))
	if p.Coverage.Report == nil && (p.RemainingSeconds == 0 || p.RemainingReviews == 0 || p.Coverage.RemainingTokens == 0) {
		p.Coverage.Outcome = "exhausted"
	}
	for _, v := range c.candidate.Criteria {
		if v.Kind == "maintainer" && v.Maintainer != nil {
			p.Maintainers = append(p.Maintainers, apitypes.SemanticMaintainerApproval{ID: v.ID, SemanticMaintainer: *v.Maintainer})
		}
	}
	return p
}

func semanticApprovalFor(run store.Run, p apitypes.SemanticGatePresentation) apitypes.SemanticGateApproval {
	return apitypes.SemanticGateApproval{PresentationRevision: run.GateRevision, PresentationDigest: hex.EncodeToString(run.GatePayloadDigest), CandidateIdentityVersion: p.Candidate.CandidateIdentityVersion, CandidateRevision: p.Candidate.Revision, SemanticCoverageBinding: p.Binding, ClaimGeneration: p.ClaimGeneration, ReviewID: p.ReviewID, InvocationID: p.InvocationID, ReportDigest: p.ReportDigest, Maintainers: p.Maintainers}
}

func semanticGateReport(ctx context.Context, q *store.Queries, run store.Run, req StateRequest) (*apitypes.SemanticGatePresentation, error) {
	p, c, err := loadSemanticGate(ctx, q, run)
	if err != nil || p == nil {
		return p, err
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration != run.ClaimGeneration || run.ClaimReleasedAt.Valid || req.CandidateDigest != p.Candidate.Digest || req.PlanMd == nil || semanticHash([]byte(*req.PlanMd)) != p.Candidate.PlanHash {
		return nil, ErrSemanticGateMismatch
	}
	fields := gateReportFields{planMd: stripNULParam(req.PlanMd), milestones: reportPlanMilestonesParam(run, req), milestonesReported: req.Milestones != nil}
	fields.caps, fields.tools, fields.sizeClass = inferredRequirementParams(req)
	if len(run.CompletionContract) > 0 {
		err = validateFrozenSemanticReport(run, req)
	} else {
		err = validateSemanticGateFields(effectiveGatePayload(gatePayloadFromRow(run), fields), p)
	}
	if err != nil {
		return nil, err
	}
	if _, err = c.payload(ctx, run.ID); err != nil {
		return nil, err
	}
	return p, nil
}

// SemanticGateForOwner serves bytes from the stored presentation's immutable
// blob references, including after the live candidate changed.
func (s *Service) SemanticGateForOwner(ctx context.Context, owner uuid.UUID, run store.Run) (*apitypes.SemanticGateDTO, error) {
	if run.UserID != owner {
		return nil, ErrRunNotFound
	}
	if s.txBeginner == nil {
		return nil, errors.New("semantic presentation requires transaction")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || snapshot.Semantic == nil {
		return nil, err
	}
	p := snapshot.Semantic
	c := &semanticCoverageTx{q: q, candidate: p.Candidate}
	blobs, err := c.payload(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	return &apitypes.SemanticGateDTO{Presentation: *p, Approval: semanticApprovalFor(run, *p), Sources: blobs.Sources, Plan: blobs.Plan}, nil
}

func validateSemanticApprovalIdentity(run store.Run, opts SubmitInputOptions, p *apitypes.SemanticGatePresentation) error {
	if opts.SemanticApproval == nil || opts.ExpectedGateRevision == nil || *opts.ExpectedGateRevision != run.GateRevision || run.Status != "awaiting_approval" || run.ClaimReleasedAt.Valid || run.GateRevision < 1 {
		return ErrSemanticGateMismatch
	}
	snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || snapshot.Semantic == nil || !sameSemanticJSON(snapshot.Semantic, p) || !sameSemanticJSON(*opts.SemanticApproval, semanticApprovalFor(run, *p)) {
		return ErrSemanticGateMismatch
	}
	_, digest, err := snapshot.digest()
	if err != nil || !bytes.Equal(digest, run.GatePayloadDigest) || snapshot.PlanMd == nil || !run.PlanMd.Valid || *snapshot.PlanMd != run.PlanMd.String || semanticHash([]byte(run.PlanMd.String)) != p.Candidate.PlanHash {
		return ErrSemanticGateMismatch
	}
	return nil
}

func validateSemanticApproval(run store.Run, opts SubmitInputOptions, p *apitypes.SemanticGatePresentation) error {
	if err := validateSemanticApprovalIdentity(run, opts, p); err != nil {
		return err
	}
	snapshot, _, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil {
		return err
	}
	live := gatePayloadFromRow(run)
	a, ae := canonicalRawJSON(live.Milestones)
	b, be := canonicalRawJSON(snapshot.Milestones)
	if ae != nil || be != nil || !bytes.Equal(a, b) || !sameSemanticJSON(sortedSet(live.RequiredCapabilities), sortedSet(snapshot.RequiredCapabilities)) || !sameSemanticJSON(sortedSet(live.RequiredTools), sortedSet(snapshot.RequiredTools)) || !sameSemanticJSON(live.SizeClass, snapshot.SizeClass) {
		return ErrSemanticGateMismatch
	}
	return nil
}

func semanticGateContract(p *apitypes.SemanticGatePresentation, approval *apitypes.SemanticGateApproval) ([]byte, error) {
	raw, err := buildSemanticCompletionContract(p.Candidate)
	if err != nil {
		return nil, err
	}
	var contract completionContract
	if err = json.Unmarshal(raw, &contract); err != nil {
		return nil, err
	}
	contract.SemanticApproval = approval
	contract.SemanticPresentation = p
	return json.Marshal(contract)
}

func (s *Service) submitSemanticApproval(ctx context.Context, tx pgx.Tx, run store.Run, body string, sel *AgentSelection, opts SubmitInputOptions, p *apitypes.SemanticGatePresentation, c *semanticCoverageTx) (SubmitInputResult, error) {
	if err := validateSemanticApprovalIdentity(run, opts, p); err != nil {
		return SubmitInputResult{}, err
	}
	if _, err := c.payload(ctx, run.ID); err != nil {
		return SubmitInputResult{}, err
	}
	// Replay authorizes the stored decision and returns its original receipt.
	// Body, selection and override are not stored as request identity and have
	// no effects here. Live admission checks apply only to the first freeze.
	if len(run.CompletionContract) > 0 {
		var frozen completionContract
		if json.Unmarshal(run.CompletionContract, &frozen) != nil || !sameSemanticJSON(frozen.SemanticApproval, opts.SemanticApproval) || !sameSemanticJSON(frozen.SemanticPresentation, p) {
			return SubmitInputResult{}, ErrSemanticGateMismatch
		}
		var id int64
		var created time.Time
		err := tx.QueryRow(ctx, "SELECT id,created_at FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan' AND gate_binding='bound' AND gate_revision=$2 ORDER BY id LIMIT 1", run.ID, run.GateRevision).Scan(&id, &created)
		if err != nil {
			return SubmitInputResult{}, err
		}
		return SubmitInputResult{ID: id, CreatedAt: created}, nil
	}
	if err := validateSemanticApproval(run, opts, p); err != nil {
		return SubmitInputResult{}, err
	}
	if changedMilestoneContract(run) {
		return SubmitInputResult{}, ErrReconciliationRequired
	}
	if !opts.OverrideCapabilities {
		if err := s.capabilityGate(ctx, run); err != nil {
			return SubmitInputResult{}, err
		}
	}
	source, exclusions := run.AgentSource, run.AgentExclusions
	if sel != nil {
		roster, err := s.rosterFor(ctx, run, sel.Source, nil)
		if err != nil {
			return SubmitInputResult{}, err
		}
		if err = validateSelection(*sel, roster); err != nil {
			return SubmitInputResult{}, err
		}
		source = pgconv.TextOrNull(sel.Source)
		exclusions, err = encodeJSONArray(sel.Exclusions)
		if err != nil {
			return SubmitInputResult{}, err
		}
		raw, err := json.Marshal(AgentSelection{Source: sel.Source, Exclusions: orEmpty(sel.Exclusions)})
		if err != nil {
			return SubmitInputResult{}, err
		}
		body = string(raw)
	}
	contract, err := semanticGateContract(p, opts.SemanticApproval)
	if err != nil {
		return SubmitInputResult{}, err
	}
	if len(run.MilestonesCandidate) == 0 {
		return SubmitInputResult{}, ErrSemanticGateMismatch
	}
	iterations := s.p.RunMaxIterations
	if iterations < 0 || iterations > math.MaxInt32 {
		return SubmitInputResult{}, ErrSemanticGateMismatch
	}
	clean, _ := stripNUL(body)
	row, err := s.q.CreateApprovePlanInput(ctx, store.CreateApprovePlanInputParams{
		RunID: run.ID, Body: pgconv.TextOrNull(clean), AgentSource: source, AgentExclusions: exclusions, CompletionContract: contract, ContractSource: run.MilestonesCandidate, ExpectedGateRevision: pgconv.Int8Ptr(opts.ExpectedGateRevision),
		RunMaxIterations: int32(iterations), RunTimeoutSeconds: int32(s.p.RunTimeout.Seconds()), MilestoneBudgetCap: milestoneBudgetCap, SizeBudgetFactorL: sizeBudgetFactorL, BudgetWallCeilingSeconds: budgetDurationSeconds(s.p.RunWallCeiling),
	})
	if err != nil {
		return SubmitInputResult{}, err
	}
	if opts.OverrideCapabilities {
		if err = s.OverrideRunRequiredCapabilities(ctx, run.UserID, run.ID, run.GateRevision); err != nil {
			return SubmitInputResult{}, err
		}
	}
	return SubmitInputResult{ID: row.ID, CreatedAt: row.CreatedAt.Time}, nil
}

// validateFrozenSemanticReport compares against the locked run's authorized execution
// fields. A capability override may have cleared capabilities from the original
// semantic presentation; a worker report must not restore them.
func validateFrozenSemanticReport(run store.Run, req StateRequest) error {
	base := gatePayloadFromRow(run)
	base.Milestones = run.MilestonesFrozen
	fields := gateReportFields{planMd: run.PlanMd, milestones: reportPlanMilestonesParam(run, req), milestonesReported: req.Milestones != nil}
	if req.PlanMd != nil {
		fields.planMd = stripNULParam(req.PlanMd)
	}
	fields.caps, fields.tools, fields.sizeClass = inferredRequirementParams(req)
	_, before, err := base.digest()
	if err != nil {
		return ErrSemanticGateMismatch
	}
	_, after, err := effectiveGatePayload(base, fields).digest()
	if err != nil || !bytes.Equal(before, after) {
		return ErrSemanticGateMismatch
	}
	return nil
}

// semanticAutoContract fences the dormant automatic freeze even when Plan
// cross-check is disabled. M4 owns the actual park and notifications.
func semanticAutoContract(ctx context.Context, q *store.Queries, run store.Run, req StateRequest) ([]byte, bool, error) {
	_, err := q.GetSemanticPlanning(ctx, run.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if len(run.CompletionContract) > 0 && req.PlanMd == nil && req.Milestones == nil {
		return nil, true, validateFrozenSemanticReport(run, req)
	}
	// A claim heartbeat carries no plan or milestones and grants no authority.
	if req.PlanMd == nil && req.Milestones == nil && len(run.MilestonesFrozen) == 0 && len(run.MilestonesCandidate) == 0 {
		return nil, true, nil
	}
	p, err := semanticGateReport(ctx, q, run, req)
	if err != nil {
		return nil, true, err
	}
	if len(run.CompletionContract) > 0 {
		return nil, true, nil
	}
	if p == nil || !run.AutoApprove || p.Coverage.Outcome != "complete" || len(p.Maintainers) > 0 {
		return nil, true, ErrSemanticGateMismatch
	}
	raw, err := semanticGateContract(p, nil)
	return raw, true, err
}
