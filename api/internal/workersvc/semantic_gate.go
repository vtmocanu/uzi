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
	// A parked gate's stored presentation carries the validated semantic_park; carry it into the
	// live projection so approval and replay compare the exact presentation the owner saw. Only a
	// reason the live projection still requires on the same candidate is carried.
	if run.Status == "awaiting_approval" {
		if stored, ok, derr := decodeGatePresentedPayload(run.GatePresentedPayload); derr == nil && ok && stored.Semantic != nil &&
			stored.Semantic.SemanticPark != "" && stored.Semantic.Candidate.Digest == p.Candidate.Digest &&
			stored.Semantic.SemanticPark == expectedSemanticPark(&p) {
			p.SemanticPark = stored.Semantic.SemanticPark
		}
	}
	return &p, c, nil
}

// semanticCoverageEvaluation is the one server-side coverage verdict for the current candidate
// and claim. semanticGateProjection (the gate presentation) and SemanticCoverageStatus (the
// worker status the agent consults) both derive from it, so a stale success from a reclaimed
// claim generation or another worker can never count complete in either.
type semanticCoverageEvaluation struct {
	Status           apitypes.SemanticCoverageStatus
	ReviewID         string
	InvocationID     string
	ReportDigest     string
	ConsumedSeconds  int
	RemainingSeconds int
	RemainingReviews int
}

// evaluateSemanticCoverage applies the single coverage rule set: a report counts only when its
// binding is the current candidate's and one of its invocations succeeded on the run's CURRENT
// claim generation and CURRENT worker; then represented/atomic/SourceOutcome decide complete,
// incomplete or uncertain. With no counting report and a spent review, time or token budget the
// outcome is exhausted; otherwise unavailable.
func evaluateSemanticCoverage(c *semanticCoverageTx, run store.Run) semanticCoverageEvaluation {
	binding := SemanticCoverageBinding(c.candidate)
	e := semanticCoverageEvaluation{}
	e.Status = apitypes.SemanticCoverageStatus{Outcome: "unavailable", SemanticCoverageBinding: binding, Reviews: len(c.ledger.Reviews), ChargedTokens: c.charged(), RemainingTokens: max(0, c.policy.MaxTokens-c.charged())}
	for _, r := range c.ledger.Reviews {
		e.ConsumedSeconds += int(r.Deadline.Sub(r.Started) / time.Second)
		e.Status.Invocations += len(r.Invocations)
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
		e.ReviewID, e.InvocationID = r.ID, invocation.ID
		raw, _ := json.Marshal(r.Report)
		e.ReportDigest = semanticHash(raw)
		e.Status.Report = r.Report
		e.Status.Deadline = r.Deadline
		e.Status.Outcome = "complete"
		for _, v := range r.Report.Requirements {
			if v.Status != "represented" {
				e.Status.Outcome = "incomplete"
			}
		}
		for _, v := range r.Report.Criteria {
			if !v.Atomic {
				e.Status.Outcome = "incomplete"
			}
		}
		if c.candidate.SourceOutcome != "complete" {
			e.Status.Outcome = "uncertain"
		}
	}
	e.RemainingSeconds = max(0, c.policy.MaxSeconds-e.ConsumedSeconds)
	e.RemainingReviews = max(0, c.policy.MaxReviews-len(c.ledger.Reviews))
	if e.Status.Report == nil && (e.RemainingSeconds == 0 || e.RemainingReviews == 0 || e.Status.RemainingTokens == 0) {
		e.Status.Outcome = "exhausted"
	}
	return e
}

