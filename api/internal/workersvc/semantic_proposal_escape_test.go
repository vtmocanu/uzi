package workersvc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestSemanticProposalMandatoryEscapes(t *testing.T) {
	encode := func(v any) []byte {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		if err := e.Encode(v); err != nil {
			t.Fatal(err)
		}
		return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	fill := func(prefix string, n int) string { return prefix + strings.Repeat("\x01", n-len(prefix)) }
	for _, variant := range []string{"compiler", "envelope"} {
		t.Run(variant, func(t *testing.T) {
			structural, proposal, sources := semanticFixture()
			structural.BaseCommit = strings.Repeat("a", 40)
			structural.SizeClass = "s"
			sources[0].Bytes = []byte("requirement")
			sources[0].ID = strings.Repeat("s", 512)
			path := strings.Repeat("p", 512)
			if variant == "envelope" {
				// Quotes are permitted identifier characters; escaping them is mandatory.
				sources[0].ID = strings.Repeat("\"", 512)
				path = strings.Repeat("\"", 512)
				// Only ordinary whitespace, preserved by the structural scrubber.
				structural.PlanMd = "plan" + strings.Repeat("\t", (256<<10)-4)
				structural.PlanningDiff = "diff" + strings.Repeat("\n", (512<<10)-4)
			}
			var ms []Milestone
			for i := 0; i < 8; i++ {
				ms = append(ms, Milestone{ID: fmt.Sprintf("m%d", i), Title: "Milestone"})
			}
			structural.Milestones = encode(ms)
			proposal.Criteria = nil
			for i := 0; i < 256; i++ {
				c := apitypes.ProposedSemanticCriterion{
					MilestoneID: fmt.Sprintf("m%d", i/32), Kind: "maintainer",
					Text: fill(fmt.Sprintf("%03d", i), 2048), DoneWhen: fill("", 2048),
					Maintainer: &apitypes.SemanticMaintainer{Reason: fill("", 2048)},
				}
				for j := 0; j < 8; j++ {
					c.Maintainer.Conditions = append(c.Maintainer.Conditions, fill(fmt.Sprintf("%d", j), 512))
					c.Maintainer.VerificationSteps = append(c.Maintainer.VerificationSteps, fill(fmt.Sprintf("%d", j), 512))
					c.EvidenceRequirements = append(c.EvidenceRequirements, apitypes.SemanticEvidence{Description: fill(fmt.Sprintf("%d", j), 512), Path: path})
					c.Citations = append(c.Citations, apitypes.SemanticCitation{SourceID: sources[0].ID, Hash: semanticHash(sources[0].Bytes), Start: 0, End: j + 1})
				}
				proposal.Criteria = append(proposal.Criteria, c)
			}
			raw := encode(proposal)
			envelope := encode(struct {
				ClaimGeneration int64                            `json:"claim_generation"`
				Structural      PlanCrossCheckCandidate          `json:"structural"`
				Proposal        json.RawMessage                  `json:"proposal"`
				Sources         []apitypes.SemanticSourceCapture `json:"sources"`
			}{1, structural, raw, sources})
			controls := bytes.Count(raw, []byte(`\u0001`))
			t.Logf("raw=%d envelope=%d freeform_decoded=%d mandatory_u0001=%d milestones=8 criteria=256 per_milestone=32 text=2048 done_when=2048 reason=2048 conditions=8x512 steps=8x512 evidence=8x512 paths=8x512 citations=8 source_bytes=%d plan_bytes=%d diff_bytes=%d", len(raw), len(envelope), 256*(2048*3+512*24), controls, len(sources[0].Bytes), len(structural.PlanMd), len(structural.PlanningDiff))
			if len(raw) <= 16<<20 || controls == 0 || !json.Valid(raw) {
				t.Fatal("probe did not cross raw limit with valid control escaping")
			}
			candidate, fp, err := compileSemanticProposal(structural, raw, sources)
			if err != nil {
				t.Fatalf("valid field-bounded proposal rejected: %v", err)
			}
			if len(candidate.Criteria) != 256 || len(fp) != 64 || candidate.SourceOutcome != "complete" {
				t.Fatal("compiler lost candidate")
			}
			expected := make(map[string]apitypes.ProposedSemanticCriterion, len(proposal.Criteria))
			for _, criterion := range proposal.Criteria {
				expected[criterion.Text] = criterion
			}
			for _, criterion := range candidate.Criteria {
				if !reflect.DeepEqual(criterion.ProposedSemanticCriterion, expected[criterion.Text]) {
					t.Fatal("compiler changed criterion bytes, identifiers or citations")
				}
			}
			if candidate.Sources[0].Hash != semanticHash([]byte("requirement")) || !bytes.Equal(sources[0].Bytes, []byte("requirement")) {
				t.Fatal("compiler changed source bytes or hash")
			}
			t.Logf("ALL_REMAINING_COMPILER_VALIDATION_PASS canonical_bytes=%d fingerprint_bytes=%d", len(encode(candidate)), len(fp))
		})
	}
}
