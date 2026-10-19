package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSemanticGateExactApproval(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	structural.PlanMd = "implement\x00 café 😀 behavior"
	proposal.Criteria[0].Kind = "maintainer"
	proposal.Criteria[0].Maintainer = &apitypes.SemanticMaintainer{Reason: "owner infrastructure", Conditions: []string{"deployment available"}, VerificationSteps: []string{"observe rollout"}}
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision = 1
	candidate.Digest = "current"
	candidate.Criteria[0].ID = uuid.NewString()
	c := &semanticCoverageTx{candidate: candidate, policy: apitypes.SemanticPolicy{MaxReviews: 3, MaxTokens: 120000, MaxSeconds: 900}}
	run := store.Run{Status: "awaiting_approval", GateRevision: 2, ClaimGeneration: 7, PlanMd: stripNULParam(&structural.PlanMd), MilestonesCandidate: structural.Milestones}
	p := semanticGateProjection(c, run)
	payload := gatePayloadFromRow(run)
	payload.Semantic = &p
	run.GatePresentedPayload, run.GatePayloadDigest, err = payload.digest()
	if err != nil {
		t.Fatal(err)
	}
	approval := semanticApprovalFor(run, p)
	opts := SubmitInputOptions{ExpectedGateRevision: i64(2), SemanticApproval: &approval}
	if err = validateSemanticApproval(run, opts, &p); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*store.Run, *SubmitInputOptions, *apitypes.SemanticGatePresentation)
	}{
		{"stored display", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.PlanMd.String += "changed"
		}},
		{"unbound", func(_ *store.Run, o *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			o.SemanticApproval = nil
		}},
		{"no expected revision", func(_ *store.Run, o *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			o.ExpectedGateRevision = nil
		}},
		{"ordinary answer", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.Status = "awaiting_input"
		}},
		{"revision", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) { r.GateRevision++ }},
		{"digest", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.GatePayloadDigest = []byte("different")
		}},
		{"candidate", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.Candidate.Digest = "new"
		}},
		{"source", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.Binding.SourceDigest = "new"
		}},
		{"criteria", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.Binding.CriteriaDigest = "new"
		}},
		{"generation", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) { p.ClaimGeneration++ }},
		{"reservation", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.InvocationID = "foreign"
		}},
		{"report", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.ReportDigest = "foreign"
		}},
		{"maintainer conditions", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.Maintainers[0].Conditions = []string{"different"}
		}},
		{"extra maintainer", func(_ *store.Run, _ *SubmitInputOptions, p *apitypes.SemanticGatePresentation) {
			p.Maintainers = append(p.Maintainers, apitypes.SemanticMaintainerApproval{ID: "foreign"})
		}},
		{"released", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.ClaimReleasedAt = pgtype.Timestamptz{Valid: true}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var copyP apitypes.SemanticGatePresentation
			if err := json.Unmarshal(semanticJSON(t, p), &copyP); err != nil {
				t.Fatal(err)
			}
			r, o := run, opts
			tc.edit(&r, &o, &copyP)
			if err := validateSemanticApproval(r, o, &copyP); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
	frozen, err := semanticGateContract(&p, &approval)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := parseCompletionContract(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSemanticJSON(decoded.SemanticApproval, &approval) || !sameSemanticJSON(decoded.SemanticPresentation, &p) {
		t.Fatal("revision decode lost semantic binding")
	}
}

func TestSemanticRawPlanDisplayIdentity(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	prd := apitypes.SemanticSourceCapture{ID: "linked-prd", Kind: "prd", Provenance: "repo:prd", Completeness: "complete", Bytes: []byte("café\x00 😀 requirement")}
	sources = append(sources, prd)
	proposal.Criteria[0].Citations = append(proposal.Criteria[0].Citations, apitypes.SemanticCitation{SourceID: prd.ID, Hash: semanticHash(prd.Bytes), Start: 0, End: len(prd.Bytes)})
	structural.PlanMd = "café\x00 😀 plan"
	raw, fp, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	structural.PlanMd = "café 😀 plan"
	visible, otherFP, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	if raw.PlanHash == visible.PlanHash || fp == otherFP || raw.PlanFieldsDigest != visible.PlanFieldsDigest {
		t.Fatal("raw identity or display projection conflated")
	}
	payload := gatePresentedPayload{PlanMd: &structural.PlanMd, Milestones: structural.Milestones}
	p := &apitypes.SemanticGatePresentation{Candidate: raw}
	if err := validateSemanticGateFields(payload, p); err != nil {
		t.Fatal(err)
	}
	// Reproduce the old writer's digest without changing persisted mappings.
	oldPlan := "café\x00 😀 plan"
	legacy := payload
	legacy.PlanMd = &oldPlan
	canonical, err := legacy.canonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	p.Candidate.PlanFieldsDigest = semanticHash(canonical)
	if err := validateSemanticGateFields(payload, p); err == nil {
		t.Fatal("legacy raw fields digest silently reinterpreted")
	}
	for _, plan := range []string{"\x00", " \x00\n\t"} {
		structural.PlanMd = plan
		if _, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources); err == nil {
			t.Fatal("display-empty raw plan accepted")
		}
	}
	structural.PlanMd = "valid plan"
	sources[0].Bytes = []byte("\x00")
	proposal.Criteria[0].Citations[0] = apitypes.SemanticCitation{SourceID: sources[0].ID, Hash: semanticHash(sources[0].Bytes), Start: 0, End: 1}
	if _, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources); err != nil {
		t.Fatalf("NUL-only raw source rejected: %v", err)
	}
}

func TestSemanticGateDigestIncludesPresentation(t *testing.T) {
	p := gatePresentedPayload{Semantic: &apitypes.SemanticGatePresentation{Candidate: apitypes.SemanticCandidate{Digest: "a"}}}
	_, a, err := p.digest()
	if err != nil {
		t.Fatal(err)
	}
	p.Semantic.Candidate.Digest = "b"
	_, b, err := p.digest()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("semantic binding absent from digest")
	}
}
