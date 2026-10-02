package apitypes

// DecisionsMemoWriteRequest is the worker's save body for the run decisions memo (issue #2083).
// ClaimGeneration is a pointer so a missing key is distinguishable from a legitimate zero; the
// server refuses a missing or negative value with reason claim_generation_required, because the
// write is fenced on the exact generation the worker holds. Identity (user, repo, branch) is never
// accepted from the body: it is derived server-side from the claimed run.
type DecisionsMemoWriteRequest struct {
	ClaimGeneration *int64 `json:"claim_generation"`
	Body            string `json:"body"`
}

// DecisionsMemoDTO is the memo a resumed mr_rework run reads back: the body is the prior
// completed run's memo on the same MR lineage, SourceRunID names that run, Format is the
// stored format_version (currently 1).
type DecisionsMemoDTO struct {
	Format      int    `json:"format"`
	Body        string `json:"body"`
	SourceRunID string `json:"source_run_id"`
}

// DecisionsMemoReadResponse is the GET answer. Enabled mirrors the admin kill-switch; Memo is
// null when the feature is off, the run is not an mr_rework run, or no compatible memo exists.
type DecisionsMemoReadResponse struct {
	Enabled bool              `json:"enabled"`
	Memo    *DecisionsMemoDTO `json:"memo"`
}
