package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func semanticFixture() (PlanCrossCheckCandidate, apitypes.SemanticProposal, []apitypes.SemanticSourceCapture) {
	source := []byte("a\x00😀 requirement")
	return PlanCrossCheckCandidate{PlanMd: "implement behavior", Milestones: json.RawMessage(`[{"id":"m1","title":"Behavior"}]`)},
		apitypes.SemanticProposal{Criteria: []apitypes.ProposedSemanticCriterion{{
			MilestoneID: "m1", Kind: "code", Text: "behavior", DoneWhen: "observable behavior works",
			Citations:            []apitypes.SemanticCitation{{SourceID: "issue", Hash: semanticHash(source), Start: 0, End: 6}},
			EvidenceRequirements: []apitypes.SemanticEvidence{{Description: "test output", Path: "api/test.go"}},
		}}}, []apitypes.SemanticSourceCapture{{ID: "issue", Kind: "issue", Provenance: "forge:issue:1", Completeness: "complete", Bytes: source}}
}
func semanticJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSemanticCompilerValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PlanCrossCheckCandidate, *apitypes.SemanticProposal, *[]apitypes.SemanticSourceCapture)
	}{
		{"unknown kind", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Kind = "other"
		}},
		{"foreign milestone", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].MilestoneID = "foreign"
		}},
		{"foreign source", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].SourceID = "foreign"
		}},
		{"hash", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].Hash = "wrong"
		}},
		{"offset boundary", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].End = 3
		}},
		{"negative offset", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].Start = -1
		}},
		{"past end", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].End = 100
		}},
		{"empty span", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Citations[0].End = 0
		}},
		{"bad UTF8", func(_ *PlanCrossCheckCandidate, _ *apitypes.SemanticProposal, s *[]apitypes.SemanticSourceCapture) {
			(*s)[0].Bytes = []byte{0xff}
		}},
		{"duplicate source", func(_ *PlanCrossCheckCandidate, _ *apitypes.SemanticProposal, s *[]apitypes.SemanticSourceCapture) {
			*s = append(*s, (*s)[0])
		}},
		{"unknown completeness", func(_ *PlanCrossCheckCandidate, _ *apitypes.SemanticProposal, s *[]apitypes.SemanticSourceCapture) {
			(*s)[0].Completeness = "complete-prefix"
		}},
		{"duplicate criterion", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria = append(p.Criteria, p.Criteria[0])
		}},
		{"duplicate milestone", func(c *PlanCrossCheckCandidate, _ *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			c.Milestones = json.RawMessage(`[{"id":"m1","title":"A"},{"id":"m1","title":"B"}]`)
		}},
		{"newline path", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].EvidenceRequirements[0].Path = "a\nb"
		}},
		{"bidi path", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].EvidenceRequirements[0].Path = "a\u202eb"
		}},
		{"NUL path", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].EvidenceRequirements[0].Path = "a\x00b"
		}},
		{"maintainer missing", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Kind = "maintainer"
		}},
		{"worker exception", func(_ *PlanCrossCheckCandidate, p *apitypes.SemanticProposal, _ *[]apitypes.SemanticSourceCapture) {
			p.Criteria[0].Maintainer = &apitypes.SemanticMaintainer{Reason: "owner"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, p, s := semanticFixture()
			tc.mutate(&c, &p, &s)
			if _, _, err := compileSemanticProposal(c, semanticJSON(t, p), s); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}
func TestSemanticStrictJSON(t *testing.T) {
	for _, raw := range []string{
		`{"criteria":[],"criteria":[]}`, `{"Criteria":[]}`, `{"criteria":[{"kind":"code","Kind":"doc"}]}`, `{"criteria":[],"invented":true}`,
		`{"criteria":[{"id":"foreign"}]}`, `{"criteria":[{"kind":"code","kind":"doc"}]}`,
		`{"criteria":[]} {}`,
	} {
		var p apitypes.SemanticProposal
		if strictSemanticJSON([]byte(raw), &p) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestSemanticCanonicalContract(t *testing.T) {
	c, p, s := semanticFixture()
	candidate, _, err := compileSemanticProposal(c, semanticJSON(t, p), s)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Criteria[0].ID = uuid.NewString()
	raw, err := buildSemanticCompletionContract(candidate, 1)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if _, exists := result["v"]; exists {
		t.Fatal("introduced a top-level schema version")
	}
	decoded, err := parseCompletionContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Profile != "semantic" || decoded.Criteria[0].DoneWhen != p.Criteria[0].DoneWhen || decoded.Criteria[0].ID != candidate.Criteria[0].ID {
		t.Fatal("compiler dropped semantic fields")
	}
	candidate.Criteria = append(candidate.Criteria, candidate.Criteria[0])
	if _, err = buildSemanticCompletionContract(candidate, 1); err == nil {
		t.Fatal("duplicate canonical ID accepted")
	}
	candidate.Criteria = candidate.Criteria[:1]
	candidate.Criteria[0].ID = "foreign"
	if _, err = buildSemanticCompletionContract(candidate, 1); err == nil {
		t.Fatal("invalid canonical ID accepted")
	}
}

func TestSemanticByteBounds(t *testing.T) {
	for _, field := range []string{"text", "done_when", "description", "path", "reason", "condition", "step", "issue", "prd", "plan"} {
		limit := 2048
		switch field {
		case "description", "path", "condition", "step":
			limit = 512
		case "issue":
			limit = 64 * 1024
		case "prd", "plan":
			limit = 256 * 1024
		}
		for _, delta := range []int{-1, 0, 1} {
			t.Run(field+"/"+string(rune('1'+delta)), func(t *testing.T) {
				c, p, s := semanticFixture()
				v := strings.Repeat("x", limit+delta)
				switch field {
				case "text":
					p.Criteria[0].Text = v
				case "done_when":
					p.Criteria[0].DoneWhen = v
				case "description":
					p.Criteria[0].EvidenceRequirements[0].Description = v
				case "path":
					p.Criteria[0].EvidenceRequirements[0].Path = v
				case "reason", "condition", "step":
					p.Criteria[0].Kind = "maintainer"
					m := &apitypes.SemanticMaintainer{Reason: "owner", Conditions: []string{"condition"}, VerificationSteps: []string{"step"}}
					p.Criteria[0].Maintainer = m
					switch field {
					case "reason":
						m.Reason = v
					case "condition":
						m.Conditions[0] = v
					case "step":
						m.VerificationSteps[0] = v
					}
				case "issue":
					s[0].Bytes = []byte(v)
					p.Criteria[0].Citations[0].Hash = semanticHash(s[0].Bytes)
				case "prd":
					s = append(s, apitypes.SemanticSourceCapture{ID: "prd", Kind: "prd", Provenance: "repo:prd", Completeness: "complete", Bytes: []byte(v)})
				case "plan":
					c.PlanMd = v
				}
				_, _, err := compileSemanticProposal(c, semanticJSON(t, p), s)
				if (err == nil) != (delta <= 0) {
					t.Fatalf("delta=%d err=%v", delta, err)
				}
			})
		}
	}
	// Byte bound must reject a multibyte string below the rune bound.
	c, p, s := semanticFixture()
	p.Criteria[0].Text = strings.Repeat("😀", 513)
	if _, _, err := compileSemanticProposal(c, semanticJSON(t, p), s); err == nil {
		t.Fatal("counted runes instead of bytes")
	}
}
func TestSemanticCanonicalIdentity(t *testing.T) {
	c, p, s := semanticFixture()
	first, fp, err := compileSemanticProposal(c, semanticJSON(t, p), s)
	if err != nil {
		t.Fatal(err)
	}
	if first.Criteria[0].ID != "" {
		t.Fatal("minted before transactional admission")
	}
	old, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	c.Milestones = json.RawMessage(`[ { "title":"Behavior", "id":"m1" } ]`)
	c.RequiredTools = []string{"go", "git", "go"}
	_, fp2, err := compileSemanticProposal(c, semanticJSON(t, p), s)
	if err != nil {
		t.Fatal(err)
	}
	c.RequiredTools = []string{"git", "go"}
	_, fp3, err := compileSemanticProposal(c, semanticJSON(t, p), s)
	if err != nil {
		t.Fatal(err)
	}
	if fp2 != fp3 || fp == fp2 {
		t.Fatal("structural fields not canonicalized")
	}
	c.RequiredTools = nil
	again, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, again) {
		t.Fatal("structural identity changed")
	}
	s[0].Completeness = "unknown"
	unknownCandidate, unknown, err := compileSemanticProposal(c, semanticJSON(t, p), s)
	if err != nil {
		t.Fatal(err)
	}
	if unknownCandidate.SourceOutcome != "unknown" || first.SourceOutcome != "complete" {
		t.Fatal("source completeness was not explicit")
	}
	if unknown == fp {
		t.Fatal("completeness not bound")
	}
}
func TestSemanticCountBounds(t *testing.T) {
	for _, field := range []string{"milestones", "criteria", "per_milestone", "evidence", "citations", "conditions", "steps", "prds"} {
		limit := 8
		switch field {
		case "milestones":
			limit = 50
		case "criteria":
			limit = 256
		case "per_milestone":
			limit = 32
		}
		for _, count := range []int{0, limit - 1, limit, limit + 1} {
			t.Run(fmt.Sprintf("%s/%d", field, count), func(t *testing.T) {
				c, p, s := semanticFixture()
				switch field {
				case "milestones":
					ms := make([]Milestone, count)
					for i := range ms {
						ms[i] = Milestone{ID: fmt.Sprintf("m%d", i+1), Title: "Behavior"}
					}
					c.Milestones = semanticJSON(t, ms)
				case "criteria", "per_milestone":
					base := p.Criteria[0]
					p.Criteria = make([]apitypes.ProposedSemanticCriterion, count)
					ms := []Milestone{{ID: "m1", Title: "Behavior"}}
					if field == "criteria" {
						for i := 2; i <= 9; i++ {
							ms = append(ms, Milestone{ID: fmt.Sprintf("m%d", i), Title: "Behavior"})
						}
					}
					c.Milestones = semanticJSON(t, ms)
					for i := range p.Criteria {
						p.Criteria[i] = base
						p.Criteria[i].Text = fmt.Sprintf("Behavior %d", i)
						if field == "criteria" {
							p.Criteria[i].MilestoneID = fmt.Sprintf("m%d", i/32+1)
						}
					}
				case "evidence":
					base := p.Criteria[0].EvidenceRequirements[0]
					p.Criteria[0].EvidenceRequirements = make([]apitypes.SemanticEvidence, count)
					for i := range p.Criteria[0].EvidenceRequirements {
						p.Criteria[0].EvidenceRequirements[i] = base
					}
				case "citations":
					base := p.Criteria[0].Citations[0]
					p.Criteria[0].Citations = make([]apitypes.SemanticCitation, count)
					for i := range p.Criteria[0].Citations {
						p.Criteria[0].Citations[i] = base
					}
				case "conditions", "steps":
					p.Criteria[0].Kind = "maintainer"
					m := &apitypes.SemanticMaintainer{Reason: "owner", Conditions: []string{"condition"}, VerificationSteps: []string{"step"}}
					p.Criteria[0].Maintainer = m
					items := make([]string, count)
					for i := range items {
						items[i] = "verification"
					}
					if field == "conditions" {
						m.Conditions = items
					} else {
						m.VerificationSteps = items
					}
				case "prds":
					for i := 0; i < count; i++ {
						s = append(s, apitypes.SemanticSourceCapture{ID: fmt.Sprintf("prd%d", i), Kind: "prd", Provenance: "repo:prd", Completeness: "complete", Bytes: []byte("requirement")})
					}
				}
				_, _, err := compileSemanticProposal(c, semanticJSON(t, p), s)
				valid := count <= limit && (count > 0 || field == "prds")
				if (err == nil) != valid {
					t.Fatalf("count=%d err=%v", count, err)
				}
			})
		}
	}
}

func TestSemanticAggregateBytes(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		c, p, s := semanticFixture()
		c.PlanMd = strings.Repeat("p", 256*1024)
		s[0].Bytes = bytes.Repeat([]byte("i"), 64*1024)
		p.Criteria[0].Citations[0].Hash = semanticHash(s[0].Bytes)
		s = append(s, apitypes.SemanticSourceCapture{ID: "prd", Kind: "prd", Provenance: "repo:prd", Completeness: "complete", Bytes: bytes.Repeat([]byte("r"), 256*1024+delta)})
		// For +1 use another PRD so neither individual snapshot exceeds its cap.
		if delta == 1 {
			s[1].Bytes = s[1].Bytes[:256*1024]
			s = append(s, apitypes.SemanticSourceCapture{ID: "prd2", Kind: "prd", Provenance: "repo:prd2", Completeness: "complete", Bytes: []byte("x")})
		}
		_, _, err := compileSemanticProposal(c, semanticJSON(t, p), s)
		if (err == nil) != (delta <= 0) {
			t.Fatalf("aggregate delta %d: %v", delta, err)
		}
	}
}

func TestSemanticRequiresTransaction(t *testing.T) {
	c, p, s := semanticFixture()
	if _, err := (&Service{}).submitSemanticCandidate(context.Background(), uuid.New(), uuid.New(), 0, c, semanticJSON(t, p), s); err == nil {
		t.Fatal("nil beginner accepted")
	}
}
