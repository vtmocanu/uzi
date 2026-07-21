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
// THE QUERY INVENTORY (PRD #98). Started on the judge family; widened one file at a time.
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
// THE THIRD STATE, EXERCISED-UNASSERTED, AND WHY IT IS NOT A SOFTER UNPINNED. The judge
// family happened to hold only two kinds of query: ones a live test drives and asserts on,
// and ones no live test reaches at all. The first file outside it produced a third, and
// forcing that one into either existing bucket would have been a false statement in the
// file whose entire value is that its rows are true. TouchCLIToken EXECUTES on every accepted
// bearer request in the CLI live-DB suite, so UNPINNED — defined right below as "no test
// executes this query" — is factually wrong; and nothing anywhere asserts last_used_at,
// last_used_ip or the ≤1/min skip, so PINNED over-credits. Worse, the caller swallows its
// error (middleware/cli_auth.go:92 logs and continues, because a forensic stamp must not
// fail an auth decision), so even "it ran without erroring" is not observable from a test.
// EXERCISED-UNASSERTED says exactly that, and like UNPINNED it is legal, green, permanent
// and requires a written reason.
//
// It is the more DANGEROUS of the two states, which is the reason it needed a name rather
// than a footnote: an UNPINNED query is visibly uncovered, while this one shows up in a
// coverage report, runs on every request, and has nobody watching what it wrote.
//
// SCOPE, stated so a reader does not over-read it: the .sql files listed in
// inventoryQueryFiles and nothing else. Re-derive the coverage arithmetic rather than
// trusting a sentence — `grep -c '^-- name: ' queries/*.sql` gives the per-file counts and
// `ls queries/*.sql | wc -l` the denominator. Measured at `ad6c63d9`: 290 queries across 28
// files repo-wide; this file declares 46 of them across ten files. (The number this paragraph
// carried before was 276, written weeks of merges earlier and never re-derived; it is quoted
// here only to make the point that the figure drifts and the command does not.)
//
// WIDENING IS DELIBERATELY ONE FILE AT A TIME, AND THE REASON IS NOT CAUTION. A repo-wide
// table written in one sitting would be mostly UNPINNED rows authored by someone who had
// investigated none of them — a WORSE artifact than no table, because it reads as an audit.
// Every row below was written with the call sites open. A smaller true table beats a larger
// performative one, so if you widen further and find yourself guessing, stop at the file you
// finished honestly.
//
// FILES ARE PICKED BY RISK — a query whose drift is SILENT AND USER-VISIBLE beats one whose
// drift reddens instantly. The files added after the judge family, with the reason each was
// picked ahead of larger files:
//
//   review_issues.sql (5)  — the WRITE half of the coordinate the judge family reads. It is
//     claim-first (claim → forge write → settle/revert), so a drifted predicate does not
//     error: it files a SECOND forge issue, or destroys a settled link, or strands a claim.
//     Irreversible and outside our database.
//   selfimprove.sql (4)    — the engine that opens runs against uzi's own repo without a
//     human plan gate. Its backlog query is the one table-wide scan in the recommendation
//     family (no user scope, no review scope), and two of its four queries are WRITES that
//     have never executed against a database.
//   cli_tokens.sql (6)     — the bearer-credential path. Owner scoping here is the whole
//     authorization boundary for the CLI, and a lost predicate is a cross-user token read or
//     revoke that returns 200.
//   notifications.sql (8)  — every row a user's inbox renders, all owner-scoped. Drift here is
//     silent and permanently visible: a wrong unread badge is the bug a user reports and nobody
//     can reproduce.
//   agent_memory.sql (6)   — user+repo scoped reads plus an eviction that DELETES, so a lost
//     predicate either leaks another tenant's notes into a prompt or destroys the wrong rows.
//
// Left for later, with the reason, so the next person does not re-derive the triage:
// runtime.sql (65), users.sql (21), user_secrets.sql (18) and slack.sql (18) are all higher
// risk than some of the above and too large to investigate honestly in one sitting; forge.sql
// (31) likewise. Best next candidates on size-versus-risk: user_vaults.sql (3) and
// settings.sql (3) are small enough to finish properly; skills.sql (13) and chat.sql (16) are
// the largest that still look tractable.
//
// WHAT THE LAST TWO FILES CHANGED ABOUT HOW TO READ THIS TABLE. agent_memory.sql came out with
// six pins and NO declared gaps, and that is worth as much as a file full of gaps: it makes the
// table a distribution rather than a list of complaints, and it is the control on the process —
// a widening exercise that only ever finds holes is one nobody should trust. notifications.sql
// came out the other way, and the split inside it is the finding: the WRITE path (insert,
// count, prune) is live-pinned and the entire READ path is not, which no per-file summary would
// have surfaced.
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
// WIDENING RE-CONFIRMED ALL THREE AND ADDED A FOURTH — recorded because the strongest reason
// to keep hand-writing this table is that every new file has produced another way to be
// wrong about it:
//
//  2'. Helper hiding is NOT a judge-family quirk. review_issues.sql has two more instances in
//      one file: ListOpenImproveUziRecommendations is called only inside openImproveUziTargets
//      (recommendation_filed_issues_integration_test.go:299) and ListFiledIssuesForReview only
//      inside listFiled (:312). Both helpers are then called from the bodies of TWO different
//      live tests in TWO different files, so a body scan misses them and a whole-file scan
//      credits the wrong test.
//  4.  NEW, and it is a false POSITIVE that no amount of call-graph cleverness reaches,
//      because the call never happens: a live test drives the route the query sits behind and
//      asserts the 401 that MIDDLEWARE returns before the handler runs. ListCLITokens is the
//      case — `GET /api/me/cli-tokens` appears in the CLI live-DB suite exactly once
//      (cli_auth_livedb_test.go:593), inside the table asserting that CRUD verbs are
//      cookie-only, so the SQL never executes. Any "is this endpoint covered by a live test"
//      heuristic says yes. The query has never run against a database.
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
// SEVEN NEGATIVE CONTROLS, all re-run at `ad6c63d9` after the widening rather than inherited,
// each RED with the specific message named and each restored to a byte-identical tree. The
// first four are the originals; the last three exist because the widening added machinery:
//
//	1. an undeclared new query arrives in a covered file  -> "have no row in queryInventory"
//	2. a pin renamed to a test that does not exist        -> "is not a test function"
//	3. a row renamed so it names no query                 -> BOTH missing AND stale
//	4. an UNPINNED row with a blank reason                -> "with no reason"
//	5. an EXERCISED-UNASSERTED row with a blank reason    -> "with no reason"
//	6. declaredGap narrowed back to unpinnedPin only      -> "is not a test function"
//	7. a covered file's whole `-- name:` spelling breaks  -> "yielded 0 query names"
//	8. `../handler` dropped from inventoryPackages        -> "is not a test function" on the
//	   SIX truthful pins whose tests live there (4 in cli_tokens.sql, 2 in
//	   judge_bulk_disposition.sql), each carrying the extend-the-package remedy
//
// Control 8 is the one that proves the package scan spans BOTH entries, and it needs its two
// halves stated together or it proves less than it looks: the GREEN half is the ordinary
// baseline (those six rows pass with `../handler` present), the RED half is dropping the entry
// and watching the SAME six rows fail. Both on one tree. Note what would NOT have worked — a
// bogus pin naming a nonexistent test in `../handler`, which is control 2: that goes red with
// or without the second entry, because a name absent from a scanned package and a name in an
// unscanned one are indistinguishable to this check. A control has to fire against the body
// you are asking about; the same trap the M4 fold table records for byte-identical query
// bodies, in a different costume.
//
// Control 6 is the one worth understanding rather than counting: it proves the third sentinel
// is WIRED, not merely declared. Without it, a declaredGap that had silently dropped
// unassertedPin would demand a test function literally named "EXERCISED-UNASSERTED" — and the
// row would go red for a reason no reader would connect to the cause.
//
// 🔴 AND A RED IS NOT AUTOMATICALLY THE RED YOU WANTED. Controls 4 and 5 first "passed" on a
// COMPILE error the mutation had introduced, which is a red that proves nothing about the
// check under test — the same shape as a live-DB fold that stops the package building. Each
// control now asserts its own message and treats a build failure as an invalid run. The first
// attempt also diffed "did the mutation apply" against HEAD, which already differed by every
// uncommitted line, so that check could not have reported a failure: it is diffed against a
// copy-aside now. Both are the standing rule that a VERIFICATION step needs its own positive
// control, failing inside the controls written to enforce it.
//
// If you change the matching below, re-run them — and when you do, A CONTROL RUN MUST ASSERT
// `build-failed=no` AND THE PRESENCE OF `--- FAIL`, not merely a non-zero exit. This is the
// requirement, stated in the tree, because the fix that produced it lived only in a scratch
// script and in prose: nothing here forces the next person to check, and a mutation that
// breaks the build exits non-zero and reads exactly like a control that fired. Two of the
// controls above "passed" that way before anyone looked. A run that cannot distinguish
// "the check caught it" from "the package did not compile" has not run a control.
// ---------------------------------------------------------------------------------------

