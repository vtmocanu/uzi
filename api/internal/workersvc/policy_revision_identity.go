package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Optional on legacy approval readers; transaction-bound Queries implements both
// reads. History is selected by the witness digest, never planning.Candidate.
type policyRevisionIdentityReader interface {
	GetSemanticPlanning(context.Context, uuid.UUID) (store.GetSemanticPlanningRow, error)
	GetSemanticCandidateByDigest(context.Context, store.GetSemanticCandidateByDigestParams) ([]byte, error)
}

// Invalid evidence returns false; storage failures remain errors so transactional
// callers can abort. Approval's boolean interface deliberately refuses on either.
func policyRevisionIdentityValid(ctx context.Context, reader any, cc store.CrossCheck) (bool, error) {
	candidate := PlanCrossCheckCandidate{
		PlanMd: cc.PlanMd.String, Milestones: cc.Milestones,
		RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools,
		SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String,
	}
	q, ok := reader.(policyRevisionIdentityReader)
	if ok {
		planning, err := q.GetSemanticPlanning(ctx, cc.LeadRunID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
		if err == nil {
			var enrollment apitypes.SemanticEnrollment
			if strictSemanticJSON(planning.Enrollment, &enrollment) != nil || enrollment.Profile != "semantic" {
				return false, nil
			}
			raw, err := q.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{
				RunID: cc.LeadRunID, Column2: hex.EncodeToString(cc.CandidateDigest),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			var immutable apitypes.SemanticCandidate
			if strictSemanticJSON(raw, &immutable) != nil {
				return false, nil
			}
			candidate.CandidateIdentityVersion, candidate.SemanticCandidate = semanticIdentityVersion, &immutable
			return validateSemanticCheckerCandidate(candidate, cc.CandidateDigest) == nil, nil
		}
	}
	// No enrollment row is the structural profile. Legacy readers without the
	// optional semantic reads can prove only the original structural identity;
	// a semantic_v1 digest cannot pass this comparison.
	digest, err := candidate.Digest()
	return err == nil && bytes.Equal(digest, cc.CandidateDigest), nil
}
