package apitypes

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

type SemanticEvidence struct {
	Description string `json:"description"`
	Path        string `json:"path,omitempty"`
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
	PlanHash                 string                 `json:"plan_hash"`
	SourceOutcome            string                 `json:"source_outcome"`
	Criteria                 []SemanticCriterion    `json:"criteria"`
	Coverage                 *SemanticCoverageState `json:"coverage,omitempty"`
}
