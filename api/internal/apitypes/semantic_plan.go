package apitypes

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// SemanticSourceCapture is trusted worker capture input, never lead authority.
// Bytes are UTF-8 bytes (including NUL); JSON encodes them as base64.
type SemanticSourceCapture struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Provenance   string `json:"provenance"`
	Completeness string `json:"completeness"`
	Bytes        []byte `json:"bytes"`
}

type SemanticSourceRef struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Hash         string `json:"hash"`
	Provenance   string `json:"provenance"`
	Completeness string `json:"completeness"`
}

type SemanticCitation struct {
	SourceID string `json:"source_id"`
	Hash     string `json:"hash"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

// UnmarshalJSON requires both offsets without changing their public wire shape.
func (c *SemanticCitation) UnmarshalJSON(raw []byte) error {
	var wire struct {
		SourceID string `json:"source_id"`
		Hash     string `json:"hash"`
		Start    *int   `json:"start"`
		End      *int   `json:"end"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return err
	}
	if wire.Start == nil || wire.End == nil {
		return fmt.Errorf("citation start and end are required")
	}
	*c = SemanticCitation{SourceID: wire.SourceID, Hash: wire.Hash, Start: *wire.Start, End: *wire.End}
	return nil
}

type SemanticEvidence struct {
	Description string `json:"description"`
	Path        string `json:"path,omitempty"`
}

// UnmarshalJSON preserves optional paths while rejecting an explicit JSON null.
func (e *SemanticEvidence) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Description string          `json:"description"`
		Path        json.RawMessage `json:"path"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return err
	}
	var path string
	if len(wire.Path) != 0 {
		if bytes.Equal(bytes.TrimSpace(wire.Path), []byte("null")) {
			return fmt.Errorf("evidence path must be a string")
		}
		if err := json.Unmarshal(wire.Path, &path); err != nil {
			return err
		}
	}
	*e = SemanticEvidence{Description: wire.Description, Path: path}
	return nil
}

type SemanticMaintainer struct {
	Reason            string   `json:"reason"`
	Conditions        []string `json:"conditions"`
	VerificationSteps []string `json:"verification_steps"`
}

// ProposedSemanticCriterion deliberately has no ID. Unknown fields are errors.
type ProposedSemanticCriterion struct {
	MilestoneID          string              `json:"milestone_id"`
	Kind                 string              `json:"kind"`
	Text                 string              `json:"text"`
	DoneWhen             string              `json:"done_when"`
	Citations            []SemanticCitation  `json:"citations"`
	EvidenceRequirements []SemanticEvidence  `json:"evidence_requirements"`
	Maintainer           *SemanticMaintainer `json:"maintainer,omitempty"`
}

type SemanticCriterion struct {
	ID string `json:"id"`
	ProposedSemanticCriterion
}

type SemanticProposal struct {
	Criteria []ProposedSemanticCriterion `json:"criteria"`
}

// SemanticEnrollment is reserved for creation-time freezing in #1231.
// M1 provides no enrollment writer, setting, or production caller.
type SemanticEnrollment struct {
	Profile string         `json:"profile"`
	Policy  SemanticPolicy `json:"policy"`
}
type SemanticPolicy struct {
	Version        int    `json:"version"`
	Model          string `json:"model"`
	MaxReviews     int    `json:"max_reviews"`
	MaxInvocations int    `json:"max_invocations"`
	MaxTokens      int    `json:"max_tokens"`
	MaxSeconds     int    `json:"max_seconds"`
}
type SemanticCoverageState struct {
	Outcome         string `json:"outcome"`
	CandidateDigest string `json:"candidate_digest"`
	Reviews         int    `json:"reviews"`
	Invocations     int    `json:"invocations"`
	Tokens          int    `json:"tokens"`
	Seconds         int    `json:"seconds"`
}
type SemanticCandidate struct {
	CandidateIdentityVersion string                 `json:"candidate_identity_version"`
	Revision                 int64                  `json:"revision"`
	Digest                   string                 `json:"digest"`
	Sources                  []SemanticSourceRef    `json:"sources"`
	PlanFieldsDigest         string                 `json:"plan_fields_digest,omitempty"`
	PlanHash                 string                 `json:"plan_hash"`
	SourceOutcome            string                 `json:"source_outcome"`
	Criteria                 []SemanticCriterion    `json:"criteria"`
	Coverage                 *SemanticCoverageState `json:"coverage,omitempty"`
}
