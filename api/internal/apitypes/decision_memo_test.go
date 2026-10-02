package apitypes

import "testing"

// TestDecisionsMemoWireTags pins the decisions-memo wire keys (issue #2083): the worker-side
// client and the handler must agree on them.
func TestDecisionsMemoWireTags(t *testing.T) {
	assertTags(t, "DecisionsMemoWriteRequest", DecisionsMemoWriteRequest{}, "claim_generation", "body")
	assertTags(t, "DecisionsMemoDTO", DecisionsMemoDTO{}, "format", "body", "source_run_id")
	assertTags(t, "DecisionsMemoReadResponse", DecisionsMemoReadResponse{}, "enabled", "memo")
}