// unpinnedPin is the sentinel for "no test executes this query, and here is why".
const unpinnedPin = "UNPINNED"

// unassertedPin is the sentinel for "a live test EXECUTES this query, and nothing asserts
// what it did". See the header for why this is a third state rather than a shade of either
// other one, and why it is the more dangerous of the two gaps.
const unassertedPin = "EXERCISED-UNASSERTED"

// declaredGap reports whether a pin is one of the two sentinels — both legal, both green,
// both requiring a written reason. Kept as one predicate so a future third sentinel cannot be
// added to the reason check and forgotten in the test-function-exists check, which would
// silently start demanding a test function named "EXERCISED-UNASSERTED".
func declaredGap(pin string) bool { return pin == unpinnedPin || pin == unassertedPin }

// queryPin is one declared row. `pin` is the Go test function that exercises the query, or
// one of the sentinels; `why` explains a sentinel row (required) and records HOW a pinned row
// is reached when that is not obvious from the test's name (optional).
type queryPin struct {
	query string
	file  string
	pin   string
	why   string
}

// inventoryQueryFiles are the .sql files this inventory covers. Adding a file here is what
// widens the scope; the test below fails if one of them is missing or yields no queries.
// Widen ONE FILE AT A TIME, with the call sites open — see the header.
var inventoryQueryFiles = []string{
	// the judge family, where this started
	"dispositions.sql",
	"judge.sql",
	"judge_bulk_disposition.sql",
	"judge_issue_close.sql",
	"judge_recommendations.sql",
	// widened by risk, one file at a time
	"review_issues.sql",
	"selfimprove.sql",
	"cli_tokens.sql",
	"notifications.sql",
	"agent_memory.sql",
}

