package workersvc

// The summary_usage run-message kind (issue #2686): the usage of one SummaryRunner model pass
// (the intent, plan or PR-description summary), sent by the worker once per pass that observed
// usage. Unlike progress_note it carries no text; it is accounting only.
//
// Two api-side rules live here:
//
//  1. INGEST (normalizeSummaryUsagePayload): the stored payload is rebuilt from scratch with
//     only {pass, model_usage}, each usage entry carrying the server-resolved costStatus (and
//     costUSD when metered). `pass` must be one of the allowlisted names, anything else is "".
//     Any other key is dropped, in particular `event` (a payload that says event:"result"
//     would be read by the usage tail and the web fold as the end of a leg) and `usage` (the
//     web fold would count it a second time).
//  2. USAGE FOLD (foldSummaryUsage): the pass's model_usage is folded into run_usage under its
//     OWN key so it can neither collapse into, nor be collapsed by, the run's own result frames
//     or a progress_note naming the same model in the same leg
//     (model = "summary_pass:<model>", lineage_epoch = the message's seq). Two passes in one
//     leg (a PR-description regeneration) therefore land on two rows that sum.

import (
	"context"
	"encoding/json"

	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	// KindSummaryUsage is the run_messages.kind of a summary pass's usage.
	KindSummaryUsage = "summary_usage"

	// summaryPassModelPrefix keys a summary pass's usage apart from the run's own result
	// frames and from progress_note rows that may name the same model in the same leg.
	summaryPassModelPrefix = "summary_pass:"
)

// summaryPasses is the allowlist of `pass` values the worker emits.
var summaryPasses = map[string]bool{"intent": true, "plan": true, "pr_description": true}

// summaryUsagePayload is the whole stored shape of a summary_usage payload.
type summaryUsagePayload struct {
	Pass       string                            `json:"pass"`
	ModelUsage map[string]progressNoteModelUsage `json:"model_usage,omitempty"`
}

// normalizeSummaryUsagePayload rebuilds a summary_usage payload from only the allowed keys.
// It never fails: a payload that is not an object, or whose fields are the wrong type, yields
// an empty payload (pass "", no usage), which every reader ignores. Cost resolution is the
// progress_note one (resolveProgressNoteCost), so see normalizeProgressNotePayload for the
// costStatus/costUSD rules.
func normalizeSummaryUsagePayload(raw json.RawMessage, harness string) json.RawMessage {
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		in = nil
	}
	out := summaryUsagePayload{}
	var s string
	if json.Unmarshal(in["pass"], &s) == nil && summaryPasses[s] {
		out.Pass = s
	}
	var usage map[string]json.RawMessage
	if json.Unmarshal(in["model_usage"], &usage) == nil {
		out.ModelUsage = normalizeModelUsageMap(usage, harness, summaryPassModelPrefix, KindSummaryUsage)
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Unreachable: every field is a string, an int64 or a sanitised raw number.
		return json.RawMessage(`{"pass":""}`)
	}
	return b
}

// foldSummaryUsage folds one summary_usage frame's model_usage into run_usage under
// model = "summary_pass:<model>", lineage_epoch = the frame's seq, lineage_index 0, usage_basis
// per_leg (see foldPrefixedModelUsage and foldProgressNoteUsage for why the seq epoch makes a
// re-delivered batch and RefoldRunUsage land on the same row). A malformed or usage-free frame
// folds nothing and never fails the append.
func foldSummaryUsage(ctx context.Context, q usageFoldQuerier, run store.Run, sessionID string, m IncomingMessage) error {
	var p summaryUsagePayload
	if err := json.Unmarshal(m.Payload, &p); err != nil || len(p.ModelUsage) == 0 {
		return nil
	}
	return foldPrefixedModelUsage(ctx, q, run, sessionID, m, summaryPassModelPrefix, p.ModelUsage, "summary pass")
}
