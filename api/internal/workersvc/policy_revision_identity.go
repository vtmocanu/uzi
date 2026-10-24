package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
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

// policyRevisionPlanning detects enrollment without selecting a witness from the
// mutable candidate. Missing enrollment retains the structural profile.
func policyRevisionPlanning(ctx context.Context, reader any, r store.Run) (store.GetSemanticPlanningRow, bool, error) {
	q, ok := reader.(policyRevisionIdentityReader)
	if !ok {
		return store.GetSemanticPlanningRow{}, false, nil
	}
	planning, err := q.GetSemanticPlanning(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return planning, false, nil
	}
	if err != nil {
		return planning, false, err
	}
	var e apitypes.SemanticEnrollment
	if strictSemanticJSON(planning.Enrollment, &e) != nil || e.Profile != "semantic" {
		return planning, true, ErrSemanticGateMismatch
	}
	return planning, true, nil
}

// Exact immutable lookup; no latest-row or text recovery is permitted.
func policyRevisionSemanticTarget(ctx context.Context, reader any, r store.Run, c *apitypes.SemanticCandidate) (bool, error) {
	q, ok := reader.(policyRevisionIdentityReader)
	if !ok || c == nil || c.CandidateIdentityVersion != semanticIdentityVersion || c.Revision < 1 {
		return false, nil
	}
	digest, err := hex.DecodeString(c.Digest)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != c.Digest {
		return false, nil
	}
	if _, err := buildSemanticCompletionContract(*c); err != nil {
		return false, nil
	}
	raw, err := q.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{RunID: r.ID, Column2: c.Digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var immutable apitypes.SemanticCandidate
	return strictSemanticJSON(raw, &immutable) == nil && sameSemanticJSON(immutable, *c), nil
}

func policyRevisionCurrentTarget(ctx context.Context, reader any, r store.Run) (*apitypes.SemanticCandidate, bool, error) {
	planning, semantic, err := policyRevisionPlanning(ctx, reader, r)
	if errors.Is(err, ErrSemanticGateMismatch) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !semantic {
		return nil, true, nil
	}
	var c apitypes.SemanticCandidate
	if strictSemanticJSON(planning.Candidate, &c) != nil || c.Revision != planning.Revision {
		return nil, false, nil
	}
	valid, err := policyRevisionSemanticTarget(ctx, reader, r, &c)
	return &c, valid, err
}

// Accepted execution is validated even with an empty checker inventory. Mutable
// planning is used only to detect enrollment, never to replace frozen identity.
func policyRevisionAcceptedTarget(ctx context.Context, reader any, r store.Run) (*apitypes.SemanticCandidate, bool, error) {
	_, semantic, err := policyRevisionPlanning(ctx, reader, r)
	if errors.Is(err, ErrSemanticGateMismatch) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !semantic && !semanticCompletionRequiresLock(r) {
		return nil, true, nil
	}
	c, err := parseCompletionContract(r.CompletionContract)
	if err != nil || c.Profile != "semantic" {
		return nil, false, nil
	}
	if _, err := completionPendingFromContract(r, c); err != nil {
		return nil, false, nil
	}
	// These callers authorize gate-zero accepted execution, including plans
	// with no checker. The frozen candidate must describe this stored plan and
	// execution definitions; matching criterion membership alone is insufficient.
	if validateSemanticGateFields(policyAutopilotPayloadFromRow(r), c.SemanticPresentation) != nil {
		return nil, false, nil
	}
	target := c.SemanticPresentation.Candidate
	valid, err := policyRevisionSemanticTarget(ctx, reader, r, &target)
	return &target, valid, err
}

// The caller authenticates the historical witness first and supplies an already
// authenticated operation-specific target. This helper never chooses latest.
func policyRevisionIdentityApplicable(ctx context.Context, reader any, r store.Run, cc store.CrossCheck, target *apitypes.SemanticCandidate, noGate bool) (bool, error) {
	_, semantic, err := policyRevisionPlanning(ctx, reader, r)
	if errors.Is(err, ErrSemanticGateMismatch) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if semantic {
		if target == nil || target.CandidateIdentityVersion != semanticIdentityVersion || target.Revision < 1 || target.Digest != hex.EncodeToString(cc.CandidateDigest) {
			return false, nil
		}
	}
	if !semantic && target != nil {
		return false, nil
	}
	projection := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones, RequiredCapabilities: capability.Filter(cc.RequiredCapabilities), RequiredTools: capability.FilterTools(cc.RequiredTools), SizeClass: nonEmptyPtr(cc.SizeClass.String)}
	if r.Kind != runkind.Issue {
		projection.Milestones = nil
	}
	current := gatePayloadFromRow(r)
	if noGate {
		current = policyAutopilotPayloadFromRow(r)
	}
	a, ae := projection.canonicalJSON()
	b, be := current.canonicalJSON()
	return ae == nil && be == nil && bytes.Equal(a, b), nil
}
