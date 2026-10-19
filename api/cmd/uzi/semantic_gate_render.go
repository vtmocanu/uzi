package main

import (
	"encoding/json"
	"fmt"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Every criterion and citation is printed, rather than a count or preview that
// could hide an owner-only condition. Source bytes remain available in --json.
func semanticGateRows(run apitypes.RunDTO) [][]string {
	if run.SemanticGate == nil {
		return nil
	}
	gate := run.SemanticGate
	rows := [][]string{{"SEMANTIC_COVERAGE", gate.Presentation.Coverage.Outcome},
		{"SEMANTIC_BUDGET", fmt.Sprintf("%d tokens consumed, %d remaining; %d seconds consumed, %d remaining; %d reviews remaining", gate.Presentation.Coverage.ChargedTokens, gate.Presentation.Coverage.RemainingTokens, gate.Presentation.ConsumedSeconds, gate.Presentation.RemainingSeconds, gate.Presentation.RemainingReviews)}}
	for _, source := range gate.Presentation.Candidate.Sources {
		raw, _ := json.Marshal(source)
		rows = append(rows, []string{"SEMANTIC_SOURCE", sanitizeTTY(string(raw))})
	}
	for _, source := range gate.Sources {
		rows = append(rows, []string{"SEMANTIC_SOURCE_TEXT", sanitizeTTY(string(source.Bytes))})
	}
	for _, criterion := range gate.Presentation.Candidate.Criteria {
		raw, _ := json.Marshal(criterion)
		rows = append(rows, []string{"SEMANTIC_CRITERION", sanitizeTTY(string(raw))})
	}
	if gate.Presentation.Coverage.Report != nil {
		raw, _ := json.Marshal(gate.Presentation.Coverage.Report)
		rows = append(rows, []string{"SEMANTIC_REPORT", sanitizeTTY(string(raw))})
	}
	raw, _ := json.Marshal(gate.Approval)
	return append(rows, []string{"SEMANTIC_APPROVAL", sanitizeTTY(string(raw))})
}
