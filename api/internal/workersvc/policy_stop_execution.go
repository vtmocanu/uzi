package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type policyStopExecutionReader interface {
	policyRevisionIdentityReader
	GetPolicyStopScopeInput(context.Context, store.GetPolicyStopScopeInputParams) (store.RunUserInput, error)
	GetPolicyStopRevisionInput(context.Context, store.GetPolicyStopRevisionInputParams) (store.RunUserInput, error)
	ListPolicyStopRevisionInputs(context.Context, uuid.UUID) ([]store.RunUserInput, error)
	GetPolicyStopCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error)
}

// policyStopExecutionContext is operation-local evidence, not a permission or DTO.
type policyStopExecutionContext struct {
	record policyStopResolutionV1
}

// policyStopExecutionLocked requires the caller to hold the parent run lock,
// acquired in a separate READ COMMITTED statement, and q to use that very
// transaction. The interface cannot prove lock or transaction custody.
// w must already be authenticated. Absence leaves ordinary approval unchanged;
// a present invalid receipt returns an error and must never fall back to approval.
// This read-only composition does not authorize Claim, ACK, or any writer.
// Negotiation and independent SQL fences remain obligations of later callers.
func policyStopExecutionLocked(ctx context.Context, q policyStopExecutionReader, w store.Worker, r store.Run, generation int64) (*policyStopExecutionContext, error) {
	if len(r.PolicyStopResolution) == 0 {
		return nil, nil
	}
	bad := func() (*policyStopExecutionContext, error) {
		return nil, stopReviewConflict("policy_stop_execution_unverifiable")
	}
	record, err := parsePolicyStopResolution(r.PolicyStopResolution, r.UserID, r.ID)
	if err != nil {
		return nil, err
	}
	if q == nil || (reflect.ValueOf(q).Kind() == reflect.Ptr && reflect.ValueOf(q).IsNil()) ||
		r.ID == uuid.Nil || !policyEligible(r) || r.TargetRunID.Valid || r.ReviewTargetRunID.Valid || r.ThenFixOfRunID.Valid ||
		r.PolicyRefusalCount <= 0 || !slices.Contains([]string{"claimed", "running", "awaiting_approval"}, r.Status) ||
		generation <= 0 || r.ClaimGeneration != generation || r.ClaimReleasedAt.Valid ||
		w.ID == uuid.Nil || w.UserID != r.UserID || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != w.ID {
		return bad()
	}
	m := record.Manifest
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyStopExecutionSourceEqual(source, m.SourceContext) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) ||
		!policyStopExecutionEqual(stopUUID(r.PolicyCandidateCrossCheckID), m.CandidateCrossCheckID) ||
		!r.PolicyLastOutcomeID.Valid || uuid.UUID(r.PolicyLastOutcomeID.Bytes).String() != m.OutcomeID ||
		!r.PolicyLastSourceGeneration.Valid || stopDecimal(r.PolicyLastSourceGeneration.Int64) != m.SourceGeneration {
		return bad()
	}
	// Review/source generations and the saved hold are historical. A reclaim may
	// advance generation, and resume clears the live hold without changing consent.
	reviewGeneration, err := strconv.ParseInt(string(m.ReviewGeneration), 10, 64)
	sourceGeneration, sourceErr := strconv.ParseInt(string(m.SourceGeneration), 10, 64)
	if err != nil || sourceErr != nil || reviewGeneration > generation || sourceGeneration > reviewGeneration {
		return bad()
	}
	matched, err := policyRevisionSourceGateMatches(ctx, q, r, source)
	if err != nil {
		return nil, err
	}
	if !matched {
		return bad()
	}
	noGate := source.Gate == nil
	target, valid, err := policyStopFrozenExecutionTarget(ctx, q, r, m.Execution, noGate)
	if err != nil {
		return nil, err
	}
	if !valid {
		return bad()
	}
	if noGate {
		if source.PrimaryRevision != nil || !policyRevisionNoGatePlanApproved(r) {
			return bad()
		}
	} else {
		payload, ok, decodeErr := decodeGatePresentedPayload(r.GatePresentedPayload)
		if decodeErr != nil || !ok || m.Gate == nil {
			return bad()
		}
		snapshot, snapshotErr := policyStopExecutionGatePayload(payload)
		if snapshotErr != nil || !policyStopExecutionEqual(snapshot, m.Gate.Payload) ||
			!policyStopExecutionHistoryMatches(r, m) {
			return bad()
		}
		// Only selected feedback applicability uses B. Frozen execution above
		// remains independently bound to accepted A, never mutable planning C.
		target = nil
		if payload.Semantic != nil {
			target = &payload.Semantic.Candidate
		}
	}
	scope, err := q.GetPolicyStopScopeInput(ctx, store.GetPolicyStopScopeInputParams{RunID: r.ID, InputID: record.ScopeInputID})
	if errors.Is(err, pgx.ErrNoRows) {
		return bad()
	}
	if err != nil {
		return nil, err
	}
	// CreateScopeCeilingInput writes an audit-only row; ConsumeRunInputs excludes
	// scope. SettleScopeInputDisposition changes disposition at completion.
	// This validator is for live execution only, so require pending, undelivered
	// audit shape. Body contains an unsaved optional reason; it is not reparsed.
	if scope.ID != record.ScopeInputID || scope.RunID != r.ID || scope.Kind != "scope" ||
		!scope.CreatedAt.Valid || !scope.CreatedAt.Time.Equal(record.AcceptedAt) ||
		scope.Disposition.Valid || scope.SettledAt.Valid || scope.SupersededByStopInputID.Valid ||
		scope.ConsumedAt.Valid || scope.ConsumedWorkerID.Valid || scope.ConsumedClaimGeneration.Valid ||
		scope.AppliedAt.Valid || scope.IncludedAt.Valid || scope.InclusionReported ||
		scope.GateBinding.Valid || scope.GateRevision.Valid || scope.QuestionID.Valid {
		return bad()
	}
	// Each saved human identity is read once, without retries. Invalid evidence
	// stops sibling reads; storage failures abort. Work is bounded by the receipt.
	for _, saved := range m.HumanFeedback {
		id, parseErr := strconv.ParseInt(string(saved.InputID), 10, 64)
		if parseErr != nil {
			return bad()
		}
		row, err := q.GetPolicyStopRevisionInput(ctx, store.GetPolicyStopRevisionInputParams{RunID: r.ID, InputID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return bad()
		}
		if err != nil {
			return nil, err
		}
		if row.ID != id || row.RunID != r.ID || row.Kind != "revise_plan" {
			return bad()
		}
		current := policyStopHumanSnapshot(row)
		if saved.AppliedAt == nil {
			if current.Disposition == nil || *current.Disposition != "superseded_by_stop" ||
				current.SettledAt == nil || !current.SettledAt.Equal(record.AcceptedAt) ||
				current.SupersededByStopInputID == nil || *current.SupersededByStopInputID != stopDecimal(record.ScopeInputID) {
				return bad()
			}
			current.Disposition, current.SettledAt, current.SupersededByStopInputID = nil, nil, nil
		}
		saved.CreatedAt = policyStopExecutionTime(saved.CreatedAt)
		saved.ConsumedAt = policyStopExecutionTime(saved.ConsumedAt)
		saved.AppliedAt = policyStopExecutionTime(saved.AppliedAt)
		saved.IncludedAt = policyStopExecutionTime(saved.IncludedAt)
		saved.SettledAt = policyStopExecutionTime(saved.SettledAt)
		if !policyStopExecutionEqual(current, saved) {
			return bad()
		}
	}
	pending, err := q.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, row := range pending {
		if !noGate && row.RunID == r.ID && row.Kind == "revise_plan" &&
			row.GateBinding.Valid && row.GateBinding.String == "bound" &&
			row.GateRevision.Valid && row.GateRevision.Int64 > 0 && row.GateRevision.Int64 < r.GateRevision {
			continue
		}
		return bad()
	}
	refs := append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil {
		refs = append(refs, *source.PrimaryRevision)
	}
	expected := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ref.CrossCheckID == nil || expected[*ref.CrossCheckID] {
			return bad()
		}
		expected[*ref.CrossCheckID] = true
	}
	if len(expected) != len(m.CrossCheckFeedback) {
		return bad()
	}
	// Exact finite references only, once each. No latest/history sweep, cache,
	// arbitrary inventory limit, or retry; one bad witness stops sibling work.
	for _, saved := range m.CrossCheckFeedback {
		id, parseErr := uuid.Parse(saved.CrossCheckID)
		if parseErr != nil || id == uuid.Nil || id.String() != saved.CrossCheckID || !expected[saved.CrossCheckID] {
			return bad()
		}
		delete(expected, saved.CrossCheckID)
		cc, err := q.GetPolicyStopCrossCheck(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return bad()
		}
		if err != nil {
			return nil, err
		}
		if cc.ID != id || cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 ||
			cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > reviewGeneration || !cc.DecidedAt.Valid ||
			(cc.Verdict != "revise" && cc.Verdict != "approve") || saved.Supersede != (saved.Verdict == "revise") ||
			saved.RevisionDisposition != nil || saved.RevisionSettledAt != nil || saved.RevisionSupersededByStopInputID != nil {
			return bad()
		}
		if saved.Supersede {
			if !cc.RevisionDisposition.Valid || cc.RevisionDisposition.String != "superseded_by_stop" ||
				!cc.RevisionSettledAt.Valid || !cc.RevisionSettledAt.Time.Equal(record.AcceptedAt) ||
				!cc.RevisionSupersededByStopInputID.Valid || cc.RevisionSupersededByStopInputID.Int64 != record.ScopeInputID {
				return bad()
			}
			cc.RevisionDisposition.Valid, cc.RevisionSettledAt.Valid, cc.RevisionSupersededByStopInputID.Valid = false, false, false
		}
		current, err := policyStopExecutionCrossCheckSnapshot(cc)
		saved.CreatedAt = policyStopExecutionTime(saved.CreatedAt)
		saved.DecidedAt = policyStopExecutionTime(saved.DecidedAt)
		saved.DeadlineAt = policyStopExecutionTime(saved.DeadlineAt)
		saved.InterruptedAt = policyStopExecutionTime(saved.InterruptedAt)
		saved.FinalizedAt = policyStopExecutionTime(saved.FinalizedAt)
		if saved.InterruptedAt == nil && current.InterruptedAt != nil {
			if current.InterruptedAt.IsZero() {
				return bad()
			}
			current.InterruptedAt = nil
		}
		if err != nil || !policyStopExecutionEqual(current, saved) {
			return bad()
		}
		valid, err := policyRevisionIdentityValid(ctx, q, cc)
		if err != nil {
			return nil, err
		}
		if !valid || (noGate && cc.Verdict != "approve") {
			return bad()
		}
		if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil &&
			*source.PrimaryRevision.CrossCheckID == saved.CrossCheckID && cc.Verdict != "revise" {
			return bad()
		}
		if m.CandidateCrossCheckID != nil && *m.CandidateCrossCheckID == saved.CrossCheckID {
			valid, err = policyRevisionIdentityApplicable(ctx, q, r, cc, target, noGate)
			if err != nil {
				return nil, err
			}
			if !valid {
				return bad()
			}
		}
	}
	return &policyStopExecutionContext{record: record}, nil
}

func policyStopExecutionTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func policyStopExecutionEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	left, err = canonicalRawJSON(left)
	if err != nil {
		return false
	}
	right, err = canonicalRawJSON(right)
	return err == nil && bytes.Equal(left, right)
}

func policyStopExecutionSourceEqual(a, b apitypes.PolicyRevisionSourceV1) bool {
	sortRefs := func(s *apitypes.PolicyRevisionSourceV1) {
		s.AssociatedCrossChecks = append([]apitypes.PolicyRevisionRef{}, s.AssociatedCrossChecks...)
		slices.SortFunc(s.AssociatedCrossChecks, func(a, b apitypes.PolicyRevisionRef) int {
			return strings.Compare(*a.CrossCheckID, *b.CrossCheckID)
		})
	}
	sortRefs(&a)
	sortRefs(&b)
	return policyStopExecutionEqual(a, b)
}

func policyStopExecutionGatePayload(p gatePresentedPayload) (apitypes.PolicyStopGatePayload, error) {
	ms, err := canonicalRawJSON(p.Milestones)
	out := apitypes.PolicyStopGatePayload{
		ReviseCount: p.ReviseCount, CheckpointWitness: p.CheckpointWitness, PlanMd: p.PlanMd,
		Milestones: ms, RequiredCapabilities: sortedSet(p.RequiredCapabilities),
		RequiredTools: sortedSet(p.RequiredTools), SizeClass: p.SizeClass,
	}
	if p.WitnessWorkerID != nil {
		v := p.WitnessWorkerID.String()
		out.WitnessWorkerID = &v
	}
	if p.WitnessClaimGeneration != nil {
		v := stopDecimal(*p.WitnessClaimGeneration)
		if v.Validate() != nil {
			return out, errors.New("invalid saved gate witness generation")
		}
		out.WitnessClaimGeneration = &v
	}
	return out, err
}

