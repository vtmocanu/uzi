package workersvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const semanticIdentityVersion = "semantic_v1"

// buildSemanticCompletionContract constructs the same completion contract that
// structural runs use. M3 owns freezing it in the existing approval transaction;
// this helper performs no write and authorizes no maintainer exception.
func buildSemanticCompletionContract(candidate apitypes.SemanticCandidate) ([]byte, error) {
	if candidate.CandidateIdentityVersion != semanticIdentityVersion || len(candidate.Criteria) == 0 || len(candidate.Criteria) > 256 {
		return nil, fmt.Errorf("invalid canonical semantic candidate")
	}
	contract := completionContract{Profile: "semantic", Revision: contractRevisionInitial, Criteria: make([]completionCriterion, len(candidate.Criteria)), Sources: candidate.Sources, PlanHash: candidate.PlanHash, CandidateIdentityVersion: candidate.CandidateIdentityVersion, SourceOutcome: candidate.SourceOutcome, Coverage: candidate.Coverage}
	seen := map[string]bool{}
	for i, c := range candidate.Criteria {
		if uuid.Validate(c.ID) != nil || seen[c.ID] {
			return nil, fmt.Errorf("invalid or duplicate canonical criterion ID")
		}
		seen[c.ID] = true
		contract.Criteria[i] = completionCriterion{ID: c.ID, MilestoneID: c.MilestoneID, Text: c.Text, Audit: nil, FindingIDs: []string{}, Kind: c.Kind, DoneWhen: c.DoneWhen, Citations: c.Citations, EvidenceRequirements: c.EvidenceRequirements, Maintainer: c.Maintainer}
	}
	return json.Marshal(contract)
}

// strictSemanticJSON rejects duplicate keys at every depth as well as unknown
// fields. encoding/json alone accepts the last duplicate silently.
// DecodeSemanticRequest applies the compiler JSON boundary to worker transport.
func DecodeSemanticRequest(raw []byte, dst any) error { return strictSemanticJSON(raw, dst) }

