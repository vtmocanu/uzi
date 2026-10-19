package apitypes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// SemanticCandidateResponse carries the persisted inputs for one exact candidate.
// Plan and source bytes use Go's base64 JSON encoding, including empty snapshots.
type SemanticCandidateResponse struct {
	Candidate SemanticCandidate       `json:"candidate"`
	Binding   SemanticCoverageBinding `json:"binding"`
	Plan      []byte                  `json:"plan"`
	Sources   []SemanticSourceCapture `json:"sources"`
}

// SemanticCoverageStatus exposes current evidence and non-resetting run charges.
type SemanticCoverageStatus struct {
	Outcome string `json:"outcome"`
	SemanticCoverageBinding
	Reviews         int                     `json:"reviews"`
	Invocations     int                     `json:"invocations"`
	ChargedTokens   int                     `json:"charged_tokens"`
	RemainingTokens int                     `json:"remaining_tokens"`
	Deadline        time.Time               `json:"deadline"`
	Report          *SemanticCoverageReport `json:"report,omitempty"`
}

// SemanticCoverageBinding names the complete immutable candidate identity.
type SemanticCoverageBinding struct {
	CandidateDigest string `json:"candidate_digest"`
	SourceDigest    string `json:"source_digest"`
	CriteriaDigest  string `json:"criteria_digest"`
}
type SemanticCoverageReserveRequest struct {
	RequestID string `json:"request_id"`
	ReviewID  string `json:"review_id,omitempty"`
	SemanticCoverageBinding
	Supported bool `json:"supported"`
}
type SemanticCoverageReservation struct {
	Outcome      string `json:"outcome"`
	ReviewID     string `json:"review_id,omitempty"`
	InvocationID string `json:"invocation_id,omitempty"`
	SemanticCoverageBinding
	Model         string                  `json:"model,omitempty"`
	Deadline      time.Time               `json:"deadline"`
	MaxTokens     int                     `json:"max_tokens"`
	Reviews       int                     `json:"reviews"`
	Invocations   int                     `json:"invocations"`
	ChargedTokens int                     `json:"charged_tokens"`
	Candidate     *SemanticCandidate      `json:"candidate,omitempty"`
	Plan          []byte                  `json:"plan,omitempty"`
	Sources       []SemanticSourceCapture `json:"sources,omitempty"`
}
type SemanticCoverageSettleRequest struct {
	ReviewID     string `json:"review_id"`
	InvocationID string `json:"invocation_id"`
	// success, transport_error, schema_error, or unknown. Unknown usage stays fully charged.
	Status string `json:"status"`
	Tokens *int   `json:"tokens,omitempty"`
}
type SemanticCoverageResult struct {
	RequirementID string             `json:"requirement_id"`
	Status        string             `json:"status"`
	Explanation   string             `json:"explanation"`
	Citations     []SemanticCitation `json:"citations"`
	CriterionIDs  []string           `json:"criterion_ids"`
}
type SemanticCoverageReport struct {
	SemanticCoverageBinding
	Requirements []SemanticCoverageResult    `json:"requirements"`
	Criteria     []SemanticCoverageAtomicity `json:"criteria"`
}
type SemanticCoveragePublishRequest struct {
	ReviewID     string                 `json:"review_id"`
	InvocationID string                 `json:"invocation_id"`
	Report       SemanticCoverageReport `json:"report"`
}

// SemanticCoverageAtomicity requires an explicit boolean, including false.
type SemanticCoverageAtomicity struct {
	CriterionID string `json:"criterion_id"`
	Atomic      bool   `json:"atomic"`
	Explanation string `json:"explanation"`
}

func (a *SemanticCoverageAtomicity) UnmarshalJSON(raw []byte) error {
	var wire struct {
		CriterionID string `json:"criterion_id"`
		Atomic      *bool  `json:"atomic"`
		Explanation string `json:"explanation"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return err
	}
	if wire.Atomic == nil {
		return fmt.Errorf("criterion atomicity is required")
	}
	*a = SemanticCoverageAtomicity{CriterionID: wire.CriterionID, Atomic: *wire.Atomic, Explanation: wire.Explanation}
	return nil
}