func semanticGateProjection(c *semanticCoverageTx, run store.Run) apitypes.SemanticGatePresentation {
	binding := SemanticCoverageBinding(c.candidate)
	e := evaluateSemanticCoverage(c, run)
	p := apitypes.SemanticGatePresentation{Candidate: c.candidate, Binding: binding, ClaimGeneration: run.ClaimGeneration, Maintainers: []apitypes.SemanticMaintainerApproval{},
		Coverage: e.Status, ReviewID: e.ReviewID, InvocationID: e.InvocationID, ReportDigest: e.ReportDigest,
		ConsumedSeconds: e.ConsumedSeconds, RemainingSeconds: e.RemainingSeconds, RemainingReviews: e.RemainingReviews}
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
	if req.State == "awaiting_approval" {
		if err = validateSemanticPark(run, req, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Bounded coverage-park reasons (PRD #1230 D3, ADR-2149 amendment). They are stored only inside
// the presented payload's semantic block, never in plan_cross_check_gate_reason.
const (
	semanticParkCoverageIncomplete  = "coverage_incomplete"
	semanticParkCoverageUncertain   = "coverage_uncertain"
	semanticParkCoverageUnavailable = "coverage_unavailable"
	semanticParkMaintainerAuth      = "maintainer_authorization"
)

// expectedSemanticPark is the park reason the server's own projection requires: any coverage
// outcome other than complete parks with its matching coverage reason (exhausted and unavailable
// are one class), otherwise pending maintainer criteria need explicit human authorization, and a
// complete, maintainer-free candidate needs no coverage park.
func expectedSemanticPark(p *apitypes.SemanticGatePresentation) string {
	switch p.Coverage.Outcome {
	case "complete":
		if len(p.Maintainers) > 0 {
			return semanticParkMaintainerAuth
		}
		return ""
	case "incomplete":
		return semanticParkCoverageIncomplete
	case "uncertain":
		return semanticParkCoverageUncertain
	default:
		return semanticParkCoverageUnavailable
	}
}

// validateSemanticPark checks the worker-reported park reason against the server projection and
// records the accepted value on the presentation. An autopilot (auto_approve) report must carry
// exactly the expected reason, or none only when the projection needs none. A human-gated run, or
// one that already parked (SetRunAwaitingApproval clears auto_approve), may not supply a reason
// except as an exact replay of the reason its stored presentation already carries; omitting it
// on that parked candidate records the server-expected reason.
func validateSemanticPark(run store.Run, req StateRequest, p *apitypes.SemanticGatePresentation) error {
	expected := expectedSemanticPark(p)
	supplied := ""
	if req.SemanticPark != nil {
		supplied = *req.SemanticPark
		if supplied == "" {
			return ErrSemanticGateMismatch
		}
	}
	if run.AutoApprove {
		if supplied != expected {
			return ErrSemanticGateMismatch
		}
	} else if supplied != "" {
		stored, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
		if err != nil || !ok || stored.Semantic == nil || stored.Semantic.SemanticPark != supplied ||
			stored.Semantic.Candidate.Digest != p.Candidate.Digest || supplied != expected {
			return ErrSemanticGateMismatch
		}
	} else if stored, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload); err == nil && ok && stored.Semantic != nil &&
		stored.Semantic.SemanticPark != "" && stored.Semantic.Candidate.Digest == p.Candidate.Digest {
		// A parked gate re-presented without its reason keeps a truthful one: the server's own
		// expected reason for this candidate (the stored one, or its successor after the projection
		// moved, e.g. a reclaim). A worker can neither erase it nor keep a stale one.
		supplied = expected
	}
	p.SemanticPark = supplied
	return nil
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

// Return the validated snapshot so submitSemanticApproval can reuse it under
// admitApproval's run-row lock, without decoding or validating identity again.
func validateSemanticApprovalIdentity(run store.Run, opts SubmitInputOptions, p *apitypes.SemanticGatePresentation) (gatePresentedPayload, error) {
	if opts.SemanticApproval == nil || opts.ExpectedGateRevision == nil || *opts.ExpectedGateRevision != run.GateRevision || run.Status != "awaiting_approval" || run.ClaimReleasedAt.Valid || run.GateRevision < 1 {
		return gatePresentedPayload{}, ErrSemanticGateMismatch
	}
	snapshot, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || snapshot.Semantic == nil || !sameSemanticJSON(snapshot.Semantic, p) || !sameSemanticJSON(*opts.SemanticApproval, semanticApprovalFor(run, *p)) {
		return gatePresentedPayload{}, ErrSemanticGateMismatch
	}
	_, digest, err := snapshot.digest()
	if err != nil || !bytes.Equal(digest, run.GatePayloadDigest) || snapshot.PlanMd == nil || !run.PlanMd.Valid || *snapshot.PlanMd != run.PlanMd.String {
		return gatePresentedPayload{}, ErrSemanticGateMismatch
	}
	if err := validateSemanticGateFields(snapshot, p); err != nil {
		return gatePresentedPayload{}, err
	}
	return snapshot, nil
}

// validateSemanticApproval checks live admission fields only on the first freeze.
// snapshot must come from validateSemanticApprovalIdentity for this locked run.
func validateSemanticApproval(run store.Run, snapshot gatePresentedPayload) error {
	live := gatePayloadFromRow(run)
	a, ae := canonicalRawJSON(live.Milestones)
	b, be := canonicalRawJSON(snapshot.Milestones)
	if ae != nil || be != nil || !bytes.Equal(a, b) || !sameSemanticJSON(sortedSet(live.RequiredCapabilities), sortedSet(snapshot.RequiredCapabilities)) || !sameSemanticJSON(sortedSet(live.RequiredTools), sortedSet(snapshot.RequiredTools)) || !sameSemanticJSON(live.SizeClass, snapshot.SizeClass) {
		return ErrSemanticGateMismatch
	}
	return nil
}

func semanticGateContract(p *apitypes.SemanticGatePresentation, approval *apitypes.SemanticGateApproval) ([]byte, error) {
	if p == nil {
		return nil, ErrSemanticCompletionAuthority
	}
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
	if err := freezeMaintainerAuthorization(&contract); err != nil {
		return nil, err
	}
	if err := validateSemanticCompletion(contract); err != nil {
		return nil, err
	}
	return json.Marshal(contract)
}

func (s *Service) submitSemanticApproval(ctx context.Context, tx pgx.Tx, run store.Run, body string, sel *AgentSelection, opts SubmitInputOptions, p *apitypes.SemanticGatePresentation, c *semanticCoverageTx) (SubmitInputResult, error) {
	snapshot, err := validateSemanticApprovalIdentity(run, opts, p)
	if err != nil {
		return SubmitInputResult{}, err
	}
	blobs, err := c.payload(ctx, run.ID)
	if err != nil {
		return SubmitInputResult{}, err
	}
	projected, _ := stripNUL(string(blobs.Plan))
	if !run.PlanMd.Valid || projected != run.PlanMd.String {
		return SubmitInputResult{}, ErrSemanticGateMismatch
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
	if err := validateSemanticApproval(run, snapshot); err != nil {
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
