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

// Issue #4684 M3: tests of the Spend statements' text and the typed document, none of which
// need a database.

// analyticsSQLBlocks returns each "-- name: X" block of queries/analytics.sql, keyed by name.
func analyticsSQLBlocks(t *testing.T) (file string, blocks map[string]string) {
	t.Helper()
	raw, err := os.ReadFile("queries/analytics.sql")
	if err != nil {
		t.Fatal(err)
	}
	file = string(raw)
	blocks = map[string]string{}
	parts := regexp.MustCompile(`(?m)^-- name: (\w+) :\w+$`).FindAllStringSubmatchIndex(file, -1)
	for i, m := range parts {
		end := len(file)
		if i+1 < len(parts) {
			end = parts[i+1][0]
		}
		blocks[file[m[2]:m[3]]] = file[m[0]:end]
	}
	return file, blocks
}

// TestAnalyticsSQLForbiddenConstructs: the file is the calendar's consumer, never its
// author. Go computes every boundary; SQL only tests half-open membership against the
// parameters. The legacy usage message tables are not an input either.
func TestAnalyticsSQLForbiddenConstructs(t *testing.T) {
	file, _ := analyticsSQLBlocks(t)
	quality, err := os.ReadFile("queries/analytics_quality.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"analytics.sql": file, "analytics_quality.sql": string(quality)} {
		for _, c := range []struct{ what, pattern string }{
			{"now()", `(?i)\bnow\s*\(`},
			{"generate_series", `(?i)generate_series`},
			{"date_trunc", `(?i)date_trunc`},
			{"run_usage_messages", `(?i)run_usage_messages`},
			{"run_usage_estimated", `(?i)run_usage_estimated`},
		} {
			if loc := regexp.MustCompile(c.pattern).FindStringIndex(text); loc != nil {
				line := strings.Count(text[:loc[0]], "\n") + 1
				t.Errorf("%s contains %s at line %d", name, c.what, line)
			}
		}
	}
}

// TestAnalyticsSpendStatementsDoNotDrift: the owner and all-scope statements are kept
// textually identical except for the lines marked owner-only / all-only (ADR-1184 D1 keeps
// them separate statements; this keeps two large documents from diverging).
func TestAnalyticsSpendStatementsDoNotDrift(t *testing.T) {
	_, blocks := analyticsSQLBlocks(t)
	norm := func(block, drop string) string {
		var out []string
		for i, line := range strings.Split(block, "\n") {
			if i == 0 || strings.HasSuffix(line, drop) {
				continue // the "-- name:" line, or a line only the other statement has
			}
			out = append(out, strings.TrimRight(line, " "))
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	owner, all := blocks["AnalyticsOwnerSpend"], blocks["AnalyticsAllSpend"]
	if owner == "" || all == "" {
		t.Fatal("missing Spend statement block")
	}
	o, a := norm(owner, "-- owner-only"), norm(all, "-- all-only")
	if o != a {
		ol, al := strings.Split(o, "\n"), strings.Split(a, "\n")
		for i := 0; i < len(ol) && i < len(al); i++ {
			if ol[i] != al[i] {
				t.Fatalf("Spend statements drifted at normalised line %d:\n owner: %s\n all:   %s", i+1, ol[i], al[i])
			}
		}
		t.Fatalf("Spend statements drifted in length: %d vs %d lines", len(ol), len(al))
	}
	// The tagged lines are exactly these, by content: a new owner-only or all-only predicate
	// changes the statement's meaning and must be added here on purpose.
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
		"(@reveal_repos::boolean IS TRUE OR fc.id IS NOT NULL) AS repo_shown,  -- owner-only",
		"LEFT JOIN forge_connections fc ON fc.id = rpo.connection_id AND fc.user_id = @user_id::uuid  -- owner-only",
		"AND r.user_id = @user_id::uuid  -- owner-only",
		"AND w.user_id = @user_id::uuid  -- owner-only",
	}
	wantAllOnly := []string{"true AS repo_shown,  -- all-only"}
	gotOO, gotAO := tagged(owner)
	if !reflect.DeepEqual(gotOO, wantOwnerOnly) || len(gotAO) != 0 {
		t.Errorf("owner statement tagged lines:\n owner-only %q\n all-only %q\nwant owner-only %q and no all-only", gotOO, gotAO, wantOwnerOnly)
	}
	gotOO, gotAO = tagged(all)
	if !reflect.DeepEqual(gotAO, wantAllOnly) || len(gotOO) != 0 {
		t.Errorf("all statement tagged lines:\n all-only %q\n owner-only %q\nwant all-only %q and no owner-only", gotAO, gotOO, wantAllOnly)
	}
	// The marked lines are exactly the scope predicates.
	for _, want := range []string{"r.user_id = @user_id::uuid", "w.user_id = @user_id::uuid"} {
		if !strings.Contains(owner, want) {
			t.Errorf("owner statement lacks %q", want)
		}
		if strings.Contains(all, "@user_id") {
			t.Error("all-scope statement references @user_id")
		}
	}
	if strings.Contains(all, "@reveal_repos") {
		t.Error("all-scope statement has a reveal flag: it always reveals")
	}
	// Fail closed: a false or NULL flag masks, so the gate is IS TRUE, never IS NOT FALSE.
	if !strings.Contains(owner, "@reveal_repos::boolean IS TRUE OR") || strings.Contains(owner, "IS NOT FALSE") {
		t.Error("owner statement's repo visibility gate is not fail-closed")
	}
	// The fold is correlated per run and fenced; shape (b) is not in use.
	for name, b := range map[string]string{"owner": owner, "all": all} {
		if !strings.Contains(b, "WHERE t.run_id = c.id\n        OFFSET 0") {
			t.Errorf("%s statement lost the per-run lateral fence", name)
		}
		for _, want := range []string{"model_fold AS MATERIALIZED", "cohort AS MATERIALIZED", "run_fold AS MATERIALIZED"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s statement lacks %q", name, want)
			}
		}
	}
	// The generated constants (what actually runs) carry the same scope predicates.
	if store.AnalyticsOwnerSpendSQL == "" || store.AnalyticsAllSpendSQL == "" {
		t.Error("generated statement constants are empty")
	}
	for _, want := range []string{`r\.user_id = \$\d+::uuid`, `w\.user_id = \$\d+::uuid`, `\$\d+::boolean IS TRUE OR`} {
		if !regexp.MustCompile(want).MatchString(store.AnalyticsOwnerSpendSQL) {
			t.Errorf("generated owner statement lacks %s", want)
		}
	}
	if loc := regexp.MustCompile(`user_id = \$\d+`).FindString(store.AnalyticsAllSpendSQL); loc != "" {
		t.Errorf("generated all-scope statement binds a user: %q", loc)
	}
}

