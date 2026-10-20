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
	if park := gate.Presentation.SemanticPark; park != "" {
		raw, _ := json.Marshal(park)
		rows = append(rows, []string{"SEMANTIC_PARK", sanitizeTTY(string(raw))})
	}
	for _, source := range gate.Presentation.Candidate.Sources {
		raw, _ := json.Marshal(source)
		rows = append(rows, []string{"SEMANTIC_SOURCE", sanitizeTTY(string(raw))})
	}
	for _, source := range gate.Sources {
		// Quote source newlines so resolveSemanticApproval cannot print forged rows.
		raw, _ := json.Marshal(string(source.Bytes))
		rows = append(rows, []string{"SEMANTIC_SOURCE_TEXT", sanitizeTTY(string(raw))})
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

// completionPendingRows is for the run detail render only, independent of any current gate (a frozen
// run can carry pending criteria without one); the approve flow must not print it. It prints each unverified maintainer criterion as one JSON-quoted row, so
// embedded newlines cannot forge rows. The criteria are publication data and grant no authority.
func completionPendingRows(run apitypes.RunDTO) [][]string {
	if run.CompletionPending == nil {
		return nil
	}
	var rows [][]string
	for _, criterion := range run.CompletionPending.Criteria {
		raw, _ := json.Marshal(criterion)
		rows = append(rows, []string{"SEMANTIC_PENDING", sanitizeTTY(string(raw))})
	}
	return rows
}

// semanticDriftRows prints PRD #1230 D5 source-drift observations for the run detail
// render. One summary row carries the derived contradiction flag and the dropped count;
// then each observation is one JSON-quoted row, so embedded newlines cannot forge rows.
// Drift is a detector: these rows grant no authority and mark nothing satisfied.
func semanticDriftRows(run apitypes.RunDTO) [][]string {
	if run.SemanticDrift == nil {
		return nil
	}
	summary, _ := json.Marshal(struct {
		Contradiction bool  `json:"contradiction"`
		Dropped       int64 `json:"dropped"`
		Observations  int   `json:"observations"`
	}{run.SemanticDrift.Contradiction, run.SemanticDrift.Dropped, len(run.SemanticDrift.Observations)})
	rows := [][]string{{"SEMANTIC_DRIFT", sanitizeTTY(string(summary))}}
	for _, observation := range run.SemanticDrift.Observations {
		raw, _ := json.Marshal(observation)
		rows = append(rows, []string{"SEMANTIC_DRIFT", sanitizeTTY(string(raw))})
	}
	return rows
}
