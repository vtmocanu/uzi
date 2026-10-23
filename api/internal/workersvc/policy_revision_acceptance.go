package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// preparePolicyRevisionAcceptance runs under the receipt run lock, BEFORE any
// batch member is stamped. Negative or unknown evidence leaves ordinary receipts
// compatible; database failures abort the transaction.
func preparePolicyRevisionAcceptance(ctx context.Context, tx pgx.Tx, w store.Worker, runID uuid.UUID, generation int64, rows []store.ListInputReceiptRowsRow) (*store.AcceptPolicyRevisionSourceParams, error) {
	hasFreshApproval := false
	for _, row := range rows {
		hasFreshApproval = hasFreshApproval || (row.Kind == "approve_plan" && !row.AppliedAt.Valid && row.GateBinding.String == "bound")
	}
	if !hasFreshApproval || generation <= 0 {
		return nil, nil
	}
	q := store.New(tx)
	r, err := q.GetRunByID(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !policyEligible(r) || r.TargetRunID.Valid || r.UserID != w.UserID ||
		!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != w.ID || r.ClaimGeneration != generation ||
		r.ClaimReleasedAt.Valid || r.Status == "queued" || terminalStatuses[r.Status] || len(r.PolicyStopResolution) != 0 {
		return nil, nil
	}
	var approvalID int64
	for _, row := range rows {
		if row.Kind == "approve_plan" && !row.AppliedAt.Valid && row.GateBinding.String == "bound" &&
			row.GateRevision.Valid && row.GateRevision.Int64 == r.GateRevision &&
			!row.Disposition.Valid && !row.SettledAt.Valid && !row.SupersededByStopInputID.Valid {
			approvalID = row.ID
			break
		}
	}
	if approvalID == 0 {
		return nil, nil
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !policyRevisionGateMatches(r, source) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}

	pending, err := q.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, row := range pending {
		if row.GateBinding.String == "bound" && row.GateRevision.Valid && row.GateRevision.Int64 > 0 && row.GateRevision.Int64 < r.GateRevision {
			continue
		}
		return nil, nil
	}
	if source.PrimaryRevision != nil && source.PrimaryRevision.InputID != nil {
		valid, err := policyAcceptanceHumanPrimary(ctx, tx, r, *source.PrimaryRevision.InputID)
		if err != nil || !valid {
			return nil, err
		}
	}
	refs := append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil {
		refs = append(refs, *source.PrimaryRevision)
	}
	kept := []apitypes.PolicyRevisionRef{}
	selected := pgtype.UUID{}
	// Finite persisted inventory only, one read per identity, no retries. A failed
	// proof stops sibling reads; no history/content search can invent coverage.
	for _, ref := range refs {
		id, err := uuid.Parse(*ref.CrossCheckID)
		if err != nil {
			return nil, nil
		}
		cc, err := q.GetPolicyStopCrossCheck(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round <= 0 || cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > r.ClaimGeneration ||
			cc.RevisionDisposition.Valid || cc.RevisionSettledAt.Valid || cc.RevisionSupersededByStopInputID.Valid {
			return nil, nil
		}
		valid, err := policyRevisionIdentityValid(ctx, q, cc)
		if err != nil || !valid {
			return nil, err
		}
		primary := source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil && *source.PrimaryRevision.CrossCheckID == *ref.CrossCheckID
		if primary && (cc.Verdict != "revise" || !cc.DecidedAt.Valid) {
			return nil, nil
		}
		projection := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones,
			RequiredCapabilities: capability.Filter(cc.RequiredCapabilities), RequiredTools: capability.FilterTools(cc.RequiredTools),
			SizeClass: nonEmptyPtr(cc.SizeClass.String)}
		// The report writer gates execution milestones to issue runs. Tasks,
		// prompts and self-improvement candidates still carry a non-NULL array
		// in cross_checks, but their presented gate omits it.
		if r.Kind != runkind.Issue {
			projection.Milestones = nil
		}
		projected, err := projection.canonicalJSON()
		current, currentErr := gatePayloadFromRow(r).canonicalJSON()
		if err != nil || currentErr != nil {
			return nil, nil
		}
		applicable := cc.LeadClaimGeneration == r.ClaimGeneration && bytes.Equal(projected, current)
		if cc.Verdict == "pending" {
			// LockPlanCrossCheckLeadForVerdict and DecidePlanCrossCheck both require
			// claimed/running plus the original generation while holding this run lock.
			// Drop historical pending evidence only when that late writer is fenced.
			fenced := cc.LeadClaimGeneration != r.ClaimGeneration || (r.Status != "claimed" && r.Status != "running")
			if cc.DecidedAt.Valid || applicable || !fenced {
				return nil, nil
			}
			continue
		}
		if !cc.DecidedAt.Valid {
			return nil, nil
		}
		switch cc.Verdict {
		case "approve":
			if applicable && !primary {
				kept = append(kept, ref)
				if r.PolicyCandidateCrossCheckID.Valid && uuid.UUID(r.PolicyCandidateCrossCheckID.Bytes) == id {
					selected = r.PolicyCandidateCrossCheckID
				}
			}
		case "revise", "block", "failed":
			// A human may waive decided advice. Preserve the immutable feedback row.
		default:
			return nil, nil
		}
	}
	source.PrimaryRevision = nil
	// A waived selection cannot be replaced by another retained checker. Keep
	// historical APPROVE rows, but retire their live references as well when no
	// selected witness survives, preserving the D18 NULL-selection invariant.
	if !selected.Valid {
		kept = []apitypes.PolicyRevisionRef{}
	}
	source.AssociatedCrossChecks = kept
	if !policyStopSelectionValid(source, stopUUID(selected)) {
		return nil, nil
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	return &store.AcceptPolicyRevisionSourceParams{
		Source: raw, SelectedCrossCheckID: selected, RunID: r.ID, OwnerID: w.UserID,
		WorkerID: w.ID, ClaimGeneration: generation, PreviousSource: r.PolicyRevisionSource,
		PreviousCrossCheckID: r.PolicyCandidateCrossCheckID, GateRevision: r.GateRevision,
		PresentationID: uuid.UUID(r.GatePresentationID.Bytes), PayloadDigest: r.GatePayloadDigest,
		PresentedPayload: r.GatePresentedPayload, ApprovalID: approvalID,
	}, nil
}

func policyAcceptanceHumanPrimary(ctx context.Context, tx pgx.Tx, r store.Run, value apitypes.PolicyStopDecimal) (bool, error) {
	id, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		return false, nil
	}
	var valid bool
	var origin pgtype.Int8
	var binding pgtype.Text
	var applied bool
	err = tx.QueryRow(ctx, `SELECT (
  run_id=$2 AND kind='revise_plan' AND consumed_at IS NOT NULL
  AND consumed_worker_id IS NOT NULL AND consumed_claim_generation>0
  AND consumed_claim_generation <= $3
  AND included_at IS NULL AND settled_at IS NULL AND disposition IS NULL
  AND superseded_by_stop_input_id IS NULL
 ) IS TRUE, gate_revision, gate_binding, applied_at IS NOT NULL
 FROM run_user_inputs WHERE id=$1`, id, r.ID, r.ClaimGeneration).Scan(&valid, &origin, &binding, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || !valid {
		return false, err
	}
	if origin.Valid && (origin.Int64 < 0 || origin.Int64 > r.GateRevision) {
		return false, nil
	}
	if binding.String == "bound" && (!origin.Valid || origin.Int64 <= 0) {
		return false, nil
	}
	var metadata struct {
		Receipt *revisionRetryReceipt `json:"revision_retry_receipt"`
	}
	if len(r.GatePresentedPayload) == 0 {
		if r.GateRevision != 0 || r.GatePresentationID.Valid {
			return false, nil
		}
	} else if json.Unmarshal(r.GatePresentedPayload, &metadata) != nil {
		return false, nil
	}
	if receipt := metadata.Receipt; receipt != nil {
		return applied && binding.String == "bound" && origin.Valid && origin.Int64 > 0 &&
			origin.Int64 < r.GateRevision && receipt.SourceRevision == origin.Int64 && receipt.RevisionInputID == id, nil
	}
	// Without publication linkage the exact consumed row is still historical
	// evidence, never approval. Pending applicability was checked before this read.
	return true, nil
}