// analyticsSpendDocLiteral is a populated Spend document written by hand, so the decoder is
// checked against the wire shape and not against the Go types it is also built from.
const analyticsSpendDocLiteral = `{
  "generated_at": "2026-11-25T12:00:00.000000Z",
  "untracked_chat_runs": 3,
  "totals": {
    "current": {
      "cost": "12.345678", "input_tokens": 100, "cache_read_tokens": 50, "cache_creation_tokens": 20,
      "output_tokens": 70, "runs": 4,
      "disclosure": {"metered": 3, "subscription": 1, "unreported": 0, "missing": 0, "incomplete": true},
      "coverage": {"metered": 3, "applicable": 4, "value": 0.75, "reason": null}
    }
  },
  "buckets": [
    {"index": 0, "cost": "1.000000", "input_tokens": 1, "cache_read_tokens": 0, "cache_creation_tokens": 0,
     "output_tokens": 2, "runs": 1,
     "disclosure": {"metered": 1, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false},
     "coverage": {"metered": 1, "applicable": 1, "value": 1, "reason": null}},
    {"index": 1, "cost": "2.500000", "input_tokens": 5, "cache_read_tokens": 6, "cache_creation_tokens": 7,
     "output_tokens": 8, "runs": 2,
     "disclosure": {"metered": 2, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false},
     "coverage": {"metered": 2, "applicable": 2, "value": 1, "reason": null}}
  ],
  "split": {
    "dimension": "usage_model", "non_additive": true,
    "groups": [
      {"rank": 1, "is_other": false, "key": "claude-opus", "label": null, "id": null,
       "totals": {"cost": "9.000000", "input_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0,
         "output_tokens": 0, "runs": 2,
         "disclosure": {"metered": 2, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false},
         "coverage": {"metered": 2, "applicable": 2, "value": 1, "reason": null}},
       "buckets": [{"index": 1, "cost": "4.250000", "input_tokens": 0, "cache_read_tokens": 0,
         "cache_creation_tokens": 0, "output_tokens": 0, "runs": 1,
         "disclosure": {"metered": 1, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false},
         "coverage": {"metered": 1, "applicable": 1, "value": 1, "reason": null}}]}
    ]
  },
  "cost_per_completed_run": {
    "current": {"numerator": "10.000000", "denominator": 2, "value": "5.000000", "reason": null,
      "disclosure": {"metered": 2, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false}},
    "buckets": [], "rolling": []
  },
  "cost_per_merged_pr": {
    "current": {"numerator": "0.000000", "denominator": 0, "value": null, "reason": "no_merged_prs",
      "disclosure": {"metered": 0, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false}},
    "buckets": [], "rolling": []
  },
  "cache_read": {"cache_read_tokens": 50, "input_tokens": 100, "cache_creation_tokens": 20, "denominator": 170,
    "value": 0.294118, "reason": null,
    "disclosure": {"metered": 3, "subscription": 1, "unreported": 0, "missing": 0, "incomplete": true}},
  "top_runs": [
    {"run_id": "11111111-1111-4111-8111-111111111111", "kind": "delivery", "harness": "claude",
     "run_status": "completed", "usage_status": "metered", "primary_model": "claude-opus",
     "created_at": "2026-11-24T10:00:00.000000Z", "cost": "7.654321", "input_tokens": 1, "cache_read_tokens": 2,
     "cache_creation_tokens": 3, "output_tokens": 4, "repo_key": "no_repo", "repo_id": null, "repo_path": null,
     "user_id": "22222222-2222-4222-8222-222222222222", "user_email": "a@example.test"}
  ],
  "breakdown": {"dimension": "repo", "rows": [
    {"rank": 1, "is_other": false, "key": "k", "label": "org/repo", "id": "33333333-3333-4333-8333-333333333333",
     "cost": "3.000000", "input_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0, "output_tokens": 0,
     "runs": 2,
     "disclosure": {"metered": 2, "subscription": 0, "unreported": 0, "missing": 0, "incomplete": false},
     "coverage": {"metered": 2, "applicable": 2, "value": 1, "reason": null},
     "finished": 2, "outcomes": {"completed": 1, "cancelled": 0, "plan_rejected": 0, "base_diverged": 0, "failed": 1},
     "failure_rate": {"value": 0.5, "reason": null}, "completion_ratio": {"value": 0.5, "reason": null},
     "cost_share": {"value": null, "reason": "not_applicable"},
     "elapsed_p50_seconds": {"value": 60, "reason": null}, "elapsed_p90_seconds": {"value": 120, "reason": null},
     "merged_prs": 1}
  ]}
}`

