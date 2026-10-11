package apitypes

// CompletionIdentity is frozen by the completed transition, before forge proof.
// MR and branch are candidates until the API verifies its own repository.
type CompletionIdentity struct {
	HoldID       string `json:"hold_id"`
	RunID        string `json:"run_id"`
	OwnerID      string `json:"owner_id"`
	WorkerID     string `json:"worker_id"`
	Generation   int64  `json:"generation"`
	FinalHead    string `json:"final_head"`
	RepoID       string `json:"repo_id"`
	ConnectionID string `json:"connection_id"`
	ProjectID    int64  `json:"project_id"`
	ForgeType    string `json:"forge_type"`
	BaseURL      string `json:"base_url"`
	Branch       string `json:"branch"`
	MRIID        *int64 `json:"mr_iid"`
}

// CompletedPublicationReceipt authorizes retirement of exactly one generation.
// It carries no capture or inventory coverage authority.
type CompletedPublicationReceipt struct {
	CompletionIdentity
	ObservedBranchHead string `json:"observed_branch_head"`
}
