package store_test

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: tests of the Capacity statements' text and the typed document, none of which
// need a database.

// capacityStatements are the six generated statement texts by name.
func capacityStatements() map[string]string {
	return map[string]string{
		"AnalyticsOwnerCapacity":        store.AnalyticsOwnerCapacitySQL,
		"AnalyticsAllCapacity":          store.AnalyticsAllCapacitySQL,
		"AnalyticsOwnerAnthropicGauges": store.AnalyticsOwnerAnthropicGaugesSQL,
		"AnalyticsAllAnthropicGauges":   store.AnalyticsAllAnthropicGaugesSQL,
		"AnalyticsOwnerCodexGauges":     store.AnalyticsOwnerCodexGaugesSQL,
		"AnalyticsAllCodexGauges":       store.AnalyticsAllCodexGaugesSQL,
	}
}

// capacitySQLBlocks returns each "-- name: X" block of queries/analytics_capacity.sql, keyed by
// name. The drift test reads the file, not the generated constants, because sqlc numbers the
// parameters per statement ($3 in one, $2 in the other).
func capacitySQLBlocks(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("queries/analytics_capacity.sql")
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

func TestAnalyticsCapacityStatementsDoNotReferenceWorkersOrClocks(t *testing.T) {
	for name, sql := range capacityStatements() {
		for _, c := range []struct{ what, pattern string }{
			{"workers", `(?i)\bworkers\b`},
			{"now()", `(?i)\bnow\s*\(`},
			{"clock_timestamp", `(?i)clock_timestamp`},
			{"generate_series", `(?i)generate_series`},
			{"date_trunc", `(?i)date_trunc`},
		} {
			if regexp.MustCompile(c.pattern).MatchString(sql) {
				t.Errorf("%s references %s", name, c.what)
			}
		}
	}
}

// TestAnalyticsCapacityStatementsDoNotDrift: the owner and all-scope historical statements are
// kept textually identical except for the lines tagged owner-only (ADR-1184 D1 keeps them
// separate statements; this keeps two large documents from diverging).
func TestAnalyticsCapacityStatementsDoNotDrift(t *testing.T) {
	blocks := capacitySQLBlocks(t)
	owner, all := blocks["AnalyticsOwnerCapacity"], blocks["AnalyticsAllCapacity"]
	if owner == "" || all == "" {
		t.Fatal("missing Capacity statement block")
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
	if o, a := norm(owner), norm(all); o != a {
		ol, al := strings.Split(o, "\n"), strings.Split(a, "\n")
		for i := 0; i < len(ol) && i < len(al); i++ {
			if ol[i] != al[i] {
				t.Fatalf("Capacity statements drifted at normalised line %d:\n owner: %s\n all:   %s", i+1, ol[i], al[i])
			}
		}
		t.Fatalf("Capacity statements drifted in length: %d vs %d lines", len(ol), len(al))
	}
	tagged := func(block string) []string {
		var out []string
		for _, line := range strings.Split(block, "\n") {
			if strings.Contains(line, "-- owner-only") {
				out = append(out, strings.TrimSpace(line))
			}
		}
		return out
	}
	// Two population branches and the usage-wait scan: exactly the user predicate, three times.
	got := tagged(owner)
	if len(got) != 3 {
		t.Fatalf("owner statement tagged lines = %q, want three user predicates", got)
	}
	for _, l := range got {
		if l != "AND r.user_id = @user_id::uuid  -- owner-only" {
			t.Errorf("owner-only line %q is not the user predicate", l)
		}
	}
	if got := tagged(all); len(got) != 0 {
		t.Errorf("all statement carries owner-only lines: %q", got)
	}
	if strings.Contains(all, "@user_id") {
		t.Error("all-scope statement filters on r.user_id")
	}
	// Every scan of runs carries the shared applicable predicate and literal status lists.
	for name, sql := range map[string]string{"owner": owner, "all": all} {
		if n := strings.Count(sql, "r.kind <> 'chat' AND NOT r.cross_check_lane"); n != 3 {
			t.Errorf("%s: ordinary-lane predicate appears %d times, want 3 (two population branches and the usage-wait scan)", name, n)
		}
		if n := strings.Count(sql, "r.kind = ANY(@kinds::text[])"); n != 3 {
			t.Errorf("%s: kinds predicate appears %d times, want 3", name, n)
		}
		for _, lit := range []string{
			"r.status IN ('completed', 'failed', 'cancelled')",
			"r.status NOT IN ('completed', 'failed', 'cancelled')",
		} {
			if n := strings.Count(sql, lit); n != 1 {
				t.Errorf("%s: %q appears %d times, want 1 (literal lists keep the partial indexes usable)", name, lit, n)
			}
		}
	}
}

// between returns the text of s from the first occurrence of from to the end of the first
// occurrence of to after it, or fails.
func between(t *testing.T, what, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("%s: %q not found", what, from)
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		t.Fatalf("%s: %q not found after %q", what, to, from)
	}
	return s[i : i+j+len(to)]
}

// trimLines trims the leading and trailing space of every line, so a block copied into a CTE at
// another indentation still compares equal on content.
func trimLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

func TestAnalyticsCapacityAnthropicGaugesCopyTheCanonicalReadingJoin(t *testing.T) {
	canon := store.ListRateLimitsForUserSQL
	join := between(t, "ListRateLimitsForUser", canon, "LEFT JOIN anthropic_rate_limits rl", "rl.enablement_rev = s.enablement_rev")
	pred := between(t, "ListRateLimitsForUser", canon, "s.kind = 'anthropic_token'", "s.disabled_at IS NULL")
	for _, name := range []string{"AnalyticsOwnerAnthropicGauges", "AnalyticsAllAnthropicGauges"} {
		sql := capacityStatements()[name]
		if !strings.Contains(sql, join) {
			t.Errorf("%s lacks the canonical reading join:\n%s", name, join)
		}
		if !strings.Contains(sql, pred) {
			t.Errorf("%s lacks the canonical enabled-token predicate %q", name, pred)
		}
	}
}

func TestAnalyticsCapacityCodexGaugesCopyTheCanonicalFence(t *testing.T) {
	for _, c := range []struct {
		name, canonical, existsFrom string
	}{
		{"AnalyticsOwnerCodexGauges", store.GetCodexAccountRateLimitsOwnerSQL, "AND EXISTS ("},
		{"AnalyticsAllCodexGauges", store.ListCodexAccountRateLimitsAdminSQL, "WHERE EXISTS ("},
	} {
		sql := capacityStatements()[c.name]
		// The reading LEFT JOIN with its enablement_sig fence and the alias lateral are byte for
		// byte the canonical text (same indentation: both sit in the outer query).
		fence := between(t, c.canonical, c.canonical, "LEFT JOIN codex_account_rate_limits rl", ") al ON true")
		if !strings.Contains(sql, fence) {
			t.Errorf("%s lacks the canonical reading fence and alias lateral", c.name)
		}
		// The enabled-alias EXISTS sits inside the identity CTE at another indentation, so it
		// is compared line by line without leading space.
		canonExists := between(t, c.canonical, c.canonical, c.existsFrom, "us.disabled_at IS NULL\n"+indentOf(c.canonical, c.existsFrom)+")")
		cte := sql[:strings.Index(sql, "SELECT\n")]
		if !strings.Contains(trimLines(cte), trimLines(canonExists)) {
			t.Errorf("%s identity CTE lacks the canonical enabled-alias EXISTS:\n%s", c.name, canonExists)
		}
	}
}

// indentOf returns the leading whitespace of the line of s that starts with marker (after its
// own leading whitespace).
func indentOf(s, marker string) string {
	i := strings.Index(s, marker)
	start := strings.LastIndex(s[:i], "\n") + 1
	return s[start:i]
}

func TestAnalyticsCapacityGaugesOrderByUniqueKeyAndCapAtTwoHundredOne(t *testing.T) {
	for _, c := range []struct{ name, innerTail, outerTail string }{
		{"AnalyticsOwnerAnthropicGauges", "s.id ASC", "s.id ASC"},
		{"AnalyticsAllAnthropicGauges", "s.id ASC", "s.id ASC"},
		{"AnalyticsOwnerCodexGauges", "a.id", "a.id"},
		{"AnalyticsAllCodexGauges", "a.id ASC", "a.id ASC"},
	} {
		sql := capacityStatements()[c.name]
		if n := strings.Count(sql, "LIMIT 201"); n != 1 {
			t.Errorf("%s: LIMIT 201 appears %d times, want once, in the identity CTE", c.name, n)
		}
		cte := sql[:strings.Index(sql, "\n)\n")]
		if !strings.Contains(cte, "LIMIT 201") || !strings.Contains(cte, "ORDER BY") {
			t.Errorf("%s: identity CTE lacks its ORDER BY or LIMIT 201:\n%s", c.name, cte)
		}
		inner := regexp.MustCompile(`ORDER BY ([^\n]+)\n\s*LIMIT 201`).FindStringSubmatch(cte)
		if inner == nil || !strings.HasSuffix(inner[1], c.innerTail) {
			t.Errorf("%s: CTE ORDER BY %v does not end in the unique key %q", c.name, inner, c.innerTail)
		}
		outer := sql[strings.Index(sql, "\n)\n"):]
		outerOrder := regexp.MustCompile(`ORDER BY ([^\n]+)\n?$`).FindStringSubmatch(outer)
		if outerOrder == nil || !strings.HasSuffix(strings.TrimSpace(outerOrder[1]), c.outerTail) {
			t.Errorf("%s: outer ORDER BY %v does not end in the unique key %q", c.name, outerOrder, c.outerTail)
		}
	}
}

func TestAnalyticsCapacityGaugesSelectNoSecretOrErrorText(t *testing.T) {
	for name, sql := range capacityStatements() {
		for _, banned := range []string{"ciphertext", "attempt_error", "sealed", "provider_user_id", "workspace_account_id"} {
			if strings.Contains(sql, banned) {
				t.Errorf("%s mentions %q", name, banned)
			}
		}
	}
}

const capacityDocValid = `{"generated_at":"2026-11-25T12:00:00.000000Z",
 "population":{"candidates":3,"with_interval":2,"without_start":1,"invalid_interval":0,"end_from_status_since":1,"ongoing":1,"cross_check_kind_included":0},
 "occupancy":{"window":{"run_seconds":7200,"duration_seconds":86400,"average_concurrent_runs":0.083333,"reason":null},
   "buckets":[{"index":0,"run_seconds":0,"duration_seconds":0,"average_concurrent_runs":null,"reason":"zero_duration_bucket"}]},
 "usage_wait":{"window":{"index":0,"count":0,"n":0,"share":null,"reason":"no_applicable_runs"},"buckets":[]}}`

func TestDecodeAnalyticsCapacityDoc(t *testing.T) {
	doc, err := store.DecodeAnalyticsCapacityDoc([]byte(capacityDocValid))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Population.Candidates != 3 || doc.Population.EndFromStatusSince != 1 {
		t.Errorf("population %+v", doc.Population)
	}
	if got := doc.Occupancy.Window.AverageConcurrentRuns; got == nil || *got != 0.083333 {
		t.Errorf("window average = %v", got)
	}
	b := doc.Occupancy.Buckets[0]
	if b.AverageConcurrentRuns != nil || b.Reason == nil || *b.Reason != store.AnalyticsReasonZeroDurationBucket {
		t.Errorf("zero-duration bucket %+v", b)
	}
	if w := doc.UsageWait.Window; w.Share != nil || w.Reason == nil || *w.Reason != store.AnalyticsReasonNoApplicableRuns {
		t.Errorf("empty usage wait %+v", w)
	}
	if doc.UsageWait.Buckets == nil || len(doc.UsageWait.Buckets) != 0 {
		t.Errorf("zero buckets decoded as %#v, want an empty non-nil slice", doc.UsageWait.Buckets)
	}
}

func TestDecodeAnalyticsCapacityDocIsStrict(t *testing.T) {
	for _, c := range []struct{ name, doc string }{
		{"unknown top-level key", strings.Replace(capacityDocValid, `{"generated_at"`, `{"extra":1,"generated_at"`, 1)},
		{"unknown nested key", strings.Replace(capacityDocValid, `"ongoing":1`, `"ongoing":1,"extra":2`, 1)},
		{"unknown bucket key", strings.Replace(capacityDocValid, `"run_seconds":0,`, `"run_seconds":0,"extra":2,`, 1)},
		{"trailing data", capacityDocValid + ` {}`},
		{"wrong type", strings.Replace(capacityDocValid, `"candidates":3`, `"candidates":"3"`, 1)},
		{"not json", `nope`},
	} {
		if _, err := store.DecodeAnalyticsCapacityDoc([]byte(c.doc)); err == nil {
			t.Errorf("%s accepted", c.name)
		}
	}
}

func TestAnalyticsCapacityReasonSetIsClosed(t *testing.T) {
	got := []string{
		store.AnalyticsReasonZeroDurationBucket, store.AnalyticsReasonZeroDurationRange,
		store.AnalyticsReasonNoApplicableRuns, store.AnalyticsReasonNotTracked,
		store.AnalyticsReasonGaugeUnavailable, store.AnalyticsReasonVaultStatusUnavailable,
		store.AnalyticsReasonNotSelected, store.AnalyticsReasonSharedQuotaNotAttributable,
	}
	want := []string{
		"zero_duration_bucket", "zero_duration_range", "no_applicable_runs", "not_tracked",
		"gauge_unavailable", "vault_status_unavailable", "not_selected", "shared_quota_not_attributable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %q, want %q", got, want)
	}
}
