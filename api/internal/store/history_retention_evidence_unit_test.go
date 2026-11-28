//go:build historyretentionevidence

package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// No-database tests of the evidence harness's label logic. None reads the environment or connects.

func heAllTables() []store.HistoryRetentionTable { return store.HistoryRetentionTables() }

// heCoverageDefaults is a run that covers the whole fixture.
func heCoverageDefaults() heCoverageIn {
	return heCoverageIn{tables: heAllTables(), cands: heCands, rows: 1_000_000, prArrivals: heDefaultPRArrivals,
		codex: "typical", smokeSeconds: 300, smokeIdx: map[store.HistoryRetentionTable]string{
			store.HistoryRetentionPRHistoryEvents: "btree", store.HistoryRetentionAnthropicUsage: "btree",
			store.HistoryRetentionCodexUsage: "btree", store.HistoryRetentionWorkerCapacitySample: "btree"}}
}

func TestHistoryRetentionEvidenceParseListDedupe(t *testing.T) {
	got, err := heParseList("btree, none,btree,none", heCands)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "btree,none" {
		t.Fatalf("heParseList = %v, want [btree none] (deduplicated, first-seen order)", got)
	}
	all, err := heParseList("", heCands)
	if err != nil || strings.Join(all, ",") != strings.Join(heCands, ",") {
		t.Fatalf("empty selector = %v, %v; want every candidate", all, err)
	}
	for _, bad := range []string{"btree,gist", "none,,btree", "BTREE"} {
		if _, err := heParseList(bad, heCands); err == nil {
			t.Fatalf("heParseList(%q) accepted, want an unknown-name error", bad)
		}
	}
	if _, _, err := heParseSelection("pr,mystery", ""); err == nil {
		t.Fatal("unknown table accepted")
	}
	if _, _, err := heParseSelection("", "btree,bogus"); err == nil {
		t.Fatal("unknown candidate accepted")
	}
}

// Four names that pass the old length check but cover one or two distinct values are not qualifying.
func TestHistoryRetentionEvidenceRepeatedSelectorsAreCoverageGaps(t *testing.T) {
	for _, tc := range []struct{ tables, cands string }{
		{"pr,pr,pr,pr", ""},
		{"", "none,none,none,none"},
		{"", "btree,btree,brin_on,brin_off"},
		{"", "btree,btree,brin_off,brin_on"},
	} {
		tables, cands, err := heParseSelection(tc.tables, tc.cands)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		in := heCoverageDefaults()
		in.tables, in.cands = tables, cands
		reasons := heCoverageReasons(in, false)
		covered := len(tables) == 4 && len(cands) == 4
		if covered != (len(reasons) == 0) {
			t.Fatalf("%+v: tables=%v cands=%v reasons=%v", tc, tables, cands, reasons)
		}
	}
	// btree,btree,brin_on,brin_off has the right length and still misses none.
	_, cands, _ := heParseSelection("", "btree,btree,brin_on,brin_off")
	in := heCoverageDefaults()
	in.cands = cands
	if len(heCoverageReasons(in, false)) == 0 {
		t.Fatal("a candidate list missing none is qualifying")
	}
	if r := heCoverageReasons(heCoverageDefaults(), false); len(r) != 0 {
		t.Fatalf("the full fixture has coverage reasons %v", r)
	}
}

func TestHistoryRetentionEvidenceCoverageReasons(t *testing.T) {
	mut := func(f func(*heCoverageIn)) heCoverageIn { in := heCoverageDefaults(); f(&in); return in }
	for name, tc := range map[string]struct {
		in    heCoverageIn
		smoke bool
	}{
		"rows":     {mut(func(i *heCoverageIn) { i.rows = 999_999 }), false},
		"no av":    {mut(func(i *heCoverageIn) { i.noAV = true }), false},
		"arrivals": {mut(func(i *heCoverageIn) { i.prArrivals = 7 }), false},
		"codex":    {mut(func(i *heCoverageIn) { i.codex = "max" }), false},
		"tables":   {mut(func(i *heCoverageIn) { i.tables = i.tables[:3] }), false},
		"smoke s":  {mut(func(i *heCoverageIn) { i.smokeSeconds = 299 }), true},
		"smoke ix": {mut(func(i *heCoverageIn) { i.smokeIdx[store.HistoryRetentionCodexUsage] = "none" }), true},
		"smoke ro": {mut(func(i *heCoverageIn) { i.rows = 10 }), true},
	} {
		if r := heCoverageReasons(tc.in, tc.smoke); len(r) == 0 {
			t.Fatalf("%s: no coverage reason", name)
		}
	}
	// The selector, autovacuum and PR-arrival settings do not apply to the smoke.
	in := mut(func(i *heCoverageIn) { i.noAV, i.prArrivals, i.cands = true, 9, []string{"none"} })
	if r := heCoverageReasons(in, true); len(r) != 0 {
		t.Fatalf("smoke coverage reasons %v from compressed-only settings", r)
	}
}

