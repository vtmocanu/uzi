package apitypes

// SemanticMaintainerApproval is an exact displayed owner-only criterion, not a
// completion assertion. Array order is significant and copied from the candidate.
type SemanticMaintainerApproval struct {
	ID string `json:"id"`
	SemanticMaintainer
}

// SemanticGatePresentation is server-owned. Source bytes are fetched separately
// from immutable server blobs; they are never copied into a gate snapshot.
type SemanticGatePresentation struct {
	Candidate        SemanticCandidate            `json:"candidate"`
	Binding          SemanticCoverageBinding      `json:"binding"`
	ClaimGeneration  int64                        `json:"claim_generation"`
	Coverage         SemanticCoverageStatus       `json:"coverage"`
	ReviewID         string                       `json:"review_id,omitempty"`
	InvocationID     string                       `json:"invocation_id,omitempty"`
	ReportDigest     string                       `json:"report_digest,omitempty"`
	ConsumedSeconds  int                          `json:"consumed_seconds"`
	RemainingSeconds int                          `json:"remaining_seconds"`
	RemainingReviews int                          `json:"remaining_reviews"`
	Maintainers      []SemanticMaintainerApproval `json:"maintainers"`
}

// SemanticGateApproval must be echoed exactly from the server presentation.
// A body string, answer, or checkbox never supplies this authority.
type SemanticGateApproval struct {
	PresentationRevision     int64  `json:"presentation_revision"`
	PresentationDigest       string `json:"presentation_digest"`
	CandidateIdentityVersion string `json:"candidate_identity_version"`
	CandidateRevision        int64  `json:"candidate_revision"`
	SemanticCoverageBinding
	ClaimGeneration int64                        `json:"claim_generation"`
	ReviewID        string                       `json:"review_id,omitempty"`
	InvocationID    string                       `json:"invocation_id,omitempty"`
	ReportDigest    string                       `json:"report_digest,omitempty"`
	Maintainers     []SemanticMaintainerApproval `json:"maintainers"`
}

type SemanticGateDTO struct {
	Presentation SemanticGatePresentation `json:"presentation"`
	Approval     SemanticGateApproval     `json:"approval"`
	// Sources is transport-only, base64 encoded by JSON, never stored in a gate.
	Sources []SemanticSourceCapture `json:"sources"`
	Plan    []byte                  `json:"plan"`
}