// inventoryPackages are the directories, relative to this one, whose *_test.go files are
// parsed for the declared test function names. A pin living outside these fails with a
// message saying so rather than silently passing.
//
// NOT widened alongside inventoryQueryFiles, and that is MEASURED rather than assumed. At
// `041c5291`, `rg -l 'func Test.*LiveDB\(' internal/ cmd/ | xargs -n1 dirname | sort -u`
// returns exactly two directories — internal/store (27 files) and internal/handler (9) — so
// there is no live-DB test anywhere else for a pin to name. Re-run that command rather than
// trusting this sentence; it is the whole justification for the list being two entries long.
//
// 🔴 THE DAY THAT STOPS BEING TRUE, EXTEND THIS LIST — DO NOT WRITE A SENTINEL ROW. A live-DB
// test landing in internal/poller, internal/forgesvc, internal/workersvc or cmd/uzi makes a
// TRUTHFUL pin fail the existence check below, and the cheapest way out is to record the query
// as a declared gap, which is a lie that reads as an audit — the exact failure this file
// exists to prevent. The failure message names this remedy FIRST for that reason.
//
// The reason this list was NOT pre-extended: the check that would prove an extension works is
// "a truthful pin naming a real test in the newly-covered package goes green", and with no
// live-DB test in any other package there is no truthful pin to write. Extending now would
// mean inventing a row to justify the extension — the same failure, arrived at from the
// opposite direction. Add the package when the row that needs it exists, in that commit.
//
// The store-layer queries reached only through middleware (cli_tokens.sql) are pinned by
// handler tests, because those drive the REAL router — middleware included — rather than
// calling handlers directly. That is why widening to cli_tokens.sql needed no new package.
var inventoryPackages = []string{".", "../handler"}