// The 300 s smoke minimum stays a coverage reason.
func TestHistoryRetentionEvidenceShortSmokeIsDiagnostic(t *testing.T) {
	in := heCoverageDefaults()
	if r := heCoverageReasons(in, true); len(r) != 0 {
		t.Fatalf("a 300 s btree smoke has coverage reasons %v", r)
	}
	in.smokeSeconds = 120
	r := heCoverageReasons(in, true)
	if len(r) != 1 || !strings.Contains(r[0], "UZI_EVIDENCE_SMOKE_SECONDS=120") {
		t.Fatalf("short smoke reasons = %v", r)
	}
}

func TestHistoryRetentionEvidenceSmokeIndexDefaultIsBtree(t *testing.T) {
	idx, err := heParseSmokeIndexes("")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range heAllTables() {
		if idx[table] != "btree" {
			t.Fatalf("%s smoke index defaults to %q, want the production btree", table, idx[table])
		}
	}
	in := heCoverageDefaults()
	in.smokeIdx = idx
	if r := heCoverageReasons(in, true); len(r) != 0 {
		t.Fatalf("a default smoke has coverage reasons %v", r)
	}
	idx, err = heParseSmokeIndexes("codex=brin_on, pr=none")
	if err != nil {
		t.Fatal(err)
	}
	if idx[store.HistoryRetentionCodexUsage] != "brin_on" || idx[store.HistoryRetentionPRHistoryEvents] != "none" || idx[store.HistoryRetentionAnthropicUsage] != "btree" {
		t.Fatalf("parsed smoke indexes = %v", idx)
	}
	in.smokeIdx = idx
	if r := heCoverageReasons(in, true); len(r) != 2 {
		t.Fatalf("an otherwise qualifying smoke with two non-btree indexes has reasons %v, want 2", r)
	}
	for _, bad := range []string{"pr", "pr=", "=btree", "mystery=btree", "pr=gist", "pr=btree,pr=none", "pr=btree,", "pr=btree=x"} {
		if _, err := heParseSmokeIndexes(bad); err == nil {
			t.Fatalf("heParseSmokeIndexes(%q) accepted", bad)
		}
	}
}

func TestHistoryRetentionEvidenceTableByShort(t *testing.T) {
	for _, tb := range heAllTables() {
		got, err := heTableByShort(heShort(tb))
		if err != nil || got != tb {
			t.Fatalf("heTableByShort(%q) = %v, %v", heShort(tb), got, err)
		}
	}
	if _, err := heTableByShort(""); err == nil {
		t.Fatal("empty table accepted")
	}
}

// UZI_EVIDENCE_CANDS and UZI_EVIDENCE_TABLES are read in exactly one function, heParseSelectors.
func TestHistoryRetentionEvidenceSelectorsReadOnlyByParser(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "history_retention_evidence_livedb_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
				if v, err := strconv.Unquote(l.Value); err == nil && (v == "UZI_EVIDENCE_CANDS" || v == "UZI_EVIDENCE_TABLES") {
					found[v] = append(found[v], fd.Name.Name)
				}
			}
			return true
		})
	}
	for _, name := range []string{"UZI_EVIDENCE_CANDS", "UZI_EVIDENCE_TABLES"} {
		if got := found[name]; len(got) != 1 || got[0] != "heParseSelectors" {
			t.Fatalf("%s is referenced in %v, want only heParseSelectors", name, got)
		}
	}
}

func heTestSpec(table store.HistoryRetentionTable, n, arrivals int) heSpec {
	return heSpec{table: table, n: n, arrivals: arrivals, start: heBase}
}

func TestHistoryRetentionEvidenceRequiredTicks(t *testing.T) {
	pr := heTestSpec(store.HistoryRetentionPRHistoryEvents, 1_000_000, 6)
	if got, err := heRequiredTicks(pr, 0.05, "churn", 6); err != nil || got != 158_334 {
		t.Fatalf("default PR churn = %d, %v; want 158334", got, err)
	}
	codex := heTestSpec(store.HistoryRetentionCodexUsage, 1_000_000, heSampleArr)
	if got, err := heRequiredTicks(codex, 0.90, "backlog", heSampleArr); err != nil || got != 1260 {
		t.Fatalf("default codex backlog = %d, %v; want 1260", got, err)
	}
	if _, err := heRequiredTicks(codex, 0.90, "backlog", 999); err != nil {
		t.Fatalf("999 arrivals refused: %v", err)
	}
	for _, a := range []int{1000, 1500, 0, -1} {
		for _, mode := range []string{"backlog", "churn"} {
			if _, err := heRequiredTicks(codex, 0.90, mode, a); err == nil {
				t.Fatalf("%d arrivals accepted in %s mode", a, mode)
			}
		}
	}
	if _, err := heRequiredTicks(codex, 0.90, "sideways", 250); err == nil {
		t.Fatal("unknown mode accepted")
	}
	// More arrivals leave less of the batch for the backlog, so the run needs more ticks.
	lo, _ := heRequiredTicks(codex, 0.90, "backlog", 100)
	hi, _ := heRequiredTicks(codex, 0.90, "backlog", 500)
	if lo >= hi {
		t.Fatalf("ticks at 100 arrivals = %d, at 500 = %d; want more at the higher rate", lo, hi)
	}
}