func strictSemanticJSON(raw []byte, dst any) error {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
		return fmt.Errorf("object or array required")
	}
	if !utf8.Valid(raw) {
		return fmt.Errorf("invalid UTF-8 JSON")
	}

	// Scan escape spellings before encoding/json can replace lone surrogates.
	// Each byte is visited at most once; syntax validation remains with the decoder.
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return fmt.Errorf("short Unicode escape")
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return err
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return fmt.Errorf("unpaired low surrogate")
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return fmt.Errorf("unpaired high surrogate")
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("unpaired high surrogate")
		}
		i += 6
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				// All semantic DTO field names are lowercase. Reject case aliases
				// before encoding/json can match them case-insensitively.
				if !ok || seen[key] || key != strings.ToLower(key) {
					return fmt.Errorf("duplicate or invalid key")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

func semanticText(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= max
}
func semanticIdentifier(s string, max int) bool {
	if !semanticText(s, max) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}
func semanticHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// compileSemanticProposal is a pure compiler. Captures must originate in the
// trusted worker capture seam; the lead proposal can only reference them.
func compileSemanticProposal(structural PlanCrossCheckCandidate, raw []byte, captures []apitypes.SemanticSourceCapture) (apitypes.SemanticCandidate, string, error) {
	var out apitypes.SemanticCandidate
	fail := func() (apitypes.SemanticCandidate, string, error) {
		return out, "", fmt.Errorf("invalid semantic proposal or capture")
	}
	if len(raw) > 16*1024*1024 {
		return fail()
	}
	var proposal apitypes.SemanticProposal
	if err := strictSemanticJSON(raw, &proposal); err != nil {
		return out, "", err
	}
	var ms []Milestone
	if err := strictSemanticJSON(structural.Milestones, &ms); err != nil {
		return out, "", err
	}
	normalized, ok := validateMilestones(ms)
	if !ok {
		return fail()
	}
	mids := map[string]bool{}
	for _, m := range normalized {
		if !utf8.ValidString(m.Title) {
			return fail()
		}
		mids[m.ID] = true
	}
	if !utf8.ValidString(structural.PlanMd) || len(structural.PlanMd) > 256*1024 {
		return fail()
	}
	sources := map[string]apitypes.SemanticSourceCapture{}
	refs := make([]apitypes.SemanticSourceRef, 0, len(captures))
	total, issues, prds := len(structural.PlanMd), 0, 0
	sourceOutcome := "complete"
	if len(captures) > 9 {
		return fail()
	}
	for _, s := range captures {
		if !semanticIdentifier(s.ID, 512) || !semanticIdentifier(s.Provenance, 2048) || !utf8.Valid(s.Bytes) {
			return fail()
		}
		if _, exists := sources[s.ID]; exists {
			return fail()
		}
		if s.Completeness != "complete" && s.Completeness != "unknown" {
			return fail()
		}
		if s.Completeness == "unknown" {
			sourceOutcome = "unknown"
		}
		switch s.Kind {
		case "issue":
			issues++
			if len(s.Bytes) > 64*1024 {
				return fail()
			}
		case "prd":
			prds++
			if len(s.Bytes) > 256*1024 {
				return fail()
			}
		default:
			return fail()
		}
		total += len(s.Bytes)
		sources[s.ID] = s
		refs = append(refs, apitypes.SemanticSourceRef{ID: s.ID, Kind: s.Kind, Hash: semanticHash(s.Bytes), Provenance: s.Provenance, Completeness: s.Completeness})
	}
	if issues != 1 || prds > 8 || total > 576*1024 || len(proposal.Criteria) == 0 || len(proposal.Criteria) > 256 {
		return fail()
	}
	perMilestone := map[string]int{}
	for i := range proposal.Criteria {
		c := &proposal.Criteria[i]
		perMilestone[c.MilestoneID]++
		if !mids[c.MilestoneID] || perMilestone[c.MilestoneID] > 32 || !semanticText(c.Text, 2048) || !semanticText(c.DoneWhen, 2048) {
			return fail()
		}
		switch c.Kind {
		case "code", "doc", "test":
			if c.Maintainer != nil {
				return fail()
			}
		case "maintainer":
			m := c.Maintainer
			if m == nil || !semanticText(m.Reason, 2048) || len(m.Conditions) < 1 || len(m.Conditions) > 8 || len(m.VerificationSteps) < 1 || len(m.VerificationSteps) > 8 {
				return fail()
			}
			for _, list := range [][]string{m.Conditions, m.VerificationSteps} {
				for _, s := range list {
					if !semanticText(s, 512) {
						return fail()
					}
				}
			}
		default:
			return fail()
		}
		if len(c.EvidenceRequirements) < 1 || len(c.EvidenceRequirements) > 8 || len(c.Citations) < 1 || len(c.Citations) > 8 {
			return fail()
		}
		for _, e := range c.EvidenceRequirements {
			if !semanticText(e.Description, 512) || (e.Path != "" && !semanticIdentifier(e.Path, 512)) {
				return fail()
			}
		}
		for _, cite := range c.Citations {
			s, exists := sources[cite.SourceID]
			if !exists || s.Completeness != "complete" || cite.Hash != semanticHash(s.Bytes) || cite.Start < 0 || cite.End <= cite.Start || cite.End > len(s.Bytes) {
				return fail()
			}
			if !utf8.Valid(s.Bytes[:cite.Start]) || !utf8.Valid(s.Bytes[:cite.End]) {
				return fail()
			}
		}
		// Citation/evidence arrays describe sets; order must not change identity.
		sort.Slice(c.Citations, func(i, j int) bool {
			a, _ := json.Marshal(c.Citations[i])
			b, _ := json.Marshal(c.Citations[j])
			return bytes.Compare(a, b) < 0
		})
		sort.Slice(c.EvidenceRequirements, func(i, j int) bool {
			a, _ := json.Marshal(c.EvidenceRequirements[i])
			b, _ := json.Marshal(c.EvidenceRequirements[j])
			return bytes.Compare(a, b) < 0
		})
	}
	sort.Slice(proposal.Criteria, func(i, j int) bool {
		a, _ := json.Marshal(proposal.Criteria[i])
		b, _ := json.Marshal(proposal.Criteria[j])
		return bytes.Compare(a, b) < 0
	})
	for i := 1; i < len(proposal.Criteria); i++ {
		a, _ := json.Marshal(proposal.Criteria[i-1])
		b, _ := json.Marshal(proposal.Criteria[i])
		if bytes.Equal(a, b) {
			return fail()
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	// Checker submission and plan writes use this existing canonical boundary.
	// Fingerprint the same structural values, while retaining the exact raw
	// plan/source snapshots below. Copy slices: normalization must not mutate
	// the trusted capture caller's input.
	canonicalStructural := structural
	canonicalStructural.RequiredCapabilities = append([]string(nil), structural.RequiredCapabilities...)
	canonicalStructural.RequiredTools = append([]string(nil), structural.RequiredTools...)
	canonicalStructural, err := NormalizePlanCrossCheckCandidate(canonicalStructural)
	if err != nil {
		return out, "", err
	}
	structuralDigest, err := canonicalStructural.Digest()
	if err != nil {
		return out, "", err
	}
	canonical, _ := json.Marshal(struct {
		Structural string
		PlanHash   string
		Sources    []apitypes.SemanticSourceRef
		Criteria   []apitypes.ProposedSemanticCriterion
	}{hex.EncodeToString(structuralDigest), semanticHash([]byte(structural.PlanMd)), refs, proposal.Criteria})
	fingerprint := semanticHash(append([]byte("uzi:semantic_v1:proposal\x00"), canonical...))
	out = apitypes.SemanticCandidate{CandidateIdentityVersion: semanticIdentityVersion, Sources: refs, PlanHash: semanticHash([]byte(structural.PlanMd)), SourceOutcome: sourceOutcome, Criteria: make([]apitypes.SemanticCriterion, len(proposal.Criteria))}
	for i, c := range proposal.Criteria {
		out.Criteria[i] = apitypes.SemanticCriterion{ProposedSemanticCriterion: c}
	}
	return out, fingerprint, nil
}
