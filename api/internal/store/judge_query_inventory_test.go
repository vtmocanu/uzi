package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------------------
// THE JUDGE-FAMILY QUERY INVENTORY (PRD #98).
//
// WHAT THIS TEST CANNOT DO, stated here because a passing test is the most credible
// artifact there is and this one is easy to over-read. It cannot prove a query is WELL
// pinned. It proves only that someone has DECLARED where a query is pinned. What it
// catches is "nobody has thought about this query at all" — an absence, which is the
// thing attention is worst at — and nothing more.
//
// The evidence that a declaration can be green while the pin is worthless is this PRD's
// own history, and both instances were caught by FOLDING, never by a declaration:
// ListJudgeTriageRowsForRuns would have declared PINNED while no fold had ever been run
// against its test, and ListJudgeTriageRowsForUser would have declared PINNED while all
// four of its coordinate halves were individually inert — dropping any one of them left
// the entire live-DB suite green, measured four times. Both are now genuinely pinned.
// Neither was fixed by this mechanism, and neither would have been.
//
// (The suite tally from that measurement — 126 pass / 0 fail — is bound to `8c6be2b8`,
// where it ran, and is not repeated as a live figure: 128 by `c1fcdfce`, 129 by
// `31080a40`. Corroborated independently rather than taken on trust: the count of top-level
// Test*LiveDB functions across internal/store + internal/handler is 108 / 110 / 111 at
// those three SHAs, and the +2 / +1 deltas match. (…the two counts measure different
// things — `RUN` counts `=== RUN` lines, which include subtests, while this counts
// top-level functions — so only the DELTAS are comparable. Said explicitly because the
// point of recording the arithmetic was that the next reader can re-check the binding
// without a database, and 108 against a tally of 126 is an unexplained gap of 18 on a
// branch where a number that looked wrong has repeatedly been wrong.) A tally drifts
// exactly like a line number, so the mechanism is the claim and the number is only its
// receipt.)
//
// Everything above is past tense on purpose. The auditor's original wording made those two
// claims in the PRESENT — and both stopped being true within the day, inside the very
// sentence warning against claims outrunning what was executed. Do not reintroduce a
// present-tense claim about any query's pin quality here; it will be stale by the review
// wave. The durable half is "declared, not good" and "an absence, and nothing more".
//
// Concretely, in this file's own terms: nothing below executes a query, folds a predicate,
// or measures isolation, so a row naming a test that merely *touches* the query is as green
// as a row naming a test that reddens when the query is mutated. The mutation folds recorded
// in prds/98-judge-menu.md are what tell those apart; this is the index of where to look,
// not the evidence.
//
// WHAT IT DOES CATCH — the one thing an index can catch, and the reason it is worth having:
// a query that ARRIVES (or is renamed) with nobody having thought about coverage. That case
// currently produces silence, and silence is indistinguishable from "covered". Here it fails
// the build until a human writes a row, which forces the question at the moment it is
// cheapest to answer.
//
// UNPINNED IS A LEGAL, GREEN, PERMANENT STATE — that is a design constraint, not a
// concession. A mechanism that fails the build for an honestly-declared gap is a mechanism
// that gets deleted the first time someone is in a hurry, taking the arriving-query check
// with it. So an UNPINNED row costs only a written reason, and the value of this file is the
// list of them being visible in one place rather than being nowhere.
//
// SCOPE, stated so a reader does not over-read it: the judge family only — the five .sql
// files listed in judgeQueryFiles, 17 queries. Repo-wide is 276 queries across 28 files, and
// the majority of those have never had this question asked, so a repo-wide table would be
// mostly UNPINNED rows written by someone who had not investigated. That is a bigger, later
// piece of work; PRD #98's blast radius is what this MR can honestly declare.
//
// WHY DECLARED AND NOT INFERRED. A prototype that infers pinning by scanning Test*LiveDB
// function bodies for the query name was measured against this same tree, and it is wrong in
// BOTH directions on the 17 queries here — three distinct mechanisms, which is why the table
// below is hand-written:
//
//  1. FALSE NEGATIVE, cross-package indirection. ListOwnedRecommendationsForCoords and
//     UpsertDispositionsForResolvedCoords appear in NO test source at all. The handler's
//     LiveDB tests drive them through workersvc, so the query name never occurs in this
//     package's test source, and an inferring rule reports them as uncovered when they are
//     among the most heavily exercised queries in the family. That is the mechanism behind
//     the 48 repo-wide queries the prototype classified as "named in tests but no LiveDB
//     caller".
//  2. FALSE NEGATIVE, helper hiding — same package, same file, and a distinct mechanism.
//     ListDispositionsForReview is called at recommendation_dispositions_integration_test.go
//     :262, inside the package-level helper listDispositions (declared :260), which
//     TestRecommendationDispositionsLiveDB calls at :225. The prototype slices source between
//     each test function's start and end offsets, so a call in a package-level helper is
//     outside every slice it looks at. A whole-file scan would instead credit every test in
//     the file. (Found while writing this table; verified at HEAD by the auditor, who had not
//     seen it — it is the cheapest of the three to check, because it lives in one file.)
//  3. FALSE POSITIVE — the opposite direction. CreateJudgeRun's first inferred pinner is
//     TestClaimRunDockerRepoAllowlistLiveDB, which uses it as fixture setup for an unrelated
//     property. A real call and a useless pin: the name appearing in a body says nothing
//     about what is asserted.
//
// An inferring rule therefore gets argued down on the day it lands, and its verdicts have to
// be overridden by hand anyway — at which point the hand-written table is the mechanism and
// the inference is decoration.
//
// DO NOT "FIX" THE INFERENCE BY RESOLVING HELPERS TRANSITIVELY. It is the obvious response to
// (2) and it is a trap: every increment of cleverness buys a new class of false positive, and
// none of it touches (3) at all, which is a judgement about whether a pin is MEANINGFUL and
// is not recoverable from the call graph. The declaration table works precisely because it
// stops trying to infer.
//
// This test needs no database and must NOT be named *LiveDB: it is static analysis, so it
// runs in the ordinary `go test ./...` gate where an arriving query is actually noticed.
//
// Four negative controls were run against this file and each one reddened it — an undeclared
// new query, a pin renamed to a test that does not exist, a row renamed so it names no query
// (which fires BOTH the missing and the stale check), and an UNPINNED row with a blank
// reason. They are recorded with the rest of the measurement in prds/98-judge-menu.md rather
// than re-run here, because a control that lives only in a commit message is one nobody can
// re-run. If you change the matching below, re-run them.
// ---------------------------------------------------------------------------------------

