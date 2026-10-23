package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyRevisionApprovalReader keeps source-proof reads out of the legacy Store
// contract. A store without these reads cannot establish constrained approval.
type policyRevisionApprovalReader interface {
	ListPolicyStopRevisionInputs(context.Context, uuid.UUID) ([]store.RunUserInput, error)
	GetPolicyStopCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error)
	HasPolicyCurrentGateApproval(context.Context, store.HasPolicyCurrentGateApprovalParams) (bool, error)
}

type policyRevisionLegacyReader interface {
	HasPolicyRevisionHistory(context.Context, uuid.UUID) (bool, error)
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
		if m.SourceContext.PrimaryRevision != nil {
			return false
		}
		// Parsing above validates the complete immutable history. Only pending
		// feedback is a waiver; applied history cannot erase a current approval.
		for _, human := range m.HumanFeedback {
			if human.AppliedAt == nil {
				return false
			}
		}
		// The accepted manifest survives settlement. Each stored witness is visited
		// once; a revision refuses immediately, without reading mutable child rows.
		for _, cc := range m.CrossCheckFeedback {
			if cc.Supersede || cc.Verdict == "revise" {
				return false
			}
		}
	}

	// SQL NULL is unknown coverage, never a V1 no-revision witness. Retain the
	// no-gate legacy implementation fallback only without recorded revision
	// history. Invalidation must not turn an unverified revision into approval;
	// later plan frames and generation changes cannot establish acceptance.
	if len(r.PolicyRevisionSource) == 0 && r.GateRevision == 0 && !r.GatePresentationID.Valid {
		if !legacyApproved {
			return false
		}
		q, ok := reader.(policyRevisionLegacyReader)
		if !ok {
			return false
		}
		hasRevision, err := q.HasPolicyRevisionHistory(ctx, r.ID)
		return err == nil && !hasRevision
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyRevisionSourceGateMatches(r, source) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
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
	// One inventory read, with no retries. Only positively bound older gates
	// are inapplicable. Current, unbound and future revisions cannot establish
	// absence, including when this snapshot predates a later publication.
	for _, input := range inputs {
		if input.GateBinding.Valid && input.GateBinding.String == "bound" &&
			input.GateRevision.Valid && input.GateRevision.Int64 > 0 && input.GateRevision.Int64 < r.GateRevision {
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
			cc.Round < 1 || cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > r.ClaimGeneration || cc.Verdict != "approve" ||
			!cc.DecidedAt.Valid || cc.RevisionSettledAt.Valid || cc.RevisionDisposition.Valid || cc.RevisionSupersededByStopInputID.Valid {
			return false
		}
		valid, err := policyRevisionIdentityValid(ctx, reader, cc)
		if err != nil || !valid {
			return false
		}
		projection := gatePresentedPayload{
			PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones,
			RequiredCapabilities: capability.Filter(cc.RequiredCapabilities),
			RequiredTools:        capability.FilterTools(cc.RequiredTools), SizeClass: nonEmptyPtr(cc.SizeClass.String),
		}
		if r.Kind != runkind.Issue {
			projection.Milestones = nil
		}
		projected, err := projection.canonicalJSON()
		currentPayload := gatePayloadFromRow(r)
		if source.Gate == nil {
			currentPayload = policyAutopilotPayloadFromRow(r)
		}
		current, currentErr := currentPayload.canonicalJSON()
		if err != nil || currentErr != nil || !bytes.Equal(projected, current) {
			return false
		}
	}
	if source.Gate == nil {
		// Birth coverage is not approval. Seeded plans are validated at creation;
		// agent plans are stored by the guarded autopilot plan/running pair.
		return policyRevisionNoGatePlanApproved(r)
	}
	// A published gate requires its own applied receipt, including auto/seeded runs.
	approved, err := q.HasPolicyCurrentGateApproval(ctx, store.HasPolicyCurrentGateApprovalParams{
		RunID: r.ID, GateRevision: r.GateRevision,
	})
	return err == nil && approved
}

// policyRevisionNoGatePlanApproved checks persisted plan provenance only. Source,
// selection, input inventory and exact checker witnesses are validated by callers.
func policyRevisionNoGatePlanApproved(r store.Run) bool {
	return r.PlanMd.Valid && strings.TrimSpace(r.PlanMd.String) != "" &&
		(!r.PlanCrossCheckRequired || r.PolicyCandidateCrossCheckID.Valid) &&
		(r.PlanSource == planSourceSeeded || (r.AutoApprove && r.PlanSource == "agent"))
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
