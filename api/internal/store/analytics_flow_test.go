package store_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729: tests of the Flow statements' text and the typed document, none of which need
// a database.

// TestAnalyticsFlowStatementsDoNotDrift: the owner and all-scope Flow statements are textually
// identical except for the lines marked owner-only (ADR-1184 D1 keeps them separate statements;
// this keeps two large documents from diverging).
func TestAnalyticsFlowStatementsDoNotDrift(t *testing.T) {
	_, blocks := analyticsSQLBlocks(t)
	owner, all := blocks["AnalyticsOwnerFlow"], blocks["AnalyticsAllFlow"]
	if owner == "" || all == "" {
		t.Fatal("missing Flow statement block")
	}
	norm := func(block string) string {
		var out []string
		for i, line := range strings.Split(block, "\n") {
			if i == 0 || strings.HasSuffix(line, "-- owner-only") {
				continue // the "-- name:" line, or a scope line only the owner statement has
			}
			out = append(out, strings.TrimRight(line, " "))
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	if o, a := norm(owner), norm(all); o != a {
		ol, al := strings.Split(o, "\n"), strings.Split(a, "\n")
		for i := 0; i < len(ol) && i < len(al); i++ {
			if ol[i] != al[i] {
				t.Fatalf("Flow statements drifted at normalised line %d:\n owner: %s\n all:   %s", i+1, ol[i], al[i])
			}
		}
		t.Fatalf("Flow statements drifted in length: %d vs %d lines", len(ol), len(al))
	}
	tagged := func(block string) (ownerOnly, allOnly []string) {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "-- owner-only") {
				ownerOnly = append(ownerOnly, line)
			}
			if strings.Contains(line, "-- all-only") {
				allOnly = append(allOnly, line)
			}
		}
		return ownerOnly, allOnly
	}
	wantOwnerOnly := []string{
		"AND r.user_id = @user_id::uuid  -- owner-only",
		"AND w.user_id = @user_id::uuid  -- owner-only",
	}
	if gotOO, gotAO := tagged(owner); !reflect.DeepEqual(gotOO, wantOwnerOnly) || len(gotAO) != 0 {
		t.Errorf("owner statement tagged lines:\n owner-only %q\n all-only %q\nwant owner-only %q and no all-only", gotOO, gotAO, wantOwnerOnly)
	}
	if gotOO, gotAO := tagged(all); len(gotOO) != 0 || len(gotAO) != 0 {
		t.Errorf("all statement carries tagged lines: owner-only %q all-only %q", gotOO, gotAO)
	}
	if strings.Contains(all, "@user_id") {
		t.Error("all-scope statement references @user_id")
	}
	// The generated constants (what actually runs) carry the same scope predicates.
	for _, want := range []string{`r\.user_id = \$\d+::uuid`, `w\.user_id = \$\d+::uuid`} {
		if !regexp.MustCompile(want).MatchString(store.AnalyticsOwnerFlowSQL) {
			t.Errorf("generated owner statement lacks %s", want)
		}
	}
	if loc := regexp.MustCompile(`user_id = \$\d+`).FindString(store.AnalyticsAllFlowSQL); loc != "" {
		t.Errorf("generated all-scope statement binds a user: %q", loc)
	}
	// The gate path is a per-run lookup, never a global scan, and never reads run_messages.
	for name, b := range map[string]string{"owner": owner, "all": all} {
		for _, want := range []string{"WHERE x.run_id = g.id AND x.kind = 'approve_plan'\n        OFFSET 0", "FROM run_gate_presentations x\n        WHERE x.run_id = g.id\n        OFFSET 0"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s statement lost the per-run lookup %q", name, want)
			}
		}
		if strings.Contains(b, "run_messages") {
			t.Errorf("%s statement reads run_messages", name)
		}
	}
}

