package runpresentation

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func sp(s string) *string { return &s }

func TestLeadHistoryRowCells(t *testing.T) {
	tests := []struct {
		name string
		row  apitypes.LeadPhaseConfigDTO
		want LeadHistoryCells
	}{
		{
			name: "explicit effort with matching observation",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:        "claude",
				RequestedModel: sp("opus"), RequestedModelSource: "user_default",
				RequestedEffort: sp("medium"), RequestedEffortMode: "explicit", RequestedEffortSource: "builtin",
				ObservedModel: sp("claude-opus-4"), ObservedModelEvidence: sp("claude_init"),
				ObservedEffort: sp("medium"), ObservedEffortEvidence: sp("claude_init"),
			},
			want: LeadHistoryCells{
				Requested: "opus · medium",
				Source:    "model: your Claude default; effort: built-in",
				Observed:  "claude-opus-4 · medium",
				Evidence:  "model: Claude SDK init; effort: Claude SDK init",
			},
		},
		{
			name: "observed effort other",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:        "claude",
				RequestedModel: sp("opus"), RequestedModelSource: "template",
				RequestedEffort: sp("high"), RequestedEffortMode: "explicit", RequestedEffortSource: "template",
				ObservedModel: sp("claude-opus-4"), ObservedModelEvidence: sp("claude_init"),
				ObservedEffort: sp("other"), ObservedEffortEvidence: sp("claude_get_settings"),
			},
			want: LeadHistoryCells{
				Requested: "opus · high",
				Source:    "model: lead template; effort: agent template",
				Observed:  "claude-opus-4 · an unrecognized level",
				Evidence:  "model: Claude SDK init; effort: Claude SDK settings",
			},
		},
		{
			name: "null observed effort with Claude evidence",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:        "claude",
				RequestedModel: sp("opus"), RequestedModelSource: "user_default",
				RequestedEffortMode: "runtime_default", RequestedEffortSource: "user_default",
				ObservedModel: sp("claude-opus-4"), ObservedModelEvidence: sp("claude_init"),
				ObservedEffortEvidence: sp("claude_init"),
			},
			want: LeadHistoryCells{
				Requested: "opus · runtime default",
				Source:    "model: your Claude default; effort: runtime default",
				Observed:  "claude-opus-4 · not sent",
				Evidence:  "model: Claude SDK init; effort: Claude SDK init",
			},
		},
		{
			name: "builtin model with no requested model shows the observed model",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:              "claude",
				RequestedModelSource: "builtin",
				RequestedEffortMode:  "runtime_default",
				ObservedModel:        sp("claude-sonnet-4"), ObservedModelEvidence: sp("claude_init"),
			},
			want: LeadHistoryCells{
				Requested: "claude-sonnet-4 · runtime default",
				Source:    "model: Claude SDK default; effort: runtime default",
				Observed:  "claude-sonnet-4 · -",
				Evidence:  "model: Claude SDK init; effort: not reported yet",
			},
		},
		{
			name: "builtin model with no requested model and nothing observed",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:              "claude",
				RequestedModelSource: "builtin",
				RequestedEffortMode:  "runtime_default",
			},
			want: LeadHistoryCells{
				Requested: "default · runtime default",
				Source:    "model: Claude SDK default; effort: runtime default",
				Observed:  "- · -",
				Evidence:  "model: not reported yet; effort: not reported yet",
			},
		},
		{
			name: "codex row",
			row: apitypes.LeadPhaseConfigDTO{
				Harness:        "codex",
				RequestedModel: sp("gpt-5"), RequestedModelSource: "user_default",
				RequestedEffort: sp("high"), RequestedEffortMode: "explicit", RequestedEffortSource: "user_default",
				ObservedModel: sp("gpt-5"), ObservedModelEvidence: sp("codex_thread_response"),
				ObservedEffort: sp("high"), ObservedEffortEvidence: sp("codex_sent"),
			},
			want: LeadHistoryCells{
				Requested: "gpt-5 · high",
				Source:    "model: your Codex default; effort: your Codex effort",
				Observed:  "gpt-5 · high",
				Evidence:  "model: Codex thread config; effort: sent to Codex",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LeadHistoryRowCells(tc.row); got != tc.want {
				t.Errorf("LeadHistoryRowCells =\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
