package workersvc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

func TestSemanticCheckerHighDensityEnvelope(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	structural.BaseCommit = strings.Repeat("a", 40)
	structural.SizeClass = "s"
	var milestones []Milestone
	for i := range 8 {
		milestones = append(milestones, Milestone{ID: fmt.Sprintf("m%d", i), Title: "Milestone"})
	}
	structural.Milestones = semanticJSON(t, milestones)
	base := proposal.Criteria[0]
	base.Kind = "maintainer"
	base.DoneWhen = strings.Repeat("d", 2048)
	base.Maintainer = &apitypes.SemanticMaintainer{
		Reason: strings.Repeat("r", 2048),
	}
	for range 8 {
		base.Maintainer.Conditions = append(base.Maintainer.Conditions, strings.Repeat("c", 512))
		base.Maintainer.VerificationSteps = append(base.Maintainer.VerificationSteps, strings.Repeat("v", 512))
	}
	base.EvidenceRequirements = nil
	base.Citations = nil
	sources[0].ID = strings.Repeat("i", 512)
	for i := range 8 {
		base.EvidenceRequirements = append(base.EvidenceRequirements, apitypes.SemanticEvidence{
			Description: fmt.Sprintf("%d", i) + strings.Repeat("e", 511), Path: strings.Repeat("f", 512),
		})
		base.Citations = append(base.Citations, apitypes.SemanticCitation{
			SourceID: sources[0].ID, Hash: semanticHash(sources[0].Bytes), Start: 0, End: 1,
		})
	}
	proposal.Criteria = nil
	for i := range 256 {
		c := base
		c.MilestoneID = fmt.Sprintf("m%d", i/32)
		c.Text = fmt.Sprintf("%03d", i) + strings.Repeat("t", 2045)
		proposal.Criteria = append(proposal.Criteria, c)
	}
	rawProposal := semanticJSON(t, proposal)
	if len(rawProposal) <= 6<<20 || len(rawProposal) >= 16<<20 {
		t.Fatalf("high-density proposal size=%d", len(rawProposal))
	}
	candidate, _, err := compileSemanticProposal(structural, rawProposal, sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision = 1
	for i := range candidate.Criteria {
		candidate.Criteria[i].ID = uuid.NewString()
	}
	digest, err := semanticCheckerDigest(structural, candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Digest = hex.EncodeToString(digest)
	structural.CandidateIdentityVersion = semanticIdentityVersion
	structural.SemanticCandidate = &candidate
	payload := &ClaimPayload{Kind: runkind.CrossCheck, RunID: uuid.NewString(), CrossCheck: &ClaimPlanCrossCheck{
		Stage: "plan", Round: 1, LeadRunID: uuid.NewString(), CandidateDigest: candidate.Digest,
		DeadlineAt: time.Now().Add(time.Minute), PlanCrossCheckCandidate: structural,
	}}
	wire, err := MarshalCrossCheckClaim(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ClaimPayload
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(wire) <= 6<<20 || len(wire) > 64<<20 || len(decoded.CrossCheck.SemanticCandidate.Criteria) != 256 ||
		decoded.CrossCheck.SemanticCandidate.Criteria[255].DoneWhen != base.DoneWhen {
		t.Fatalf("candidate lost at claim boundary: bytes=%d", len(wire))
	}
	padding := strings.Repeat("x", (64<<20)-len(wire)+16)
	payload.Branch = &padding
	if _, err := MarshalCrossCheckClaim(payload); err == nil {
		t.Fatal("semantic child claim above 64 MiB accepted")
	}
	payload.Branch = nil
	t.Logf("proposal=%d canonical=%d claim=%d", len(rawProposal), len(semanticJSON(t, candidate)), len(wire))

	// Measure the remaining representation boundary without changing identities
	// or JSON encoding policy: compact lead JSON can expand under Go HTML escaping.
	for i := range proposal.Criteria {
		c := &proposal.Criteria[i]
		c.Text = fmt.Sprintf("%03d", i) + strings.Repeat("<", 2045)
		c.DoneWhen = strings.Repeat("<", 2048)
		c.Maintainer.Reason = strings.Repeat("<", 2048)
		for j := range c.Maintainer.Conditions {
			c.Maintainer.Conditions[j] = strings.Repeat("<", 512)
			c.Maintainer.VerificationSteps[j] = strings.Repeat("<", 512)
		}
		for j := range c.EvidenceRequirements {
			c.EvidenceRequirements[j].Description = strings.Repeat("<", 512)
			c.EvidenceRequirements[j].Path = strings.Repeat("<", 512)
		}
	}
	var compact bytes.Buffer
	encoder := json.NewEncoder(&compact)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(proposal); err != nil {
		t.Fatal(err)
	}
	expanded, _, err := compileSemanticProposal(structural, compact.Bytes(), sources)
	if err != nil {
		t.Fatal(err)
	}
	expandedBytes := len(semanticJSON(t, expanded))
	if compact.Len() >= 16<<20 || expandedBytes <= 20<<20 {
		t.Fatalf("unexpected HTML expansion: proposal=%d canonical=%d", compact.Len(), expandedBytes)
	}
	expanded.Revision = 2
	for i := range expanded.Criteria {
		expanded.Criteria[i].ID = uuid.NewString()
	}
	digest, err = semanticCheckerDigest(structural, expanded)
	if err != nil {
		t.Fatal(err)
	}
	expanded.Digest = hex.EncodeToString(digest)
	payload.CrossCheck.CandidateDigest = expanded.Digest
	payload.CrossCheck.SemanticCandidate = &expanded
	escapedWire, err := MarshalCrossCheckClaim(payload)
	if err != nil || len(escapedWire) <= 20<<20 || len(escapedWire) > 64<<20 {
		t.Fatalf("HTML-expanded claim: bytes=%d err=%v", len(escapedWire), err)
	}
	t.Logf("HTML escaping: valid proposal=%d canonical=%d complete claim=%d", compact.Len(), expandedBytes, len(escapedWire))
}

func TestSemanticCheckerIdentity(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	// Raw snapshots remain lossless while checker identity uses its established
	// canonical structural boundary (including display-control scrubbing).
	structural.PlanMd += "\u202e"
	structural.PlanningDiff += "\x1b"
	structural.RequiredTools = []string{" git "}
	structural.BaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	structural.SizeClass = "s"
	candidate, fp, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Revision = 1
	candidate.Criteria[0].ID = uuid.NewString()
	identity, err := json.Marshal(struct {
		Fingerprint string
		Candidate   apitypes.SemanticCandidate
	}{fp, candidate})
	if err != nil {
		t.Fatal(err)
	}
	candidate.Digest = semanticHash(append([]byte("uzi:semantic_v1:candidate\x00"), identity...))
	structuralDigest, err := structural.Digest()
	if err != nil {
		t.Fatal(err)
	}
	c := structural
	c.CandidateIdentityVersion, c.SemanticCandidate = semanticIdentityVersion, &candidate
	unchanged, err := c.Digest()
	if err != nil || hex.EncodeToString(unchanged) != hex.EncodeToString(structuralDigest) {
		t.Fatal("seven-field structural digest changed")
	}
	c, err = NormalizePlanCrossCheckCandidate(c)
	if err != nil || c.PlanMd == structural.PlanMd || candidate.PlanHash != semanticHash([]byte(structural.PlanMd)) {
		t.Fatal("raw plan snapshot or canonical checker boundary changed")
	}
	payload := &ClaimPayload{Kind: runkind.CrossCheck, RunID: uuid.NewString(), CrossCheck: &ClaimPlanCrossCheck{
		Stage: "plan", Round: 1, LeadRunID: uuid.NewString(), CandidateDigest: candidate.Digest, DeadlineAt: time.Now().Add(time.Minute), PlanCrossCheckCandidate: c,
	}}
	if err := ValidateCrossCheckClaim(payload); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*PlanCrossCheckCandidate){
		func(c *PlanCrossCheckCandidate) { c.PlanMd += "changed" },
		func(c *PlanCrossCheckCandidate) { c.PlanningDiff += "changed" },
		func(c *PlanCrossCheckCandidate) { c.BaseCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" },
		func(c *PlanCrossCheckCandidate) { c.Milestones = json.RawMessage(`[{"id":"m2","title":"Changed"}]`) },
		func(c *PlanCrossCheckCandidate) { c.RequiredCapabilities = []string{"network"} },
		func(c *PlanCrossCheckCandidate) { c.RequiredTools = []string{"tool"} },
		func(c *PlanCrossCheckCandidate) { c.SizeClass = "l" },
		func(c *PlanCrossCheckCandidate) { c.SemanticCandidate.Criteria[0].DoneWhen += "changed" },
		func(c *PlanCrossCheckCandidate) { c.SemanticCandidate.Sources[0].Provenance += "changed" },
		func(c *PlanCrossCheckCandidate) { c.CandidateIdentityVersion = "" },
		func(c *PlanCrossCheckCandidate) { c.SemanticCandidate = nil },
	} {
		var altered PlanCrossCheckCandidate
		if err := json.Unmarshal(semanticJSON(t, c), &altered); err != nil {
			t.Fatal(err)
		}
		mutate(&altered)
		payload.CrossCheck.PlanCrossCheckCandidate = altered
		if err := ValidateCrossCheckClaim(payload); err == nil {
			t.Fatal("altered semantic checker accepted")
		}
	}
	payload.CrossCheck.PlanCrossCheckCandidate = structural
	payload.CrossCheck.CandidateDigest = candidate.Digest
	if err := ValidateCrossCheckClaim(payload); err == nil {
		t.Fatal("semantic digest fell back to structural")
	}
}

func TestSemanticProposalRawPlanIdentity(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	rawProposal := semanticJSON(t, proposal)
	first, firstFP, err := compileSemanticProposal(structural, rawProposal, sources)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := NormalizePlanCrossCheckCandidate(structural)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := canonical.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range []string{"\u202e", "\x00"} {
		t.Run(fmt.Sprintf("%x", []byte(control)), func(t *testing.T) {
			changed := structural
			changed.PlanMd += control
			normalized, err := NormalizePlanCrossCheckCandidate(changed)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := normalized.Digest()
			if err != nil || !bytes.Equal(digest, firstDigest) {
				t.Fatal("fixture must preserve canonical seven-field structural digest")
			}
			candidate, fp, err := compileSemanticProposal(changed, rawProposal, sources)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.PlanHash == first.PlanHash || fp == firstFP {
				t.Fatal("different raw plans reused proposal fingerprint")
			}
			retry, retryFP, err := compileSemanticProposal(changed, rawProposal, sources)
			if err != nil || retryFP != fp || !bytes.Equal(semanticJSON(t, retry), semanticJSON(t, candidate)) {
				t.Fatal("identical raw retry changed compiler identity")
			}
		})
	}
}

func TestSemanticFrozenIssueCapture(t *testing.T) {
	sources := []apitypes.SemanticSourceCapture{{ID: "issue", Kind: "issue", Provenance: "worker:forged", Completeness: "unknown", Bytes: []byte("frozen")}}
	got, err := frozenIssueCaptures("frozen", sources)
	if err != nil || got[0].Provenance != "server:run-issue" || got[0].Completeness != "complete" {
		t.Fatalf("binding: %v", err)
	}
	if sources[0].Provenance != "worker:forged" {
		t.Fatal("mutated caller capture")
	}
	if _, err := frozenIssueCaptures("changed", sources); err == nil {
		t.Fatal("accepted changed issue")
	}
	if _, err := frozenIssueCaptures("frozen", append(sources, sources[0])); err == nil {
		t.Fatal("duplicate issue")
	}
}
