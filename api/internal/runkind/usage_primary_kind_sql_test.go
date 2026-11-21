package runkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Issue #4209: Overview usage run counts measure primary work runs. The population is defined
// once, in the SQL function fn_usage_primary_kind. Issue #4577: the run OUTCOME queries use a
// separate predicate, fn_outcome_kind (every kind except chat). These tests pin both
// definitions against the run-kind registry and pin which queries call which function.

var usagePrimaryKinds = []string{Issue, Task, Prompt, Job, SelfImprove}

// usagePrimaryHelperKinds are the kinds deliberately excluded from the primary population.
var usagePrimaryHelperKinds = []string{Chat, Judge, CrossCheck, MRRework, CIFix}

// liveUsagePrimaryKinds reads the allowlist from the highest-numbered migration Up section
// that defines fn_usage_primary_kind.
func liveUsagePrimaryKinds(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defRe := regexp.MustCompile(`(?i)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+fn_usage_primary_kind\b`)
	numRe := regexp.MustCompile(`^(\d+)`)
	bestNum, bestUp := -1, ""
	for _, e := range entries {
		m := numRe.FindStringSubmatch(e.Name())
		if m == nil || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		raw, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: fixed migrations dir
		if err != nil {
			t.Fatal(err)
		}
		up := string(raw)
		if i := strings.Index(up, "-- +goose Down"); i >= 0 {
			up = up[:i]
		}
		if defRe.MatchString(up) && num > bestNum {
			bestNum, bestUp = num, up
		}
	}
	if bestNum < 0 {
		t.Fatal("no migration Up section defines fn_usage_primary_kind")
	}
	loc := defRe.FindStringIndex(bestUp)
	body := bestUp[loc[1]:]
	inRe := regexp.MustCompile(`(?is)\bIN\s*\(([^)]*)\)`)
	m := inRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("fn_usage_primary_kind body has no IN (...) allowlist")
	}
	var kinds []string
	for _, part := range strings.Split(m[1], ",") {
		kinds = append(kinds, strings.Trim(strings.TrimSpace(part), "'"))
	}
	sort.Strings(kinds)
	return kinds
}

func TestUsagePrimaryKindAllowlistCoversRegistry(t *testing.T) {
	got := liveUsagePrimaryKinds(t)
	want := append([]string(nil), usagePrimaryKinds...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fn_usage_primary_kind allowlist = %v, want %v", got, want)
	}
	classified := map[string]int{}
	for _, k := range usagePrimaryKinds {
		classified[k]++
	}
	for _, k := range usagePrimaryHelperKinds {
		classified[k]++
	}
	all := All()
	for _, k := range all {
		switch classified[k] {
		case 0:
			t.Errorf("run kind %q is not classified as primary or helper for usage counting; update fn_usage_primary_kind and this test", k)
		case 1:
		default:
			t.Errorf("run kind %q is classified as both primary and helper", k)
		}
	}
	if len(classified) != len(all) {
		t.Errorf("classified %d kinds, registry has %d", len(classified), len(all))
	}
}

func TestUsagePrimaryKindQuerySites(t *testing.T) {
	raw, err := os.ReadFile("../store/queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	comments := regexp.MustCompile(`--[^\n]*`)
	blocks := namedQueryBlocks(string(raw))
	// name -> {fn_usage_primary_kind calls, fn_outcome_kind calls}
	want := map[string][2]int{
		"SelfUsage": {1, 0}, "AdminUsageTotals": {1, 0}, "AdminUsagePerUser": {6, 0},
		"SelfRunOutcomes": {0, 5}, "AdminRunOutcomes": {0, 5}, "AdminRunOutcomesPerUser": {0, 5},
	}
	handList := regexp.MustCompile(`(?i)NOT\s+IN\s*\(\s*'chat'`)
	literalChat := regexp.MustCompile(`(?i)\bkind\s*<>\s*'chat'`)
	for name, n := range want {
		body, ok := blocks[name]
		if !ok {
			t.Errorf("query %s not found", name)
			continue
		}
		body = comments.ReplaceAllString(body, "")
		if got := strings.Count(body, "fn_usage_primary_kind("); got != n[0] {
			t.Errorf("%s calls fn_usage_primary_kind %d times, want %d", name, got, n[0])
		}
		if got := strings.Count(body, "fn_outcome_kind("); got != n[1] {
			t.Errorf("%s calls fn_outcome_kind %d times, want %d", name, got, n[1])
		}
		if n[1] > 0 {
			if handList.MatchString(body) {
				t.Errorf("%s still has a hand-written NOT IN ('chat', ...) kind list", name)
			}
			if literalChat.MatchString(body) {
				t.Errorf("%s has a literal kind <> 'chat' predicate; call fn_outcome_kind", name)
			}
		}
	}
}

// liveOutcomeKindBody reads the predicate from the highest-numbered migration Up section that
// defines fn_outcome_kind, whitespace-normalised.
func liveOutcomeKindBody(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defRe := regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+fn_outcome_kind\b.*?\$\$\s*(.*?)\s*\$\$`)
	numRe := regexp.MustCompile(`^(\d+)`)
	bestNum, bestBody := -1, ""
	for _, e := range entries {
		m := numRe.FindStringSubmatch(e.Name())
		if m == nil || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		raw, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: fixed migrations dir
		if err != nil {
			t.Fatal(err)
		}
		up := string(raw)
		if i := strings.Index(up, "-- +goose Down"); i >= 0 {
			up = up[:i]
		}
		if dm := defRe.FindStringSubmatch(up); dm != nil && num > bestNum {
			bestNum, bestBody = num, strings.Join(strings.Fields(dm[1]), " ")
		}
	}
	if bestNum < 0 {
		t.Fatal("no migration Up section defines fn_outcome_kind")
	}
	return bestBody
}

// Chat is the only kind outside the outcome population (#4577).
func TestOutcomeKindExcludesOnlyChat(t *testing.T) {
	if got, want := liveOutcomeKindBody(t), "SELECT run_kind <> 'chat'"; got != want {
		t.Fatalf("fn_outcome_kind body = %q, want %q", got, want)
	}
	found := false
	for _, k := range All() {
		if k == Chat {
			found = true
		}
	}
	if !found {
		t.Fatal("Chat is not in All(); the fn_outcome_kind exclusion no longer names a registry kind")
	}
}
