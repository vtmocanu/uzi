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

// Issue #4730: tests of the Quality statements' text and the typed document, none of which
// need a database.

func qualitySQLBlocks(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("queries/analytics_quality.sql")
	if err != nil {
		t.Fatal(err)
	}
	file := string(raw)
	blocks := map[string]string{}
	parts := regexp.MustCompile(`(?m)^-- name: (\w+) :\w+$`).FindAllStringSubmatchIndex(file, -1)
	for i, m := range parts {
		end := len(file)
		if i+1 < len(parts) {
			end = parts[i+1][0]
		}
		blocks[file[m[2]:m[3]]] = file[m[0]:end]
	}
	return blocks
}

// TestAnalyticsQualityStatementsDoNotDrift: the owner and all-scope statements are kept
// textually identical except for the lines marked owner-only.
func TestAnalyticsQualityStatementsDoNotDrift(t *testing.T) {
	blocks := qualitySQLBlocks(t)
	owner, all := blocks["AnalyticsOwnerQuality"], blocks["AnalyticsAllQuality"]
	if owner == "" || all == "" {
		t.Fatal("missing Quality statement block")
	}
	norm := func(block string) string {
		var out []string
		for i, line := range strings.Split(block, "\n") {
			if i == 0 || strings.HasSuffix(line, "-- owner-only") {
				continue // the "-- name:" line, or a line only the owner statement has
			}
			out = append(out, strings.TrimRight(line, " "))
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	o, a := norm(owner), norm(all)
	if o != a {
		ol, al := strings.Split(o, "\n"), strings.Split(a, "\n")
		for i := 0; i < len(ol) && i < len(al); i++ {
			if ol[i] != al[i] {
				t.Fatalf("Quality statements drifted at normalised line %d:\n owner: %s\n all:   %s", i+1, ol[i], al[i])
			}
		}
		t.Fatalf("Quality statements drifted in length: %d vs %d lines", len(ol), len(al))
	}
	tagged := func(block string) (ownerOnly, other []string) {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "-- owner-only") {
				ownerOnly = append(ownerOnly, line)
			}
			if strings.Contains(line, "-- all-only") {
				other = append(other, line)
			}
		}
		return ownerOnly, other
	}
	// The tagged lines are exactly these, by content: a new owner-only predicate changes the
	// statement's meaning and must be added here on purpose.
	wantOwnerOnly := []string{
		"AND r.user_id = @user_id::uuid  -- owner-only",    // rework seed
		"AND w.user_id = @user_id::uuid  -- owner-only",    // merge witness
		"AND user_id = @user_id::uuid  -- owner-only",      // retained reworks
		"AND fc.user_id = @user_id::uuid  -- owner-only",   // shared-PR detection connection
		"AND r.user_id = @user_id::uuid  -- owner-only",    // CI-fix runs
		"AND lead.user_id = @user_id::uuid  -- owner-only", // cross-check lead
		"AND r.user_id = @user_id::uuid  -- owner-only",    // judge targets
	}
	gotOO, gotAO := tagged(owner)
	if !reflect.DeepEqual(gotOO, wantOwnerOnly) || len(gotAO) != 0 {
		t.Errorf("owner statement tagged lines:\n owner-only %q\nwant %q and no all-only", gotOO, wantOwnerOnly)
	}
	gotOO, gotAO = tagged(all)
	if len(gotOO) != 0 || len(gotAO) != 0 {
		t.Errorf("all statement has tagged lines: %q %q", gotOO, gotAO)
	}
	if strings.Contains(all, "@user_id") {
		t.Error("all-scope statement references @user_id")
	}
	// Stage selects the stored result field: the legacy code verdict is never read as a result.
	for name, b := range map[string]string{"owner": owner, "all": all} {
		for _, want := range []string{
			"CASE WHEN cc.stage = 'code' THEN cc.outcome ELSE cc.verdict END::text AS result",
			"(cc.stage = 'plan' AND cc.verdict = 'pending') OR (cc.stage = 'code' AND cc.outcome = 'pending')",
			"isfinite(b.decided_at) AND isfinite(b.created_at)",
			"j.skip_expr IS NOT TRUE",
			"rw_seed AS MATERIALIZED", "rework AS MATERIALIZED",
			"ORDER BY c.idx",
		} {
			if !strings.Contains(b, want) {
				t.Errorf("%s statement lacks %q", name, want)
			}
		}
		// The attempt population has no time predicate at all: malformed time evidence (an infinite or
		// far-future created_at) must reach the dated rule and be disclosed as undated, not be dropped
		// here. Only the decided_at window of cc_win bounds the dated series.
		pend := b[strings.Index(b, "cc_base AS MATERIALIZED"):strings.Index(b, "cc_dec AS MATERIALIZED")]
		if strings.Contains(pend, "@from") || strings.Contains(pend, "sqlc.arg('from')") || strings.Contains(pend, "@now") {
			t.Errorf("%s statement restricts cross-check attempts by time before classifying them", name)
		}
		for _, want := range []string{"lead.kind = ANY(@plan_lead_kinds::text[])", "lead.kind = ANY(@code_lead_kinds::text[])"} {
			if !strings.Contains(pend, want) {
				t.Errorf("%s statement cc_base lacks the stage-applicable lead kind filter %q", name, want)
			}
		}
	}
	// The generated constants (what actually runs) carry the same scope predicates.
	for _, want := range []string{`r\.user_id = \$\d+::uuid`, `w\.user_id = \$\d+::uuid`, `lead\.user_id = \$\d+::uuid`, `fc\.user_id = \$\d+::uuid`} {
		if !regexp.MustCompile(want).MatchString(store.AnalyticsOwnerQualitySQL) {
			t.Errorf("generated owner statement lacks %s", want)
		}
	}
	if loc := regexp.MustCompile(`user_id = \$\d+`).FindString(store.AnalyticsAllQualitySQL); loc != "" {
		t.Errorf("generated all-scope statement binds a user: %q", loc)
	}
}

