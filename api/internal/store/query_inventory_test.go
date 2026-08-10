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
// in prds/done/98-judge-menu.md are what tell those apart; this is the index of where to look,
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
// files repo-wide; this file declared 52 of them across twelve files. (The number that paragraph
// carried before was 276, written weeks of merges earlier and never re-derived; it is quoted
// here only to make the point that the figure drifts and the command does not.)
// **It drifted again, exactly as predicted: the table holds FIFTY-THREE rows today** — the
// wave-3 integration merge added ListAllCLITokensForAdmin. The `ad6c63d9` figures are left as
// the receipt of that sweep rather than overwritten, because rewriting them would erase a
// measurement someone took; the live count is stated beside them so a reader who counts the
// rows does not conclude the record is wrong. Same treatment as control 8's "SIX truthful
// pins" below. If you are reading this and the count is again not 53, that is the point of
// the command in the sentence above, not an error in it.
//
// 🔴 HOW TO ESTABLISH THAT A QUERY RUNS: OBSERVE IT, DO NOT REASON ABOUT IT. Every row here
// was first written by reading call sites, and that method PUT A FALSEHOOD IN THIS FILE —
// CountActiveSelfImproveRuns was declared "No live test executes it" when a live test executes
// it. The reasoning was not sloppy; it was answering the wrong question. That query sits behind
// GET /api/admin/selfimprove, whose PUT sibling IS cookie-only and 401s at middleware, and the
// auth posture of the route got substituted for the execution of the query.
//
// The method that settles it, and the one to use before adding or changing any row:
//
//	docker run -d --rm --name cdr-<yours>-pg -e POSTGRES_USER=uzi -e POSTGRES_DB=uzi \
//	  -e POSTGRES_PASSWORD=... -p 127.0.0.1:<port>:5432 postgres:17 -c log_statement=all
//	UZI_TEST_DATABASE_URL=... go test -count=1 -p 1 -run 'LiveDB$' ./internal/store/... ./internal/handler/...
//	docker logs cdr-<yours>-pg > pg.log 2>&1        # ORDER MATTERS, see below
//	grep -- '-- name: <QueryName> :' pg.log | grep -vc 'STATEMENT:'
//
// Anchor on the `-- name:` header, never on the query's body text: sqlc emits each query's
// `-- name: X :kind` header as the FIRST LINE of the SQL it sends, so it belongs to exactly one
// query. Body text does not — a partial index whose predicate is textually identical to the
// query it serves gets emitted as DDL by store.Migrate on every fresh database, and a grep on
// that WHERE clause counts the DDL as an execution. Keep the trailing ` :` too: without it,
// `ListAppSettings` also matches `ListAppSettingsForUpdate`.
//
// 🔴 AND EXCLUDE `STATEMENT:` LINES, which is the correction this note needed after it was
// first written. Postgres echoes the offending statement on a `STATEMENT:` line beside every
// ERROR, so any query a test makes fail — an expected unique violation, a deliberate conflict —
// is logged TWICE and counted twice. Measured here: 17 such lines across one sweep, inflating 10
// queries (CreateUserOIDC 32->27, InsertUserSecret 129->126, UpsertRecommendationDisposition
// 7->5). None of the figures already published in this file moved, because the affected queries
// were inside a stated range rather than quoted individually — luck, not method.
//
// THE THREE WAYS THIS MEASUREMENT LIES, ALL FOUND BY USING IT, and they do not point the same
// way — which is why each needs its own guard rather than general care:
//   - a broken capture UNDER-counts to zero (see the stderr note below);
//   - a body-text anchor OVER-counts by catching DDL;
//   - a `STATEMENT:` echo OVER-counts by double-counting errors.
// A zero is the only reading that no over-count can fake, and it is also the reading this file
// leans on hardest — so verify the capture before trusting one.
//
// 🔴 `docker logs X > f 2>&1` AND `docker logs X 2>&1 > f` ARE NOT THE SAME COMMAND, and
// Postgres logs to STDERR. The second sends stderr to the terminal and only stdout to the file,
// producing a log file that is well-formed, non-empty, and missing every statement. It cost a
// measurement here: an isolated run reported 0 executions of a query that runs once, which
// briefly looked like evidence AGAINST the finding being fixed. Always redirect stdout first,
// and sanity-check the capture with `grep -c -- '-- name:'` before trusting a zero — a zero from
// a broken capture is indistinguishable from a zero from a query nobody calls.
//
// EXECUTION COUNTS FOR THE 52 ROWS THE RUN=141 SWEEPS MEASURED (RUN=141 PASS=141 FAIL=0 SKIP=0).
// Two different things live in the list and they have different shelf lives: the LIST ITSELF IS A
// MAINTAINED CENSUS and must track the table, while the FIGURES are receipts of those sweeps and
// move with the fixtures. The ZEROES are the load-bearing part.
//
// 🔴 ROW 53 IS UNMEASURED BY THIS METHOD, and saying otherwise was a third pass at the same
// defect. A previous version of this line read "ALL 53 ROWS … measured on whole-sweep runs
// (RUN=141 …)". No RUN=141 sweep ever saw row 53: re-derived here, `git show
// 536f9730:…/queries/cli_tokens.sql | grep -c ListAllCLITokensForAdmin` is 0, and so is the same
// at `321a25b2` — the query arrived via `c309e8a0` on the lim branch and reached this tree only
// in the wave-3 integration merge. It is not named in the list either, so it fell into
// "1..39 everything else", which asserts a measured execution count for a query that did not
// exist when the measurement ran. And no RUN=141 sweep can ever describe this tree: the live
// sweep at the integrated tip counts RUN=162.
//
// So the honest statement is the one above: 52 measured, row 53 unmeasured by the statement-log
// method. Establishing it properly needs a fresh `log_statement=all` sweep at this tip — real
// work, not worth it for one row, but then this header must not claim it. That the row is
// nonetheless PINNED is a separate claim, made by its own `why` and by a fold receipt; the two
// are different questions and this file exists partly to keep them apart.
// It is worth naming what went wrong three times in one paragraph: the defect is a figure that is
// REASONED rather than OBSERVED, in the file whose founding instruction is "OBSERVE IT, DO NOT
// REASON ABOUT IT".
//
// 🔴 CORRECTED TWICE, and the second correction is an instance of the defect it sits beside.
// This header said "ALL 46 ROWS"; the first repair made it "all 46 the table held AT THAT SWEEP …
// the seven added since are NOT in the list below". That is FALSE — `321a25b2` added four of them
// to the zero list four lines down (the three `user_vaults.sql` rows and
// `ListAppSettingsForUpdate`), and the correspondence assertion twenty lines below depends on the
// list having been maintained. A paragraph cannot declare itself a frozen receipt while a census
// claim downstream requires it to be live.
//
// THE DISCRIMINATOR, because "is this a receipt or a census" is the question that keeps arising
// here: HAS THIS TEXT BEEN EDITED SINCE THE RUN IT CLAIMS TO RECORD? Edited ⇒ census, and the
// numerals must track the tree (this list, and `:90`'s self-description). Not edited ⇒ receipt:
// bind it, never overwrite it (control 8's "SIX truthful pins" and the `ad6c63d9` 290-across-28
// figures, both left exactly as measured).
//
// AND HERE IS HOW TO ANSWER IT, because a rule with no command is a rule people cite rather than
// follow — everywhere else this file hands one over (`grep -c '^-- name: '`, the `docker logs`
// redirect order, the `-- name:` anchor):
//
//	git log -L <first-line>,<last-line>:api/internal/store/query_inventory_test.go <run-sha>..HEAD
//
// Non-empty output means the block has been edited since that sweep, i.e. it is a maintained
// CENSUS and its numerals must track the tree. Empty means it is a RECEIPT: bind it and leave it.
// `-L` follows the range through the intervening commits, which is what makes the answer survive
// the line drift that a plain `git blame` on today's numbers would hide.
//
// One thing `RUN=141` cannot do is date the sweep it is the receipt of: `536f9730` (46 rows) and
// `321a25b2` (52 rows) BOTH report RUN=141 — necessarily, since adding inventory rows adds no
// tests. Cite the SHA when the tree matters; the tally cannot stand in for it.
//
//	0  CreateSelfImproveRun, MarkImproveUziRecommendationsAddressed  (selfimprove.sql)
//	0  ListCLITokens                                                 (cli_tokens.sql)
//	0  ListNotificationsForUser, CountUnreadNotificationsForUser,
//	   MarkNotificationRead, ListAllNotifications, CountAllNotifications (notifications.sql)
//	0  GetUserVault, CreateUserVaultIfAbsent, DeleteUserVault      (user_vaults.sql — ALL of it)
//	0  ListAppSettingsForUpdate                                    (settings.sql)
//	1  CountActiveSelfImproveRuns  — the row that was wrong
//	35 TouchCLIToken               — runs constantly, asserted nowhere
//	1..39 everything else
//
// Those twelve zeroes are exactly the twelve UNPINNED rows below, and the two non-zero sentinels
// are exactly the two EXERCISED-UNASSERTED rows. That correspondence is the check: if a row's
// state and its execution count ever disagree, the ROW is wrong.
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
//   cli_tokens.sql (7)     — the bearer-credential path. Owner scoping here is the whole
//     authorization boundary for the CLI, and a lost predicate is a cross-user token read or
//     revoke that returns 200. The seventh, ListAllCLITokensForAdmin, is the deliberate
//     exception that proves the rule: it is factory-wide BY DESIGN, so its pin asserts the
//     absence of the predicate every other row here asserts the presence of.
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
// settings.sql (3) are small enough to finish properly; skills.sql (14) and chat.sql (16) are
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
// Control 8's "SIX" is left exactly as it was measured at `ad6c63d9` and is NOT updated here,
// because it is a receipt for a run that happened, not a live count — rewriting it would erase
// the evidence rather than refresh it. Re-running it today yields SEVEN, not six:
// ListAllCLITokensForAdmin joined the `../handler`-pinned set in the wave-3 integration merge.
// Recorded as an addition so a re-runner reconciles the difference instead of concluding the
// record was wrong; the CONTROL is the durable claim and the numeral is only its receipt.
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
	"user_vaults.sql",
	"settings.sql",
	// PRD #71 M4 — the ci-autofix loop guard
	"ci_autofix.sql",
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
	{"GetActiveJudgeRunForTarget", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go — the \"pending judge\" subtest (PRD #119 M1). " +
			"The pin is the predicate↔index equivalence, not mere reachability: the subtest " +
			"asserts an active judge IS found (queued AND a non-queued active status, so a " +
			"queued-only narrowing reddens) and that each of completed/failed/cancelled is NOT " +
			"(ErrNoRows), which is the uq_runs_one_active_judge_per_target active set spelled " +
			"out one status at a time. It also pins the target scoping (a judge pointed at " +
			"ANOTHER run is not returned), the kind term (an ACTIVE NON-judge run carrying this " +
			"target_run_id is not returned — legal to insert, since runs_kind_shape requires " +
			"target_run_id on a judge row but forbids it on no other kind, and the partial index " +
			"does not cover it) and the projection (status + created_at read back). Every term " +
			"of the WHERE — each of the three terminal statuses, the target equality and " +
			"kind = 'judge' — lands on a distinct assertion when folded"},
	{"ListToolTraceForRun", "judge.sql", "TestJudgeQueriesLiveDB",
		"direct call, judge_integration_test.go — a RENAME of ListToolResultPayloadsForRun, " +
			"not an arrival (PRD #121 M3 widened it to kind IN (tool_use, tool_result) and " +
			"projected seq). The pin got stronger with the rename: it was a bare row-count, " +
			"and now asserts the kind filter, the projection and the ASC ordering, so a " +
			"fold of ORDER BY seq ASC → DESC reddens it. It did not before — the old query " +
			"returned [][]byte and threw the ordering guarantee away at the type boundary"},
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
	{"CountActiveSelfImproveRuns", "selfimprove.sql", unassertedPin,
		"🔴 THIS ROW SAID UNPINNED / 'No live test executes it' AND THAT WAS FALSE. It EXECUTES: " +
			"TestCLIAdminSurfaceLiveDB asserts 200 for a uza_ token on GET /api/admin/selfimprove " +
			"(cli_auth_livedb_test.go:415 in adminReadPaths) -> GetSelfimproveConfig -> " +
			"selfimproveConfig() -> handler/selfimprove.go:198. Measured with log_statement=all, not " +
			"reasoned: 1 execution running that test alone against a fresh database, 1 across the " +
			"whole sweep. The wrong row came from reading the route's AUTH POSTURE — the PUT half IS " +
			"cookie-only and 401s at middleware (:442), which is true and was the wrong question. " +
			"Nothing asserts what it returned: no test anywhere touches the DTO's Active field, and " +
			"the caller SWALLOWS the error (`if active, err := ...; err == nil`), so 'it ran without " +
			"erroring' is unobservable — the same profile as TouchCLIToken, which is why the sentinel " +
			"and not a pin. Its status list ('completed','failed','cancelled') is still a second, " +
			"hand-maintained copy of what terminal means, and no live row has been compared against it"},
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
	// not one of these seven names appears in any test source.
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
	// Arrived in the wave-3 integration merge, not on either branch alone: `feature/prd-98-t2-lim`
	// added the query and `feature/prd-98-tier2` widened the inventory to this file, and neither
	// was red by itself. This row is the mechanism working exactly as its header describes — a
	// query that ARRIVED with nobody having decided its coverage — with the twist that the arrival
	// was a MERGE rather than a commit, which no single branch's gate could have caught.
	{"ListAllCLITokensForAdmin", "cli_tokens.sql", "TestAdminCLITokenInventoryIsFactoryWideAndLeaksNoCredentialLiveDB",
		"named in NO test source: reached via handler/cli_tokens.go:226. THE ONLY ROW IN THIS FILE " +
			"WHOSE PIN IS THE ABSENCE OF AN OWNER PREDICATE — every neighbour above pins a `WHERE " +
			"user_id = @user_id` that must hold, and this one pins that there is none. The test seeds " +
			"two owners and Fatalfs on `len(got) != len(fixtures)` with 'a per-user-scoped query would " +
			"look exactly like this', which is what makes a re-scoped query red rather than merely " +
			"shorter. Also pins the explicit projection as a SECURITY boundary and does it on the RAW " +
			"RESPONSE BYTES, not the DTO's shape: plaintext token, the base64 of its sha256 (what a " +
			"[]byte token_hash marshals to if the projection regains the column), and the literal " +
			"`token_hash` / `\"token\"` keys are each asserted absent — a struct-field check would pass " +
			"against a handler that re-marshalled the store row. owner_email pins the users JOIN to the " +
			"OWNER, not merely to the presence of an address: the assertion is an EQUALITY against the " +
			"deterministic `cli-<user-uuid>@e2e` cliSeedUser writes, so a wrongly-attributed row reddens. " +
			"That clause first landed on the strength of a shape check (`!= \"\" && contains \"@\"`) and " +
			"WAS FALSE — folding this query's JOIN to `ON true` vetted clean, turned 10 rows into 40 and " +
			"still passed, because every seeded address has the same shape. Re-folded after the equality " +
			"landed (5d5d0be4 + the fix): RED at that assertion, reporting a token under a different " +
			"human's address. Note what still does NOT fire under that fold — `len(got) != len(fixtures)` " +
			"stays green, because the duplicated rows collapse in the id-keyed map. HOW MANY rows redden " +
			"is NOT a property of the fix and must not be quoted as one: two independent runs got one of " +
			"four and three of four, both honest, because `ORDER BY u.email ASC` over freshly-generated " +
			"uuids decides which row wins each id's map slot, and only ids whose winner is the wrong user " +
			"redden. The MECHANISM is the durable half. Also: " +
			"revoked-sorts-last is asserted PAIRWISE WITHIN THE FIXTURE rather than table-wide, so the " +
			"shared live database's other rows may interleave without touching it"},
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

	// ── user_vaults.sql — ALL THREE UNPINNED, and this is the convergence worth naming ──
	// The per-user vault (Argon2 KEK + wrapped DEK) is the mechanism protecting every
	// sealed_with='dek' secret, and NOT ONE of its queries has executed against a database.
	// Measured, 0/0/0. It is not that nobody wrote a live test near it — four live-DB files
	// mention "vault", which is exactly the false positive a name-based scan would report.
	// They do not reach these queries: secrets_crud_livedb_test.go:24 states it passes a NIL
	// vault on purpose ("real box (nil vault → the master-box seal path, which is all these
	// tests need)"), a deliberate and locally reasonable choice that leaves the vault path
	// with no live exercise anywhere.
	//
	// 🔴 THIS MEETS THE WAVE'S OTHER VAULT FINDING. The same surface has /vault/unlock and
	// /vault/passphrase sitting unguarded by any limiter-mount assertion. So the vault is
	// simultaneously the place where a brute-force guard is unasserted and the place where no
	// SQL has been executed under test. Recorded here rather than left implicit in three rows,
	// because the two gaps are individually minor and jointly the weakest surface in the tree.
	{"GetUserVault", "user_vaults.sql", unpinnedPin,
		"0 executions. Production callers are vault.go:117, :200 and :265; every test in its path " +
			"is a fake (vault/vault_test.go:76, workersvc/vault_gate_test.go:27, " +
			"handler/vault_test.go:31), each returning a canned store.UserVault. So the SELECT that " +
			"fetches the KEK salt and wrapped DEK has never run against the real table"},
	{"CreateUserVaultIfAbsent", "user_vaults.sql", unpinnedPin,
		"0 executions — and it is the query in this file whose whole point a fake CANNOT stand in " +
			"for. Its `ON CONFLICT (user_id) DO NOTHING` exists to make two concurrent first-unlocks " +
			"safe: one insert wins, the loser gets pgx.ErrNoRows and re-reads the winner's row, so " +
			"the cached DEK always equals the persisted one. That is a race fixed by database " +
			"semantics, and a fake has no unique index to conflict on. The comment states the " +
			"consequence of getting it wrong (each request caching a different DEK than the DB " +
			"holds); nothing has ever made Postgres produce the conflict"},
	{"DeleteUserVault", "user_vaults.sql", unpinnedPin,
		"0 executions, and it has NO CALLER AT ALL — not a test, not production. `rg` over " +
			"internal/ and cmd/ finds only the generated method. It is a deliberate primitive for " +
			"PRD #32's password reset (its own comment says reset is out of scope and this is what " +
			"that flow will build on), so it is dead-but-intended rather than dead-by-accident. " +
			"Worth a row precisely because 'unused' and 'untested' are indistinguishable from a " +
			"coverage report, and only one of them is fine"},

	// ── settings.sql — the write path and the plain read are pinned; the LOCK is not ──
	{"UpsertAppSetting", "settings.sql", "TestSlackBotTokenSealedAtRestLiveDB",
		"direct call, slack_integration_test.go:191 (1 execution). Discriminating rather than " +
			"incidental: the test writes a sealed Slack bot token through it, then reads the row " +
			"back with raw SQL and asserts the stored value neither equals nor contains the token, " +
			"decodes as base64, and has no plaintext in the ciphertext"},
	{"ListAppSettings", "settings.sql", "TestSlackBotTokenSealedAtRestLiveDB",
		"2 executions in that test, reached through settings.New(q,0).AdminView() at :215 — NOT a " +
			"direct call, so a body scan sees the constructor and not the query. The AdminView it " +
			"builds is then asserted three ways (secret reported configured, the secret key absent " +
			"from Values, and no token or ciphertext bytes anywhere in the rendered struct), so the " +
			"rows this query returns are what those assertions are about"},
	{"ListAppSettingsForUpdate", "settings.sql", unpinnedPin,
		"0 executions. Its only caller is handler/settings.go:203, inside the settings PUT " +
			"transaction, and no live test drives that route. The `FOR UPDATE` is the entire " +
			"difference from ListAppSettings above: it row-locks so a concurrent PUT blocks and " +
			"reads this writer's committed values, which is what closes PRD #19 M2's cross-key " +
			"prd_label != autopilot_label TOCTOU. A lock that is never taken under contention is a " +
			"lock nothing has tested — and like the vault's ON CONFLICT, it is a property no fake " +
			"can exhibit, because a fake has no transactions"},

	// ── ci_autofix.sql — the ci-autofix loop guard (PRD #71 M4) ────────────────────────
	// All seven are pinned by one live test, TestCIAutofixLiveDB, verified green against
	// a throwaway Postgres 17 on 2026-08-10 (the whole internal/store LiveDB suite ran
	// clean at that tip). M4 builds these primitives; the M6 poller detector is their
	// production caller and does not exist yet, so the live test IS the only executor
	// today — that is deliberate, not a gap.
	{"ListCIAutofixCandidateRefs", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call. Discriminating in BOTH directions on every gate: one eligible ref (u1's " +
			"agent/issue-1) survives while each of the default-branch guard, the mr_iid guard, a " +
			"green (non-failed) pipeline, kind-awareness (a self_improve run on an agent branch), the " +
			"owner opt-out and the missing-token case is separately excluded — so dropping any one " +
			"predicate admits a neighbour and the len==1 check fires. Also pins the DISTINCT-ON " +
			"newest-run pick (an older no-mr_iid run on the same branch loses to the newer one, " +
			"asserted via mr_iid==101)"},
	{"GetCIAutofixAttempt", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: asserts ErrNoRows on a never-attempted ref and reads back count/signature/" +
			"pipeline/halt after each ledger transition below"},
	{"UpsertCIAutofixAttempt", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: first proceed INSERTs count=1, a second proceed increments to 2 and overwrites " +
			"last_signature/last_pipeline_id (sig-A/9001 → sig-B/9010) — the insert-then-increment " +
			"and the EXCLUDED overwrite both asserted"},
	{"RecordCIAutofixPipeline", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: the silent path moves last_pipeline_id to 9020 while attempt_count STAYS at 2 " +
			"(never advances), and on a never-attempted ref inserts a row at the default count=0"},
	{"SetCIAutofixHaltNotified", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: sets halt_notified true and stamps last_pipeline_id=9030, read back"},
	{"DeleteCIAutofixAttempt", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: reset-on-green returns 1 for a present row and 0 for an absent one (the no-op case)"},
	{"DeleteCIAutofixAttemptsNotIn", "ci_autofix.sql", "TestCIAutofixLiveDB",
		"direct call: seeds three refs, keeps one, asserts exactly 2 evicted and the kept ref survives"},
	{"GetActiveCIFixTargetForRef", "ci_autofix.sql", unpinnedPin,
		"Arrived in PRD #71 M6 for the detector's active-run swallow cap. No live-DB test in " +
			"internal/store executes it yet: its only production caller is the M6 poller detector " +
			"(internal/poller/ci_autofix.go), which is exercised by unit tests against a fake store " +
			"(poller is not in inventoryPackages, and has no live-DB test). The `status NOT IN " +
			"(completed,failed,cancelled)` active-set predicate and the pipeline_ref scoping are " +
			"therefore unpinned against the real schema — a live pin belongs with a future M-b " +
			"integration test, recorded here rather than left silent"},

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
