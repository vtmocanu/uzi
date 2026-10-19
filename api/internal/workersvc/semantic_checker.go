package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// semanticCheckerDigest reproduces the immutable compiler identity, including
// all seven structural fields, source references, criteria IDs and revision.
func semanticCheckerDigest(structural PlanCrossCheckCandidate, candidate apitypes.SemanticCandidate) ([]byte, error) {
	if candidate.CandidateIdentityVersion != semanticIdentityVersion || candidate.Revision < 1 {
		return nil, ErrCrossCheckRefused
	}
	if _, err := buildSemanticCompletionContract(candidate); err != nil {
		return nil, ErrCrossCheckRefused
	}
	structuralDigest, err := structural.Digest()
	if err != nil {
		return nil, err
	}
	criteria := make([]apitypes.ProposedSemanticCriterion, len(candidate.Criteria))
	for i, c := range candidate.Criteria {
		criteria[i] = c.ProposedSemanticCriterion
	}
	canonical, err := json.Marshal(struct {
		Structural string
		Sources    []apitypes.SemanticSourceRef
		Criteria   []apitypes.ProposedSemanticCriterion
	}{hex.EncodeToString(structuralDigest), candidate.Sources, criteria})
	if err != nil {
		return nil, err
	}
	fp := semanticHash(append([]byte("uzi:semantic_v1:proposal\x00"), canonical...))
	candidate.Digest = ""
	identity, err := json.Marshal(struct {
		Fingerprint string
		Candidate   apitypes.SemanticCandidate
	}{fp, candidate})
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(semanticHash(append([]byte("uzi:semantic_v1:candidate\x00"), identity...)))
}

func validateSemanticCheckerCandidate(c PlanCrossCheckCandidate, expected []byte) error {
	if c.CandidateIdentityVersion != semanticIdentityVersion || c.SemanticCandidate == nil {
		return ErrCrossCheckRefused
	}
	digest, err := semanticCheckerDigest(c, *c.SemanticCandidate)
	if err != nil || !bytes.Equal(digest, expected) || c.SemanticCandidate.Digest != hex.EncodeToString(expected) {
		return ErrCrossCheckRefused
	}
	return nil
}

// validateSemanticPlanWrite keeps the structural SQL plan-write guard meaningful
// when candidate_digest names semantic history. Caller already owns the lead lock.
func validateSemanticPlanWrite(ctx context.Context, q *store.Queries, runID uuid.UUID, p store.SetRunAutopilotPlanParams) error {
	planning, err := q.GetSemanticPlanning(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var current apitypes.SemanticCandidate
	if strictSemanticJSON(planning.Candidate, &current) != nil || current.Digest != hex.EncodeToString(p.CandidateDigest) {
		return ErrCrossCheckRefused
	}
	cc, err := q.GetPlanCrossCheck(ctx, runID)
	if err != nil || !bytes.Equal(cc.CandidateDigest, p.CandidateDigest) {
		return ErrCrossCheckRefused
	}
	semantic, err := rehydrateSemanticChecker(ctx, q, cc)
	if err != nil || semantic == nil {
		return ErrCrossCheckRefused
	}
	input, err := crossCheckClaimInput(cc, semantic)
	if err != nil {
		return err
	}
	c := input.PlanCrossCheckCandidate
	c.PlanMd, c.Milestones, c.RequiredCapabilities, c.RequiredTools, c.SizeClass = p.PlanMd.String, p.MilestonesFrozen, p.InferredCapabilities, p.InferredTools, p.SizeClass.String
	return validateSemanticCheckerCandidate(c, p.CandidateDigest)
}

// bindSemanticCheckerCandidate selects identity only from frozen enrollment.
// The caller holds the lead run lock. Wire-supplied semantic payloads grant no
// authority: the server resolves the current immutable history row itself.
func bindSemanticCheckerCandidate(ctx context.Context, q *store.Queries, runID uuid.UUID, c PlanCrossCheckCandidate) (PlanCrossCheckCandidate, []byte, error) {
	planning, err := q.GetSemanticPlanning(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		if c.CandidateIdentityVersion != "" || c.SemanticCandidate != nil {
			return c, nil, ErrCrossCheckRefused
		}
		digest, err := c.Digest()
		return c, digest, err
	}
	if err != nil {
		return c, nil, err
	}
	var enrollment apitypes.SemanticEnrollment
	if strictSemanticJSON(planning.Enrollment, &enrollment) != nil || enrollment.Profile != "semantic" {
		return c, nil, ErrCrossCheckRefused
	}
	var current apitypes.SemanticCandidate
	if strictSemanticJSON(planning.Candidate, &current) != nil {
		return c, nil, ErrCrossCheckRefused
	}
	raw, err := q.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{RunID: runID, Column2: current.Digest})
	if err != nil {
		return c, nil, ErrCrossCheckRefused
	}
	var immutable apitypes.SemanticCandidate
	if strictSemanticJSON(raw, &immutable) != nil {
		return c, nil, ErrCrossCheckRefused
	}
	c.CandidateIdentityVersion, c.SemanticCandidate = semanticIdentityVersion, &immutable
	digest, err := hex.DecodeString(immutable.Digest)
	if err != nil || validateSemanticCheckerCandidate(c, digest) != nil {
		return c, nil, ErrCrossCheckRefused
	}
	return c, digest, nil
}

// rehydrateSemanticChecker resolves history by the stored digest, never the
// current candidate. Historical checker evidence stays historical after revision.
func rehydrateSemanticChecker(ctx context.Context, q *store.Queries, cc store.CrossCheck) (*apitypes.SemanticCandidate, error) {
	planning, err := q.GetSemanticPlanning(ctx, cc.LeadRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var enrollment apitypes.SemanticEnrollment
	if strictSemanticJSON(planning.Enrollment, &enrollment) != nil || enrollment.Profile != "semantic" {
		return nil, ErrCrossCheckRefused
	}
	raw, err := q.GetSemanticCandidateByDigest(ctx, store.GetSemanticCandidateByDigestParams{RunID: cc.LeadRunID, Column2: hex.EncodeToString(cc.CandidateDigest)})
	if err != nil {
		return nil, ErrCrossCheckRefused
	}
	var candidate apitypes.SemanticCandidate
	if strictSemanticJSON(raw, &candidate) != nil {
		return nil, ErrCrossCheckRefused
	}
	return &candidate, nil
}