const analyticsQualityDocLiteral = `{
  "rework": {
    "applicable_prs": 3,
    "groups": {"zero": 1, "one": 1, "two": 1, "three_plus": 0},
    "buckets": [{"index": 0, "prs": 3, "groups": {"zero": 1, "one": 1, "two": 1, "three_plus": 0}}],
    "unwitnessed_prs": 2, "unverified_links": 1, "shared_external_prs": 2, "shared_detection_unavailable": 1
  },
  "ci_fix": {
    "runs": 4, "finished": 3, "in_flight": 1,
    "outcomes": {"completed": 1, "cancelled": 1, "plan_rejected": 0, "base_diverged": 0, "failed": 1},
    "buckets": [{"index": 0, "runs": 4, "finished": 3, "in_flight": 1,
      "outcomes": {"completed": 1, "cancelled": 1, "plan_rejected": 0, "base_diverged": 0, "failed": 1}}]
  },
  "cross_check": {
    "plan": {
      "decided_in_window": 2,
      "stored_result": {"approve": 1, "revise": 1, "block": 0, "failed": 0},
      "classes": {"passed": 1, "raised_issues": 1, "didnt_finish": 0, "replaced": 0, "unclassified": 0},
      "checker_harness": {"claude": 1, "codex": 1, "unknown": 0},
      "undated": {"count": 1, "stored_result": {"approve": 1, "revise": 0, "block": 0, "failed": 0}},
      "buckets": [{"index": 0, "decided": 2,
        "stored_result": {"approve": 1, "revise": 1, "block": 0, "failed": 0},
        "classes": {"passed": 1, "raised_issues": 1, "didnt_finish": 0, "replaced": 0, "unclassified": 0}}]
    },
    "code": {
      "decided_in_window": 1,
      "stored_result": {"completed": 1, "failed": 0},
      "classes": {"passed": 0, "raised_issues": 0, "didnt_finish": 0, "replaced": 0, "unclassified": 1},
      "checker_harness": {"claude": 0, "codex": 0, "unknown": 1},
      "undated": {"count": 0, "stored_result": {"completed": 0, "failed": 0}},
      "buckets": [{"index": 0, "decided": 1,
        "stored_result": {"completed": 1, "failed": 0},
        "classes": {"passed": 0, "raised_issues": 0, "didnt_finish": 0, "replaced": 0, "unclassified": 1}}]
    },
    "pending": {"total": 2, "plan": 1, "code": 1}
  },
  "judge": {
    "candidates": {"total": 5, "completed": 4, "failed": 1},
    "eligible": {"total": 4, "unreviewed": 1, "unreviewed_skipped": 1, "failed": 1,
      "complete": {"total": 2, "ideal": 1, "ok": 0, "issues": 1}},
    "excluded": {"cancelled": 1, "in_flight": 2, "skip_origin": 1, "reviewed_excluded": 1},
    "buckets": [{"index": 0, "eligible": 4, "complete": 2}]
  }
}`

