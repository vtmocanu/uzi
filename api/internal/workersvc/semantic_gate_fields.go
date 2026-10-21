package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func semanticEnrollmentPresent(ctx context.Context, q *store.Queries, id uuid.UUID) (bool, error) {
	_, err := q.GetSemanticPlanning(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// semanticPlanFieldsDigest binds the gate-visible structural plan fields in
// addition to semantic_v1's existing checker identity (which also includes diff
// and base). Witnesses and revision counters belong to the presentation digest.
func semanticPlanFieldsDigest(plan string, milestones json.RawMessage, caps, tools []string, size string) (string, error) {
	// Match the existing PostgreSQL display projection; raw identity stays in PlanHash.
	plan, _ = stripNUL(plan)
	p := gatePresentedPayload{PlanMd: &plan, Milestones: milestones, RequiredCapabilities: caps, RequiredTools: tools, SizeClass: nonEmptyPtr(size)}
	raw, err := p.canonicalJSON()
	if err != nil {
		return "", err
	}
	return semanticHash(raw), nil
}

// SemanticGateBindingFromRun projects only the immutable presentation, never a newer candidate.
func SemanticGateBindingFromRun(run store.Run) (*apitypes.SemanticGatePresentation, *apitypes.SemanticGateApproval) {
	p, ok, err := decodeGatePresentedPayload(run.GatePresentedPayload)
	if err != nil || !ok || p.Semantic == nil {
		return nil, nil
	}
	approval := semanticApprovalFor(run, *p.Semantic)
	return p.Semantic, &approval
}

func validateSemanticGateFields(payload gatePresentedPayload, p *apitypes.SemanticGatePresentation) error {
	size := ""
	if payload.SizeClass != nil {
		size = *payload.SizeClass
	}
	if payload.PlanMd == nil {
		return ErrSemanticGateMismatch
	}
	digest, err := semanticPlanFieldsDigest(*payload.PlanMd, payload.Milestones, payload.RequiredCapabilities, payload.RequiredTools, size)
	if err != nil || p.Candidate.PlanFieldsDigest == "" || digest != p.Candidate.PlanFieldsDigest {
		return ErrSemanticGateMismatch
	}
	return nil
}