// The gate digest excludes revision_retry_receipt. Bind that metadata separately
// to the exact approved saved applied history, including every predecessor ID.
func policyStopExecutionHistoryMatches(r store.Run, m apitypes.PolicyStopManifest) bool {
	if policyStopUniqueJSON(r.GatePresentedPayload) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(r.GatePresentedPayload, &fields) != nil {
		return false
	}
	var active *apitypes.PolicyStopHumanFeedback
	var predecessors []int64
	for i := range m.HumanFeedback {
		h := &m.HumanFeedback[i]
		if h.AppliedAt == nil {
			continue
		}
		if h.Disposition == nil {
			active = h
		} else {
			id, err := strconv.ParseInt(string(h.InputID), 10, 64)
			if err != nil {
				return false
			}
			predecessors = append(predecessors, id)
		}
	}
	if active == nil {
		_, present := fields["revision_retry_receipt"]
		return !present
	}
	id, err := strconv.ParseInt(string(active.InputID), 10, 64)
	if err != nil {
		return false
	}
	receipt, valid := policyStopHistoryReceipt(r.GatePresentedPayload, id, r.GateRevision)
	if !valid || active.GateRevision == nil || stopDecimal(receipt.SourceRevision) != *active.GateRevision {
		return false
	}
	slices.Sort(predecessors)
	return slices.Equal(predecessors, receipt.SupersededIDs)
}