func TestDecodeAnalyticsQualityDoc(t *testing.T) {
	raw := []byte(analyticsQualityDocLiteral)
	doc, err := store.DecodeAnalyticsQualityDoc(raw)
	if err != nil {
		t.Fatalf("populated literal: %v", err)
	}
	if doc.Rework.ApplicablePRs != 3 || doc.Rework.Groups.Two != 1 || doc.Rework.UnverifiedLinks != 1 ||
		doc.Rework.SharedExternalPRs != 2 || doc.Rework.SharedDetectionUnavailable != 1 || len(doc.Rework.Buckets) != 1 {
		t.Errorf("rework = %+v", doc.Rework)
	}
	if doc.CIFix.Runs != 4 || doc.CIFix.InFlight != 1 || doc.CIFix.Outcomes.Failed != 1 || len(doc.CIFix.Buckets) != 1 {
		t.Errorf("ci_fix = %+v", doc.CIFix)
	}
	if doc.CrossCheck.Plan.StoredResult.Revise != 1 || doc.CrossCheck.Plan.Undated.Count != 1 ||
		doc.CrossCheck.Code.StoredResult.Completed != 1 || doc.CrossCheck.Code.Classes.Unclassified != 1 ||
		doc.CrossCheck.Pending != (store.AnalyticsQualityPending{Total: 2, Plan: 1, Code: 1}) {
		t.Errorf("cross_check = %+v", doc.CrossCheck)
	}
	if doc.Judge.Eligible.Complete.Issues != 1 || doc.Judge.Eligible.UnreviewedSkipped != 1 ||
		doc.Judge.Excluded.ReviewedExcluded != 1 || len(doc.Judge.Buckets) != 1 || doc.Judge.Buckets[0].Complete != 2 {
		t.Errorf("judge = %+v", doc.Judge)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["surprise"] = json.RawMessage(`1`)
	withExtra, _ := json.Marshal(m)
	if _, err := store.DecodeAnalyticsQualityDoc(withExtra); err == nil {
		t.Error("an unknown top-level key was accepted")
	}
	nested := strings.Replace(analyticsQualityDocLiteral, `"pending": {"total": 2,`, `"pending": {"surprise": 1, "total": 2,`, 1)
	if _, err := store.DecodeAnalyticsQualityDoc([]byte(nested)); err == nil {
		t.Error("an unknown nested key was accepted")
	}
	if _, err := store.DecodeAnalyticsQualityDoc(append(append([]byte{}, raw...), []byte(`{}`)...)); err == nil {
		t.Error("trailing data was accepted")
	}
	if _, err := store.DecodeAnalyticsQualityDoc([]byte(`{"rework":{"applicable_prs":"x"}}`)); err == nil {
		t.Error("a mistyped field was accepted")
	}
}