// unpinnedPin is the sentinel for "no test executes this query, and here is why".
const unpinnedPin = "UNPINNED"

// queryPin is one declared row. `pin` is the Go test function that exercises the query, or
// unpinnedPin; `why` explains an UNPINNED row (required) and records HOW a pinned row is
// reached when that is not obvious from the test's name (optional).
type queryPin struct {
	query string
	file  string
	pin   string
	why   string
}

// judgeQueryFiles are the .sql files this inventory covers. Adding a file here is what
// widens the scope; the test below fails if one of them is missing or yields no queries.
var judgeQueryFiles = []string{
	"dispositions.sql",
	"judge.sql",
	"judge_bulk_disposition.sql",
	"judge_issue_close.sql",
	"judge_recommendations.sql",
}

// judgeQueryPackages are the directories, relative to this one, whose *_test.go files are
// parsed for the declared test function names. A pin living outside these fails with a
// message saying so rather than silently passing.
var judgeQueryPackages = []string{".", "../handler"}

// judgeQueryInventory is the declaration. Every row was verified by opening the call site
// named in `why`; none was inferred.
var judgeQueryInventory = []queryPin{
	{"UpsertRecommendationDisposition", "dispositions.sql", "TestRecommendationDispositionsLiveDB",
		"direct call, recommendation_dispositions_integration_test.go:108"},
	{"DeleteRecommendationDisposition", "dispositions.sql", "TestRecommendationDispositionsLiveDB",
		"direct call, recommendation_dispositions_integration_test.go:243"},
	{"ListDispositionsForReview", "dispositions.sql", "TestRecommendationDispositionsLiveDB",
		"reached through the listDispositions helper (:260), called at :225 — NOT visible to a body scan"},
	{"ListJudgeTriageRowsForUser", "dispositions.sql", "TestJudgeTriageRowsForUserAreCoordinateScopedLiveDB",
		"the coordinate-scoping pin; also called directly by TestRecommendationDispositionsLiveDB:183"},
	{"CreateJudgeRun", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:65 (TestClaimRunDockerRepoAllowlistLiveDB also calls it, but only as fixture setup)"},
	{"GetActiveJudgeRunForWorkerTarget", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:91"},
	{"ListToolResultPayloadsForRun", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:84"},
	{"ListRunInputsForRun", "judge.sql", unpinnedPin,
		"NO live exercise. Its only production caller is workersvc/judge_trace.go:89, and every " +
			"test that reaches JudgeTrace runs against workersvc's fakeStore (service_test.go:393), " +
			"which returns a canned slice — so the SQL itself has never executed under test. The " +
			"judge's oldest-first input cap rides this query (see follow_up_inputs_integration_test.go:21). " +
			"Declared rather than fixed because PRD #98's scope is frozen; recorded in the PRD's Remaining Work."},
	{"UpsertRunReviewWithRecommendations", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:103"},
	{"GetRunReviewForTarget", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:140"},
	{"ListRecommendationsForReview", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go:147"},
	{"ListOwnedRecommendationsForCoords", "judge_bulk_disposition.sql", "TestBulkDispositionFiledMemberIsNotOpenLiveDB",
		"named in NO test source: reached via handler.BulkSetDispositions -> workersvc. This pin is " +
			"the one whose folds are recorded — dropping the filed-issues join's coordinate half " +
			"reddens it (updated=0), as does neutering filed_settled (updated=2)"},
	{"UpsertDispositionsForResolvedCoords", "judge_bulk_disposition.sql", "TestBulkDispositionFansOutAcrossRunsLiveDB",
		"named in NO test source: the write half of the same handler path; the test asserts the " +
			"fan-out landed by reading recommendation_dispositions back from the table"},
	{"ListFiledIssueCloseEdges", "judge_issue_close.sql", "TestJudgeBacklogProjectsEveryColumnLiveDB",
		"direct call, judge_recommendations_integration_test.go:492"},
	{"ApplyFiledIssueCloseEdge", "judge_issue_close.sql", "TestJudgeBacklogProjectsEveryColumnLiveDB",
		"direct call, judge_recommendations_integration_test.go:507"},
	{"ListJudgeRecommendationRowsForUser", "judge_recommendations.sql", "TestJudgeBacklogProjectsEveryColumnLiveDB",
		"direct call, judge_recommendations_integration_test.go:521 — the M1 read model, also " +
			"exercised by the anchor/recency/tenant tests in the same file"},
	{"ListJudgeTriageRowsForRuns", "judge_recommendations.sql", "TestJudgeRunTodoTriageRowsAreCoordinateScopedLiveDB",
		"direct call, judge_recommendations_integration_test.go:1106"},
}

var sqlQueryNameRe = regexp.MustCompile(`(?m)^-- name: (\w+) `)

// scanJudgeQueryNames returns file -> the query names sqlc will generate from it.
func scanJudgeQueryNames(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range judgeQueryFiles {
		path := filepath.Join("queries", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v (judgeQueryFiles names a file that is not there — if it was "+
				"renamed, rename it here and in the inventory)", path, err)
		}
		for _, m := range sqlQueryNameRe.FindAllStringSubmatch(string(b), -1) {
			out[name] = append(out[name], m[1])
		}
		// Per-file self-check. A file that parses to zero queries means the `-- name:` spelling
		// changed and every assertion below silently stops covering it.
		if len(out[name]) == 0 {
			t.Fatalf("%s yielded 0 query names — the `-- name:` scan is broken, so this test "+
				"proves nothing about that file", path)
		}
	}
	return out
}

// scanTestFuncNames returns every Go test function declared in judgeQueryPackages.
func scanTestFuncNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range judgeQueryPackages {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
					continue
				}
				out[fn.Name.Name] = path
			}
		}
	}
	return out
}

// TestJudgeQueryInventoryIsDeclared holds the inventory to the tree: every judge-family
// query has exactly one row, every row names a query that still exists, every pin names a
// test function that still exists, and every UNPINNED row carries a reason.
//
// Re-read the file header before reading a green from this as coverage. It is an index.
func TestJudgeQueryInventoryIsDeclared(t *testing.T) {
	byFile := scanJudgeQueryNames(t)

	scanned := map[string]bool{}
	total := 0
	for file, names := range byFile {
		for _, n := range names {
			scanned[file+":"+n] = true
			total++
		}
	}
	// Whole-scan self-check, on top of the per-file one. Both exist because the two ways this
	// test goes vacuously green are "the glob found nothing" and "the regex matched nothing",
	// and each check catches only its own.
	if total == 0 {
		t.Fatal("scanned 0 queries across judgeQueryFiles — this test would pass for any tree")
	}

	declared := map[string]queryPin{}
	for _, p := range judgeQueryInventory {
		key := p.file + ":" + p.query
		if prev, dup := declared[key]; dup {
			t.Errorf("%s is declared twice (pins %q and %q) — one row per query, so a reader "+
				"cannot be shown the stale half", key, prev.pin, p.pin)
			continue
		}
		declared[key] = p
	}

	// 1. Every query in the tree is declared. This is the arriving-query gate — the whole
	//    reason the file exists, and the only assertion here that fails on new work.
	var missing []string
	for key := range scanned {
		if _, ok := declared[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d judge-family query/queries have no row in judgeQueryInventory:\n  %s\n"+
			"Add a row for each. If a live test exercises it, name that test; if none does, use "+
			"UNPINNED with a reason — UNPINNED is a legal, permanent, green state and is much "+
			"better than a guessed pin.", len(missing), strings.Join(missing, "\n  "))
	}

	// 2. Every declared row still corresponds to a real query. Catches a rename or a deletion
	//    leaving a row that reads as coverage of something that no longer exists.
	var stale []string
	for key := range declared {
		if !scanned[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%d inventory row(s) name a query that is not in the .sql file any more:\n  %s\n"+
			"It was renamed or removed — update or delete the row.", len(stale), strings.Join(stale, "\n  "))
	}

	// 3. Every pin names a test function that exists; every UNPINNED row carries a reason.
	funcs := scanTestFuncNames(t)
	if len(funcs) == 0 {
		t.Fatal("parsed 0 test functions out of judgeQueryPackages — the pin check below would " +
			"fail for every row rather than checking anything")
	}
	for _, p := range judgeQueryInventory {
		if p.pin == unpinnedPin {
			if strings.TrimSpace(p.why) == "" {
				t.Errorf("%s/%s is UNPINNED with no reason. UNPINNED is allowed; unexplained is not "+
					"— the reason is the entire value of the row.", p.file, p.query)
			}
			continue
		}
		if _, ok := funcs[p.pin]; !ok {
			t.Errorf("%s/%s is pinned to %q, which is not a test function in %v. It was renamed or "+
				"deleted; re-point the row, or set it to UNPINNED with the reason.",
				p.file, p.query, p.pin, judgeQueryPackages)
		}
	}
}
