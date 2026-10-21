package workersvc

import (
	"encoding/json"
	"fmt"
	"strings"
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
	snapshot, err := validateSemanticApprovalIdentity(run, opts, &p)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateSemanticApproval(run, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*store.Run, *SubmitInputOptions, *apitypes.SemanticGatePresentation)
	}{
		{"stored display", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.PlanMd.String += "changed"
		}},
		{"live milestones", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.MilestonesCandidate = json.RawMessage(`[{"id":"m1","title":"Changed"}]`)
		}},
		{"live capabilities", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.RequiredCapabilities = []string{"changed"}
		}},
		{"live tools", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.RequiredTools = []string{"changed"}
		}},
		{"live size", func(r *store.Run, _ *SubmitInputOptions, _ *apitypes.SemanticGatePresentation) {
			r.SizeClass = "l"
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
			snapshot, err := validateSemanticApprovalIdentity(r, o, &copyP)
			if err == nil {
				err = validateSemanticApproval(r, snapshot)
			}
			if err == nil {
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

// BenchmarkSemanticApprovalValidation measures the identity and first-freeze
// validation work performed by submitSemanticApproval, excluding storage I/O.
func BenchmarkSemanticApprovalValidation(b *testing.B) {
	structural, proposal, sources := semanticFixture()
	proposal.Criteria = nil
	for i := range 256 {
		c := apitypes.ProposedSemanticCriterion{
			MilestoneID: fmt.Sprintf("m%d", i/32), Kind: "maintainer",
			Text:       fmt.Sprintf("%03d", i) + strings.Repeat("<", 2045),
			DoneWhen:   strings.Repeat("<", 2048),
			Maintainer: &apitypes.SemanticMaintainer{Reason: strings.Repeat("<", 2048)},
		}
		for range 8 {
			c.Citations = append(c.Citations, apitypes.SemanticCitation{SourceID: "issue", Hash: semanticHash(sources[0].Bytes), Start: 0, End: 6})
			c.EvidenceRequirements = append(c.EvidenceRequirements, apitypes.SemanticEvidence{Description: strings.Repeat("<", 512), Path: strings.Repeat("<", 512)})
			c.Maintainer.Conditions = append(c.Maintainer.Conditions, strings.Repeat("<", 512))
			c.Maintainer.VerificationSteps = append(c.Maintainer.VerificationSteps, strings.Repeat("<", 512))
		}
		proposal.Criteria = append(proposal.Criteria, c)
	}
	var milestones []Milestone
	for i := range 8 {
		milestones = append(milestones, Milestone{ID: fmt.Sprintf("m%d", i), Title: "Behavior"})
	}
	var err error
	structural.Milestones, err = json.Marshal(milestones)
	if err != nil {
		b.Fatal(err)
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		b.Fatal(err)
	}
	candidate, _, err := compileSemanticProposal(structural, raw, sources)
	if err != nil {
		b.Fatal(err)
	}
	candidate.Revision, candidate.Digest = 1, "current"
	run := store.Run{Status: "awaiting_approval", GateRevision: 2, ClaimGeneration: 7, PlanMd: stripNULParam(&structural.PlanMd), MilestonesCandidate: structural.Milestones}
	p := semanticGateProjection(&semanticCoverageTx{candidate: candidate}, run)
	payload := gatePayloadFromRow(run)
	payload.Semantic = &p
	run.GatePresentedPayload, run.GatePayloadDigest, err = payload.digest()
	if err != nil {
		b.Fatal(err)
	}
	approval := semanticApprovalFor(run, p)
	opts := SubmitInputOptions{ExpectedGateRevision: i64(2), SemanticApproval: &approval}
	b.ReportAllocs()
	b.SetBytes(int64(len(run.GatePresentedPayload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snapshot, err := validateSemanticApprovalIdentity(run, opts, &p)
		if err != nil {
			b.Fatal(err)
		}
		if err := validateSemanticApproval(run, snapshot); err != nil {
			b.Fatal(err)
		}
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
