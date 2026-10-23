package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Prepared only from the locked prewrite row. An identical stored-plan retry
// never obtains this proof and cannot accept a later primary revision.
type policyAutopilotAcceptance struct {
	previous store.Run
	source   apitypes.PolicyRevisionSourceV1
	checks   []store.CrossCheck
}

func preparePolicyAutopilotAcceptance(ctx context.Context, tx pgx.Tx, w store.Worker, r store.Run, generation *int64) (*policyAutopilotAcceptance, error) {
	if tx == nil || generation == nil || *generation <= 0 || *generation != r.ClaimGeneration ||
		!policyEligible(r) || r.TargetRunID.Valid || r.UserID != w.UserID ||
		!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != w.ID || r.ClaimReleasedAt.Valid ||
		(r.Status != "claimed" && r.Status != "running") || len(r.PolicyStopResolution) != 0 ||
		r.PlanMd.Valid || !r.AutoApprove || r.PlanSource != "agent" {
		return nil, nil
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || source.Gate != nil || !policyRevisionSourceGateMatches(r, source) ||
		!policyStopSelectionValid(source, stopUUID(r.PolicyCandidateCrossCheckID)) {
		return nil, nil
	}
	q := store.New(tx)
	pending, err := q.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if len(pending) != 0 {
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
	proof := &policyAutopilotAcceptance{previous: r, source: source}
	// Visit the finite persisted identities once, with no retries. A failed proof
	// stops sibling reads; database failures abort the caller's whole transaction.
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
		if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round <= 0 ||
			cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > r.ClaimGeneration ||
			cc.RevisionDisposition.Valid || cc.RevisionSettledAt.Valid || cc.RevisionSupersededByStopInputID.Valid {
			return nil, nil
		}
		valid, err := policyRevisionIdentityValid(ctx, q, cc)
		if err != nil || !valid {
			return nil, err
		}
		primary := source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil &&
			*source.PrimaryRevision.CrossCheckID == *ref.CrossCheckID
		if primary && (cc.Verdict != "revise" || !cc.DecidedAt.Valid) {
			return nil, nil
		}
		switch cc.Verdict {
		case "pending":
			// Running makes the same-generation verdict writer live again. Only a
			// permanently generation-fenced pending writer can be retired here.
			if cc.DecidedAt.Valid || cc.LeadClaimGeneration == r.ClaimGeneration {
				return nil, nil
			}
		case "approve", "revise", "block", "failed":
			if !cc.DecidedAt.Valid {
				return nil, nil
			}
		default:
			return nil, nil
		}
		proof.checks = append(proof.checks, cc)
	}
	return proof, nil
}

// Autopilot freezes issue milestones directly, without publishing a gate's
// milestones_candidate. Other kinds omit execution milestones, as at a human gate.
func policyAutopilotPayloadFromRow(r store.Run) gatePresentedPayload {
	payload := gatePayloadFromRow(r)
	payload.Milestones = nil
	if r.Kind == runkind.Issue {
		payload.Milestones = r.MilestonesFrozen
	}
	return payload
}

// Both guarded plan and running writes must already have affected exactly one
// row. This uses their persisted candidate, never a latest-cross-check search.
func acceptPolicyAutopilotRevision(ctx context.Context, tx pgx.Tx, proof *policyAutopilotAcceptance, plan pgtype.Text) error {
	q := store.New(tx)
	current, err := q.GetRunByID(ctx, proof.previous.ID)
	if err != nil {
		return err
	}
	if current.Status != "running" || !current.PlanMd.Valid || current.PlanMd != plan {
		return fmt.Errorf("policy autopilot persisted plan mismatch")
	}
	kept := []apitypes.PolicyRevisionRef{}
	selected := pgtype.UUID{}
	for _, cc := range proof.checks {
		if cc.Verdict != "approve" || cc.LeadClaimGeneration != current.ClaimGeneration {
			continue
		}
		projection := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones,
			RequiredCapabilities: capability.Filter(cc.RequiredCapabilities), RequiredTools: capability.FilterTools(cc.RequiredTools),
			SizeClass: nonEmptyPtr(cc.SizeClass.String)}
		if current.Kind != runkind.Issue {
			projection.Milestones = nil
		}
		projected, err := projection.canonicalJSON()
		actual, actualErr := policyAutopilotPayloadFromRow(current).canonicalJSON()
		if err != nil || actualErr != nil {
			return nil
		}
		if !bytes.Equal(projected, actual) {
			// A current selected APPROVE must be the candidate the normal writer stored.
			if current.PolicyCandidateCrossCheckID.Valid && uuid.UUID(current.PolicyCandidateCrossCheckID.Bytes) == cc.ID {
				return nil
			}
			continue
		}
		id := cc.ID.String()
		kept = append(kept, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id})
		if current.PolicyCandidateCrossCheckID.Valid && uuid.UUID(current.PolicyCandidateCrossCheckID.Bytes) == cc.ID {
			selected = current.PolicyCandidateCrossCheckID
		}
	}
	if current.PlanCrossCheckRequired && !selected.Valid {
		// Do not persist a first required plan backed only by a historical
		// selected APPROVE. After a later reclaim that history can legitimately
		// describe an accepted stored plan, so refuse this pair before it exists.
		return fmt.Errorf("%w: policy autopilot current selected approval missing", ErrInvalidState)
	}
	if !selected.Valid {
		kept = []apitypes.PolicyRevisionRef{}
	}
	source := proof.source
	source.PrimaryRevision = nil
	source.AssociatedCrossChecks = kept
	if !policyStopSelectionValid(source, stopUUID(selected)) {
		return nil
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return err
	}
	rows, err := q.AcceptPolicyAutopilotRevisionSource(ctx, store.AcceptPolicyAutopilotRevisionSourceParams{
		Source: raw, SelectedCrossCheckID: selected, RunID: current.ID, OwnerID: current.UserID,
		WorkerID: uuid.UUID(current.WorkerID.Bytes), ClaimGeneration: current.ClaimGeneration,
		PreviousSource: proof.previous.PolicyRevisionSource, PreviousCrossCheckID: proof.previous.PolicyCandidateCrossCheckID,
		PlanMd: plan,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("policy autopilot source fence: affected %d rows", rows)
	}
	return nil
}
