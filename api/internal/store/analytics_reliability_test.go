package store_test

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4685: tests of the Reliability and latest-failure statements' text and the typed
// document, none of which need a database.

// analyticsPairDiff fails unless the owner and all-scope blocks are identical once their name
// line and the lines tagged for the other scope are dropped, and unless the tagged lines are
// exactly the allowlists. It returns the two blocks.
func analyticsPairDiff(t *testing.T, ownerName, allName string, wantOwnerOnly, wantAllOnly []string) (owner, all string) {
	t.Helper()
	_, blocks := analyticsSQLBlocks(t)
	owner, all = blocks[ownerName], blocks[allName]
	if owner == "" || all == "" {
		t.Fatalf("missing statement block %s / %s", ownerName, allName)
	}
	norm := func(block, drop string) string {
		var out []string
		for i, line := range strings.Split(block, "\n") {
			if i == 0 || strings.HasSuffix(line, drop) {
				continue
			}
			out = append(out, strings.TrimRight(line, " "))
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	o, a := norm(owner, "-- owner-only"), norm(all, "-- all-only")
	if o != a {
		ol, al := strings.Split(o, "\n"), strings.Split(a, "\n")
		for i := 0; i < len(ol) && i < len(al); i++ {
			if ol[i] != al[i] {
				t.Fatalf("%s and %s drifted at normalised line %d:\n owner: %s\n all:   %s", ownerName, allName, i+1, ol[i], al[i])
			}
		}
		t.Fatalf("%s and %s drifted in length: %d vs %d lines", ownerName, allName, len(ol), len(al))
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
	if oo, ao := tagged(owner); !reflect.DeepEqual(oo, wantOwnerOnly) || len(ao) != 0 {
		t.Errorf("%s tagged lines:\n owner-only %q\n all-only %q\nwant owner-only %q and no all-only", ownerName, oo, ao, wantOwnerOnly)
	}
	if oo, ao := tagged(all); !reflect.DeepEqual(ao, wantAllOnly) || len(oo) != 0 {
		t.Errorf("%s tagged lines:\n all-only %q\n owner-only %q\nwant all-only %q and no owner-only", allName, ao, oo, wantAllOnly)
	}
	return owner, all
}

// TestAnalyticsReliabilityStatementsDoNotDrift: the owner and all-scope Reliability statements
// are textually identical except the lines tagged owner-only / all-only, and those lines are
// exactly the scope predicates and the repo-visibility gate.
func TestAnalyticsReliabilityStatementsDoNotDrift(t *testing.T) {
	owner, all := analyticsPairDiff(t, "AnalyticsOwnerReliability", "AnalyticsAllReliability",
		[]string{
			"(@reveal_repos::boolean IS TRUE OR fc.id IS NOT NULL) AS repo_shown,  -- owner-only",
			"LEFT JOIN forge_connections fc ON fc.id = rpo.connection_id AND fc.user_id = @user_id::uuid  -- owner-only",
			"AND r.user_id = @user_id::uuid  -- owner-only",
		},
		[]string{"true AS repo_shown,  -- all-only"})
	if !strings.Contains(owner, "r.user_id = @user_id::uuid") {
		t.Error("owner statement lacks its user predicate")
	}
	if strings.Contains(all, "@user_id") || strings.Contains(all, "@reveal_repos") {
		t.Error("all-scope statement references @user_id or @reveal_repos")
	}
	if !strings.Contains(owner, "@reveal_repos::boolean IS TRUE OR") || strings.Contains(owner, "IS NOT FALSE") {
		t.Error("owner statement's repo visibility gate is not fail-closed")
	}
	for name, b := range map[string]string{"owner": owner, "all": all} {
		for _, want := range []string{"cohort AS MATERIALIZED", "run_fold AS MATERIALIZED", "run_outcome_classes oc WHERE oc.run_id = c.id OFFSET 0",
			"WHERE mf.run_id = c.id AND @breakdown::text = 'primary_model'\n          AND c.created_at >= sqlc.arg('from')::timestamptz  -- only current-window rows read primary_model\n        OFFSET 0"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s statement lacks %q", name, want)
			}
		}
		// The failure-origin set is Go-owned: never spelled in the statement.
		if strings.Contains(b, "workflow_scope_missing") || strings.Contains(b, "push_secret_blocked") {
			t.Errorf("%s statement spells a landable origin instead of binding @landable_origins", name)
		}
	}
	for _, c := range []struct{ name, sql string }{{"owner", store.AnalyticsOwnerReliabilitySQL}, {"all", store.AnalyticsAllReliabilitySQL}} {
		if c.sql == "" {
			t.Fatalf("generated %s constant is empty", c.name)
		}
	}
	for _, want := range []string{`r\.user_id = \$\d+::uuid`, `\$\d+::boolean IS TRUE OR`} {
		if !regexp.MustCompile(want).MatchString(store.AnalyticsOwnerReliabilitySQL) {
			t.Errorf("generated owner statement lacks %s", want)
		}
	}
	if loc := regexp.MustCompile(`user_id = \$\d+`).FindString(store.AnalyticsAllReliabilitySQL); loc != "" {
		t.Errorf("generated all-scope statement binds a user: %q", loc)
	}
}

// TestAnalyticsLatestFailureStatementsDoNotDrift: the same pairing rule for the lifetime
// latest-failure statements, plus the verbatim SelfRunOutcomes predicate and the absence of any
// created_at cutoff.
func TestAnalyticsLatestFailureStatementsDoNotDrift(t *testing.T) {
	owner, all := analyticsPairDiff(t, "AnalyticsOwnerLatestFailure", "AnalyticsAllLatestFailure",
		[]string{
			"AND user_id = @user_id::uuid  -- owner-only",
			"AND r3.user_id = @user_id::uuid  -- owner-only",
		}, nil)
	if strings.Contains(all, "@user_id") {
		t.Error("all-scope statement references @user_id")
	}
	for name, b := range map[string]string{"owner": owner, "all": all} {
		var code []string
		for _, l := range strings.Split(b, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(l), "--") {
				code = append(code, l)
			}
		}
		if text := strings.Join(code, "\n"); strings.Contains(text, "created_at") || strings.Contains(text, "@now") {
			t.Errorf("%s latest-failure statement carries a created_at cutoff or a clock", name)
		}
		for _, want := range []string{
			"WHERE status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND fail_origin IS DISTINCT FROM 'base_diverged'\n      AND fn_outcome_kind(kind)",
			"ORDER BY COALESCE(finished_at, status_since) DESC, id DESC LIMIT 1",
			"JOIN runs r3 ON COALESCE(r3.finished_at, r3.status_since) > f.ended_at",
			"r3.status = 'completed' AND fn_outcome_kind(r3.kind)",
			"r3.kind = ANY(@kinds::text[])",
		} {
			if !strings.Contains(b, want) {
				t.Errorf("%s latest-failure statement lacks %q", name, want)
			}
		}
	}
	// The predicate really is SelfRunOutcomes' last_failure predicate, character for character.
	runtimeSQL, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(runtimeSQL)
	pred := "status = 'failed' AND fail_origin IS DISTINCT FROM 'plan_rejected' AND fail_origin IS DISTINCT FROM 'base_diverged'\n      AND fn_outcome_kind(kind)\n    ORDER BY COALESCE(finished_at, status_since) DESC, id DESC LIMIT 1"
	if !strings.Contains(raw, pred) {
		t.Error("queries/runtime.sql no longer carries the last_failure predicate this statement mirrors; reconcile the two")
	}
	for _, c := range []struct{ name, sql string }{{"owner", store.AnalyticsOwnerLatestFailureSQL}, {"all", store.AnalyticsAllLatestFailureSQL}} {
		if c.sql == "" {
			t.Fatalf("generated %s constant is empty", c.name)
		}
	}
	if loc := regexp.MustCompile(`user_id = \$\d+`).FindString(store.AnalyticsAllLatestFailureSQL); loc != "" {
		t.Errorf("generated all-scope statement binds a user: %q", loc)
	}
}

// TestAnalyticsReliabilityPrimaryModelMatchesSpend: Reliability's Primary-model expression is
// the Spend statement's, line for line, so the two sections can never disagree on a run's model.
func TestAnalyticsReliabilityPrimaryModelMatchesSpend(t *testing.T) {
	_, blocks := analyticsSQLBlocks(t)
	expr := func(block string) []string {
		lines := strings.Split(block, "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "COALESCE((array_agg(mf.model ORDER BY") {
				return []string{strings.TrimSpace(l), strings.TrimSpace(lines[i+1])}
			}
		}
		return nil
	}
	spend := expr(blocks["AnalyticsOwnerSpend"])
	if len(spend) != 2 {
		t.Fatal("no Primary-model expression in AnalyticsOwnerSpend")
	}
	for _, name := range []string{"AnalyticsOwnerReliability", "AnalyticsAllReliability", "AnalyticsAllSpend"} {
		if got := expr(blocks[name]); !reflect.DeepEqual(got, spend) {
			t.Errorf("%s Primary-model expression %q differs from Spend's %q", name, got, spend)
		}
	}
}

// analyticsReliabilityDocLiteral is a populated Reliability document written by hand, so the
// decoder is checked against the wire shape and not against the Go types it is built from.
const analyticsReliabilityDocLiteral = `{
  "generated_at": "2026-11-25T12:00:00.000000Z",
  "totals": {
    "current": {"finished": 10, "outcomes": {"completed": 6, "cancelled": 1, "plan_rejected": 1, "base_diverged": 0, "failed": 2},
                "failure_rate": {"value": 0.2, "reason": null}, "requeued_runs": 3},
    "previous": {"finished": 0, "outcomes": {"completed": 0, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 0},
                 "failure_rate": {"value": null, "reason": "no_finished_runs"}, "requeued_runs": 0}
  },
  "buckets": [
    {"index": 0, "finished": 4, "outcomes": {"completed": 3, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 1}},
    {"index": 1, "finished": 0, "outcomes": {"completed": 0, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 0}}
  ],
  "causes": {"failed": 2, "items": [
    {"rank": 1, "is_other": false, "origin": "agent_failure", "runs": 1, "share": 0.5},
    {"rank": 6, "is_other": true, "origin": null, "runs": 1, "share": 0.5}
  ]},
  "rolling": {
    "overall": [{"index": 0, "finished": 2, "failed": 1, "value": null, "reason": "insufficient_runs"}],
    "harnesses": [
      {"harness": "claude", "points": [{"index": 0, "finished": 5, "failed": 1, "value": 0.2, "reason": null}]},
      {"harness": "codex", "points": [{"index": 0, "finished": 0, "failed": 0, "value": null, "reason": "insufficient_runs"}]}
    ]
  },
  "breakdown": {"dimension": "repo", "rows": [
    {"rank": 1, "is_other": false, "key": "k", "label": "org/repo", "id": "33333333-3333-4333-8333-333333333333",
     "n": 6, "outcomes": {"completed": 5, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 1},
     "failure_rate": {"value": 0.166667, "reason": null}, "requeued_runs": 1},
    {"rank": 51, "is_other": true, "key": null, "label": null, "id": null,
     "n": 2, "outcomes": {"completed": 2, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 0},
     "failure_rate": {"value": null, "reason": "insufficient_runs"}, "requeued_runs": 0}
  ]},
  "needs_landing": 1
}`

func TestDecodeAnalyticsReliabilityDoc(t *testing.T) {
	raw := []byte(analyticsReliabilityDocLiteral)
	doc, err := store.DecodeAnalyticsReliabilityDoc(raw)
	if err != nil {
		t.Fatalf("populated literal: %v", err)
	}
	c := doc.Totals.Current
	if c.Finished != 10 || c.Outcomes.Failed != 2 || c.RequeuedRuns != 3 || c.FailureRate.Value == nil || *c.FailureRate.Value != 0.2 {
		t.Errorf("totals.current = %+v", c)
	}
	if doc.Totals.Previous == nil || doc.Totals.Previous.FailureRate.Reason == nil || *doc.Totals.Previous.FailureRate.Reason != store.AnalyticsReasonNoFinishedRuns {
		t.Errorf("totals.previous = %+v", doc.Totals.Previous)
	}
	if len(doc.Buckets) != 2 || doc.Buckets[0].Outcomes.Completed != 3 || doc.Buckets[1].Index != 1 {
		t.Errorf("buckets = %+v", doc.Buckets)
	}
	if doc.Causes.Failed != 2 || len(doc.Causes.Items) != 2 || doc.Causes.Items[0].Origin == nil || *doc.Causes.Items[0].Origin != "agent_failure" ||
		!doc.Causes.Items[1].IsOther || doc.Causes.Items[1].Origin != nil || doc.Causes.Items[1].Share != 0.5 {
		t.Errorf("causes = %+v", doc.Causes)
	}
	if len(doc.Rolling.Overall) != 1 || doc.Rolling.Overall[0].Reason == nil || *doc.Rolling.Overall[0].Reason != store.AnalyticsReasonInsufficientRuns ||
		len(doc.Rolling.Harnesses) != 2 || doc.Rolling.Harnesses[0].Harness != "claude" || doc.Rolling.Harnesses[0].Points[0].Finished != 5 {
		t.Errorf("rolling = %+v", doc.Rolling)
	}
	rows := doc.Breakdown.Rows
	if doc.Breakdown.Dimension != "repo" || len(rows) != 2 || rows[0].N != 6 || rows[0].Label == nil || *rows[0].Label != "org/repo" ||
		rows[0].ID == nil || !rows[1].IsOther || rows[1].Key != nil || rows[1].ID != nil || rows[1].FailureRate.Value != nil {
		t.Errorf("breakdown = %+v", doc.Breakdown)
	}
	if doc.NeedsLanding != 1 {
		t.Errorf("needs_landing = %d", doc.NeedsLanding)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["surprise"] = json.RawMessage(`1`)
	withExtra, _ := json.Marshal(m)
	if _, err := store.DecodeAnalyticsReliabilityDoc(withExtra); err == nil {
		t.Error("an unknown top-level key was accepted")
	}
	nested := strings.Replace(analyticsReliabilityDocLiteral, `"requeued_runs": 3`, `"requeued_runs": 3, "surprise": 1`, 1)
	if _, err := store.DecodeAnalyticsReliabilityDoc([]byte(nested)); err == nil {
		t.Error("an unknown nested key was accepted")
	}
	if _, err := store.DecodeAnalyticsReliabilityDoc(append(append([]byte{}, raw...), []byte(`{}`)...)); err == nil {
		t.Error("trailing data was accepted")
	}
	if _, err := store.DecodeAnalyticsReliabilityDoc([]byte(`{"totals":{"current":{"finished":"ten"}}}`)); err == nil {
		t.Error("a mistyped field was accepted")
	}
	if _, err := store.DecodeAnalyticsReliabilityDoc([]byte(`{"totals":`)); err == nil {
		t.Error("a truncated document was accepted")
	}
}
