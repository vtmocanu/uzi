package store_test

import (
	"encoding/json"
	"os"
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
	for _, c := range []struct{ what, pattern string }{
		{"now()", `(?i)\bnow\s*\(`},
		{"generate_series", `(?i)generate_series`},
		{"date_trunc", `(?i)date_trunc`},
		{"run_usage_messages", `(?i)run_usage_messages`},
		{"run_usage_estimated", `(?i)run_usage_estimated`},
	} {
		if loc := regexp.MustCompile(c.pattern).FindStringIndex(file); loc != nil {
			line := strings.Count(file[:loc[0]], "\n") + 1
			t.Errorf("analytics.sql contains %s at line %d", c.what, line)
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
	if store.AnalyticsOwnerSpendSQL == "" || store.AnalyticsAllSpendSQL == "" {
		t.Error("generated statement constants are empty")
	}
}

func TestDecodeAnalyticsSpendDoc(t *testing.T) {
	// A zero document survives a marshal/decode round trip, so the decoder accepts exactly
	// the shape the types describe.
	raw, err := json.Marshal(store.AnalyticsSpendDoc{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecodeAnalyticsSpendDoc(raw); err != nil {
		t.Fatalf("round trip: %v", err)
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
