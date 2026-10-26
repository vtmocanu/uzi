package apitypes

// CodeCrossCheckCoverage is the worker's inventory account, independent of findings.
type CodeCrossCheckCoverage struct {
	Version             int                           `json:"version"`
	Entries             []CodeCrossCheckCoverageEntry `json:"entries"`
	InventoryIncomplete bool                          `json:"inventory_incomplete"`
	OmittedPaths        int64                         `json:"omitted_paths"`
	OmittedCountExact   bool                          `json:"omitted_count_exact"`
	UnknownJudgments    int64                         `json:"unknown_judgments"`
	InvalidJudgments    int64                         `json:"invalid_judgments"`
	OmittedJudgments    int64                         `json:"omitted_judgments"`
	ActivityComplete    bool                          `json:"activity_complete"`
	Reasons             []string                      `json:"reasons"`
}

type CodeCrossCheckCoverageEntry struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	RecordedRead bool   `json:"recorded_read"`
}