// queryInventory is the declaration. Every row was verified by opening the call site named in
// `why`; none was inferred.
var queryInventory = []queryPin{
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
	{"ListRunInputsForRun", "judge.sql", "TestListRunInputsForRunLiveDB",
		"WAS the one UNPINNED row, and this mechanism is how it was found — it named a query " +
			"nobody was looking at, outside the work that motivated the inventory. Now pinned by " +
			"judge_trace_inputs_integration_test.go: run scoping, every kind (not just follow_up), " +
			"oldest-first, and the cap taking the OLDEST n. FOUR folds, each RED, hitting THREE " +
			"distinct assertions: ORDER BY id DESC and a cap-from-the-wrong-end subquery isolate " +
			"the order and the cap; a neutered run predicate and a follow_up-only filter both land " +
			"on the same row-count check. NOT covered, and the fixture cannot cover it: " +
			"consumed_at and created_at (see the note at the fixture)."},
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

	// ── review_issues.sql — the claim-first filing path (PRD #68) ──────────────────────
	// All five are pinned by one live test that walks the whole claim → settle → re-judge →
	// revert → sweep sequence. That is not five separate pins of equal strength: they share a
	// fixture, and a single early Fatalf takes the rest of the sequence with it.
	{"ClaimRecommendationFiledIssue", "review_issues.sql", "TestRecommendationFiledIssuesLiveDB",
		"direct call, recommendation_filed_issues_integration_test.go:88. Discriminating in both " +
			"directions: the first claim must win, a SECOND claim on the same coordinate must " +
			"return pgx.ErrNoRows (:95, the ON CONFLICT DO NOTHING), and two duplicate improve_uzi " +
			"target='' recommendations must collapse to ONE row (:105) rather than fan out"},
	{"SettleRecommendationFiledIssue", "review_issues.sql", "TestRecommendationFiledIssuesLiveDB",
		"direct call, :115 asserts n=1, and :124 asserts a RE-settle affects 0 rows — the " +
			"filed_at IS NULL guard, which is what makes the handler's created-with-warning path " +
			"reachable rather than a forge retry"},
	{"RevertRecommendationFiledIssue", "review_issues.sql", "TestRecommendationFiledIssuesLiveDB",
		"direct call, :173, then :176 asserts the row is GONE and :179 that the coordinate is " +
			"claimable again. NOT covered: the `AND filed_at IS NULL` guard, which is what stops a " +
			"late revert destroying a SETTLED link — no test reverts a settled row"},
	{"ListFiledIssuesForReview", "review_issues.sql", "TestRecommendationFiledIssuesLiveDB",
		"reached through the listFiled helper (:312), called at :131 and :155 — NOT visible to a " +
			"body scan. The :155 call is the load-bearing one: it asserts the filed link SURVIVES a " +
			"re-judge with the same id, which is the whole reason the link lives in its own table. " +
			"NOT covered: the ORDER BY (settled first, NULLS LAST) — every fixture has one row"},
	{"SweepStrandedRecommendationClaims", "review_issues.sql", "TestRecommendationFiledIssuesLiveDB",
		"direct call. The table-wide `swept != 1` this row used to flag is FIXED: it now asserts " +
			"three fixture-scoped facts (stranded row reaped, FRESH claim survives, settled row " +
			"untouched) with `swept >= 1` kept only as a floor. Folded both directions — `<`->`>` " +
			"reddens the reap check, an always-true cutoff reddens the survives check, each alone. " +
			"NOT covered: `filed_at IS NULL` in isolation, which the settled-row check reaches only " +
			"because that row is also filing_since NULL"},

	// ── selfimprove.sql — the engine that opens runs against uzi's own repo ────────────
	{"ListOpenImproveUziRecommendations", "selfimprove.sql", "TestRecommendationFiledIssuesLiveDB",
		"reached through the openImproveUziTargets helper (:299), called at :83, :110 and :161 — " +
			"NOT visible to a body scan. Those three calls pin the PRD #68 Decision 12 exclusion in " +
			"both directions (present before any claim, absent while merely CLAIMED, still absent " +
			"after a re-judge). TestRecommendationDispositionsLiveDB pins the OTHER exclusion (PRD " +
			"#94 Decision 9) through the same helper at :163/:248/:257, and " +
			"TestFiledIssueCloseAutoDonesOnceLiveDB calls it directly at " +
			"judge_issue_close_livedb_test.go:232. Three tests, two of them invisible to any scan"},
	{"CreateSelfImproveRun", "selfimprove.sql", unpinnedPin,
		"No live test executes it. Every caller path is faked: selfimprove/engine_test.go:86 is a " +
			"fakeRuns, and workersvc.Service.CreateSelfImproveRun (self_improve.go:30) is reached " +
			"from no live-DB test. So the INSERT has never run against the real schema — including " +
			"the self_improve kind-shape CHECK and the uq_runs_one_active_self_improve partial " +
			"index, which is the guard that stops a boot re-run double-creating onto the fixed " +
			"branch. engine_test.go:326 asserts the 23505 handling against a fake that RETURNS " +
			"23505; nothing has made Postgres produce one"},
	{"CountActiveSelfImproveRuns", "selfimprove.sql", unpinnedPin,
		"No live test executes it. handler/selfimprove_test.go drives it through a fake store and " +
			"selfimprove/engine_test.go:52 returns a canned count. Its status list " +
			"('completed','failed','cancelled') is a second, hand-maintained copy of what 'terminal' " +
			"means, and no live row has ever been compared against it"},
	{"MarkImproveUziRecommendationsAddressed", "selfimprove.sql", unpinnedPin,
		"No live test executes it — only the fake at selfimprove/engine_test.go:56. It is a WRITE " +
			"with two guards nothing has exercised against a database: `category = 'improve_uzi'` " +
			"and `addressed_by_run_id IS NULL`. Losing the second makes a concurrent stamp " +
			"non-idempotent; losing the first stamps addressed_by_run_id onto rows of other " +
			"categories, which silently removes them from every backlog that filters on it"},

	// ── cli_tokens.sql — the bearer-credential path ────────────────────────────────────
	// These live in internal/store but are reached ONLY through internal/middleware and
	// internal/handler. The pins are handler live-DB tests that drive h.Routes(), so the
	// middleware runs for real — mechanism 1 (cross-package indirection) at its strongest:
	// not one of these six names appears in any test source.
	{"GetCLITokenByHash", "cli_tokens.sql", "TestCLIExpiredRevokedReject401LiveDB",
		"named in NO test source: reached via middleware/cli_auth.go:53 on every Bearer request. " +
			"This pin is the FAIL-CLOSED half — a past-expiry token and a revoked token are both " +
			"401, with a valid token of the same user asserted 200 first so the 401 is the predicate " +
			"and not a broken fixture. Every other bearer test in the CLI suite exercises the " +
			"accepting half"},
	{"CreateCLIToken", "cli_tokens.sql", "TestCLIAuthFlowMintOnceLiveDB",
		"named in NO test source: reached via handler/cli_auth_flow.go:293 (device-auth claim). A " +
			"producer -> consumer pin rather than a status-code one — cli_auth_flow_livedb_test.go" +
			":133 uses the minted token as a real Bearer credential and asserts 200, so the row this " +
			"INSERT wrote must satisfy GetCLITokenByHash. TestCLIAdminROMintGateLiveDB also reaches " +
			"it through the other caller (cli_tokens.go:147) but asserts only the 201"},
	{"RevokeCLIToken", "cli_tokens.sql", "TestCLICRUDBearerRejectAndOwnerScopeLiveDB",
		"named in NO test source: reached via handler/cli_tokens.go:186. Pins the OWNER half of " +
			"`WHERE id = @id AND user_id = @user_id` in both directions — an attacker deleting the " +
			"victim's token id gets 404 (zero rows), and the owner's own delete of that SAME id then " +
			"gets 204, which is what proves the 404 was the scoping and not a missing row"},
	{"RevokeAllCLITokens", "cli_tokens.sql", "TestCLIRevokeAllLiveDB",
		"named in NO test source: reached via handler/cli_tokens.go:208. Caller-scoped in both " +
			"directions: the caller's two tokens end 2/2 revoked while a second user's stays 0, and " +
			"a repeated call is asserted idempotent"},
	{"ListCLITokens", "cli_tokens.sql", unpinnedPin,
		"No live test executes it, and this is the case that looks covered. `GET /api/me/cli-tokens` " +
			"appears in the CLI live-DB suite exactly once — cli_auth_livedb_test.go:593, inside the " +
			"table asserting CRUD verbs are cookie-only — where RequireAuth returns 401 BEFORE the " +
			"handler runs. So the route has a live test and the query has never executed. Its " +
			"`WHERE user_id = @user_id` is the only thing keeping one user's token list " +
			"(token_prefix, last_used_at, last_used_ip — the whole forensic surface) out of " +
			"another's, and losing it returns 200"},
	// ── notifications.sql — the inbox. THE WRITE PATH IS PINNED; THE READ PATH IS NOT ───
	// The shape is worth seeing before the rows: one live test covers insert/count/prune, and
	// the entire LIST + MARK-READ half — every query the user's inbox actually renders from —
	// has never touched a database. handler/notifications_test.go looks like coverage and is
	// fake-store throughout, so the owner predicates below are pinned only by a fake that takes
	// the scoping as a parameter.
	{"PruneNotificationsForUser", "notifications.sql", "TestNotificationsPruneLiveDB",
		"direct call through the prune helper, notifications_integration_test.go:64, driven by " +
			"several subtests. Discriminating: under-cap keeps everything, over-cap deletes exactly " +
			"the excess, and one user's prune leaves another user's rows untouched"},
	{"InsertNotification", "notifications.sql", "TestNotificationsPruneLiveDB",
		"direct call, :157 — the 'write-seam round-trip' subtest, NOT mere fixture setup: it " +
			"inserts four, asserts the count reads four, prunes to two, and asserts the rows were " +
			"genuinely removed"},
	{"CountNotificationsForUser", "notifications.sql", "TestNotificationsPruneLiveDB",
		"direct call, :164 and :170, asserted on both sides of a prune in the same subtest"},
	{"ListNotificationsForUser", "notifications.sql", unpinnedPin,
		"No live test executes it. The only caller is handler/notifications.go:162, and " +
			"handler/notifications_test.go is fake-store — TestListNotificationsOwnScope asserts the " +
			"owner scoping against a fake that receives user_id as a parameter and therefore cannot " +
			"be wrong about where it came from. This is the query the inbox page renders from"},
	{"CountUnreadNotificationsForUser", "notifications.sql", unpinnedPin,
		"No live test executes it. Callers are handler/notifications.go:134 and :192 only. It " +
			"drives the unread BADGE, so drift is silent and permanently visible — a stuck count is " +
			"the kind of bug a user reports and nobody can reproduce"},
	{"MarkNotificationRead", "notifications.sql", unpinnedPin,
		"No live test executes it. handler/notifications_test.go:332-363 drives the handler " +
			"against a fake, including TestMarkNotificationReadCrossUserDenied — so the cross-user " +
			"denial that matters is asserted where the SQL is not. Its `user_id = @user_id` is what " +
			"stops one user marking another's notification read"},
	{"ListAllNotifications", "notifications.sql", unpinnedPin,
		"No live test executes it. Caller is handler/notifications.go:142, the ADMIN all-users " +
			"view — the one read deliberately NOT owner-scoped, which is exactly why its pagination " +
			"and ordering going wrong would be invisible rather than caught by a scoping assertion"},
	{"CountAllNotifications", "notifications.sql", unpinnedPin,
		"No live test executes it. Caller is handler/notifications.go:148, the admin view's total"},

	// ── agent_memory.sql — pinned end to end, recorded because a table of gaps is not the ──
	// point. One live test covers all six with discriminating assertions in both directions;
	// nothing here is a declared gap, and that is a result worth having in the same list.
	{"InsertAgentMemory", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, agent_memory_integration_test.go:71/:86/:92 — three entries across two users " +
			"and two repos, which is what makes the scoping assertions below able to fail"},
	{"ListAgentMemoryForUserRepo", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, :98. Asserts EXACTLY the (u1,r1) entry comes back — so a lost user half or " +
			"repo half admits a neighbouring row and the length check fires. Also the read-back for " +
			"the eviction window at :134"},
	{"ListAgentMemoryForUser", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, :106: exactly 2 entries (both of u1's repos, none of u2's), plus a per-row " +
			"assertion that repo_name is non-empty, which is the only thing pinning the repos JOIN"},
	{"CountAgentMemoryForRun", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, :81"},
	{"EvictAgentMemoryOverCap", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, :131. The strongest pin in this file: 25 rows with hand-stamped created_at, " +
			"keep 20, and the assertion names the WINDOW (newest m24, oldest survivor m05) rather " +
			"than the count — so evicting the wrong end, or the right count from the wrong order, " +
			"both fail"},
	{"DeleteAgentMemory", "agent_memory.sql", "TestAgentMemoryLiveDB",
		"direct call, :147 and :154 — owner scoping in BOTH directions: a foreign user's delete " +
			"affects 0 rows, then the owner's delete of that SAME id affects 1, which proves the 0 " +
			"was the predicate and not a missing row"},

	{"TouchCLIToken", "cli_tokens.sql", unassertedPin,
		"EXECUTES on every accepted Bearer request in the CLI live-DB suite " +
			"(middleware/cli_auth.go:92), so it is not UNPINNED — but nothing anywhere asserts " +
			"last_used_at, last_used_ip, or the `last_used_at < now() - interval '1 minute'` skip " +
			"that keeps a single-replica api off a write per CLI call. `rg last_used_at` over the " +
			"whole tree hits only models.go, the generated file, and the handler DTO: no test. And " +
			"the caller SWALLOWS its error (log-and-continue, because a forensic stamp must not fail " +
			"an auth decision), so even 'it ran without erroring' is unobservable. last_used_ip is " +
			"described in the query as the only detection control the design has"},
}

var sqlQueryNameRe = regexp.MustCompile(`(?m)^-- name: (\w+) `)

// scanInventoryQueryNames returns file -> the query names sqlc will generate from it.
func scanInventoryQueryNames(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, name := range inventoryQueryFiles {
		path := filepath.Join("queries", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v (inventoryQueryFiles names a file that is not there — if it was "+
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

// scanTestFuncNames returns every Go test function declared in inventoryPackages.
func scanTestFuncNames(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range inventoryPackages {
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

// TestQueryInventoryIsDeclared holds the inventory to the tree: every query in a covered
// file has exactly one row, every row names a query that still exists, every pin names a test
// function that still exists, and every sentinel row carries a reason.
//
// Re-read the file header before reading a green from this as coverage. It is an index.
func TestQueryInventoryIsDeclared(t *testing.T) {
	byFile := scanInventoryQueryNames(t)

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
		t.Fatal("scanned 0 queries across inventoryQueryFiles — this test would pass for any tree")
	}

	declared := map[string]queryPin{}
	for _, p := range queryInventory {
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
		t.Errorf("%d covered-file query/queries have no row in queryInventory:\n  %s\n"+
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

	// 3. Every pin names a test function that exists; every sentinel row carries a reason.
	funcs := scanTestFuncNames(t)
	if len(funcs) == 0 {
		t.Fatal("parsed 0 test functions out of inventoryPackages — the pin check below would " +
			"fail for every row rather than checking anything")
	}
	for _, p := range queryInventory {
		if declaredGap(p.pin) {
			if strings.TrimSpace(p.why) == "" {
				t.Errorf("%s/%s is %s with no reason. A declared gap is allowed; an unexplained one "+
					"is not — the reason is the entire value of the row.", p.file, p.query, p.pin)
			}
			continue
		}
		if _, ok := funcs[p.pin]; !ok {
			// Remedy ORDER is deliberate. The obvious escape from this failure is to relabel the
			// row as a declared gap, and that is the one wrong answer: a truthful pin whose test
			// simply lives in a package this scan does not read would be recorded as uncovered,
			// which is a lie that reads as an audit. So the message offers the two honest fixes
			// first and the sentinel last, with the condition on it.
			t.Errorf("%s/%s is pinned to %q, which is not a test function in %v.\n"+
				"  1. If that test EXISTS but lives elsewhere, add its package to inventoryPackages "+
				"— do NOT downgrade a real pin to a sentinel to make this pass.\n"+
				"  2. If it was renamed, re-point the row.\n"+
				"  3. Only if NO test exercises the query, use UNPINNED (or EXERCISED-UNASSERTED if "+
				"something runs it without asserting) with the reason.",
				p.file, p.query, p.pin, inventoryPackages)
		}
	}
}
