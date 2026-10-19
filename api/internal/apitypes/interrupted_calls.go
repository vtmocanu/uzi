package apitypes

// InterruptedCallsResponse is advisory evidence from a bounded history window.
// SourceGeneration is null when that window contains no prior stamped tools.
type InterruptedCallsResponse struct {
	Format            int               `json:"format"`
	RunID             string            `json:"run_id"`
	WorkerID          string            `json:"worker_id"`
	RegisterNonce     string            `json:"register_nonce"`
	ClaimGeneration   int64             `json:"claim_generation"`
	SourceGeneration  *int64            `json:"source_generation"`
	InspectedCount    int32             `json:"inspected_count"`
	HistoryIncomplete bool              `json:"history_incomplete"`
	Unavailable       bool              `json:"unavailable"`
	Truncated         bool              `json:"truncated"`
	Entries           []InterruptedCall `json:"entries"`
}

type InterruptedCall struct {
	Seq              int32   `json:"seq"`
	SourceGeneration int64   `json:"source_generation"`
	Agent            string  `json:"agent"`
	AgentInstance    *string `json:"agent_instance"`
	AgentLabel       *string `json:"agent_label"`
	ToolName         string  `json:"tool_name"`
	Command          string  `json:"command"`
	DisplayOmitted   bool    `json:"display_omitted"`
}