func TestHistoryRetentionEvidenceCaseCutoff(t *testing.T) {
	codex := heTestSpec(store.HistoryRetentionCodexUsage, 1_000_000, heSampleArr)
	cut, eligible := heCaseCutoff(codex, 0.0205)
	if eligible != 25_000 {
		t.Fatalf("codex 2.05%% eligible = %d, want 25000 (whole five-minute buckets, not 20500)", eligible)
	}
	if cut.Truncate(5*time.Minute) != cut || cut.Sub(codex.start) != 25*time.Minute {
		t.Fatalf("codex cutoff %v is not bucket-aligned 25 min after the start", cut)
	}
	if _, e := heCaseCutoff(codex, 0.90); e != 900_000 {
		t.Fatalf("codex 90%% eligible = %d, want 900000", e)
	}
	if cut, e := heCaseCutoff(codex, 0); e != 0 || !cut.Before(codex.start) {
		t.Fatalf("frac 0 = %v, %d; want a cutoff before every row and 0 eligible", cut, e)
	}
	pr := heTestSpec(store.HistoryRetentionPRHistoryEvents, 1_000_000, 6)
	cut, eligible = heCaseCutoff(pr, 0.0205)
	if !cut.Equal(pr.at(20_500)) {
		t.Fatalf("PR cutoff = %v, want the observation time of row 20500", cut)
	}
	// 20,500 rows below the cutoff, of which g = 2, 92, ... (1 in 90) are first_merge rows.
	if eligible != 20_272 {
		t.Fatalf("PR 2.05%% eligible = %d, want 20272 (first_merge rows excluded)", eligible)
	}
	if _, e := heCaseCutoff(pr, 0.90); e != 890_000 {
		t.Fatalf("PR 90%% eligible = %d, want 890000", e)
	}
}

func TestHistoryRetentionEvidenceEligibleWhere(t *testing.T) {
	if w := heEligibleWhere(store.HistoryRetentionPRHistoryEvents); w != "observed_at < $1 AND NOT first_merge" {
		t.Fatalf("pr predicate = %q", w)
	}
	for _, tb := range heAllTables()[1:] {
		if w := heEligibleWhere(tb); w != "bucket_start < $1" {
			t.Fatalf("%s predicate = %q", tb, w)
		}
	}
}

func TestHistoryRetentionEvidenceErrKind(t *testing.T) {
	for e, want := range map[string]string{"": heKindOK, "57014": heKindTimeout, "55P03": heKindRollback, "40001": heKindRollback,
		"skipped": heKindSkipped, "unknown: boom": heKindUnknown, "unknown: close: x": heKindUnknown} {
		if got := heErrKind(e); got != want {
			t.Fatalf("heErrKind(%q) = %q, want %q", e, got, want)
		}
	}
}

func TestHistoryRetentionEvidenceRunReasons(t *testing.T) {
	ok := heRunFacts{Required: 100, Confirmed: 100, Stop: heStopComplete}
	if r := heRunReasons(ok); len(r) != 0 {
		t.Fatalf("a clean run has reasons %v", r)
	}
	for name, f := range map[string]heRunFacts{
		"short":           {Required: 100, Confirmed: 99, Stop: heStopComplete},
		"wall cap":        {Required: 100, Confirmed: 130, Stop: heStopWallCap},
		"error stop":      {Required: 100, Confirmed: 100, Stop: heStopArrivalError},
		"empty stop":      {Required: 100, Confirmed: 100},
		"one unknown":     {Required: 100, Confirmed: 100, Stop: heStopComplete, Unknown: 1},
		"timeout":         {Required: 100, Confirmed: 100, Stop: heStopComplete, Timeouts: 1},
		"other rollback":  {Required: 100, Confirmed: 100, Stop: heStopComplete, Rollbacks: 1},
		"skipped":         {Required: 100, Confirmed: 100, Stop: heStopComplete, Skipped: 1},
		"residual":        {Required: 100, Confirmed: 100, Stop: heStopComplete, Residual: 1},
		"pass error":      {Required: 100, Confirmed: 100, Stop: heStopComplete, PassErrors: 1},
		"probe error":     {Required: 100, Confirmed: 100, Stop: heStopComplete, ProbeErrors: 1},
		"probe deleted":   {Required: 100, Confirmed: 100, Stop: heStopComplete, NoopDeleted: 1},
		"unknown at goal": {Required: 3, Confirmed: 3, Stop: heStopComplete, Unknown: 1},
	} {
		if r := heRunReasons(f); len(r) == 0 {
			t.Fatalf("%s: no run reason", name)
		}
	}
	if r := heRunReasons(heRunFacts{Required: 100, Confirmed: 130, Stop: heStopWallCap}); !strings.Contains(strings.Join(r, ";"), heStopWallCap) {
		t.Fatalf("wall-capped reasons %v do not name the stop", r)
	}
}