func TestDecodeAnalyticsSpendDoc(t *testing.T) {
	raw := []byte(analyticsSpendDocLiteral)
	doc, err := store.DecodeAnalyticsSpendDoc(raw)
	if err != nil {
		t.Fatalf("populated literal: %v", err)
	}
	if doc.UntrackedChatRuns != 3 || doc.Totals.Current.Cost != "12.345678" || doc.Totals.Previous != nil {
		t.Errorf("totals: chat %d cost %q previous %v", doc.UntrackedChatRuns, doc.Totals.Current.Cost, doc.Totals.Previous)
	}
	if len(doc.Buckets) != 2 || doc.Buckets[1].Index != 1 || doc.Buckets[1].Cost != "2.500000" || doc.Buckets[1].Runs != 2 {
		t.Errorf("buckets[1] = %+v", doc.Buckets)
	}
	if len(doc.Split.Groups) != 1 || !doc.Split.NonAdditive {
		t.Fatalf("split = %+v", doc.Split)
	}
	g := doc.Split.Groups[0]
	if g.Key == nil || *g.Key != "claude-opus" || g.Label != nil || g.Totals.Cost != "9.000000" ||
		len(g.Buckets) != 1 || g.Buckets[0].Index != 1 || g.Buckets[0].Cost != "4.250000" {
		t.Errorf("split group = %+v", g)
	}
	if v := doc.CostPerCompletedRun.Current.Value; v == nil || *v != "5.000000" {
		t.Errorf("cost_per_completed_run value = %v", v)
	}
	if r := doc.CostPerMergedPR.Current.Reason; r == nil || *r != store.AnalyticsReasonNoMergedPRs {
		t.Errorf("cost_per_merged_pr reason = %v", r)
	}
	if len(doc.TopRuns) != 1 || doc.TopRuns[0].Cost != "7.654321" || doc.TopRuns[0].UserEmail != "a@example.test" {
		t.Errorf("top_runs = %+v", doc.TopRuns)
	}
	if len(doc.Breakdown.Rows) != 1 || doc.Breakdown.Rows[0].Outcomes.Failed != 1 || doc.Breakdown.Rows[0].MergedPRs != 1 ||
		doc.Breakdown.Rows[0].ElapsedP90Seconds.Value == nil || *doc.Breakdown.Rows[0].ElapsedP90Seconds.Value != 120 {
		t.Errorf("breakdown = %+v", doc.Breakdown)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["surprise"] = json.RawMessage(`1`)
	withExtra, _ := json.Marshal(m)
	if _, err := store.DecodeAnalyticsSpendDoc(withExtra); err == nil {
		t.Error("an unknown key was accepted")
	}
	if _, err := store.DecodeAnalyticsSpendDoc(append(append([]byte{}, raw...), []byte(`{}`)...)); err == nil {
		t.Error("trailing data was accepted")
	}
	if _, err := store.DecodeAnalyticsSpendDoc([]byte(`{"totals":{"current":{"cost":1}}}`)); err == nil {
		t.Error("a mistyped field was accepted")
	}
}