// FinalizeCodeCrossCheckDispositions only finalizes stage=code, so finalized_at
// remains exact for plan evidence. settle_exited_plan_cross_check can stamp a
// previously NULL interrupted_at on custody loss while preserving a decided
// verdict. The caller permits that one-way stamp, but preserves an existing
// saved interruption exactly. Verdict/reason/candidate/provenance stay exact.
func policyStopExecutionCrossCheckSnapshot(cc store.CrossCheck) (apitypes.PolicyStopCrossCheckFeedback, error) {
	ms, err := canonicalRawJSON(cc.Milestones)
	if err != nil {
		return apitypes.PolicyStopCrossCheckFeedback{}, err
	}
	findings, err := canonicalRawJSON(cc.Findings)
	if err != nil {
		return apitypes.PolicyStopCrossCheckFeedback{}, err
	}
	return apitypes.PolicyStopCrossCheckFeedback{
		CrossCheckID: cc.ID.String(), LeadRunID: cc.LeadRunID.String(), Stage: cc.Stage, Round: cc.Round,
		CandidateGeneration: stopDecimal(cc.LeadClaimGeneration), CandidateDigest: hex.EncodeToString(cc.CandidateDigest),
		Candidate: apitypes.PolicyStopCrossCheckCandidate{PlanMd: textPtr(cc.PlanMd), Milestones: ms,
			RequiredCapabilities: sortedSet(cc.RequiredCapabilities), RequiredTools: sortedSet(cc.RequiredTools),
			SizeClass: textPtr(cc.SizeClass), BaseCommit: textPtr(cc.BaseCommit), PlanningDiff: textPtr(cc.PlanningDiff)},
		Verdict: cc.Verdict, Supersede: cc.Verdict == "revise", ReasonClass: textPtr(cc.ReasonClass), Findings: findings,
		CreatedAt: stopTime(cc.CreatedAt), DecidedAt: stopTime(cc.DecidedAt), DeadlineAt: stopTime(cc.DeadlineAt),
		InterruptedAt: stopTime(cc.InterruptedAt), FinalizedAt: stopTime(cc.FinalizedAt),
		AutomaticRevisionLimit: cc.AutomaticRevisionLimit, AutomaticRoundsEnabled: cc.AutomaticRoundsEnabled,
		CheckerRunID: stopUUID(cc.CheckerRunID), CheckerHarness: textPtr(cc.CheckerHarness), CheckerModel: textPtr(cc.CheckerModel),
		CheckerEffort: textPtr(cc.CheckerEffort), CheckerModelSource: textPtr(cc.CheckerModelSource), CheckerEffortSource: textPtr(cc.CheckerEffortSource),
		RevisionDisposition: textPtr(cc.RevisionDisposition), RevisionSettledAt: stopTime(cc.RevisionSettledAt),
		RevisionSupersededByStopInputID: stopOptionalDecimal(cc.RevisionSupersededByStopInputID),
	}, nil
}
