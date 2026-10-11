package apitypes

// CodeSnapshotCleanup describes retained code-check lineage for snapshot cleanup.
// It grants no execution or inventory authority.
//
// RowPresent=false is a valid answer: the lead exists for the caller but has no code
// cross-check row, so HeadCommit, Outcome and the checker fields are null and
// LeadClaimGeneration is 0. ChildPresent=false on a present row is likewise a valid
// answer (no child run is mapped), with the checker fields null and
// CheckerClaimGeneration 0. CheckerClaimGeneration is also 0 for a child never claimed.
// LeadClaimGeneration is cross_checks.lead_claim_generation, the lead claim generation
// that created the row and whose snapshot the worker pinned.
type CodeSnapshotCleanup struct {
	Protocol               string  `json:"protocol"`
	LeadRunID              string  `json:"lead_run_id"`
	LeadStatus             string  `json:"lead_status"`
	OwnedByWorker          bool    `json:"owned_by_worker"`
	RowPresent             bool    `json:"row_present"`
	LeadClaimGeneration    int64   `json:"lead_claim_generation"`
	HeadCommit             *string `json:"head_commit"`
	Outcome                *string `json:"outcome"`
	ChildPresent           bool    `json:"child_present"`
	CheckerRunID           *string `json:"checker_run_id"`
	CheckerClaimGeneration int64   `json:"checker_claim_generation"`
	CheckerStatus          *string `json:"checker_status"`
}
