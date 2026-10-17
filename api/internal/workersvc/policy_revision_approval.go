package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyRevisionApprovalReader keeps source-proof reads out of the legacy Store
// contract. A store without these reads cannot establish constrained approval.
type policyRevisionApprovalReader interface {
	ListPolicyStopRevisionInputs(context.Context, uuid.UUID) ([]store.RunUserInput, error)
	GetPolicyStopCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error)
	HasPolicyCurrentGateApproval(context.Context, store.HasPolicyCurrentGateApprovalParams) (bool, error)
}

// policyRevisionPlanApproved is read-only and independent of paused eligibility
// and Stop actions. Future state/ACK/running callers must supply their locked row
// and transaction-bound reader; claim assembly currently supplies its claim row.
// legacyApproved is used only outside the constrained source-authority boundary.
func policyRevisionPlanApproved(ctx context.Context, reader any, r store.Run, legacyApproved bool) bool {
	if r.PolicyRefusalCount <= 0 || !policyEligible(r) {
		return legacyApproved
	}
	if len(r.PolicyStopResolution) != 0 {
		record, err := parsePolicyStopResolution(r.PolicyStopResolution, r.UserID, r.ID)
		if err != nil {
			return false
		}
		m := record.Manifest
		if m.SourceContext.PrimaryRevision != nil || len(m.HumanFeedback) != 0 {
			return false
		}
		// The accepted manifest survives settlement. Each stored witness is visited
		// once; a revision refuses immediately, without reading mutable child rows.
		for _, cc := range m.CrossCheckFeedback {
			if cc.Supersede || cc.Verdict == "revise" {
				return false
			}
		}
	}

	// SQL NULL is unknown coverage, never a V1 no-revision witness. Preserve
	// ordinary legacy implementation approval only when there is no gate/source.
	if len(r.PolicyRevisionSource) == 0 && r.GateRevision == 0 && !r.GatePresentationID.Valid {
		return legacyApproved
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyRevisionGateMatches(r, source) {
		return false
	}
	// A primary revision remains unapproved even after its row was Stop-settled.
	// Gate authentication above binds the original persisted source to this candidate.
	if source.PrimaryRevision != nil {
		return false
	}
	q, ok := reader.(policyRevisionApprovalReader)
	if !ok {
		return false
	}
	inputs, err := q.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return false
	}
	// One inventory read, with no retries. Other bound gates are inapplicable;
	// an unbound pending revision cannot establish absence at this gate.
	for _, input := range inputs {
		if input.GateBinding.Valid && input.GateBinding.String == "bound" &&
			input.GateRevision.Valid && input.GateRevision.Int64 != r.GateRevision {
			continue
		}
		return false
	}
	// Read only exact UUIDs from the finite full-source association set. All
	// reads use ctx and run once; any missing/invalid witness stops sibling reads.
	for _, ref := range source.AssociatedCrossChecks {
		id, err := uuid.Parse(*ref.CrossCheckID)
		if err != nil {
			return false
		}
		cc, err := q.GetPolicyStopCrossCheck(ctx, id)
		if err != nil || cc.LeadRunID != r.ID || cc.Stage != "plan" ||
			cc.Round < 1 || cc.LeadClaimGeneration <= 0 || cc.Verdict != "approve" ||
			!cc.DecidedAt.Valid || cc.RevisionSettledAt.Valid || cc.RevisionDisposition.Valid {
			return false
		}
		candidate := PlanCrossCheckCandidate{
			PlanMd: cc.PlanMd.String, Milestones: cc.Milestones,
			RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools,
			SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String,
		}
		digest, err := candidate.Digest()
		if err != nil || !bytes.Equal(digest, cc.CandidateDigest) {
			return false
		}
		projection := gatePresentedPayload{
			PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones,
			RequiredCapabilities: capability.Filter(cc.RequiredCapabilities),
			RequiredTools:        capability.FilterTools(cc.RequiredTools), SizeClass: nonEmptyPtr(cc.SizeClass.String),
		}
		projected, err := projection.canonicalJSON()
		current, currentErr := gatePayloadFromRow(r).canonicalJSON()
		if err != nil || currentErr != nil || !bytes.Equal(projected, current) {
			return false
		}
	}
	// Auto/seeded authority is permitted only after verified absence of applicable
	// revisions. Historical approval alone cannot authorize this candidate.
	approved, err := q.HasPolicyCurrentGateApproval(ctx, store.HasPolicyCurrentGateApprovalParams{
		RunID: r.ID, GateRevision: r.GateRevision,
	})
	return err == nil && (approved || r.AutoApprove || r.PlanSource == planSourceSeeded)
}

// policyRevisionGateMatches uses the same canonical gate helpers as
// policyStopManifestLocked without requiring a paused row or completed scope.
func policyRevisionGateMatches(r store.Run, source apitypes.PolicyRevisionSourceV1) bool {
	if source.Gate == nil || r.GateRevision <= 0 || !r.GatePresentationID.Valid {
		return false
	}
	gate := apitypes.PolicyStopGateContext{
		Revision:       stopDecimal(r.GateRevision),
		PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(),
		PayloadDigest:  hex.EncodeToString(r.GatePayloadDigest),
	}
	if *source.Gate != gate {
		return false
	}
	payload, ok, err := decodeGatePresentedPayload(r.GatePresentedPayload)
	if err != nil || !ok {
		return false
	}
	_, digest, err := payload.digest()
	if err != nil || !bytes.Equal(digest, r.GatePayloadDigest) {
		return false
	}
	payload.ReviseCount = nil
	payload.CheckpointWitness = nil
	payload.WitnessWorkerID = nil
	payload.WitnessClaimGeneration = nil
	visible, err := payload.canonicalJSON()
	current, currentErr := gatePayloadFromRow(r).canonicalJSON()
	return err == nil && currentErr == nil && bytes.Equal(visible, current)
}