const analyticsFlowDocLiteral = `{
  "generated_at": "2033-06-15T12:00:00.000000Z",
  "waiting_now": {"time_basis": "now", "population": "current_runs", "states": [
    {"state": "queued", "count": 2, "oldest_status_since": "2033-06-14T10:00:00.000000Z", "oldest_age_seconds": 93600},
    {"state": "paused", "count": 0, "oldest_status_since": null, "oldest_age_seconds": null}], "total": 2},
  "activity": {
    "created": {"time_basis": "created_at", "population": "created_in_window", "total": 3,
      "buckets": [{"index": 0, "count": 1}, {"index": 1, "count": 2}]},
    "finished": {"time_basis": "finished_at", "population": "finished_in_window", "excluded_kinds": ["chat"],
      "total": {"completed": 1, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 1, "total": 2},
      "buckets": [{"index": 0, "completed": 1, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 1, "total": 2}]}},
  "first_start_delay": {"time_basis": "created_at", "population": "created_in_window", "approximate": true,
    "limitations": ["first_started_at_legacy_backfill"], "observed": 3, "not_started": 1, "invalid": 0,
    "p50_seconds": {"value": 600, "reason": null}, "p90_seconds": {"value": 900.5, "reason": null}},
  "plan_response": {"time_basis": "created_at", "population": "created_in_window_gate_pairs", "approximate": true,
    "n": 0, "p50_seconds": {"value": null, "reason": "no_observations"}, "p90_seconds": {"value": null, "reason": "no_observations"},
    "unknown": {"total": 2, "reasons": {"ambiguous": 1, "no_presentation": 0, "invalid_chronology": 0,
      "excluded_disposition": 0, "reconciliation_only": 0, "legacy_unbound": 1}}, "not_approved": 4},
  "delivery_funnel": {"time_basis": "created_at", "population": "created_in_window_delivery_kinds", "applicable": true, "reason": null,
    "stages": {"created": 5, "plan_settled": 3, "completed": 2, "pr_present": 2, "merged": 1},
    "planning": {"settled_human": 2, "settled_auto": 1, "skipped": 0, "rejected": 0, "in_progress": 1, "failed": 0, "cancelled": 0, "unknown": 1},
    "outcomes": {"completed": 2, "failed": 1, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "in_progress": 2},
    "disclosure": {"historical_approval_only": 1, "ambiguous_current": 0, "legacy_evidence": 0},
    "pr_units": {"present": 2, "merged": 1, "buckets": [{"index": 0, "present": 1, "merged": 1}]}},
  "approval": {"time_basis": "created_at", "population": "created_in_window_plan_cohort", "applicable": false, "reason": "not_applicable",
    "total": 0, "human": 0, "auto_approve_configured": 0,
    "unknown": {"total": 0, "reasons": {"ambiguous": 0, "legacy_unbound": 0, "historical_approval_only": 0, "no_evidence": 0}}},
  "issue_elapsed": {"time_basis": "created_at", "population": "created_in_window_issue_runs", "applicable": true, "reason": null,
    "n": 6, "unfinished": 1, "invalid": 0, "incomplete": true,
    "p50_seconds": {"value": 100, "reason": null}, "p90_seconds": {"value": 200, "reason": null},
    "histogram": [{"lower_seconds": 0, "upper_seconds": 900, "count": 6}, {"lower_seconds": 900, "upper_seconds": null, "count": 0}],
    "buckets": [{"index": 0, "n": 3, "unfinished": 1, "invalid": 0, "incomplete": true,
      "p50_seconds": {"value": null, "reason": "insufficient_observations"}, "p90_seconds": {"value": null, "reason": "insufficient_observations"}}]}
}`

func TestDecodeAnalyticsFlowDoc(t *testing.T) {
	raw := []byte(analyticsFlowDocLiteral)
	doc, err := store.DecodeAnalyticsFlowDoc(raw)
	if err != nil {
		t.Fatalf("populated literal: %v", err)
	}
	w := doc.WaitingNow
	if len(w.States) != 2 || w.States[0].Count != 2 || w.States[0].OldestAgeSeconds == nil || *w.States[0].OldestAgeSeconds != 93600 ||
		w.States[1].OldestStatusSince != nil || w.Total != 2 {
		t.Errorf("waiting_now = %+v", w)
	}
	if doc.Activity.Finished.ExcludedKinds[0] != "chat" || doc.Activity.Finished.Buckets[0].Total != 2 || doc.Activity.Created.Buckets[1].Count != 2 {
		t.Errorf("activity = %+v", doc.Activity)
	}
	if doc.FirstStartDelay.P90Seconds.Value == nil || *doc.FirstStartDelay.P90Seconds.Value != 900.5 {
		t.Errorf("first_start_delay = %+v", doc.FirstStartDelay)
	}
	if r := doc.PlanResponse.P50Seconds.Reason; r == nil || *r != store.AnalyticsReasonNoObservations || doc.PlanResponse.Unknown.Reasons.LegacyUnbound != 1 {
		t.Errorf("plan_response = %+v", doc.PlanResponse)
	}
	if doc.DeliveryFunnel.Stages.PlanSettled != 3 || doc.DeliveryFunnel.PRUnits.Buckets[0].Merged != 1 || doc.DeliveryFunnel.Disclosure.HistoricalApprovalOnly != 1 {
		t.Errorf("delivery_funnel = %+v", doc.DeliveryFunnel)
	}
	if r := doc.Approval.Reason; doc.Approval.Applicable || r == nil || *r != store.AnalyticsReasonNotApplicable {
		t.Errorf("approval = %+v", doc.Approval)
	}
	el := doc.IssueElapsed
	if len(el.Histogram) != 2 || el.Histogram[1].UpperSeconds != nil || *el.Histogram[0].UpperSeconds != 900 ||
		el.Buckets[0].P50Seconds.Reason == nil || *el.Buckets[0].P50Seconds.Reason != store.AnalyticsReasonInsufficientObservations {
		t.Errorf("issue_elapsed = %+v", el)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["surprise"] = json.RawMessage(`1`)
	withExtra, _ := json.Marshal(m)
	if _, err := store.DecodeAnalyticsFlowDoc(withExtra); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, err := store.DecodeAnalyticsFlowDoc([]byte(strings.Replace(analyticsFlowDocLiteral, `"population": "current_runs",`, `"population": "current_runs", "repo_id": "x",`, 1))); err == nil {
		t.Error("a nested unknown key was accepted")
	}
	if _, err := store.DecodeAnalyticsFlowDoc(append(append([]byte{}, raw...), []byte(`{}`)...)); err == nil {
		t.Error("trailing data was accepted")
	}
	if _, err := store.DecodeAnalyticsFlowDoc([]byte(`{"waiting_now":{"total":"x"}}`)); err == nil {
		t.Error("a mistyped field was accepted")
	}
}

func TestFlowConstants(t *testing.T) {
	if store.FlowMinPercentileN != 5 {
		t.Errorf("FlowMinPercentileN = %d, want 5", store.FlowMinPercentileN)
	}
	want := []float64{0, 900, 1800, 3600, 7200, 14400, 28800, 86400, 259200}
	if !reflect.DeepEqual(store.FlowHistogramEdgesSeconds, want) {
		t.Errorf("FlowHistogramEdgesSeconds = %v, want %v", store.FlowHistogramEdgesSeconds, want)
	}
}
