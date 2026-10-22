package workersvc

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestSemanticCompilerJSONBTextNUL(t *testing.T) {
	for _, field := range []string{"text", "done_when", "reason", "conditions", "steps", "evidence"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"before\x00after", "before\x01\t\n\r\u202eafter"} {
				structural, proposal, sources := semanticFixture()
				c := &proposal.Criteria[0]
				c.Kind = "maintainer"
				c.Maintainer = &apitypes.SemanticMaintainer{Reason: "owner access", Conditions: []string{"owner available"}, VerificationSteps: []string{"check behavior"}}
				var got func(apitypes.SemanticCandidate) string
				switch field {
				case "text":
					c.Text = value
					got = func(out apitypes.SemanticCandidate) string { return out.Criteria[0].Text }
				case "done_when":
					c.DoneWhen = value
					got = func(out apitypes.SemanticCandidate) string { return out.Criteria[0].DoneWhen }
				case "reason":
					c.Maintainer.Reason = value
					got = func(out apitypes.SemanticCandidate) string { return out.Criteria[0].Maintainer.Reason }
				case "conditions":
					c.Maintainer.Conditions[0] = value
					got = func(out apitypes.SemanticCandidate) string { return out.Criteria[0].Maintainer.Conditions[0] }
				case "steps":
					c.Maintainer.VerificationSteps[0] = value
					got = func(out apitypes.SemanticCandidate) string { return out.Criteria[0].Maintainer.VerificationSteps[0] }
				case "evidence":
					c.EvidenceRequirements[0].Description = value
					got = func(out apitypes.SemanticCandidate) string {
						return out.Criteria[0].EvidenceRequirements[0].Description
					}
				}
				out, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
				if value == "before\x00after" {
					if err == nil {
						t.Error("compiler admitted JSONB-bound NUL prose")
					}
				} else if err != nil {
					t.Fatalf("non-NUL prose controls rejected: %v", err)
				} else if got(out) != value {
					t.Fatal("prose controls changed")
				}
			}
		})
	}
}

func TestSemanticCoverageJSONBExplanationNUL(t *testing.T) {
	structural, proposal, sources := semanticFixture()
	candidate, _, err := compileSemanticProposal(structural, semanticJSON(t, proposal), sources)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assessment", "requirement"} {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"before\x00after", "before\x01\t\n\r\u202eafter"} {
				report := apitypes.SemanticCoverageReport{
					SemanticCoverageBinding: SemanticCoverageBinding(candidate),
					Criteria:                []apitypes.SemanticCoverageAtomicity{{CriterionID: candidate.Criteria[0].ID, Atomic: true, Explanation: "atomic behavior"}},
					Requirements:            []apitypes.SemanticCoverageResult{{RequirementID: "r1", Status: "represented", Explanation: "represented behavior", Citations: candidate.Criteria[0].Citations, CriterionIDs: []string{candidate.Criteria[0].ID}}},
				}
				if field == "assessment" {
					report.Criteria[0].Explanation = value
				} else {
					report.Requirements[0].Explanation = value
				}
				out, err := DecodeSemanticCoverageReport(semanticJSON(t, report), candidate, sources)
				if value == "before\x00after" {
					if err == nil {
						t.Error("report admitted JSONB-bound NUL explanation")
					}
				} else if err != nil {
					t.Fatalf("non-NUL explanation controls rejected: %v", err)
				} else if !reflect.DeepEqual(out, report) {
					t.Fatal("report prose changed")
				}
			}
		})
	}
}
