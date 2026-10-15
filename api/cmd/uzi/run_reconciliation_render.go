package main

import (
	"encoding/json"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// reconciliationReviewRows bounds each cell, each collection and the audit page.
// A malformed audit entry is shown as unavailable and does not hide sibling entries.
func reconciliationReviewRows(r apitypes.RunDTO) [][]string {
	if r.ResetStateDigest == "" && len(r.MilestoneReconciliations) == 0 {
		return nil
	}
	rows := [][]string{
		{"RESET_WARNING", "Reset clears milestone progress, scope ceiling and prior decisions/waivers; invalidates prior permits."},
		{"GATE_REVISION", fmt.Sprint(r.GateRevision)},
		{"COMPLETION_REVISION", reviewValue(r.CompletionRevision)},
		{"RESET_STATE_DIGEST", cellText(r.ResetStateDigest)},
	}
	for _, field := range []struct {
		name  string
		value any
	}{
		{"FROZEN_MILESTONES", r.Milestones}, {"CANDIDATE_MILESTONES", r.MilestonesCandidate},
		{"MILESTONES_COMPLETED", r.MilestonesCompleted}, {"MILESTONES_IN_PROGRESS", r.MilestonesInProgress},
		{"MILESTONES_AGENTS", r.MilestonesAgents}, {"COMPLETION_ACCEPTED", r.CompletionAccepted},
		{"COMPLETION_DEFERRED", r.CompletionDeferred}, {"SCOPE_CEILING", r.ScopeCeiling},
		{"RESET_STOP_KIND", r.StopKind}, {"RESET_STOP_REASON", r.StopReason},
	} {
		rows = append(rows, reviewCollectionRows(field.name, field.value)...)
	}
	for i, raw := range r.MilestoneReconciliations {
		if i == 50 {
			rows = append(rows, []string{"RESET_AUDIT", "additional entries omitted; use JSON for this page"})
			break
		}
		var entry struct {
			GateRevision    int64                      `json:"gate_revision"`
			Reason          string                     `json:"reason"`
			CreatedAt       json.RawMessage            `json:"created_at"`
			ReasonTruncated bool                       `json:"reason_truncated"`
			DetailsOmitted  bool                       `json:"details_omitted"`
			BeforeState     map[string]json.RawMessage `json:"before_state"`
			AfterState      map[string]json.RawMessage `json:"after_state"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			rows = append(rows, []string{"RESET_AUDIT", "unavailable (malformed entry)"})
			continue
		}
		rows = append(rows, []string{"RESET_AUDIT", fmt.Sprintf("gate %d; time %s; reason %s", entry.GateRevision, reviewValue(entry.CreatedAt), cellText(entry.Reason))})
		if entry.ReasonTruncated {
			rows = append(rows, []string{"RESET_AUDIT", "reason_truncated: server shortened the reason"})
		}
		if entry.DetailsOmitted {
			rows = append(rows, []string{"RESET_AUDIT", "details_omitted: server omitted oversized details"})
		}
		for _, state := range []struct {
			name   string
			fields map[string]json.RawMessage
		}{
			{"BEFORE", entry.BeforeState}, {"AFTER", entry.AfterState},
		} {
			if state.fields == nil {
				rows = append(rows, []string{state.name, "snapshot unavailable"})
				continue
			}
			for _, key := range []string{"milestones_frozen", "milestones_candidate", "milestones_completed", "milestones_in_progress", "milestones_agents", "completion_contract", "scope_ceiling", "stop_kind", "stop_reason"} {
				if value, ok := state.fields[key]; ok {
					rows = append(rows, []string{state.name + " " + key, reviewValue(value)})
				}
			}
		}
	}
	if r.MilestoneReconciliationsNextBeforeGateRevision != nil {
		rows = append(rows, []string{"RECONCILIATIONS_NEXT", fmt.Sprintf("--reconciliation-before-gate-revision %d", *r.MilestoneReconciliationsNextBeforeGateRevision)})
	}
	return rows
}

// reviewCollectionRows exposes explicit empty arrays and preserves null distinctly.
func reviewCollectionRows(name string, value any) [][]string {
	raw, err := json.Marshal(value)
	if err != nil {
		return [][]string{{name, "unavailable"}}
	}
	var items []json.RawMessage
	if string(raw) != "null" && json.Unmarshal(raw, &items) == nil {
		if len(items) == 0 {
			return [][]string{{name, "[] (empty)"}}
		}
		rows := make([][]string, 0, min(len(items), 40)+1)
		for i, item := range items {
			if i == 40 {
				rows = append(rows, []string{name, "additional items omitted; use --json"})
				break
			}
			rows = append(rows, []string{name, reviewValue(item)})
		}
		return rows
	}
	return [][]string{{name, reviewValue(value)}}
}

func reviewValue(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "unavailable"
	}
	if string(raw) == "null" {
		return "null"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return cellText(text)
	}
	return cellText(string(raw))
}
