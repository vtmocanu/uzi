package apitypes

// CodeSnapshotCleanup describes retained code-check lineage for snapshot cleanup.
// It grants no execution or inventory authority.
type CodeSnapshotCleanup struct {
	Protocol               string  `json:"protocol"`
	LeadRunID              string  `json:"lead_run_id"`
	HeadCommit             *string `json:"head_commit"`
	Outcome                string  `json:"outcome"`
	LeadStatus             string  `json:"lead_status"`
	OwnedByWorker          bool    `json:"owned_by_worker"`
	CheckerRunID           string  `json:"checker_run_id"`
	CheckerClaimGeneration int64   `json:"checker_claim_generation"`
	CheckerStatus          string  `json:"checker_status"`
}
