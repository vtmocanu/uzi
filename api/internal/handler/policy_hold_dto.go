package handler

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func policyHoldDTO(r store.Run) *apitypes.PolicyHoldDTO {
	if !r.PolicyOutcomeID.Valid {
		return nil
	}
	var capturedHead *string
	if r.Status == "paused" && r.HoldReason.String == "codex_policy_hold" && !r.PolicyResolvedAt.Valid {
		capturedHead = textPtrValue(r.HoldCapturedHead.Valid, r.HoldCapturedHead.String)
	}
	return &apitypes.PolicyHoldDTO{
		PreserveRequired: r.PolicyPreserveRequired, PendingFresh: r.PolicyPendingFresh,
		OutcomeID: *uuidPtrValue(r.PolicyOutcomeID), ContinuationMode: r.PolicyContinuationMode.String,
		ResolvedAt:         timePtr(r.PolicyResolvedAt.Valid, r.PolicyResolvedAt.Time),
		ApplyingWorkerID:   uuidPtrValue(r.PolicyApplyingWorkerID),
		ApplyingGeneration: r.PolicyApplyingGeneration.Int64, SourceGeneration: r.PolicySourceGeneration.Int64,
		RefusalCount: int(r.PolicyRefusalCount), PolicyTag: textPtrValue(r.PolicyRefusalTag.Valid, r.PolicyRefusalTag.String),
		Phase: textPtrValue(r.PolicyRefusalPhase.Valid, r.PolicyRefusalPhase.String), CorrelationID: textPtrValue(r.PolicyRefusalCorrelationID.Valid, r.PolicyRefusalCorrelationID.String),
		Published: boolPtrValue(r.PolicyHoldPublished), CapturedHead: capturedHead,
		HoldCause: textPtrValue(r.PolicyHoldCause.Valid, r.PolicyHoldCause.String), ResumeModes: policyResumeModes(r),
		RecoveryRequired: r.PolicyRequiredHead.Valid,
		RecoveryVerifiedThisClaim: r.PolicyRequiredHead.Valid && r.PolicyRecoveryVerifiedGeneration.Valid &&
			r.PolicyRecoveryVerifiedGeneration.Int64 == r.ClaimGeneration &&
			r.PolicyRecoveryVerifiedHead.Valid && r.PolicyRecoveryVerifiedHead.String == r.PolicyRequiredHead.String,
	}
}

// policyResumeModes names the owner actions the exact held episode accepts. Only the shipped modes
// appear: the lane decides which one, and any other state offers none.
func policyResumeModes(r store.Run) []string {
	modes := []string{}
	if r.Status != "paused" || r.HoldReason.String != "codex_policy_hold" || r.PolicyResolvedAt.Valid {
		return modes
	}
	switch r.PolicyContinuationMode.String {
	case "fresh_from_saved_work":
		modes = append(modes, "fresh_from_saved_work")
	case "same_conversation":
		modes = append(modes, "preserve")
	}
	return modes
}
