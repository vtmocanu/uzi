package apitypes

// MilestoneReconciliation is explicit owner consent to replace the displayed contract.
// ExpectedCompletionRevision is mandatory JSON, and null for legacy milestone runs.
type MilestoneReconciliation struct {
	Mode                       string `json:"mode"`
	Reason                     string `json:"reason"`
	ExpectedCompletionRevision *int   `json:"expected_completion_revision"`
}

// GateCheckpointWitness binds the source tree H to its published checkpoint P.
// The API binds this projection to the publishing worker and current claim generation.
type GateCheckpointWitness struct {
	SourceHead          string `json:"source_head"`
	PublishedCheckpoint string `json:"published_checkpoint"`
}
