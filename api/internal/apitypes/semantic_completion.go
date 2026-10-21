package apitypes

// CompletionPendingDTO is publication data, never evidence of completion.
// Strings are immutable source data; terminal consumers must quote them.
type CompletionPendingDTO struct {
	Version          int                          `json:"version"`
	ContractRevision int                          `json:"contract_revision"`
	Criteria         []CompletionPendingCriterion `json:"criteria"`
}

type CompletionPendingCriterion struct {
	CriterionID string `json:"criterion_id"`
	MilestoneID string `json:"milestone_id"`
	Kind        string `json:"kind"`
	Text        string `json:"text"`
	SemanticMaintainer
	AuthorizationRevision int    `json:"authorization_revision"`
	Status                string `json:"status"`
}
