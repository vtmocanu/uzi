# PRD #98: Judge menu — a dedicated cross-run recommendation workbench

**GitLab Issue**: [#98](https://gitlab.example.com/vtmocanu/uzi/-/issues/98)
**Status**: In progress (2026-07-20) — branch `feature/prd-98-judge-menu`. **M8's e2e leg is deferred** until PRD #97 (e2e suite hardening) merges: it rewrites ~450 lines of `e2e/run-e2e.sh` and this PRD's e2e leg must be written against its `create_run` / `retry_read` / positive-control conventions, not the pre-#97 ones.

**Progress (2026-07-21, end of day)** — **FIRST MR MERGED.** [MR !90](https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/90) landed on `main` as `8515cfab` with CI green across every stage: all seven implementation milestones (M1-M7) plus M8a's docs half, 95 files, +15,594/−225. The migration shipped as `00081_judge_issue_close_sync.sql`, renumbered above the live head at landing (the draft `00075` collided with a *different* migration already on `main`; the `00076` gap was deliberately not filled, since a free number below the applied head is the boot-refusing case).

**A second branch, `feature/prd-98-followup`, carried the cheap tier of Remaining work** — four items closed there and reviewed. **MERGED 2026-07-21 as [MR !91](https://gitlab.example.com/vtmocanu/uzi/-/merge_requests/91), landing on `main` as `ad6c63d9`.** *(This line read "unmerged as of this writing" until 2026-07-21 late; it was stale for hours and a resuming session read it as live. A sentence containing "as of this writing" with no date is a claim that cannot be checked — it is always true of the moment it was typed and never true afterwards. The durable form is the SHA.)*

**A third wave — SIX branches, all based on `ad6c63d9`.** State as of 2026-07-21 late; SHAs are the
last observed tip, not a guarantee about now.

| branch | tip | state |
|---|---|---|
| `feature/prd-98-t2-web` | `abf039e4` | anchored-href render pin + first `OccurrenceFileIssue` tests. **Reviewed + audited, 0 Blocking.** |
| `feature/prd-98-tier2` | `536f9730` | M1 fold re-derivation + query inventory (10 files, 46 queries). Reviewed; **one BLOCKING row fixed**; audit of the fix open. |
| `feature/prd-98-t2-lim` | `a42b346d` | route mount-table test (**new**, user-added scope). **Reviewed + audited, 0 Blocking.** |
| `feature/prd-98-t2-fid` | `e30f97e0` | architect's design note: seam 6 **ruled**, backstop + M8b passes pending. |
| `feature/prd-98-t2-seam6` | — | seam 6 implementation, in progress. |
| `feature/prd-98-t2-cli` | — | M8b. **Not started** — blocked on the backstop design pass. |

### RESUME HERE (session end, 2026-07-21 late) — what is done, what is in flight

**FULLY VALIDATED (reviewer + auditor, 0 Blocking each), unmerged:**
`t2-web` @ `abf039e4` · `t2-lim` @ `b72b921b`.

**⚠️ `tier2` @ `f04f3a30` IS *NOT* FULLY REVIEWED — and I recorded it as validated before checking.**
The reviewer approved `528360d4`; **three code commits have landed since and only one was ever
dispatched to it**: `ba3f2cd9` (the widening to ten files, +220), `536f9730` (**the
`CountActiveSelfImproveRuns` blocking fix**, +128) and `321a25b2` (`user_vaults`/`settings`, +95).
Real unreviewed delta: **three commits, four files, +380/−49** — against a dispatch of mine that
called it a "small delta, one file at +95".
**The blocking fix is the part that matters.** I told the reviewer I would "send the fix SHA
separately" and never did, so the resolution of the one finding that gated this branch has had an
**auditor** verification but **no review** — and it is the row where the state choice (`UNPINNED`
vs `EXERCISED-UNASSERTED` vs `PINNED`) is the entire question.
**On resume: review `528360d4..f04f3a30` as one range** (skipping `aaba8992`/`f04f3a30`, which are
lead-authored docs). Reviewing `321a25b2` alone would credit the other two with a review neither
has had.
*(Caught by the reviewer checking the range against the branch before acting on a dispatch that
quoted it — the branch's own lesson, applied to the lead. A "validated" label is an assertion like
any other, and this one was written from memory of dispatches rather than from `git rev-list`.)*

**LANDED AT SESSION END BUT ⚠️ WHOLLY UNVALIDATED — no reviewer or auditor pass on ANY of the
commits below.** All pushed. Verified against the tree, not from the coders' reports:

**⚠️ A LEAD CLAIM CORRECTED BY THE IMPLEMENTER, because it would have mis-set the next session's
expectations:** I wrote in seam 6's dispatch that *"the fixture pins truncation regardless of demo
reachability, and the design already requires truncation as one of its cases"*. **It does not, and
it structurally cannot.** That came from this PRD's original framing list; the architect's design
**narrowed it** at A7 item 2 — the row cap and `truncated` are a SQL/service property that cuts
rows *before* grouping, the Go grouper never sees the cap, and they are assigned to **M8b/B4**. The
fixture therefore has **no truncation case**, correctly. Adding one would have been worse than the
gap: Go's cut lives inline in `JudgeRecommendationBacklog` (needs a `*Service` and a database),
and exporting a pure helper would test it at a value neither production path uses while the
interaction that actually matters (`Lim: JudgeBacklogMaxRows + 1`, which distinguishes a full page
from an exactly-full one) stays out of reach — a proxy that would *read* as pinning the cap.
**So truncation is pinned MOCK-side only (5 folds); the cross-implementation pin remains M8b/B4's.**

- **`t2-seam6` @ `37c4dd98`** — 6 commits, 9 files, **+5172/−69**. Seam 6 is *complete for its
  scope* (see the correction above for what that scope is NOT):
  `edc8e585` trims with Go's cutset, `c96e0f49` extracts the grouper to the server's layer,
  `70c1e301` adds the golden fixture (`fixtures/judge-fidelity/{cases,expected}.json` + README),
  `5429ebe9` the sort-tiebreak case, `71cc2a63` the demo-truncation toggle. **Both halves of the
  mock fix are in** — `Array.from(s)` for the rune count *and* a spelled-out cutset class, with a
  comment stating they are separate divergences. That was the thing most likely to land half-done;
  it did not.
- **`t2-cli` @ `79fada44`** — the backstop split by kind and executed in e2e; **`./e2e/run-e2e.sh`
  ran to completion: exit 0, 187 PASS / 0 FAIL**, clean `down -v`. Landing state: **3 rows executed
  against a live stack** (`uzi review undo` ×2 addresses, `uzi review show`, `uzi repo list`),
  **2 declared with reasons** (`review backlog --run` gated on B4; `uzi login` permanent — no flags
  declared, device-auth polling loop), **4 HELP entries** checked by path resolution.
  **M8b (Part B) is still NOT started** and belongs in this same worktree: both live in
  `e2e/run-e2e.sh` and one writer per file is the only safe arrangement. Part C's truncation-remedy
  row is gated on B4's 2001-row seed, so **B4 lands last, the two land together, and B4's author
  should extend the existing block rather than open a second one.** That row flips from
  `evidenceNotExecuted` to `evidenceE2E` in the same commit that lands B4.
  **TWO THINGS A COLD START CANNOT RECOVER, both now on disk and repeated here because this block
  is the entry point:**
  1. **The classifier's baseline is ZERO UNKNOWNs** — all 9 candidates resolved to a definite kind
     at landing, and none was resolved away. So a later `kindUnknown` is a **genuinely new emitter,
     not an original gap**, and widening the `emitters` set is a decision to record with its
     reason. It is the single edit that can quietly re-open the hole: every string printed through
     a wrongly-added wrapper becomes HELP and is exempt from the execution bar.
  2. **An open design question for the architect, implemented but not ruled.** Part C does not say
     how a candidate chooses between two nested entries — `uzi review backlog` (HELP) versus
     `uzi review backlog --run <run-id>` (RUNTIME), where the prefix matcher matches both and the
     shorter entry's derived kind is therefore ambiguous. The coder implemented **most-specific
     wins** (a candidate goes to the longest matching entry) and documented it at the site, because
     the kind derivation is incoherent without *some* rule. **Confirm or overrule on resume** — not
     a blocker, but it is a semantic decision currently made by an implementer.
- **`t2-lim` @ `8ce7ba50`** — 5 commits, 14 files, +1378. `c309e8a0` is the **admin CLI-token
  inventory (new product code)**; `537394fc` records the `/repos/{id}/sync` mutation in the file;
  `8ce7ba50` pins which config field each limiter is constructed from, **closing the construction
  residual** — and its control **came back AGAINST the proposed design**, which is why it landed as
  something else. The reviewer's stem-matching rule (variable stem ↔ config-field stem) is
  convention-brittle *and fails on the tree as it stands*: `cliPollLimiter` reads
  `cfg.CLIPollRateLimitMax`, and naive capitalisation gives `CliPoll`, not a prefix of `CLIPoll` —
  **Go initialisms break the transform before any refactor happens**, so the convention needs two
  standing exceptions, not one. Built case-insensitively (the strongest form, so as not to measure
  a strawman) it is green today but still reddens on a legitimate rename. What landed instead is an
  **exact declared table**, keyed by the `limForge`-style constant `limiterNames` already forces you
  to rename — green at zero edits across that same rename, and with no exception list at all.
  **The pattern, worth more than either incident:** twice now a parse mechanism was specified from
  *reading* the convention, and twice the convention had exceptions that only appeared when someone
  ran it. The hole was real both times; the proposed mechanism needed changing both times.

**So the validation debt is the whole story on resume.** Three branches carrying ~8,400 insertions
have had **no review and no audit**, including new product code (an admin endpoint) and a new
public artifact directory (`fixtures/`). The wave's own repeated lesson is that the implementation
was sound and the certifying layer was not — and right now there is no certifying layer at all for
this tier.

**The design is COMPLETE**: `prds/98-judge-menu-m8-design.md` on `feature/prd-98-t2-fid` @
`2befec6f` covers Part A (seam 6), Part B v2 (M8b) and Part C (backstop), all rulings folded in.
**Read it before resuming any of the three in-flight branches** — it is the specification, and
three places in it are known to be under-specified in practice: `expected.json` authoring for cases
8-10 (the answer depends on the mock fix being *both* changes), `UNKNOWN` classification in the AST
classifier (fail-closed is deliberate; widening the emitter list is a real decision, not a
nuisance), and B6a's probe placement (a probe landing before its window opens is **the constraint
biting, not flakiness**).

**Four follow-ups raised to the user, none of them this PRD's to close:** the limiter posture on
24 unasserted mounts (test now landed; the *construction* residual and the two unlimited
credential-minting routes remain), `forge-fake`'s `updated_after` fidelity, the `user_vaults.sql`
zero-execution convergence, and the wider rate-limit posture on `POST /api/me/cli-tokens/` and
`POST /api/workers/`.

**Two items of NEW scope the user added mid-wave, neither in the original PRD:**
1. **A route mount-table test.** It began as "our route's limiter is untested" and generalised three
   times under measurement to **all 24 per-user limiter mounts across six limiters, none asserted** —
   see the follow-up entry below. Landed as a test that walks the real `Routes(...)` table and names
   which limiter instance each route carries.
2. **An admin-wide CLI-token inventory** — the first new *product* functionality this wave produced.
   Everything else here is tests, fixtures and docs. It exists because the mount sweep surfaced two
   credential-minting routes with no limiter, and the severity read landed on something other than
   the rate: **workers are admin-visible, CLI tokens are not.** All six queries in `cli_tokens.sql`
   are per-user or by-hash, so **no admin-wide token inventory exists anywhere in the product**, while
   user-scope tokens carry no expiry. The missing limiter caps the rate; the missing visibility caps
   the consequence, and only the first had any remediation. Hard constraint on the build: **never
   project `token_hash`** — an admin-wide query that returns hashes converts a visibility fix into a
   credential-disclosure surface.

**THE PRD REMAINS OPEN.** Work resumes here, not in a follow-up PRD. The substantial remainder is: M8b (e2e), the **executing half** of the printed-instruction backstop, seam 6's golden fixture, and widening the query inventory beyond the judge family. **None is cheaper later than now** — they were deferred for scope, not because they are blocked.

**Read the checkboxes below, not a count.** This paragraph said "eight items remain, four of them substantial" and **two of the four were wrong within the day**, in opposite directions and both silently:
- the **filed-issues join** item was already CLOSED in the tree it was listed as open against (`45381961` + `8c6be2b8`, both ancestors of `ad6c63d9`) — the prescribed fixture change was sitting at `judge_recommendations_integration_test.go:383` with its folds recorded RED;
- the **printed-instruction backstop** had already landed its registry half (`api/cmd/uzi/instructions_test.go`, both scan tests, an AST lift and a vacuity guard), leaving only the executing half — so the item was real but half its stated scope was not.
A tally is a claim about a tree, and it goes stale exactly like a line number while reading as a summary. Both errors were caught by validators re-deriving the items against the code before any coder started, which is the cheapest place this class is ever caught: one was found while priming, before a single commit existed.

**What this PRD does NOT cover, stated because the alternative is a false claim:** no e2e coverage exists for it at all; mock↔server fidelity is not asserted (and the landing merge widened this — `mockApi` now emulates a composite-FK cascade); `OccurrenceFileIssue` is 236 lines with zero tests and is the only forge-writing web path here; and the query inventory test proves someone *declared* where a query is pinned, not that the pin is good. A claim that the judge backlog is covered end to end would be wrong on four counts.

### RESUME HERE — what is left, in order (updated 2026-07-21, late)

**All seven implementation milestones have landed.** M5 closed at `a48c5afe`, which was the
last one. What remains is documentation, one merge hazard that expires, and the MR.

1. **M8a docs** — `docs/judge.md`, `docs/cli.md`, `specs/ai.md`. **Do not describe deferred
   work as shipped**: no e2e coverage exists, mock↔server fidelity is not asserted (measured:
   0 divergences found, but the demo fixture cannot reach the two riskiest behaviours — see
   Remaining work), and `OccurrenceFileIssue` has no tests. `specs/ai.md` already carries M5's
   correction; the rest of its judge section has not been reviewed against the shipped code.
2. ✅ **DONE at the landing merge (2026-07-21) — the migration is `00081_judge_issue_close_sync.sql`.**
   It was `00075`, which by then was **TAKEN on `origin/main` by a different migration**
   (`00075_run_message_instance.sql`) — a duplicate version, not merely a low one, so two
   failure modes stacked: strict goose with no `allow-missing` refuses to boot below the
   applied head, and a duplicate version is its own problem before that. It was described in
   passing as a renumbering chore; running the check rather than inheriting that framing is
   what found it.
   **Re-derived immediately before merging, not inherited:** `origin/main` @ `6080e12b`, head
   `00080`, derived 2026-07-21T13:59Z — so `00081`. `00076` was left alone: it is a gap on
   `main` and a free number *below* the applied head is exactly the boot-refusing case.
   **The reference sweep was re-run on the MERGED tree and the earlier measurement held:**
   exactly one reference to renumber besides the file itself (M6's draft-number note below).
   The merge brought further `00075` references, **every one of them naming main's**
   `00075_run_message_instance.sql`: in live code (`runtime.sql.go`, `queries/runtime.sql`,
   `run_message_instance_integration_test.go`), in `specs/ai.md`, in `mocks/data.ts`, and in a
   neighbouring migration's own comment (`00077_user_secret_labels.sql`). Alongside them sit
   the archived PRDs' historical draft numbers — `adr/0042`, `prds/done/42`, `/46`, `/49`, and
   after the merge `/99` and `/104` too.
   **Cited as a FILE LIST and a mechanism, not a tally, and that is a correction to what this
   entry first said.** It read "six further hits … plus four `prds/done/` + `adr/` hits …
   would have corrupted all ten", which conflated LINE counts with FILE counts and was
   understated the moment the merge added two more archived files. The durable claim is the
   classification — *code/spec hits are main's, archived hits are other PRDs' history, exactly
   one line is ours* — and a blind `sed 00075→00081` corrupts every one of them. The number is
   whatever the tree holds when you read this; the classification is the finding.
   **Provenance, stated precisely because the timing decides what this is evidence OF:** the
   auditor sent the same seven-file list as a mid-merge warning, and it arrived **after** the
   merge was committed. So this is **two derivations reaching the same answer without reading
   each other**, not "the auditor warned and the coder complied" — a message that arrives too
   late to have caused anything cannot be credited with having done so. The independence is
   the finding; a warning would have been worth less.
3. **Merge `origin/main`**, then re-run the three gates (api, web, `./e2e/run-store-it.sh`)
   before the MR. The live sweep needs its positive control checked, not just its exit code.
4. **Then the MR**, after a review pass.

**Superseded resume items, kept only so a stale copy of this file is recognisable:** the
in-flight fixture fix (item 1 as of midday) is closed — `bulkFixture` writes distinct
rationales per row and the folds were run; the bulk query's own filed-issues join is pinned
at `31080a40`. M5 (item 2) landed at `a48c5afe` with the together-mount test the item asked
for. See the milestone entries for what was measured.

**Working rules:** `.claude/agent-team.md` (tracked) — the citing-across-a-moving-tree rules and the repo's measured traps; `CLAUDE.md` for the live-DB green rule, the sqlc fold-shape rule and the never-glob-`uzi-` docker rule. **The pre-MR migration gate is at the top of this file (see "RESUME HERE"), not in a handoff doc** — it must be re-derived, never inherited. *(Corrected 2026-07-21: this line used to point at `.claude/agent-team-tasks/prd-98-m3-checkpoint.md`, which `.gitignore:27` excludes, so after the merge the pointer resolves to nothing — and what it sent readers to fetch was the migration gate. The durable sections were migrated to `.claude/agent-team.md` in `234236c2`.)*

- **Done, reviewed + audited**: M1 (`0874d3f6`), M2 (`30204a61`, later collapsed to one atomic statement), M6 (`d6a8545c`), M4 (`1da5ac32`), M3 (`c629ce28`), M7 (`de2d8de3`). Plus a merge of `origin/main` (`ad5abca1`, 38 commits — PRD #97 landed, which unblocked M8b).
- **In this MR**: M5, M8a docs, and the last open Blocking (a measured-false CLI instruction).
- **Still open IN THIS PRD after the MR** — see "Remaining work" below. Four items, all with recorded evidence.
- **Handoff notes**: the durable rules live in `.claude/agent-team.md` and `CLAUDE.md`, both tracked. The pre-MR migration gate is in the RESUME block at the top of this file. *(The former `.claude/agent-team-tasks/` checkpoint is gitignored and does not survive the merge — see line 813, which says so.)*

**What the review loop cost and bought** (recorded because it shaped the design): 3 Blocking in M3's first wave, then **every Blocking after M3 was about evidence rather than behaviour** — seven SQL projections no test executed, tests asserting properties of `encoding/json` rather than of the service, assertions credited for gates they sit behind, comments crediting guards in other files, and two printed CLI instructions that had never been run and were false. The implementation was sound throughout; the layer certifying it was not.

**Superseded progress note (2026-07-20, end of day)** — 5 of 8 milestones landed on `feature/prd-98-judge-menu`, no MR yet:
- **Done + reviewed + audited**: M1 (`0874d3f6`), M2 (`30204a61`, rewritten to one atomic statement in `082d8651`/`c962435d`), M6 (`d6a8545c`), M4 (`1da5ac32`). Full Go + web + live-DB gates pass at the tip.
- **Done, review PENDING**: M3 (Judge page + nav, `c629ce28`) — the largest milestone, first substantial `web/` surface. Gates green (web typecheck + 837 tests + build; api build/vet/test) — 837 is the vitest count **at `c629ce28`**, not a current figure, the four validator pre-flags built in and test-pinned, but it has **not yet had a review/audit wave** — that is the first task next session. Six implementation decisions are flagged for confirmation in the M3 checkpoint; one (anchored deep-link defaulting to `bucket=all`, not `todo`) is a product-behaviour call touching M5.
- **Not started**: M7 (CLI), M5 (notification retarget — its `/judge` + `?run=` route dependency is now satisfied), M8a (vitest + docs + specs). **M8b (e2e) stays blocked** on #97.
- **One Blocking bug** found and fixed before merge: a duplicate-coordinate `SQLSTATE 21000` crash on legal judge output, invisible to fakes (the M2 fan-out collapse; fixed with `DISTINCT ON` on the resolved member set, `c962435d`).
- **Open follow-ups**: **AK** — a bucket-literal→constant sweep is partial; the producer side (`BucketOf` in `triage.go`, #94's shared helper) is deliberately left as a cross-PRD change, with `TestBucketConstantsMatchTheLadder` pinning the coupling meanwhile. **AL** — a comment-precision split in `judge_bulk_disposition.go`. Both tracked in the M3 checkpoint.
- The Design Decisions and Risks below were **corrected repeatedly against the code during implementation** and are current; several load-bearing claims (the `?run=` semi-join, the `EXPLAIN`-measured backlog cost, the `set_via` provenance mirror, the two-layer dedup asymmetry) were rewritten after being disproved by execution rather than review.
**Priority**: Medium
**Mockup**: static concept mock (ember shell + buckets + worklist + the three deltas) at the design artifact; **note** it renders the worklist grouped *by run* — a precursor. This PRD supersedes that with **group-by-target + dedup** (Decision 2); a revised in-repo mock lands with M3 as `prds/mockups/98-judge-menu-mock.html`.
**Depends on**: PRD #46 (the judge: `run_reviews` + `review_recommendations`, `users.judge_enabled`), PRD #68 (`recommendation_filed_issues`, the coordinate-keyed claim-first file flow), PRD #94 (`recommendation_dispositions`, the `bucketOf` ladder, `GET /me/judge/stats`, the global RunsList strip this promotes). Related: PRD #64 (the `uzi` CLI, second consumer), PRD #69 (the judge **control plane** — mode/model/spend/accuracy/consent; this PRD is the complementary output workbench, cleanly separable — see Decision 5's digest-scope note), PRD #47 (RunHealth badge — the per-row badge grammar this mirrors).
**Review**: fable adversarial pass on the concept mock folded in (2026-07-20). Load-bearing corrections adopted: **group by target with cross-run dedup**, not by run (the same recommendation recurs across runs and must be one triage decision, with frequency as the priority signal); **one canonical to-triage number** single-sourced from `triage.todo` so the nav badge, the notification, and the page tab cannot drift; the **/runs badge uses one verdict-first grammar** (`⚖ issues · 2`), not the mock's two grammars; the **triage state machine is already closed by #94** (the mock's flat three-button row under-represented it — Done / Dismiss ▾ Won't-do|Not-an-issue already exist). Deferred by review + user scoping: keyboard triage and target-file staleness → Future Work.
**PRD review**: a second fable adversarial pass, this one on the PRD itself and verified against the code (2026-07-20). Corrections folded in: **Filed→Done must be edge-triggered** — the poller holds a synced `issues.state` *snapshot*, not transition events, so a naïve "upsert done while closed" re-fires every tick and would make Undo impossible and overwrite a `dismissed` member; fixed with a `close_synced_at` edge marker + `INSERT … ON CONFLICT DO NOTHING` (Decision 6) — **so this PRD is NOT migration-free** (Decision 6 ships one migration). **Bulk file-as-one-issue is descoped to a follow-up** — it needs a repo pick, a human draft gate + the #68 sanitizer, `forgeLimiter`, and reverses #68's cookie+CSRF posture; v1 keeps clean bulk *disposition* + per-rec browser filing (Decision 3 + Open Questions). **M5 re-sequenced after M3** (it deep-links `/judge`, which M3 creates). The **/runs todo count buckets in Go, not a second SQL ladder** (#94 Decision 2 forbids a SQL `CASE`; Decision 7). Honesty edits: the grouped read is a **new, wider query** (same join *shape* as `/stats`, but adds a `runs` join + the filed row — Decision 1); **filed issues are NOT owner-scoped** (#68 Decision 8 lets an admin file on another user's review — Decision 6).

## Problem

The judge produces good output with no cross-run home. Every judged run gets a
verdict plus structured recommendations (`review_recommendations`, six
categories), and today that output surfaces in exactly three places: a
`judge_review` notification in the inbox, a **count-only** strip on the `/runs`
header (PRD #94, "Judge recommendations · all your runs"), and the full
`JudgePanel` on each run's detail page. What is missing is the place you actually
*work* the backlog:

- **There is no cross-run worklist.** To act on a recommendation you deep-link
  into one run at a time. The strip tells you *how many* are open; it gives you
  nowhere to *do* them.
- **The same recommendation is re-triaged in every run it appears in.** "Add
  `rg` to the worker image" or "coder re-ran a failing test without reading the
  error" recurs across many runs. Each occurrence is an independent coordinate
  with independent triage state, so the user files or dismisses the *same idea*
  N times. Nothing surfaces that a recommendation is recurring — which is
  precisely the signal that it is worth acting on.
- **Recommendations are about improving the factory, not reviewing a run.** The
  six categories (`enable_tool`, `install_worker_tool`, `adjust_template`,
  `improve_agent`, `add_agent`, `improve_uzi`) are all "make the factory better"
  actions. Anchoring them to the run that happened to surface them is the wrong
  spine for the work.

## Solution Overview

A dedicated **Judge** menu — a top-level destination in the **Factory** nav
group — that is the cross-run recommendation workbench. It reads the exact
owner-scoped aggregate PRD #94 already computes, but **dedups by
`(category, target)` across all your runs** and lets you triage a whole group in
one action.

- **Group by target, dedup across runs; frequency is the priority.** One row per
  `(category, target)`, with a "seen in N runs" evidence chip and an expander
  listing each run occurrence (run title, verdict, per-run triage state).
  Recurrence, not the judge's self-reported confidence, is the trustworthy
  ranking signal.
- **A group *disposition* action fans out to its member coordinates — no new
  storage.** The group is a *display* construct over #94's per-coordinate rows.
  "Dismiss" / "Mark done" applies the disposition to every open member (`bucket ==
  todo`); bulk multi-select does the same across several groups. **Filing** in v1
  stays the existing #68 per-recommendation browser draft (reachable from a
  group's occurrence expander); **bulk "file N as one issue" is a follow-up** — it
  needs a repo pick, a human draft gate, and a forge limiter #68 already imposes
  (Decision 3, Open Questions).
- **The notification stays a ping — its deep-link just retargets here.** No
  event is removed from the inbox; the `judge_review` row and its Slack DM now
  deep-link to `/judge?run={id}` instead of `/runs/{id}`. Mirror, don't move.
- **The strip leaves `/runs`; each run row gains a verdict badge.** The global
  count strip becomes the Judge page's bucket header. In its place, every run row
  gets a one-grammar `⚖ verdict · N` badge — a per-run glance it never had.
- **Inbox grouping + Filed→Done sync close the loop at scale.** The in-app inbox
  groups consecutive judge rows (no Slack digest — Slack DMs stay one-per-review);
  and when a filed issue *closes*, its recommendation auto-moves to Done — once,
  edge-triggered off the poller's synced issue state, never overwriting a human's
  own verdict.
- **One canonical number.** The nav badge, the notification, and the page's "To
  triage" tab all read `triage.todo` from #94's shared `bucketOf` helper. "Seen
  in N runs" communicates the grouping without minting a second count.
- **CLI parity in the same MR** (`uzi review backlog` + bulk *disposition* verbs;
  filing stays browser-only), per the CLAUDE.md second-consumer rule.

## Design Decisions

1. **The Judge page is a new READ endpoint — the *same join shape* as #94's
   `/stats`, but a genuinely new, wider query; the read model itself needs no
   migration.** `GET /api/me/judge/recommendations` (`RequireUser`, owner-scoped,
   all-time). #94's `ListJudgeTriageRowsForUser` returns only three columns
   (`queries/dispositions.sql`, the flat per-rec `(disposition_status,
   filed_settled, category)` the Go `BucketOf` needs). This endpoint keeps that
   join's spine — `run_reviews` where `user_id = caller` → `review_recommendations`
   → LEFT JOIN `recommendation_filed_issues` + `recommendation_dispositions` on the
   `(review_id, category, target)` coordinate — but **additionally joins `runs`**
   (for `issue_title` → `run_title`) and selects `run_reviews.verdict`, the rec's
   `confidence`/`rationale_md`, `rec_id`, and the filed row's `filed_issue_iid`/URL.
   So it is a **new query**, not a reuse of `ListJudgeTriageRowsForUser` verbatim —
   same shape, wider projection. It returns the rows **grouped by `(category,
   target)`**. Per group: `{category, target, occurrences: [{run_id, run_title,
   review_id, rec_id, verdict, confidence, bucket, filed_issue?}], open_count,
   run_count, rationale_preview}` where `rationale_preview` is the most-recent
   occurrence's `rationale_md`, truncated and length-capped, shipped as **plain
   text — NOT server-side HTML-escaped**. (Corrected 2026-07-20 against the code,
   which this PRD had wrong: the no-raw-render guarantee is **client-side**.
   `RunView.tsx:959` renders these fields "as escaped plain text (React's default
   + whitespace-pre-wrap), never markdown/HTML", and `apitypes/review.go:8` ships
   the scrubbed free text raw; secrets and control chars are already stripped at
   the review-POST ingest (`workersvc/judge_review.go`). Escaping server-side
   would double-escape in the SPA and print HTML entities into the terminal from
   `uzi review backlog`.) Each `bucket` comes from the shared **`bucketOf`** (PRD #94
   Decision 2 — same helper, no re-implementation), so the page's tab totals and
   the nav badge equal the existing strip exactly. A
   `?bucket=todo|filed|done|dismissed|all` filter (default `todo`) and a `?run=`
   anchor (for the notification deep-link, Decision 4) bound/scope the pull.
   **Response shape, settled at implementation (2026-07-20) — M2/M3/M7 build on
   this, they do not re-derive it:** the response is an envelope `{bucket, run,
   groups[], triage}`, not a bare array, and **`triage` comes from #94's own
   `/me/judge/stats` query, called directly — not tallied off the page rows.**
   (Revised 2026-07-20 once the pull became bounded: with a `LIMIT` in play,
   tallying `triage` off the returned rows would make the canonical number *wrong*
   on exactly the heavy accounts that need it most. Sourcing it from the stats query
   makes "nav badge == notification == To-triage tab" **literally the same query**
   rather than equal-by-construction, and it survives both filters and truncation.
   Cost: one extra cheap 3-column query per request.) Each
   group carries its explicit rollup `bucket` alongside `open_count`/`run_count`, so
   neither the page nor the CLI recomputes the ladder. `?run=` filters **which
   groups** return (those with ≥1 occurrence in that run) but never trims a kept
   group's occurrence list — arriving from a notification must still show that the
   recommendation recurs elsewhere, since that recurrence is the priority signal.
   **`?run=` is therefore a coordinate-level SEMI-JOIN in SQL, not an equality
   predicate and not a Go post-filter** (settled 2026-07-20; this note previously
   said "applied in Go, do not fix it into the WHERE clause" and was superseded
   within the hour — see below). The naive reading is a false dilemma: an equality
   `WHERE rv.target_run_id = @run` does trim exactly the occurrences Decision 4
   exists to preserve and corrupts `run_count`, but a Go post-filter is not the only
   alternative. The shipped form selects *coordinates* —
   `EXISTS (… WHERE rv2.user_id = rv.user_id AND rv2.target_run_id = @run_anchor
   AND rr2.category = rr.category AND rr2.target = rr.target)` — so a group opened
   from a notification keeps its other-run occurrences while coordinates absent from
   the anchor run drop out, and the bound applies in the database rather than after
   a full materialization. The subquery is scoped to the caller's own reviews, so an
   anchor naming another user's run matches nothing (no oracle).
   **Once the row cap exists, a Go post-filter is not merely inferior — it is a
   defect.** The `LIMIT` would apply *before* the anchor filter, so an anchored
   request whose coordinates fall outside the newest `JudgeBacklogMaxRows` rows
   would return empty while reporting `truncated: true` — the notification
   deep-link's worst case, silently, on exactly the heavy accounts the cap exists
   for. In SQL the bound applies after the anchor. The two designs were equivalent
   before the cap and are not equivalent after it, which is why this note is
   load-bearing rather than stylistic.
   **`nullableUUID`, not the shared `pgUUID`, carries the "no anchor" case**: `pgUUID`
   always sets `Valid=true`, so passing `uuid.Nil` would have sent the all-zero uuid
   as a *real* anchor, the query's `IS NULL` escape hatch would never fire, and
   **every unanchored backlog request would have returned nothing**. Caught by the
   live-DB test, not by a fake.
   A malformed uuid or unknown bucket is a **400**; a well-formed unknown/foreign
   run uuid is an **empty list, not a 404** (no existence oracle) — so a CLI typo
   can never look like an empty backlog.
   **Exception, user-decided 2026-07-21: an ANCHORED `/judge?run={id}` defaults to
   `?bucket=all`, not `todo`.** Un-anchored `/judge` still defaults to `todo` as
   above. The reason is a direct consequence of Decision 2's cross-run dedup: a
   notified run's coordinate may already be **settled via a different run**, so a
   `todo` default would deep-link a fresh `judge_review` notification to an
   apparently-empty page, with no hint the item exists under another bucket — the
   worst possible first impression for the feature, and precisely the "row vanished"
   confusion the `bucket=all` re-read exists to prevent elsewhere. M5's notification
   deep-link therefore lands on `all`. Do not "correct" this back to `todo` for
   consistency with the un-anchored default; the two defaults differ deliberately. `rationale_preview` is capped at
   `RationalePreviewMaxRunes = 280` **runes** (never bytes — a byte cut splits
   UTF-8), ellipsis appended only on an actual cut. Occurrence order is
   `rv.updated_at DESC` (most-recently-**judged** first, so a re-judge counts as
   recent), not `created_at` — a re-judge upserts in place, bumping `updated_at` and
   leaving `created_at`, so `created_at` ordering shows staler preview text than the
   group actually contains. **This depends on a property a future PRD could silently
   break:** `judge.sql`'s `ON CONFLICT (target_run_id) DO UPDATE … updated_at =
   now()` is currently the **only** writer of `run_reviews.updated_at` anywhere in
   `queries/` (verified 2026-07-20), so the column moves only on a re-judge. If any
   later change adds a status-change or bookkeeping write to that column, this
   ordering silently stops meaning "freshest rationale" and the preview regresses
   without a test failing. **The
   read is migration-free; the PRD as a whole is not — Decision 6 ships one
   migration.**

2. **Grouping grain is `(category, target)`; occurrences stay per-run; triage
   state stays per-coordinate.** Because #68/#94 key filed/disposition on
   `(review_id, category, target)` — **per review** — the "same" recommendation in
   two runs is two coordinates with independent state. The menu groups them for
   display and priority; a group is **not** a stored entity and needs no new
   table. Consequences, stated so the counts are unambiguous:
   - The **nav badge and the "To triage" tab count are per-recommendation** (the
     existing `triage.todo`), NOT per-group — so they equal PRD #94's strip and
     `uzi review stats` to the digit. "Seen in N runs" carries the grouping.
     (This is the deliberate resolution of the review's competing-numbers worry:
     one canonical to-triage number; the group count is never a second badge.)
   - **"Open" means `bucket == todo`** (a *filed* member is not open — it is on the
     ladder's `filed` rung). `open_count` = members with `bucket == todo`; a group
     is under **To triage** iff `open_count ≥ 1`. A fully-settled group rolls up to
     the highest state among members via the #94 ladder (`dismissed > done > filed
     > to-do`) — so a group of 3 done + 1 dismissed shows under **Dismissed**
     (highest wins; a display quirk, documented). The occurrence expander always
     shows the per-run truth, so a mixed group (2 dismissed, 2 open) is never
     misrepresented.

3. **A group *disposition* action FANS OUT to member coordinates — reusing #94's
   mutation semantics unchanged, owner-only. Bulk *filing* is descoped to a
   follow-up.** The one new bulk endpoint in v1:
   - `PUT /api/me/judge/recommendations/disposition`
     `{items: [{category, target}], status, reason?, scope: open|all}` — resolves
     the caller's member coordinates for each item and upserts a disposition on
     each (idempotent per #94 Decision 6; re-stamps the `rationale_hash` per its
     Decision 3). `scope=open` (default) touches only members with `bucket == todo`
     (Decision 2's definition — a filed member is left filed); `all` re-asserts.
     Returns the updated groups. **Owner-only by construction**: every member is
     caller-owned — the resolve is scoped `user_id = caller` (the
     `SubmitInput(user.ID)` / #94 Decision 5 strict-ownership pattern, verified in
     `handler/review_disposition.go`), so a uza_ `admin_ro` token can only ever
     dispose its **own** rows and `IsAdmin` is never consulted. This is the clean
     half: a local, non-forge, non-spend upsert applied N times.
   - **Bulk "file N as one issue" is NOT in v1** — fable's PRD pass showed it is a
     mini-PRD, not a #68 reuse. #68's `FileIssue` (`handler.go:680-687`) files into
     a **user-picked repo** (`GetRepoForUser`, caller-owns-repo — and #68 Decision 4
     notes the default repo is *unresolvable* for 4 of 6 categories for
     non-admins), behind a **human-editable draft** it calls "the primary control"
     for cross-project secret leakage (#68 Decision 10's fence/`/`-strip/secret
     scan), on the **cookie+CSRF `RequireAuth` path with `forgeLimiter`**, and is
     deliberately **browser-only** (filing is out of scope for the CLI, #68
     Interactions). A bulk endpoint on `RequireUser` with a server-templated
     aggregate body would drop the repo pick, the draft gate, and the limiter, and
     reverse the auth posture (letting a uza_ token drive a forge *issue* write). So
     **v1 keeps per-recommendation filing on the existing #68 browser draft**,
     reachable from a group's occurrence expander; **bulk file-as-one-issue (repo
     pick + aggregated draft + limiter + posture decision) is a follow-up PRD.**
     (Recorded as an Open Question — the one scope item to confirm.)
   - **Multi-select** across groups (the checkbox bar) is a multi-item call to the
     disposition endpoint — the UI batches selection, the API takes a list of
     `(category, target)` coordinates in one round-trip.
   - **Settled at implementation (2026-07-20), both deliberate:** the N upserts are
     **not wrapped in a transaction**. Each is local, side-effect-free and
     last-writer-wins, with no forge write and no spend to make exactly-once — #94
     Decision 6's own reasoning — so a partial failure is safely retried and
     converges. **A mid-fan-out failure surfaces as a plain 500 and the partial
     apply is NOT reported** (measured, not reasoned, in the M2 review: with the
     2nd of 3 upserts failing, one upsert had landed, the service discarded the
     `updated` counter and returned a generic 500, and the re-read never ran).
     That is accepted: a 500 makes no false claim of success, the landed subset is
     visible on the next read, and a retry converges because every upsert is
     idempotent. What is NOT accepted is describing it otherwise — the requirement
     is that the endpoint never *claims* completeness it does not have. Moving to a
     partial-success report (207, or 200 with a `partial` flag) would be a design
     change, deliberately not taken in v1. The response is `{updated, groups, triage}` with **`groups` re-read at
     `bucket=all`** — the subtle part: a just-dismissed group has left To triage but
     must still come back so M3 can re-render the row instead of having it vanish
     mid-interaction. **M3 must not "optimize" that re-read down to the active
     filter**; doing so reintroduces exactly that flicker. `items` is capped at
     `JudgeDispositionMaxItems = 100`, deduplicated **before** the cap check so the
     cap counts distinct work rather than body length.
   - **The item cap does NOT bound the fan-out — members-per-coordinate does, and
     it is unbounded** (M2 audit, 2026-07-20). One coordinate matches *every*
     occurrence across *all* the caller's reviews, and the resolve carries no
     `LIMIT`, so ≤100 coordinates in a ~4 KB body can drive tens of thousands of
     sequential upserts, each its own round-trip, holding a pool connection, on a
     mount with no rate limiter. Self-inflicted, own-data and idempotent, so not a
     vulnerability — but it made M2 materially less bounded than M1, which caps at
     2000 rows. **Resolution: collapse the N upserts into ONE multi-row `INSERT …
     ON CONFLICT` driven by `unnest` of the RESOLVED coordinates.** That removes the
     round-trip amplification and the partial-failure window in a single move while
     keeping the resolved-not-body invariant intact — and it supersedes the
     "fail-fast, partial apply surfaces as a 500" note above, since one statement
     cannot partially apply. **That collapse REQUIRES a `DISTINCT` on the
     coordinate** (verified against live Postgres, 2026-07-20, before the code was
     written): `review_recommendations` has **no unique constraint on `(review_id,
     category, target)`** — only `pkey(id)`, the partial `improve_uzi` index, and
     `idx(review_id)` — because a judge may legitimately emit the same coordinate
     twice in one review. So the resolve can return that coordinate twice, and a
     multi-row upsert over it raises `ON CONFLICT DO UPDATE command cannot affect
     row a second time` (SQLSTATE 21000) at runtime: rare, data-dependent, and
     invisible to any fake. `dedupeCoords` does **not** cover this — it dedupes the
     *request* coordinates, while the duplication arises inside the *resolved member
     set*. **The member set stays deliberately UNBOUNDED in v1, and a hard cap was
     considered and rejected** (2026-07-20). Collapsing to one statement removed the
     round-trip amplification and the partial-apply window, but not the bound: the
     item cap bounds **coordinates** (100), not **members**, since one coordinate
     matches every occurrence across all the caller's reviews. What the collapse
     traded is many short autocommit statements for **one statement whose parameter
     arrays scale with member count, in a single transaction holding row locks on
     every affected row for its duration** — better on round trips, longer on lock
     hold under concurrency. A cap returning 400 was drafted and **withdrawn**: it
     would make a large group *permanently un-dispositionable*, which is precisely
     the failure the SQLSTATE 21000 crash caused and which this PRD just fixed —
     reintroducing it by policy rather than by bug is not an improvement. If it is
     ever bounded, the shape is a `LIMIT` on the resolve paired with a `truncated`
     signal so the client repeats (mirroring M1), **never** a rejection. The
     operation is own-data, idempotent and authenticated; the honest
     characterisation lives in the query comment where the next reader will find it.
     Two arguments settled it, both stronger than "the heaviest user is
     inconvenienced". **A single group can exceed any cap on its own** — members
     expand per coordinate, so one coordinate recurring across a long history blows
     the cap by itself, and there is nothing smaller for the user to select. The
     action becomes permanently impossible for *precisely* the recommendation this
     feature exists to surface, since frequency is the priority signal and the
     most-recurring group is both the most valuable and the first to exceed. And
     **members are invisible to the client** — the UI shows groups and `run_count`,
     and nothing anywhere tells a user that three selected groups expand to 4,000
     members — so "select fewer" is guess-and-check against a quantity they cannot
     observe. An unactionable error is bad; one that is unactionable *in principle*
     is a dead end.
     **If protection is ever wanted, the instrument is a per-user rate limiter on
     the `/me/judge` group, not a member cap.** Re-derived at `415d08bb`: that group
     mounts `RequireUser` and nothing else — no limiter — and carries all three
     routes. The real exposure there is **repetition, not any single request's
     size**: `GET /me/judge/stats` is completely unbounded and, per the measured
     plan above, seq-scans `review_recommendations` in full on every call. A member
     cap would harden the one route that is already atomic and self-limiting while
     leaving the two cheaper-to-abuse reads open. The codebase already has the
     pattern (`judgeLimiter`, `forgeLimiter`, `hostedLimiter` are per-user
     middleware). Deliberately **not** done here: it should be triggered by an
     observation rather than a theory.
     The per-member loop was immune only because each upsert was its own
     statement, which is a reason nobody had written down until changing the shape
     removed it. **This was missed on the first attempt and shipped a hard 500** —
     worth recording *why*, because the reason is structural rather than careless.
     The bulk suite's fake `memberRow` helper mints `ReviewID: uuid.New()` per
     member, so every member the fake can produce carries a **distinct**
     `review_id` — the fake is incapable of constructing the colliding triple. The
     headline test of the new statement (`TestBulkDispositionIsOneRoundTrip`, 500
     members on one coordinate) passes precisely because those 500 members all have
     different review ids, and no live-DB test seeded a duplicate. **The one shape
     that breaks the write was the one shape no existing test could construct.** The
     `DISTINCT` must therefore be keyed `(review_id, category, target)` on the
     RESOLVED member set, and the fixture helper must be able to mint members
     sharing a `review_id` — otherwise the next change hits the same wall. A test
     helper that cannot construct the failing input silently bounds every test built
     on it.
     **The dedup key is the full `(review_id, category, target)` triple and MUST NOT
     be reduced to `(category, target)`** — caught during implementation, before it
     shipped. Keying on the pair looks natural, because the pair is what the request
     carries and what the group is named by. But members legitimately repeat a
     coordinate **across different reviews**, and that recurrence is the entire
     premise of this PRD. Deduping on the pair would have silently disposed **one
     run per group instead of all of them** — the fan-out this endpoint exists to
     perform. It is the same shape as the crash it was fixing: a guard whose
     correctness depends on a distinction nobody had written down. The dedup test
     therefore carries a **negative control** — reducing the key to the pair must
     fail with "wrote 1 members, want 2".
     *(This note originally said the pair-keyed version would have shipped "with
     every existing test still green". **Measured false** in the `e4934c2c` review:
     re-keying to the pair is caught in two independent places — the fake-backed
     control, and three live-DB tests led by `TestBulkDispositionFansOutAcrossRunsLiveDB`
     with `updated = 1, want 3 (one per run the coordinate recurs in)`. So the suite
     would have caught it; the coder self-caught it earlier, which is better, but the
     safety net was real. The claim was the implementer's, relayed by the lead
     without checking — the same inherited-assertion failure this PRD keeps finding,
     this time in the PRD's own prose. The audit added the symmetry that explains
     it: those pre-existing multi-member fixtures build members with `memberRow`,
     which mints a **fresh `ReviewID` per member**, so every one of them is
     implicitly a *cross-review* fixture — exactly what a pair-keyed dedup destroys.
     **The same fixture limitation that made the same-review duplicate
     unconstructible, and so hid the 21000 crash, is what would have exposed the
     pair-key mistake.** One property, two opposite effects, depending on which bug
     you are hunting.)*
   - **The Go dedup layer is deliberate defence-in-depth, and adding it was not
     free — state both halves.** On the live path it is **dead code by
     construction**: the resolve is `SELECT DISTINCT ON (…)`, so it cannot return a
     duplicate triple and `seen[key]` can never fire against a real database. It is
     exercised only by fake-backed tests — which is the point, since a fake cannot
     model SQL and a fake-backed duplicate test would otherwise be theatre. It earns
     its place the day someone relaxes the `DISTINCT ON` or adds a second caller of
     the write. **But divergence between the two layers is asymmetric, and the
     dangerous direction is the new one:** a wrong *SQL* key is masked by the Go
     layer (which still keys on the conflict key, so the statement stays legal —
     degraded, not broken), while a wrong *Go* key is **not** masked by SQL, because
     the Go pass runs downstream and can *remove* members the SQL correctly kept —
     silently under-disposing. So the second layer added a way to be wrong that did
     not exist before it, which is why the Go layer is the one that must carry the
     test. "Add a second layer" is not automatically free. (Implementation detail
     that is safe *for a reason* rather than by luck: `seen[key]` is marked **before**
     the scope switch, so a member excluded by scope still consumes its key — fine
     only because duplicates of one triple always carry identical
     `disposition_status`/`filed_settled`, since those joins are on the coordinate
     and not on `rr.id`, so both copies take the same scope branch anyway.)
   - **Why "write the resolved row, never the body" is defence-in-depth rather than
     the mechanism** (recorded so a future refactor does not undo it): the resolve
     matches by *equality* (`want.category = rr.category AND want.target =
     rr.target`), so for any row that matches, the resolved values are
     byte-identical to the body values. The actual security mechanism is the JOIN
     plus the owner predicate yielding **zero rows** for anything bogus. The two
     become observably different only if the match is ever loosened
     (case-insensitive, `LIKE`, trimming) — at which point writing from the body
     would start writing attacker-shaped text. So the rule stands even though no
     test can distinguish it today; that limit is inherent to the design, not a
     coverage gap.

4. **The notification is KEPT as a ping; only its deep-link retargets to the
   Judge menu.** The `judge_review` payload already anchors `run_id` + `review_id`
   (#46/#94) and the inbox is a generic surface (`notifysvc` untouched — "the
   judge is simply tenant #1"). No payload change; two link changes:
   - Slack DM: `reviewDeepLink` (`handler/judge_worker.go:318`, today
     `baseURL + "/runs/" + targetID`) → `baseURL + "/judge?run=" + targetID` (a
     true one-liner).
   - Web inbox: **kind-conditional, not a one-liner.** `Notifications.tsx` links
     `/runs/${n.run_id}` generically for **any** kind carrying a `run_id`
     (`Notifications.tsx:59`), so the retarget is a `kind === 'judge_review'` guard
     that routes to `/judge?run={id}` while every other kind keeps `/runs/{id}`.
   `/judge?run={id}` opens the menu scrolled/filtered to that run's occurrences —
   which requires the M1 endpoint's `?run=` anchor (Decision 1) or a client-side
   occurrence filter, so **M5 depends on M3** (the route + the filter), not Phase
   1. The inbox row itself stays — the ping's job ("a review landed while you were
   away") is preserved; only its destination changes.

5. **Digest is web-inbox only; NO Slack digest.** At factory throughput a stream
   of one `judge_review` row per finished run floods the *in-app inbox*, so
   `Notifications.tsx` groups consecutive `judge_review` rows under one expandable
   "N reviews ready" header, keyed on `kind` + a time window. Rows are still
   individually persisted (their `run_id`/`review_id` anchors and read-state
   survive) — grouping is render-only, no new storage, no scheduler.
   - **Slack DMs are left exactly as they are — one DM per review, un-throttled,
     un-batched** (the existing #46/#94 best-effort judge DM). This PRD changes
     only its **deep-link** (Decision 4), never its cadence. No throttle, no
     roll-up, no `judge_dm_throttle` state. [user-decided 2026-07-20]
   - **Scoped to `kind == judge_review`.** The inbox grouping keys strictly on the
     `judge_review` kind, so it never groups a *different* judge notification — in
     particular PRD #69 M7a's deterministic pre-start **infra-skip** notification
     (a distinct kind) renders as its own row. And because there is no Slack digest
     at all, nothing can delay that infra DM — the #56 (Slack notifications UX)
     seam evaporates.

6. **Filed→Done sync — edge-triggered off the poller's synced state, never
   overwriting a human verdict. This is the PRD's one migration.** #68's
   `recommendation_filed_issues` links a coordinate to a forge issue
   (`filed_issue_iid` + `filed_repo_id`). Crucially, **the poller has no transition
   events** — `issues.state` is a synced *snapshot* (`store/migrations/00002_forge.sql`;
   `forgesvc` FullSync/IncrementalSync upsert the cache). A naïve "upsert `done`
   while the linked issue's cached state is closed" is **level-triggered**: it
   re-fires every tick, so a human **Undo** is silently re-applied on the next
   sync, and reusing #94's `ON CONFLICT DO UPDATE` upsert would **overwrite a
   member the user had already `dismissed`** with `done`. Both are wrong. So:
   - **Edge marker:** add `close_synced_at TIMESTAMPTZ` (nullable) to
     `recommendation_filed_issues`. The post-sync pass acts on a linked issue only
     on the open→closed **edge** (cached `state = closed` AND `close_synced_at IS
     NULL`), then stamps `close_synced_at` — exactly once per close.
   - **Never overwrite:** the disposition is written `INSERT … ON CONFLICT DO
     NOTHING` (NOT #94's DO-UPDATE upsert), so a coordinate the user already
     dismissed/marked keeps their verdict, and after Undo deletes the row the edge
     is already consumed — Undo **sticks**. A reopen does not re-open (no
     auto-reopen; flapping avoided); a re-close does nothing (`close_synced_at`
     set).
   - **Provenance + ownership:** the row carries `set_via='issue_close'` (a nullable
     column on `recommendation_dispositions`, default `NULL`) so the UI labels
     "done via #IID"; `set_by_user_id = NULL` marks the system action.
     **A human write MUST clear `set_via` back to `NULL`** (found in the M6 review,
     2026-07-20, and measured against live Postgres). The tests covered
     dismiss-then-close; the **reverse order was wrong**. #94's
     `UpsertRecommendationDisposition` DO-UPDATE sets status, reason, hash,
     `set_by_user_id`, `set_at` and `updated_at` but **never touches `set_via`** —
     the column did not exist when that query was written. So a human overriding an
     auto-done leaves a row claiming `set_by_user_id = <the human>` **and**
     `set_via = 'issue_close'` simultaneously, and the UI would render that human's
     `dismissed` verdict with system provenance — destroying exactly the
     auto-vs-human distinction this decision exists to preserve. It is the precise
     mirror of PF-4: that stops a system action being attributed to a human, this
     attributes a human action to the system. **Fix: `set_via = NULL` in that
     DO-UPDATE**, plus a live-DB test for the auto-done→human-override ordering
     beside the existing reverse-order one. **This must land before M3 renders the
     label**; M6 created the interaction by adding the column, so M6 owns it.
     *(Both validators found this independently and proposed different one-liners:
     `set_via = NULL` versus `set_via = EXCLUDED.set_via`. They are equivalent
     today only because the INSERT column list omits `set_via`, so `EXCLUDED` is
     NULL. `NULL` is chosen deliberately: it states the invariant — a human write
     always means human provenance — instead of depending on a column list
     elsewhere in the same statement staying as it is. If someone later adds
     `set_via` to that INSERT list, the `EXCLUDED` form would silently start
     carrying system provenance through a human write, with no edit to the line
     that guarantees it. That is exactly the class of latent breakage this PRD has
     been finding all run.)*
     **Postscript (2026-07-21): the visible half is what makes the invisible half
     checkable.** `set_via` reached the wire only at M3's B3 fix — before that it
     lived entirely inside `api/internal/store`, so no consumer could distinguish an
     auto-done from a hand-marked one and the PRD documented a label that did not
     exist. The moment the field became visible, the **mock reproduced this exact
     misattribution**: `mockApi`'s disposition upsert used `Object.assign(existing,
     next)`, which copies only the keys `next` carries, so a human overriding an
     auto-done **kept** `set_via='issue_close'` and the chip went on reading "Done
     via #91" after the user had overridden it. That is precisely what the literal
     `NULL` above prevents server-side, re-created client-side the instant the field
     had a reader — and it was uncatchable before, because nothing could observe the
     value. Fixed in both mock write paths with an end-to-end override test. The
     general lesson: **a provenance field no consumer reads cannot be tested, only
     asserted** — descoping the visible half would have left the invisible half
     permanently unverifiable. The
     disposition lands on the **review owner's** coordinate regardless of who
     filed — **filed issues are NOT owner-scoped** (#68 Decision 8 keeps admin
     filing on another user's review; `filed_by_user_id` may be an admin).
   - **Join the issue cache on `(repo_id, forge_issue_iid)` — never on iid alone —
     and skip rows whose `filed_repo_id IS NULL`** (audit requirement, 2026-07-20,
     verified against the code). `issues` is keyed `ON CONFLICT (repo_id,
     forge_issue_iid)` (`queries/forge.sql:174`): an iid is **per-project, not
     global**. Since `filed_repo_id` is `ON DELETE SET NULL`, a NULL-repo row joined
     on iid alone would match any repo's issue with that number — closing issue #7
     in repo X would auto-Done a recommendation filed as #7 into repo Y, cross-repo
     and possibly cross-user. Excluding NULL-repo rows makes the documented
     disabled-repo no-op below a **safe** no-op, not just a silent one.
   - **Preconditions (documented limits):** the issue cache only holds
     **PRD-labeled issues of enabled repos** (`forgesvc/service.go` — reconcile
     evicts de-labeled issues), and `filed_repo_id` is `ON DELETE SET NULL`. So a
     filed issue that loses its PRD label, or whose repo is disabled/disconnected,
     is no longer observable and **won't auto-Done** — a silent no-op, called out
     here and in `docs/judge.md`, not a bug.
   The #94 ladder then buckets an auto-done as **done** — it leaves To triage and,
   for `improve_uzi`, the self-improvement backlog (#94 Decision 9). Rides the
   existing poll loop; no new worker. **Migration:** `set_via` on
   `recommendation_dispositions` + `close_synced_at` on `recommendation_filed_issues`
   — one migration, draft-numbered above the live head and renumbered at merge per
   CLAUDE.md.

7. **Per-run verdict badge on `/runs` — verdict via a safe single join, the count
   bucketed in Go (never a second SQL ladder); the strip moves out.** `RunListItem`
   (`web/src/lib/api.ts` — today carries `mr_state`, `health`, … but **no** judge
   field) gains `judge_verdict` (`ideal|ok|issues|null`) and `judge_todo_count`.
   Two different mechanisms, deliberately:
   - `judge_verdict`: a **safe LEFT JOIN `run_reviews` ON `target_run_id = run.id`**
     in the list query (`handler/runs.go` / `ListRunsForUser`). `target_run_id` is
     UNIQUE, so this stays strictly one-row-per-run — no fan-out.
   - `judge_todo_count`: **NOT** computed in SQL. Joining through
     `review_recommendations` would fan the run list out (≤50 recs/review → up to
     50 duplicate run rows, breaking `ListRunsForUser`'s one-row-per-run
     contract), and counting `todo` in SQL means re-implementing the ladder's
     bottom rung (`disposition IS NULL AND filed_at IS NULL`) — which #94 Decision
     2 categorically forbids (no SQL `CASE`, one Go `BucketOf`). Instead the
     handler fetches the per-rec rows **for the runs on the page** and buckets them
     with the shared `BucketOf`, attaching `judge_todo_count` per run in Go.
   The row renders **one** compact badge, verdict-first with the count appended
   only when `> 0` (`⚖ issues · 2`, `⚖ ideal`) — a single grammar, fixing the
   mock's two-grammar bug, mirroring the RunHealth badge (#47). Click → the run's
   `JudgePanel` (unchanged). The global `TriageSummary` strip is **removed** from
   `RunsList.tsx` (PRD #94 Decision 8's header render + its `getJudgeStats` call) —
   but that removal lands **with M3** (the Judge page header is its new home), so
   the aggregate count is never homeless. `GET /me/judge/stats` stays.

8. **The empty / inbox-zero state is a first-class view — because to-triage = 0
   is the goal.** When `triage.todo == 0`, the Judge page is not blank: it shows a
   recent-verdict trend (last N runs' verdicts, from the same join), recently
   Filed / Done groups, and — if the user has not opted into the judge
   (`users.judge_enabled`, #46/#69) — an opt-in card linking Settings. A
   badge-less nav item most of the week is expected; the zero state is what keeps
   the destination worth opening and earns the top-level slot.

9. **Nav placement: the Factory group, after Workers.** `<NavGroup label="Factory">`
   (`AppShell.tsx:304`) today holds Agents / Skills / Workers; Judge joins as
   `<NavItem to="/judge" label="Judge" badge={judgeTodo} />`. The categories are
   all factory-improvement actions, so the group is thematically exact. The badge
   is the existing `NavItem` unread-count pill (`AppShell.tsx:106-145`, `>99 →
   99+`), fed by a poll of `/me/judge/stats.todo` owned by `AppShell` alongside
   the notifications-unread poll (`AppShell.tsx` M2 poll). Name is **Judge** (not
   "Reviews"): it matches the run kind (`RunKindJudge`), the `JudgePanel`, the
   `uzi review` CLI group, and the Settings opt-in; "Reviews" collides with
   MR/code-review connotations. The page subtitle ("Recommendations across all
   your runs") carries the backlog framing.

10. **CLI parity — `uzi review backlog` + bulk *disposition* verbs**
    (`api/cmd/uzi/review.go`, the #94 group). `uzi review backlog [--bucket
    todo|filed|done|dismissed|all] [--run <run-id>] [--json]` prints the deduped groups (`category ·
    target · seen in N runs · open N`) from the M1 endpoint; `uzi review
    resolve|dismiss --category C --target T [--reason wont-do|not-an-issue]` drives
    the M2 bulk **disposition** endpoint (group fan-out). **No `uzi review file`** —
    filing stays browser-only (#68 Interactions kept it out of the CLI, and bulk
    file-as-one-issue is a follow-up, Decision 3). The existing per-run `uzi review
    show/resolve/dismiss/undo/stats` stay. The web-only surfaces (nav badge, inbox
    grouping, per-row `/runs` badge) have no CLI analogue and are called out as
    such.
    **Correction 2026-07-21: `--run` was added, and this decision's flag list did not
    originally name it.** The omission was treated as deliberate during M7 and the flag
    was left out; the review then measured why that was wrong. The truncation warning
    told the user to "re-check with `uzi review backlog --bucket all`", and **no bucket
    value can reach what the cap cut**: `truncated` is computed and the rows sliced
    *before* `filterGroups` runs, so every bucket truncates identically (measured with
    the cap lowered to 2 against a 9-row fixture — `all`, `todo`, `filed`, `done` and
    `dismissed` all returned `truncated=true`, and `all` returned the same surviving
    groups as `todo`). The `?run=` anchor is the **only** predicate pushed into SQL
    *before* the `LIMIT` (Decision 1's semi-join), so it is the only parameter that can
    change what gets cut — which made it the only possible remedy for a warning the CLI
    was already printing. The flag list described the surface being specified, not a
    prohibition on the endpoint's other parameter, and CLAUDE.md's second-consumer rule
    points the same way: a route capability only the web drives is the "CLI silently
    stale" case. Both truncation warnings now name `--run` (and `--json` on the original
    call, which is the only complete record of what a write did).

**Interactions (for completeness):** **Hosted workers / claim / agent code** are
untouched — API + web + CLI + poller only. **Run deletion** cascades the review,
its recommendations, dispositions (#94 CASCADE) and filed links (#68) away; a
deleted run simply drops out of the owner-scoped join, so its groups shrink or
vanish with no orphan work. **`improve_uzi` self-improve backlog** (#94 Decision
9) is unchanged in mechanism: a group dismiss/done or a Filed→Done sync writes the
same disposition rows the engine already excludes on. **Board cache** is
untouched by triage (no forge write); only the *file* path writes a forge issue,
exactly as #68 already does.

## Milestones

- [x] **M1 — Grouped read model (api)** — DONE `0874d3f6`; review wave dispatched.: `GET /api/me/judge/recommendations`
      (`RequireUser`, owner-scoped, `?bucket=` filter + `?run=` anchor) — the new
      **wider** query (the #94 join shape plus the `runs` join for `issue_title` and
      the verdict/confidence/filed projection, Decision 1), returning groups keyed
      `(category, target)` with the occurrence list, `open_count` (= `bucket==todo`),
      `run_count`, and a plain-text (NOT server-escaped) `rationale_preview`.
      **Read is migration-free.**
      Store/handler test: dedup groups the same `(category, target)` across ≥2 runs
      into one group with a correct occurrence list; the bucketed totals equal `GET
      /me/judge/stats` for the same fixture (shared `BucketOf`, no re-implementation).
- [x] **M2 — Bulk disposition (api)** — DONE `30204a61`; review wave dispatched.: `PUT .../recommendations/disposition`
      (Decision 3) — coordinate fan-out reusing #94's idempotent disposition upsert,
      `scope=open|all` (`open` = `bucket==todo`). Owner-only authz matrix (owner fans
      out; non-owner → 404; uza_ `admin_ro` → 404 on another user's rows, allowed on
      its own; `IsAdmin` never consulted); idempotent double-call; a partial group
      (some members already settled) dismisses/marks only the open ones. Depends on
      M1 (shared coordinate-resolve helper + DTO). **Bulk filing is NOT here** — see
      the follow-up note below. **Audit requirements folded in 2026-07-20** (each
      re-derived from the code): this endpoint is the **first place a
      `category`/`target` arrives from a request body**, and `00071`/`00073` both
      carry a verbatim comment that they omit a category CHECK *on purpose* because
      "the handler never accepts a category from the request body" — so the
      disposition row must be written off the **resolved** recommendation, never
      echoed from the body (a bogus coordinate then resolves to zero members and
      writes nothing); `len(items)` is **capped** (400 above it) since this is N
      resolves + N upserts on a no-CSRF token path; and the response carries **no
      per-item existence oracle** — "absent" and "another user's" both yield zero
      members (#94 Decision 5's one-404 rule). The `admin_ro`-on-another-user's-rows
      test asserts at the **DB level** (their row unchanged), not on HTTP status:
      with coordinates there is no id to 404 on, so a status-only assertion is
      vacuous.
- [x] **M3 — Judge page + nav (web)** — DONE `c629ce28`; reviewed + audited. Web gates green
      (`typecheck` + 837 vitest **at `c629ce28`**; the suite has grown since — cite the SHA,
      not the tally). **One interpretation to confirm:** an anchored
      deep-link (`/judge?run={id}`) with no explicit `?bucket=` defaults to the `all`
      bucket, not `todo`, so a notification reliably shows that run's recs even when its
      coordinates rolled up settled elsewhere (dedup); un-anchored still defaults `todo`.
      The AL comment-precision fix in `judge_bulk_disposition.go` is folded into this
      commit.: route `/judge` (with the `?run=` filter for
      the notification deep-link); `<NavItem>` in the Factory group with the
      `triage.todo` badge poll (Decisions 8/9); bucket tabs from `triage`; the
      deduped worklist (group rows + "seen in N runs" + occurrence expander +
      per-group overflow **Mark done / Dismiss ▾**, and **per-recommendation File
      issue via the existing #68 browser draft** from the occurrence expander); the
      multi-select checkbox bar → bulk **disposition** M2 calls with an undo toast;
      the **inbox-zero** state (Decision 8). **Also removes the `TriageSummary` strip
      from `RunsList.tsx`** (its aggregate moves to this page's header — Decision 7).
      `mockApi.ts` + `data.ts` render every state. A revised in-repo mock
      `prds/mockups/98-judge-menu-mock.html` (by-target). Depends on M1 + M2.
- [x] **M4 — `/runs` per-row badge (web + api)** — DONE `1da5ac32`; review dispatched.
      **Three behaviours settled at implementation, recorded so M3 does not re-decide
      them differently:** (a) the `judge_todo_count` read is **best-effort** — a
      failure logs and leaves counts at 0 rather than 500-ing the whole run list,
      because a badge is decoration and an ornament must not cause an outage;
      (b) an **unjudged run renders no badge at all**, not a neutral pill — "never
      judged" and "judged and fine" are different claims and a placeholder asserts
      the second (the same reason the DTO field is nullable rather than defaulted);
      (c) the **verdict survives a cleared backlog** (`⚖ issues` with count 0),
      because the badge reports the judge's finding, not the triage state. All three
      are pinned by tests so the intent is visible to the next reader.
      **Trap for anyone adding a field here:** the Go and TS type structures are NOT
      symmetric — Go has separate `RunDTO`/`RunListItemDTO`, while TS has
      `RunListItem extends Run`. A field added to the TS `Run` therefore *inherits*
      into `RunListItem` instead of erroring, silently telling the client that
      `GET /runs/{id}` returns something the API only ever sends on the list. Caught
      here by `tsc` via the run-view fixtures, and only by that.
      **The verdict join is safe by INVARIANT, not by predicate** (M4 audit): `LEFT
      JOIN run_reviews rv ON rv.target_run_id = r.id` carries no user predicate, and
      is correct only because *two* things both hold — the outer query filters
      `r.user_id = @user_id`, **and** a review's `user_id` always equals its target
      run's owner (`PostReview` binds `UserID: target.UserID`). The count query, by
      contrast, is scoped in its own right (`WHERE rv.user_id = @user_id AND
      rv.target_run_id = ANY(@run_ids)`), so a spoofed run id yields nothing. The
      cheap hardening is `AND rv.user_id = r.user_id` on the join — free (the planner
      has both columns) and self-standing, exactly as `ee834583` made the `?run=`
      semi-join self-standing.
      **The count has TWO silent-understatement paths, both indistinguishable from a
      genuine 0:** a failed count read (best-effort → renders `⚖ issues` instead of
      `⚖ issues · 3`) and truncation, if the page cap and `JudgeRunTodoMaxRows` ever
      diverge. Acceptable for decoration — and in both cases the load-bearing half,
      the verdict, is unaffected because it rides the join rather than the count.: `judge_verdict` via the safe
      single `run_reviews` join + `judge_todo_count` **bucketed in Go** (never a SQL
      ladder; Decision 7); `RunListItem` type + the one-grammar per-row badge.
      Independent of the endpoints (own join) — starts immediately. (The strip
      *removal* is M3's, so the aggregate is never homeless.)
- [x] **M5 — Notification retarget + inbox grouping (web + api)** — DONE: `reviewDeepLink`
      → `/judge?run=` (one-liner); the inbox link retarget as a
      **`kind==='judge_review'` guard** (not a one-liner — `Notifications.tsx` linked
      `/runs/${run_id}` for any kind, Decision 4); web inbox grouping of
      consecutive judge rows (Decision 5). **No Slack digest** — Slack DMs keep their
      one-per-review cadence, only the link changes. **Depends on M3** (deep-links
      `/judge` + its `?run=` filter) — Phase 3, not Phase 1.
      **What landed, and the reasoning that is not visible from the diff:**
      - The link lives in ONE pure function, `notificationLink(kind, runID)` in
        `web/src/lib/notifications.ts`, so the guard is unit-testable away from a render.
        **The gate is the NON-judge case**, and the test enumerates five kinds rather than
        one: the judge row lands on `/judge` under both the correct guard and the
        unconditional URL change, so a test that checks only the judge row passes under
        exactly the bug the guard exists for. A single non-judge case would also be
        satisfied by a guard spelt `kind !== 'run_failed'`.
      - **The API side is a plain URL change and that asymmetry is deliberate**, recorded at
        `reviewDeepLink` itself: it is called from exactly one place, the judge review
        notification, so it is judge-only by construction. The web surface renders EVERY
        kind, which is what makes the same edit a guard there. `reviewDeepLink` had **no
        test at all** before this; it has one now, covering the anchor, the trailing-slash
        base and the empty base (no link rather than a broken one).
      - **The third `triage.todo` consumer reads the value, it does not poll for it.**
        `JudgeTodoContext` gained a READ side (`JudgeTodoValueContext` / `useJudgeTodo`).
        Polling `/me/judge/stats` from the inbox is a defensible reading of "read the
        canonical count" and it is wrong for the BLK-BADGE reason: a shared SOURCE without
        shared PROPAGATION is exactly the configuration in which the nav badge read 3 while
        the tab read 0. The value is `number | null`; `null` (no provider) renders **no
        number**, because a displayed 0 is the claim "nothing left to triage" and a
        provider-less component has not been told that.
      - **Grouping is a pure partition** (`groupNotifications`), asserted as such: every row
        appears exactly once, in order, so read-state, ids and offset paging are untouched.
        A run of ONE stays a plain row. An unparseable timestamp **breaks** the run — `NaN >
        window` is false, so the arithmetic alone would fold an unknown-age row in silently.
      - **Demo fixture: one non-judge row added to `data.ts`, placed BETWEEN two judge rows.**
        It renders all three inbox states at once (an ungrouped judge row with its `/judge`
        link, a non-judge row with its `/runs` link, and a grouped pair). Before it, demo
        mode showed only the group — and the grouping is precisely what would have hidden
        the retarget from anyone looking at the demo.
      - **`mockApi.notifications.test.ts` was rewritten to DERIVE from the fixture.** Adding
        that one demo row turned five of its six tests red, none for a reason about mockApi:
        they had snapshotted the fixture's ids and counts. The property is the mock's
        semantics (own-scoping, newest-first, offset paging, unread bookkeeping); the
        fixture's shape never was, and pinning it made `data.ts` unable to gain a row.
      - **Two existing tests changed meaning and were moved off `judge_review`**: the inbox's
        "renders a run deep link" case and its paging case. The first had become a test of
        the judge path wearing the generic path's name; the second was asserting on 30
        rows that now collapse into one header, so it was measuring the grouping instead of
        the paging.
      - **`navBadgeText()`'s selector had to be anchored** (`/^Judge/`): the group header
        carries its own "Open Judge" link, so the old substring match found two links and
        threw. Same shape as the repo's `role="status"` ambiguity trap, in a new place.
      **Negative controls, each RED then restored green:** (1) header renders `items.length`
      instead of the canonical count → 3 of the together-mount tests fail; (2) the
      notification holds a frozen copy (the own-poll shape) → the post-dispose agreement test
      fails while the other two pass, which is what proves that assertion is load-bearing
      rather than riding its neighbours; (3) `notificationLink` written as the unconditional
      `/judge?run=` URL change → the non-judge gate fails in both the lib and the page suite,
      and **no judge-row assertion moves**.
      **Also corrected in the same commit:** `specs/ai.md` said the recommendation free text
      "stays on the run page behind the deep link" — true until this milestone moved the
      link. And `judge_notify_test.go` asserted the old `/runs/` URL; it was found by the
      suite, not by a grep for `reviewDeepLink`, because it pins the literal string.
- [x] **M6 — Filed→Done sync (api/poller) — the migration** — DONE `d6a8545c`
      (migration **landed as `00081`**; drafted as `00075`, renumbered at the landing merge
      on 2026-07-21 — `00075` was by then TAKEN on `main` by a different migration); review wave
      dispatched.: add `set_via` on
      `recommendation_dispositions` + `close_synced_at` on
      `recommendation_filed_issues` (draft number above the live head, renumber at
      merge); the post-sync **edge** pass (cached `state=closed` AND `close_synced_at
      IS NULL` → `INSERT … ON CONFLICT DO NOTHING` a `set_via='issue_close'`,
      `set_by_user_id=NULL` done → stamp `close_synced_at`), Decision 6. Test: a
      close drops the rec from To triage and (for `improve_uzi`) the self-improve
      backlog; **Undo sticks** (next tick does not re-apply); a coordinate the user
      **dismissed** is **not** overwritten; a reopen does not re-open. Builds on
      #68/#94 + the existing poll loop — parallel from the start.
- [x] **M7 — CLI (`api/cmd/uzi/review.go`)** — DONE `de2d8de3`; reviewed + audited: `uzi review backlog` (grouped,
      `--bucket`, `--json`) + `resolve`/`dismiss --category/--target` (Decision 10);
      **no `file` verb** (filing stays browser-only). Tests cover the grouped output
      and the bulk disposition fan-out. **Correction, measured 2026-07-21 while
      implementing M7: this milestone used to also ask for "a uza_ token refused on a
      bulk disposition mutation", and that test cannot honestly exist.** There is no
      refusal to assert on this route: it is owner-only BY CONSTRUCTION (the service
      resolves members under `user_id = caller`) and coordinates are not ids, so a uza_
      token aimed at another user's coordinate gets **200 `updated: 0`** — identical to a
      misspelt or already-settled coordinate, which is exactly Decision 5's
      no-existence-oracle rule working. The branch's own
      `judge_bulk_disposition_livedb_test.go` had already written this down: "the authz
      case the PRD calls for has no id to 404 on … a status-only assertion is therefore
      VACUOUS." A CLI test scripting a 404 here would have gone green only because the
      fake returned an error the real server never sends. What the CLI *can* get wrong is
      presenting that silence as a completed action, so
      `TestReviewGroupZeroUpdatedIsNotReportedAsSuccess` pins that instead — and the
      genuine 404 refusal stays covered where it genuinely exists, on the per-run route
      (`TestReviewMutationRefusedForReadOnlyToken`). Depends on M1 + M2.
## What this PRD learned about pinning SQL, and the evidence for it (2026-07-21)

Recorded here rather than in `.claude/agent-team-tasks/` because **that directory is
gitignored** (`.gitignore:27`) — rules written there die with the worktree, which is the
session's own thesis landing on the session. The short, findable form of the two most
reusable facts is in `CLAUDE.md`'s api section; this is the rationale and the evidence.
Agent-process rules (citing across a moving tree, instructions expiring) are in
`.claude/agent-team.md`.

**THE RULE, stated so it names a query SHAPE rather than a file list:** *any LEFT JOIN onto
a coordinate-keyed side table needs a fixture where two rows in one review share EXACTLY ONE
half of the coordinate.* A file list catches two of the four sites below; the shape catches
all four. The failure it prevents is specific and it recurred four times: a fixture whose
rows differ in *both* halves at once makes every half individually inert, so the weaker
mutation passes while only the both-halves mutation fails — and a passing weaker mutation
reads as coverage.

| site | state before this PRD's sweep |
|---|---|
| `ListJudgeRecommendationRowsForUser` (backlog) | coordinate halves pinned at `45381961`; **review halves were not** |
| `ListJudgeTriageRowsForRuns` (`/runs` badge) | **nothing pinned at all** until `2e941ced` |
| `ListOwnedRecommendationsForCoords` (bulk resolve) | review half pinned; coordinate halves inert; the `f` join never exercised |
| `ListJudgeTriageRowsForUser` (#94 stats) | every half individually inert; only the both-halves fold caught anything |

**All four are now pinned**, each half reddening its own named assertion, every fold run on a
fresh database with a positive control asserted. The `/runs` badge query and the #94 stats
query needed new tests; the backlog query needed new fixture rows (a cross-review coordinate,
a cross-category one, a claimed-but-not-filed one, a second tenant, and a run the caller owns
but does not request).

**THE POSITIVE CONTROL HAS THREE CLAUSES BECAUSE THREE DIFFERENT MECHANISMS PRODUCE "no
failures observed" WITH NOTHING EXECUTED**, and no single weaker check catches all three:

| mechanism | what it looks like | what misses it |
|---|---|---|
| the suite skipped (no DSN) | `rc=0`, both packages `ok`, `RUN=n PASS=0 SKIP=n` (`n` was 108 that day, 128 within hours — `PASS=0` is the finding) | exit code, "no FAIL lines" |
| the mutation silently did not apply | a real, fully green run of **unmutated** code | everything except diffing the file |
| the run never happened | `run-store-it.sh` exits 1, log holds only "postgres never became ready", `PASS=0 SKIP=0 FAIL=0` | "no FAIL lines" |

The third was observed twice on 2026-07-21 by two agents on two different commands.

**THE POSITIVE CONTROL CATCHES TWO OF THESE THREE, NOT ALL THREE, AND THE DISTINCTION IS
LOAD-BEARING.** `PASS > 0` **and** `SKIP == 0` **and** the named test appearing as
`--- PASS`/`--- FAIL` catches the skipped suite and the run that never happened — which is why
the rule is not "check the exit code". It **cannot** catch the silently-unapplied mutation, and
no property of the *run* can: the suite genuinely runs, every assertion genuinely executes, the
control passes cleanly, and the result is green because the code under test was never mutated.
Only comparing the TREE sees that one, which is why `.claude/agent-team.md`'s "assert the
mutation actually applied — not just that the test ran" is a **separate** rule and must stay
one. A reader who believes the control covers all three will drop the tree comparison as
duplicated effort, and that is the one of the three that has already produced a false green on
this branch. (Corrected 2026-07-21: the lead originally wrote "one check catches all three",
which the table above already contradicted.)

Note also that the control was written before the third mechanism was seen and caught it
anyway: that is the argument for a mechanism over an enumeration of known failure modes.

**The tenant boundary, and the precise claim.** Neither `recommendation_filed_issues`
(00071) nor `recommendation_dispositions` (00073) has an owner column. `filed_by_user_id`
and `set_by_user_id` are `ON DELETE SET NULL` **attribution** pointers — nullable, and NULL
by design for every M6 auto-done — not ownership. Ownership reaches both tables *only*
through `review_id → run_reviews.user_id`, and `WHERE rv.user_id = @user_id` scopes `rv`,
not the joined side table. **The production code is correct**; what was missing was any test
that could observe a break in it. Do not summarise this as a shipped leak.
Corollary worth keeping: the natural "hardening", `AND d.set_by_user_id = @user_id`, would
**silently drop every auto-done**.

**A PRD that makes another PRD's query load-bearing inherits its coverage.** Decision 1 calls
#94's stats query directly so the nav badge, the page tab and M5's notification are literally
the same query rather than equal-by-construction. That choice buys the guarantee *and* the
risk: a broken coordinate half there makes all three consumers read the same wrong number and
agree perfectly, so the cross-check the design relies on cannot fire. The decision that buys
the guarantee inherits the obligation to cover it.

**Two mechanisms that turned out to matter more than any individual finding.**
- **A positive control on every mutation run** — assert the named test appears as
  `--- PASS`/`--- FAIL` and that `SKIP` is 0. Three agents independently leaned on weaker
  evidence (exit code, "no failures printed", a contention argument) before this was
  measured; see `CLAUDE.md` for the measurement. It caught a real dead fold in this sweep,
  *and* it caught its own regex being wrong first.
- **Compile the mutation before believing it.** Four separately-prescribed folds this session
  did not build, one of them prescribed inside the correction of another.

**A fold must be SELECTIVE, not merely discriminating.** A fold that reddens a spread of
assertions manufactures several confidently-wrong diagnoses along with the true one. Measured:
`f.filed_at -> now()::timestamptz` reddened every assertion ORing `FiledAt.Valid` in with
other fields, several blaming join predicates that were never mutated;
`f.filed_at -> d.set_at` reddened exactly one.

**Cite the assertion, not the line — and not the tally either.** Line numbers drifted three
times in one session from comment edits alone; an assertion *count* drifted the same way
(one agent said three, another five, both right for their own tree).

## Remaining work — OPEN IN THIS PRD after the first MR (2026-07-21)

Five items. All found by execution, all with evidence recorded here or in the M3 checkpoint.
**These are PRD #98 work, not a follow-up PRD** — resume here.

> **⚠️ THE CHECKBOXES BELOW ARE STALE AS OF 2026-07-21 LATE. READ THE `RESUME HERE` BLOCK AT THE
> TOP OF THIS FILE INSTEAD.** Five of the seven unchecked items **landed today** — the backstop's
> executing half, seam 6, the query-inventory widening, the anchored deep-link render pin, and N2.
> They are **deliberately not ticked**: every one is **unreviewed and unaudited**, and a ticked box
> for unvalidated work is precisely the false completion this PRD has spent itself removing. Tick
> them when a validator has cleared them, not when a coder has reported them.
> **Genuinely open: M8b (the e2e leg), and M8a's e2e half which is the same work.**
> Not duplicating per-item status here on purpose — two places to update is how the entries above
> went stale in the first place.

- [x] **DONE `0da9186a` (reviewed) — An unscoped assertion in an M6 test — a landmine with a measured detonation.** Fixed by scoping the assertion to the recommendation's row id (the returned row carries no `review_id`, so the id is the only handle), plus a **decoy** row that trips the old assertion. Reviewer measured the discrimination: reverting the fix with the decoy left in place reddens, and the failure names the decoy itself (`RationaleMd:decoy`). Recorded against itself in the commit: the FIRST decoy was never in the backlog at all — `seedCloseSync` always writes a `recommendation_filed_issues` row and #68 Decision 12 makes the row's *existence* the exclusion — and the test's own precondition caught it before any result was claimed.
      `ListOpenImproveUziRecommendations` (selfimprove.sql) selects
      `WHERE rr.category = 'improve_uzi'` across the **whole table** — no user scope, no
      review scope — and `TestFiledIssueCloseAutoDonesOnceLiveDB`
      (`judge_issue_close_livedb_test.go`) iterates that result **filtering only by target**.
      So any future fixture, in any package, that seeds an *open* `improve_uzi` row on target
      `rg` fails that test for reasons entirely unrelated to it. **This already cost time on
      2026-07-21**: it failed the first baseline run of the M4 badge fixture, whose only sin
      was using `improve_uzi` as its second category. That is the branch's own
      "scope live-DB assertions to the fixture, never the whole table" rule, violated inside a
      reviewed-and-audited test. **The fix is to assert on the recommendation's ID**, since
      the returned row carries no `review_id` to scope by. Not fixed at the time because scope
      was frozen and the test was a validated artifact; the M4 fixture was moved to an inert
      category instead, with the reason recorded at the constant. Related, unfixed, same
      shape: `crossCat` in `TestJudgeBacklogProjectsEveryColumnLiveDB` also seeds an open
      `improve_uzi` row and avoids the collision **only** because its target is `rg-auto`.

- [ ] **M8b — the e2e leg.** Unblocked since PRD #97 merged and this branch merged `main`
      (`create_run`, `retry_read`, positive controls are all in the tree). Its value is
      **entirely in assertions fakes structurally cannot make** — a happy-path walkthrough
      would duplicate coverage that already exists three times over. It is the natural home
      for the two mechanisms below.
- [ ] **The printed-instruction backstop.** Three instructions existed in the CLI, **none had
      ever been executed, and two were false** (the revert hint, fixed; the truncation
      remedy, fixed). A string that tells a user what to do is an assertion nothing
      typechecks. The mechanism: a table of `arrange → produce → extract → assert` rows that
      **executes the printed text verbatim** (never a hand-written copy — both false
      instructions parsed perfectly), asserts an **outcome** rather than an exit code, and
      **asserts its own precondition** or is vacuous. **The piece that makes it a class
      mechanism and not three patches**: a backstop scanning `api/cmd/uzi/` for printed
      backticked `uzi …` commands that **fails if any has no row in the table** — so the
      *fourth* instruction, the one nobody has written yet, fails the build until someone
      runs it. That half needs **no stack** (a grep and a set difference) and can land
      independently. Constraint: each row must bind to the command that **emits** it —
      running an instruction against the wrong command manufactures a false finding exactly
      as reading it manufactures a false pass.
      **STATUS 2026-07-21 (late): the registry half LANDED and its central guarantee is FALSE.**
      `api/cmd/uzi/instructions_test.go` ships both `TestPrintedInstructionsAreRegistered` and
      `TestRegisteredInstructionsAreStillPrinted`, with an AST lift, a cobra-tree verb filter and
      a "0 instructions found" vacuity guard — and it has already fired once in anger, on a PRD
      #104 instruction that arrived at the landing merge. So the *scanning* half is done. **What
      remains is the executing half**, and by the file's own bar at `:38-41` ("adding a line here
      is a claim that the instruction has been EXECUTED and its outcome asserted") **0 of its 8
      entries qualify today**: six disclaim execution outright, and the two that do not rest on
      `FakeClient` component pins — one of them asserting only that stderr *contains the word*
      `"refresh"`, which is presence, not the command.
      **The guarantee at `:32-36` — "a FOURTH instruction cannot land silently" — does not hold
      for the shape a real instruction takes.** `instructionRE` (`:130`) is
      `` `(uzi [a-z][a-z0-9 -]*)` ``; the class excludes `%`, `<` and `|`, so the group ends at
      the disallowed byte and the required closing backtick is not there. Three printed
      instructions are therefore **unliftable**: `` `uzi review show %s` `` (`review.go:531`),
      `` `uzi review backlog --run <run-id>` `` (`:403`), and `` `uzi review resolve|dismiss …` ``
      (`:54`). The first is the instruction the `uzi review show` registry entry exists for — **it
      is registered only because a human happened to type it**; had they not, the check designed
      to force it would have stayed green.
      **The root cause is not the character class, it is that the two directions use different
      matchers for the same concept.** `TestRegisteredInstructionsAreStillPrinted` uses
      `strings.Contains` on raw literals, so it finds `uzi review show` *inside*
      `uzi review show %s` — while the registration scan never saw it. Only the weaker matcher is
      the one that fails the build on a new instruction. A fix that only widens the class leaves
      the disagreement in place.
      **The resolution, and it dissolves the problem rather than working around it:** `%s` exists
      only in the SOURCE literal. `uzicli.Exitf` is `fmt.Errorf(format, a...)`
      (`api/internal/uzicli/output.go:48`), so by the time the text reaches a user it is a
      complete runnable command with no verb in it. **Never read the source literal on the
      execution path** — capture the emitting command's real stderr/stdout, lift the backticked
      span from *that*, and exec what was lifted. Which gives the split the two matchers should
      have had from the start: the **static** check ("does a row exist?") must read source, and
      needs the widened class plus a boundary-aware prefix match (`cmd == k.command ||
      strings.HasPrefix(cmd, k.command+" ")` — today `uzi review backlog-export` is silently
      absorbed by the `uzi review backlog` entry, confirmed by execution); the **runtime** row
      ("does the printed text work?") must read emitted output, and needs no regex widening at
      all. Splitting them by what they are FOR, not by what they match, is the actual fix.
      **Known cost, priced before committing:** a runtime row for `uzi review show` needs the id
      `resolveRecID` printed, so its `arrange` step requires a real no-match against a booted API
      — an e2e-shaped cost for a unit-shaped test, and the likely reason six entries say NOT
      EXECUTED rather than nobody having tried. If only a subset is reachable, the registry is
      already the right artifact for saying which, honestly.
- [ ] **Seam 6 — mock↔server fidelity. MEASURED 2026-07-21: no divergence found, but the
      demo fixture cannot reach the two riskiest behaviours.** A differential harness dumped
      the shipped `mockReviews`, ran the real `GroupJudgeRecommendations` over rows built in
      `rv.updated_at DESC` order, and structurally diffed against `mockApi.getJudgeBacklog`:
      **7 groups, 0 field diffs, identical ordering.** Detection power proven (swapping the JS
      `BUCKET_RANK` produced 4 immediate divergences), and **sort stability is genuinely
      exercised** — the fixture contains a four-way tie at `(run_count=1, open_count=0)` and
      both sides order it identically, so `sort.SliceStable` vs JS `.sort()` is covered by
      data rather than by reading.
      **The gap: the demo fixture contains ZERO instances of `occurrences > run_count`** (the
      same coordinate twice in one review — the Go grouper's own comment calls it out, and it
      is the shape that crashed the endpoint with SQLSTATE 21000) **and ZERO fully-settled
      groups with disagreeing members** — so `topRung` never has to *choose*, and the
      `dismissed > done > filed` precedence ladder, the single most-duplicated logic across
      the two implementations, is **never exercised**. Extending the fixture with both showed
      the implementations agree (9 groups, 0 diffs), so this is a **coverage gap, not a
      defect** — but the fixture *is* the demo, so the blind spot is shared by the demo and by
      every mock-backed vitest.
      **Second finding: truncation is unreachable in demo mode.** `mockApi.ts:381` and `:1812`
      hardcode `truncated: false`, so the banner cannot render. M3 requires `mockApi.ts` +
      `data.ts` to "render every state"; truncation is a state, it is subtle, and seam 5
      showed its CLI remedy was outright false — making it the state you would most want
      demoable.
      **Boundary of what was measured, stated not implied:** the harness compares the Go
      **grouper**, a pure function over rows. The `?run=` anchor and the row cap live in
      **SQL** and were *not* executed — they read as equivalent (SQL's coordinate-level
      `EXISTS` filtering rows pre-grouping vs the mock's `occurrences.some(...)` filtering
      groups post-grouping, both retaining other-run occurrences) but that comparison needs a
      live DB and belongs in M8b.
      **Constraint on the golden-fixture mechanism, demonstrated by this run:** the fixture
      must be **authored to discriminate, not snapshotted from the demo** — a golden file
      derived from `mockReviews` would lock in exactly the blind spot above, agree on
      everything it covers, and *read as full coverage*. One case per reimplemented behaviour
      (dedup; occurrences>runs; partial settle; each rollup precedence pair; anchor;
      scope=open; truncation) **plus an assertion that the fixture actually exercises each**,
      in the shape of the Go grouper test's own "fixture broken … otherwise this test proves
      nothing" guard. Without that self-check the golden file rots into a snapshot the moment
      someone regenerates it.
      *Original framing:* `mockApi` is no longer a stub: it reimplements
      dedup, the rollup ranks, `run_count`, the `?run=` semi-join, the `scope=open` fan-out,
      `updated` triples, truncation and `set_via` provenance. Its agreement with Go was
      verified **only by reading** — the mode that failed repeatedly on this branch. If it
      drifts, **the demo lies AND ~860 mock-backed vitests pass while asserting a fiction**:
      two failures, one cause, nothing announces either. Settling execution: a golden fixture
      — one input through Go's `GroupJudgeRecommendations` and through `mockApi`, asserting
      identical output. Attack the cases the mock had to *reimplement* (cross-run recurrence,
      a partially-settled group, the `dismissed > done > filed > todo` rollup, the anchor
      keeping other-run occurrences, truncation cutting **before** grouping), and check
      ordering explicitly — Go uses `sort.SliceStable(run_count DESC, open_count DESC)` and a
      JS `.sort()` is not stable the same way for equal keys.
- [x] **DONE — and it was already done when this checkbox still said otherwise (closed 2026-07-21, late).**
      The prescribed fix — an unfiled coordinate INSIDE `autoRev` — is at
      `api/internal/store/judge_recommendations_integration_test.go:383`
      (`const unfiledInAutoRev = "rg-unfiled-sibling"`, used at `:386` and `:642`), landed by
      `45381961` + `8c6be2b8`, **both ancestors of `ad6c63d9`**, with the folds recorded RED in
      the comment at `:361-366` — including the exact `drop AND f.target = rr.target` this item
      prescribes. Verified by ancestry, not by reading a commit message.
      **The item was dispatched to a coder as work before anyone checked**, and a validator
      priming its context found it closed before a single commit existed. So the cost was one
      re-scope message rather than a duplicated fixture — but the lesson is the cheaper one only
      because someone re-derived an open item instead of trusting the checkbox.
      **Its residual claim was stale too, which is the subtler half:** the text below still says
      "what remains open in this item is only the JOIN predicate, and only on the M1 read query".
      That is disproved by the same fixture comment. An item can be closed in the tree while its
      own status paragraph — written to be precise about what remains — goes on describing a gap
      that no longer exists. **A checkbox is an assertion; so is the sentence explaining it, and
      the explanation rots first** because it is the part nobody re-runs.
      *Original entry preserved below unchanged, because the measurements in it are still the
      record of how the gap was found:*
- ~~**The filed-issues join's coordinate half is asserted but NOT exercised**~~ (measured
      2026-07-21). Dropping `AND f.target = rr.target` from the `LEFT JOIN` leaves
      `TestJudgeBacklogProjectsEveryColumnLiveDB` **green**, because the fixture's `autoRev`
      and `handRev` are **different reviews** — so `f.review_id = rv.id` alone separates the
      filed row from the unfiled one and the coordinate half never carries weight. **This
      covers four columns**: `filed_settled`, `filed_issue_iid`, `filed_issue_url`,
      `filed_at`. Drift would leak a filed link onto **sibling coordinates of the same
      review** — a never-filed coordinate rendering "Filed #4242", and `filed_settled`
      flipping it to the `filed` rung so the ladder hides it from To triage. Silent and
      user-visible. **One-line fix: add an unfiled coordinate INSIDE `autoRev`**, so dropping
      the predicate cross-matches and the assertion becomes load-bearing. (Caveat from the
      measurement: only the *minimal type-preserving* fold is a valid test here — two earlier
      attempts changed the generated Go type or perturbed an unrelated test, so a green from
      a non-minimal fold proves nothing.)
      **Honest count for the MR: 15 of 16 projections pinned with VERIFIED isolation, plus
      one unpinned JOIN predicate that is not a projection at all.** The `filed_at`
      *projection* IS isolated (folding `f.filed_at → now()` reddens via the unfiled-row
      absence check); what is unpinned is the filed-issues join's **coordinate predicate**,
      which affects the row-scoping of the four columns riding it. The one unisolated
      projection is `rationale_md`, and **nothing in the live-DB suite catches its fold** —
      no incidental coverage. **Both remaining gaps share one root cause: every fixture row
      carries identical values.** `bulkFixture` hardcodes `'because'` as the rationale on
      every row, so folding `rr.rationale_md → 'because'::text` collapses the stored hash and
      the read-back value to the same thing and the test **goes green** (measured). And
      `autoRev`/`handRev` put the filed and unfiled coordinates in *different reviews*, so
      `f.review_id = rv.id` alone separates them and the join's coordinate half never works.
      **One fixture principle fixes both — distinct values per fixture row**, which is the
      `memberRowIn` lesson from `136acb53` never applied to the rationale text or the
      filed/unfiled split. The fix is in the fixture, not the assertions.
      **STATUS CORRECTION, 2026-07-21 (later the same day): the `rationale_md` half of the
      paragraph above is STALE and is left visible rather than deleted, because the staleness
      is itself the lesson.** `bulkFixture` no longer hardcodes `'because'` — it writes
      `"rationale for %s/%s in run %d of %s"`, distinct per row, and the fixture's own comment
      records the measurement (GREEN under `rr.rationale_md → 'because'::text` before the
      change, RED after, on a fresh database, at both the per-coordinate hash assertion and
      the two-hashes-must-differ one). So "15 of 16 with one unisolated projection" understates
      the tree it is now read against. **What remains open in this item is only the JOIN
      predicate**, and only on the M1 read query (`judge_recommendations.sql`) — see the
      distinction recorded immediately below.
      **The BULK query carries its OWN copy of that join, and it is now PINNED (2026-07-21).**
      `judge_bulk_disposition.sql`'s `ListOwnedRecommendationsForCoords` has a second,
      independent `LEFT JOIN recommendation_filed_issues` — a different query body, so the M1
      fix above would not have covered it. It was worse than unexercised: measured at
      `a2b554a6`, `grep -c "recommendation_filed_issues\|filed_settled"` over
      `api/internal/handler/judge_bulk_disposition_livedb_test.go` returned **0**. No live bulk
      fixture had ever inserted a filed row, so `filed_settled` was FALSE on every row of every
      live exercise and Decision 2's "a FILED member is not open" rung was pinned only by a
      fake — which takes the boolean as a **parameter** and therefore cannot be wrong about
      where the boolean comes from.
      `TestBulkDispositionFiledMemberIsNotOpenLiveDB` closes it with one review holding TWO
      coordinates and a filed row on exactly ONE, which is what makes it discriminating in
      both directions. Measured, each fold on a fresh throwaway Postgres with the positive
      control passing (control RUN/PASS lines = 2, `SKIP == 0`) and the restore verified by
      `sqlc generate` giving a zero diff:
      **baseline GREEN** (129 PASS / 0 FAIL / 0 SKIP **at `31080a40`** — the tally is the
      receipt for "0 FAIL across the whole suite", and it is bound to a SHA because it was 126
      at `8c6be2b8` and 128 at `c1fcdfce`; the count is that tree's inventory, not a constant);
      **`ON f.review_id = rv.id AND f.category = rr.category AND f.target = rr.target` →
      `ON f.review_id = rv.id`** ⇒ **RED**, `updated = 0` (the one filed row cross-matches its
      sibling coordinate, so BOTH members bucket `filed` and neither is open);
      **`(f.filed_at IS NOT NULL)::bool` → `false::bool`** ⇒ **RED**, `updated = 2` (nothing
      reads as filed, so both members are open). Each fold reddened **exactly this one test**.
      A one-coordinate fixture would have caught only the second. Both folds were compiled
      (`sqlc generate` + `go vet`) before being believed; `false::bool` is type-preserving here
      because the projection is already NOT NULL via the cast — the nullability trap in
      `CLAUDE.md` applies to folding a nullable LEFT-JOIN column, which this is not.
- [x] **DONE `a5235362` + `d5121684` (reviewed) — `ListRunInputsForRun` has NO live exercise — found by the query inventory, 2026-07-21.** The inventory's first find, against a query outside the work that motivated it. Four folds, all RED. The fourth was added after review: the commit claimed the cap taking the oldest *n* was "the strongest property" and **no fold reached it** (`:102` fatals first), so it was asserted, plausible and unmeasured — the fourth fold (`LIMIT` inside a subquery, applied before `ORDER BY`) proved it reachable. Two of the first three folds land on the same assertion (`:96`); noted rather than left implying three independent pins. Known limit recorded at the site: `consumed_at` and `created_at` are unpinnable on this fixture twice over — neither is asserted, and the insert supplies only `(run_id, kind, body)` so both are uniform across all rows.
      `judge.sql`'s `ListRunInputsForRun` is called from exactly one place in production
      (`workersvc/judge_trace.go:89`) and from **no test that touches a database**. Every test
      reaching `JudgeTrace` runs against workersvc's `fakeStore`
      (`workersvc/service_test.go:393`), which returns a canned slice, so the SQL text has
      never executed under test. The judge's oldest-first input cap rides this query
      (`follow_up_inputs_integration_test.go:21` describes the shape). Recorded rather than
      fixed because scope is frozen; it is declared `UNPINNED` with this reason in
      `api/internal/store/query_inventory_test.go` (renamed off the `judge` prefix at `041c5291`
      when the table outgrew the judge family — it was `judge_query_inventory_test.go`), which is
      the only reason it is
      visible at all.

- [ ] **Widen the query inventory beyond the judge family.** The declaration test landed for
      the five judge-family `.sql` files (**17 queries**). Repo-wide is **276 queries across
      28 files**, and a repo-wide table written in one sitting would be mostly `UNPINNED` rows
      authored by someone who had not investigated any of them — which is a worse artifact
      than none, because it reads as an audit. Widen one file at a time, with the call sites
      opened.
      **What the mechanism is and is not, so a later reader does not over-credit it:** it
      proves only that someone has DECLARED where a query is pinned, **not that the pin is
      good**. Nothing in it executes a query or folds a predicate; a row naming a test that
      merely touches the query is exactly as green as a row naming a test that reddens under
      mutation. What it does catch is the case that currently produces silence — a query
      **arriving or being renamed with nobody having thought about coverage** — and it fails
      the build until a human writes a row.
      **`UNPINNED` is a legal, green, permanent state, and that is a design constraint rather
      than a concession:** a mechanism that fails the build for an honestly-declared gap gets
      deleted the first time someone is in a hurry, taking the arriving-query check with it.
      **Declared, not inferred, and the inference was measured wrong in BOTH directions on
      these same 17 queries** (the auditor's prototype, `Test*LiveDB` body scan): (a) the two
      `judge_bulk_disposition.sql` queries appear in **no test source at all** — reached only
      through workersvc from a handler test — which is the mechanism behind the 48 repo-wide
      queries it classified as "named in tests but no LiveDB caller"; (b) **a second, distinct
      false-negative mechanism found while writing the table**: `ListDispositionsForReview` is
      called at `recommendation_dispositions_integration_test.go:262`, inside the
      package-level helper `listDispositions` (`:260`) that the test calls at `:225` — in the
      file, but in no test function's body, so a body scan misses it and a whole-file scan
      would instead credit every test in the file; (c) false positive: `CreateJudgeRun`'s
      first inferred pinner is `TestClaimRunDockerRepoAllowlistLiveDB`, which uses it as
      fixture setup for an unrelated property.
      **Negative controls run at the tip, all four RED then restored green:** an undeclared
      new query in `judge.sql`; a pin renamed to a test that does not exist; a row renamed so
      it names no query (fires BOTH the missing and the stale check); an `UNPINNED` row whose
      reason is blank. Plus two self-checks against a vacuous green — a per-file "0 query
      names parsed" abort and a whole-scan "0 queries total" abort — because the two ways this
      test passes for any tree are "the glob found nothing" and "the regex matched nothing",
      and each check catches only its own.

- [x] **DONE `97bd9528` + `0745c5f1` (audited) — The inbox is the ONLY judge-text renderer with no escaping pin** (M5 audit,
      2026-07-21). `notificationBody` returns `payload.body`, which carries
      `reviewSummaryPreview(sub.SummaryMd)` — scrubbed and capped, but untrusted judge free
      text. React escapes it, so the property HOLDS; nothing asserts it. The other two
      renderers both carry the pin (`RunView.test.tsx:256`, `Judge.test.tsx:117`);
      `Notifications.test.tsx` has no equivalent.
      **Provenance, which is why it is not fixed here:** the auditor checked `a48c5afe^` and
      the inbox already rendered those fields before M5 — M5 only EXTRACTED them into
      `lib/notifications.ts`. Pre-existing, so the freeze applies.
      **Why it matters more now than it did yesterday:** the M3 pre-flag that produced the
      other two pins was explicitly about a new author reaching for a markdown renderer, and
      M5 makes the inbox a first-class judge surface — so it is now the obvious next place
      someone does that, and the one place nothing would fail.

- [ ] **The anchored deep-link's RENDER PATH is rarely exercised in real use — a testing
      concern, not a product gap** (M5 review, 2026-07-21; **corrected the same day**).
      **The behaviour is COHERENT and the user ruled ship-as-is.** The three cases line up,
      and each default is right for what was clicked:

      | clicked | goes to | bucket | why |
      |---|---|---|---|
      | ungrouped judge notification | `/judge?run={id}` | `all` | one run; dedup may have settled its coordinate elsewhere, so `todo` could show an empty page |
      | group header | `/judge` | `todo` | no single run applies — with several reviews waiting the user is working the backlog, not one run's slice |
      | row inside the expander | `/judge?run={id}` | `all` | per-run, identical to ungrouped |

      A group header **cannot** carry a meaningful anchor: it spans several runs, and
      anchoring it to (say) the most recent would be arbitrary and would misrepresent what
      was clicked.
      **What survives is that a rarely-taken path rots quietly.** If judge notifications
      typically arrive in bursts and get grouped, the anchored + `bucket=all` path is taken
      rarely — and its whole reason for existing (avoiding an apparently-empty page on a
      fresh notification) manifests only in the case nobody routinely hits.
      **Measured, so the item is actionable rather than a worry:** `notificationLink` itself
      is covered as a pure function regardless of grouping (`notifications.test.ts`), so the
      LOGIC is pinned. What is not pinned is the RENDER path — the only test asserting an
      anchored `href` in the DOM uses a **single ungrouped** judge notification, and the
      grouping test opens the expander but asserts titles and Mark-read controls, never an
      `href`. So DOM coverage of the anchored link depends on exactly the configuration that
      is rare in production. **The item: assert the anchored `href` on a row INSIDE an opened
      group.** One assertion, in a test that already opens the expander.
      **User ruling: ship as-is, no product change.** The only alternative considered —
      defaulting the expander open for small groups (2-3), which puts the anchored rows in
      front of the user without inventing an anchor for the header — was not taken and is not
      in scope.
      *(Provenance, kept because it is the point: this entry first said the anchored path was
      "reachable only from an ungrouped row", which reads as a defect. False — the expander
      renders each `NotificationRow`, which carries the per-notification anchored link, so it
      is one click deeper. The coder wrote that expander and relayed the claim into this file
      without checking it against its own code; the lead caught it by reading the source
      rather than re-relaying the report. The reviewer's original framing — "worth a product
      look, not a defect" — was careful, and it was the compression in between that was
      wrong.)*

- [x] **A CLASS of dangling pointers to the gitignored `.claude/agent-team-tasks/` — CLASSIFIED
      AND CLOSED 2026-07-21.** The sweep found **five** hits, not the four dispatched
      (`prds/done/39` carries two). Each was classified before being touched, per the rule this
      PRD's own work put in `.claude/agent-team.md`; **three were left alone deliberately**, and
      that is the result rather than a shortfall.
      **Fixed — CURRENT CLAIMS a reader would follow today:**
      `specs/ai.md` (PRD #83's "Design grounding: <dead path>") — the live decision register,
      present tense, pointing at a file that cannot be fetched; now says the note did not survive
      and names `prds/done/83-docker-capable-worker.md` as the surviving record.
      `prds/done/39-chat-agent.md:101` — the one archived hit that makes a present-tense
      AUTHORITY claim ("Authoritative Phase-3 wire catalog: <dead path>") with no historical
      label, while the wire details it defers to are enumerated immediately above it. Annotated,
      not rewritten: the original sentence stands and a dated parenthetical says the file did not
      survive and that "authoritative" now names the surviving text.
      **Left alone — HISTORICAL RECORDS that describe their own mortality:**
      `prds/done/58-hosted-k8s-workers.md:1311` sits inside a dated 2026-07-16 log entry whose entire
      subject IS that the directory is gitignored and the notes die with the worktree — it also
      records a coder correctly declining to `git add -f` over a deliberate ignore. Rewriting it
      would erase the reasoning. (This is the one the dispatch expected to be a current claim
      because #58 is active; the file disagreed.)
      `prds/done/25-slack-integration.md:118` sits under a heading that literally reads "Original
      session-1 resume note (historical)".
      `prds/done/39-chat-agent.md:168` is inside a "Team roster to re-spawn tomorrow" paragraph
      dated to a day in July — self-evidently a record of a session, not an instruction.
      **The distinguishing test, since it is the reusable part:** *would a reader today follow
      this and be misled about where truth lives?* A dated log entry that says the file is gone
      fails that test harmlessly; an undated "Authoritative: <path>" passes it and misleads.
      **Earlier in this PRD**, two instances of the same class were corrected inside this file —
      they cited the checkpoint as authoritative for the pre-MR migration gate, the one item with
      the worst failure mode on the branch, while another line of the same document said the
      directory is gitignored. The grep that finds the whole class is one string:
      `agent-team-tasks`. The dispatched line numbers were already one hit short of what it
      returns, which is the argument for re-running it rather than inheriting the list.

- [ ] **N2 — `OccurrenceFileIssue` tests.** 236 lines, **zero tests**, and M3's **only
      forge-writing web path**; no test ever opens the occurrence expander. Its security
      controls were verified by line-by-line diff against RunView's filer (same CSRF path,
      `forgeLimiter`, draft gate, provenance box, `isHttpsUrl`) — but that duplication also
      duplicated away its coverage. A test on the duplicate is wanted, **not** a refactor.
      Also still open: its stale-filed-link warning is absent because `JudgeOccurrenceDTO`
      carries no review `updated_at`; ~~the comment now says so plainly rather than implying
      there is nothing to guard.~~
      **CORRECTION 2026-07-21 (late): that last clause was FALSE when written, and stayed false
      until `8b2ac005`.** At `ad6c63d9` the comment at the site did not mention the DTO at all —
      it said the occurrence's link is always settled "so … there is nothing to re-derive here",
      which reads as *no hazard*, the exact implication the PRD sentence claimed it had stopped
      making. Found by the coder writing the tests, who checked the comment against the code
      instead of against this file.
      **The hazard is real and the mechanism is now recorded at the site:**
      `UpsertRunReviewWithRecommendations` upserts the review IN PLACE
      (`ON CONFLICT (target_run_id) DO UPDATE … updated_at = now()`) and deletes/reinserts
      `review_recommendations`, while `recommendation_filed_issues` is
      `UNIQUE (review_id, category, target)` and cascades only from `run_reviews` (migration
      `00071`) — **so a filed link survives a re-judge.** What is missing is visibility, not the
      guard: `rv.updated_at` is available in the backlog query's `FROM` and simply unprojected,
      so RunView's `filed_at < updated_at` comparison is not computable client-side. Still not
      fixed — documented, per this item. The absence is deliberately NOT pinned as behaviour;
      pinning it would freeze the gap.
      **Why this one is worth the space: a PRD sentence asserted the state of a comment, the
      comment asserted the state of the code, and only the code was checked.** Two assertion
      layers, one gate, and the gate was on the wrong layer.

      **Implementation landed** on `feature/prd-98-t2-web` (`8b2ac005`, comments corrected in
      `3194633a` + `4a469178`): a new `OccurrenceFileIssue.test.tsx` reaching the filer through
      the Judge occurrence expander for the first time, with nine mutations each reddening
      exactly its target. **Pending review/audit at `4a469178`** — do not mark this item closed
      on the strength of this paragraph; that is precisely the mistake recorded above.

**Known limitation, accepted deliberately (2026-07-21):** the `⚖ issues` badge is
**byte-identical on `/runs` and on Judge occurrence rows**, but means different things. On
`/runs` the count is always rendered when > 0, so a bare `⚖ issues` there means *"nothing
left to triage"* (M4 behaviour (c)); on the Judge page an occurrence carries no count at all
— and those rows are by construction still open. **Only the tooltip distinguishes them**; a
user who does not hover reads the `/runs` meaning. Splitting the *label* was rejected because
it reintroduces the two-grammar problem the fable review removed and N8 closed, so the title
carries the distinction alone. Revisit only with a design that keeps one visual grammar.

**Not this PRD's, verified twice so it is not challenged at MR time:** `gofmt -l
internal/store/` flags three unrelated test files — `pipeline_statuses`, `skills`,
`worker_concurrency`. A stash comparison proves only that uncommitted work did not cause it;
the auditor ran the same check at **`06ce378b`, the branch base before any PRD #98 commit**,
and the same three are flagged there. So it is not merely "not mine", it is not this branch's.
Leave them: reformatting unrelated files inside this MR is noise a reviewer has to read.

**Also unresolved, lower priority:** the live-DB harness's intermittent *"postgres never became
ready"* — **mechanism unknown**, with two confident explanations already disproved (a "fixed
container name" that does not exist, and load contention measured at 3-6× headroom for one
concurrent suite). Recorded in the checkpoint as unknown. Sequencing is the mitigation and it
costs nothing; do not attribute it.

- [ ] **M8a — Tests + Docs (docs half in the first MR)**: e2e leg (dedup grouping; a group **Dismiss** fans out
      across runs and drops an `improve_uzi` rec from the backlog; **issue-close →
      Done, edge-once, Undo sticks, dismissed not overwritten**; the notification
      deep-links to `/judge?run=`; ~~**no token spend** on any disposition~~); vitest for
      the page/tabs/zero-state + the `/runs` badge; `docs/judge.md` (the menu, dedup
      grain, group disposition fan-out, inbox grouping, filed-sync incl. its
      PRD-label/enabled-repo preconditions) + `docs/cli.md` (`review backlog` +
      disposition verbs); `specs/ai.md` records the decisions.
      **CORRECTION 2026-07-21: "no token spend, proven by the e2e leg" IS NOT ACHIEVABLE AND THE
      CRITERION IS RETIRED, NOT DEFERRED.** An e2e spend assertion here **cannot fail**: a
      disposition creates no run, so a before/after run-count or `run.usage` delta sits at zero
      whether or not the property holds. The harness already says so in its own words at
      `e2e/run-e2e.sh:2832-2836` — the structural proof is *"strictly stronger than this harness's
      before/after run count and forge-state signature, which could only catch a write that
      happened to land"*. The one honest precedent (`run-e2e.sh:1690-1700`) could attach to
      `run.usage` only **because a run existed**.
      **Where the property IS proven:** `TestDispositionTouchesStoreOnly`
      (`api/internal/handler/review_disposition_test.go:421`) and
      `TestBulkDispositionTouchesStoreOnly` (`judge_bulk_disposition_test.go:678`) — positive
      store-call allowlists, which is a stronger proof than any delta assertion.
      **Struck through rather than deleted, deliberately.** The architect wrote this e2e assertion
      into its M8b design *while quoting the refuting comment two blocks earlier*, and a silent
      deletion here would invite the next reader to re-add it from this very bullet — which is how
      it got written the first time. The strike-through is the guard.
      **The general defect, since it recurs:** a criterion phrased as *"proven by <mechanism>"* is
      two claims, and the second one is the one nobody checks. This bullet named a mechanism that
      is structurally incapable of proving it, and the mechanism sounded more rigorous than the
      test that actually does the job.

**Follow-up (not in this PRD), found 2026-07-21 while designing M8b: `forge-fake` IGNORES
`updated_after`, so the e2e harness STRUCTURALLY CANNOT exercise the incremental-sync path.**
Measured, and it corrected the design's own earlier claim in the opposite direction.
`IncrementalSync` does pass `UpdatedAfter` (`forgesvc/service.go:294-298`) and the GitLab driver
does send it (`forge/gitlab.go:257`) — but **`forge-fake`'s `GET /issues` returns every recorded
issue by deliberate design**, so the parameter has no effect in the harness.
**Two consequences, and the second is the one that matters.**
1. The M8b issue-close mutator must bump `updated_at` — **but not for the reason first written.**
   The design said an unbumped close would be "invisible until the next `FullSync`"; measured, in
   the harness it lands immediately either way. It must bump because **a real forge does, and the
   fake must not lie about the forge.** The instruction survived; its justification was backwards,
   which is the form that later gets removed by someone simplifying.
2. **The gap is in the FAKE, not in M6 — and stating it the other way round would read as a
   shipped defect, which it is not.** M6's production path is correct: `IncrementalSync` sends the
   filter, the GitLab driver honours it, and a real GitLab bumps `updated_at` on close itself, so a
   real close is never missed. **What is missing is the harness's ability to EXERCISE that path.**
   Because the fake returns every recorded issue regardless of `updated_after`, the e2e leg proves
   the close→Done edge fires *given the cache was updated* — it cannot prove the incremental sync
   would have observed the close in the first place, because in the harness that filter never
   discriminates. A harness that cannot exercise the path it certifies is this PRD's recurring
   finding, arriving in the harness itself.
   *(Corrected 2026-07-21 immediately after first being written: the lead's original wording said
   "against a real GitLab an unbumped issue is excluded and the close is missed", which describes a
   production defect that does not exist. The architect caught it. A coverage gap written as a
   behaviour claim is the more expensive error of the two, because the next reader goes looking for
   a bug.)*
**Deliberately NOT fixed here, and the reason is the scope of the blast radius:** making the fake
honour `updated_after` changes `GET /issues` semantics for **every** phase that relies on
"return all recorded issues", and that handler's own comment records the behaviour as load-bearing
for reconcile-eviction. It needs its own change, its own measurement and its own full harness run —
a semantics change to a shared fixture buried inside a test-quality tier would be reviewed as a
footnote. Raised to the user for a follow-up issue.

**Follow-up (not in this PRD), found 2026-07-21 while covering `OccurrenceFileIssue`: ALL 24
PER-USER LIMITER MOUNTS ARE UNASSERTED, PROVEN BY EXECUTION.** It began as "our route looks
uncovered" and generalised three times under measurement; each widening was run, not reasoned.
**Measured, in throwaway detached worktrees, restored after:** `api/internal/handler/handler.go`
carries **24** `PerUserMiddleware` mounts (a bare grep says 27; three are comments) across six
limiter instances — forge 13, auth 3, chat 3, slackDM 2, hosted 2, judge 1. Removing all 13
`forgeLimiter` mounts leaves `go vet` clean and `go test ./...` at **zero failures**; removing the
8 non-forge, non-auth mounts does the same; **removing all 24 at once, across all six limiters,
does the same.** Two agents ran the forge half independently, in separate worktrees, without
reading each other — convergent measurement, not a shared assumption. It COMPILES because each
limiter stays used as a signature argument, so this is a live mutation and not a build break.
**IT WAS "21 OF 24" FOR AN HOUR, AND THE CORRECTION IS THE MOST INSTRUCTIVE PART OF THIS ENTRY.**
One validator carved out `authLimiter`'s 3 per-user mounts as genuinely covered, citing
`e2e/run-e2e.sh:1850-1871`, "which drives repeated logins through the real stack and asserts a
429". **That e2e assertion is real and it covers a DIFFERENT MOUNT ON THE SAME LIMITER OBJECT.**
`/api/auth/login` is `authLimiter.`**`Middleware`** (`handler.go:275`, IP-keyed); the three
unasserted mounts are `authLimiter.`**`PerUserMiddleware`** — `/cli/approve` (`:300`),
`/vault/unlock` (`:403`), `/vault/passphrase` (`:406`). One limiter instance, two methods, two
disjoint sets of mounts.
**Settled by the lead reading the harness rather than choosing between two reports:** the e2e
block hammers `POST /api/auth/login` with forged `X-Forwarded-For` headers (PRD #58's XFF bypass)
and touches no per-user mount at all.
**This is the "two validators can both be right because they asked different questions"
corollary, with a sting: one of them WAS wrong, and it was the one who had executed more code.**
The over-credit came from reasoning about the limiter INSTANCE where the claim was about the
MOUNT — the precise conflation this entire finding is about, committed inside the report that
established it. Running more of the code does not immunise you against answering a different
question than the one asked.
**And the sharpest instance sits in the half that was nearly written off:** `/vault/unlock` and
`/vault/passphrase` are brute-force controls on a passphrase endpoint — the single worst place in
the table to wrongly mark green. Had the carve-out stood, both would have been filed as covered.
**The mistake was available in exactly one place, and that is worth stating precisely so nobody
guards the wrong thing.** `authLimiter` is the **only** limiter mounted under BOTH methods — 6
`.Middleware` (IP-keyed: `/register` `:274`, `/login` `:275`, `/config` `:278`, the OIDC pair
`:282-283`, `/cli/start` `:291`) and 3 `.PerUserMiddleware`. The other five per-user limiters carry
one method each, so the same mis-credit could not have happened to any of them. Verified by the
one command that settles it: `comm -12` over the two greps returns `authLimiter` and nothing else
(the only other `.Middleware` user is `cliPollLimiter`, which has no per-user mount). So this is
**not** a general hazard across six limiters; it is a specific property of one, and it is cheap to
re-check.
**Diagnosis from the agent that made the error, recorded because it is more useful than the
correction:** its census command was right — it grepped `[a-zA-Z]+Limiter\.PerUserMiddleware`,
which counts only per-user mounts. Then, attributing coverage, it stopped using that predicate and
reasoned about the **object**: "authLimiter is exercised by e2e, therefore its mounts are covered."
The grep was mount-level; the sentence built on top of it was instance-level, and the join between
them was never re-derived. It had the disproof in hand — when it mutated the 8 non-forge non-auth
mounts it *excluded* auth on the strength of that reasoning, where simply including them would
have shown green and ended the question in one run. **It chose to reason where it had already
built the tool to measure.** Its own generalisation: a stack of real measurements lends unearned
credibility to the one unmeasured sentence sitting between them.
**The sharpest single fact:** the two routes carrying the ONLY `PerUserMiddleware` assertions in
the entire tree — `chat_test.go`'s `/{id}/continue` and `slack_test.go`'s `/test-dm` — are
**among the 8 deleted, and both suites stayed green.** Those tests do not cover even their own
routes' mounts. `chat_test.go:210-213` hand-writes `chi.NewRouter()` and **constructs the very
`.With(...)` it then observes working** — a tautology with respect to the mount.
**Worse, and easy to state backwards:** neither assertion is on a `forgeLimiter` route at all —
`/{id}/continue` rides `chatLimiter` (`:759`), `/test-dm` rides `slackDMLimiter` (`:467`). So
`forgeLimiter`, the instance guarding all 13 forge routes, has **zero assertions of any kind**.
What those two prove is that `mw.Limiter.PerUserMiddleware`, the shared TYPE, 429s past budget,
via two other instances. "The middleware is exercised" is fair; "forgeLimiter is exercised" is
false.
**Why nothing catches it, which is the durable half:** the file covering the judge route
(`review_issue_livedb_test.go`) calls `h.FileIssue(rr, req)` **directly as a function** at ten
sites and never constructs `h.Routes()` — **no middleware chain executes at all**, so it could not
observe a missing mount even in principle. The only tests that build the real route table use
`noLimit` or a 100k/minute budget that cannot fire. `chi.Walk` appears **nowhere** in `api/`, so
no route-table snapshot exists or could be trivially derived. And the only 429 assertion in
`e2e/run-e2e.sh` (`:1850-1871`, PRD #58's XFF forgery) is on `authLimiter.Middleware` at
`/api/auth/login` — an **IP-keyed** mount, not a per-user one, so it closes none of the 24.
**CLOSED IN THIS WAVE (`feature/prd-98-t2-lim`) — and closing it surfaced a SECOND, live
production hole that nothing could see.** `api/internal/handler/route_limiter_mounts_test.go`
walks the real `Routes(...)` table and pins, per route, **which limiter instance** is mounted —
identity by *driving* each middleware with a distinct budget, because `reflect` tags all 24 mounts
**equal** (8 instances collapse to 1 code pointer; a bound-method value's pointer is the
receiver-independent wrapper). It enumerates all 141 routes, so a route added *without* a limiter
fails as unlisted.
**The second hole, found by running the positive control on a fix rather than trusting its
framing:** the *signature*-reorder gap everyone assumed was open turned out already caught, while
swapping two **arguments at main's call site** (`cmd/server/main.go:566`, signature untouched)
**built clean and passed the entire suite** while forge routes ran on the auth budget in
production. Two `go/ast` parses now pin signature order and call-site order; measured, the
call-site swap makes exactly one test in the whole api suite fail.
**THE RESIDUAL WAS REAL AND IS NOW CLOSED — and this entry said "remains open" for about an hour
after it stopped being true.** As measured by the auditor, swapping the budgets at *construction*
(`main.go:483-484`), names untouched, gave `BUILD OK` and `go test ./...` exit 0 across 41
packages: every link then pinned — budget→name, name→signature, signature→call — while the server
ran forge routes on the auth budget. **`8ce7ba50` closes it** with a fourth parse,
`TestEachLimiterIsBuiltFromItsOwnConfigField`, reading each `x := mw.NewLimiter(cfg.Y, …)` out of
main and comparing against an exact declared table (`limiterConfigFields`, verified present with
all eight entries). The auditor's fold re-run verbatim at the new tip now fails, naming **both**
sides: *"main builds authLimiter from cfg.ForgeRateLimitMax, but limiterConfigFields declares
cfg.RateLimitMax"*.
**Do not carry "unpinned at its source" forward — it is pinned to the cfg field.** What remains is
one step further out and **unpinnable by any test**: that `cfg.ForgeRateLimitMax` is the *right*
number. It could be `1` or `10000` and all four tests stay green. That is a product judgement, and
the chain honestly stops at the name.
*(Recorded because it is the same failure this PRD keeps finding: a follow-up entry that outlived
the fix, in the document a resuming session reads first. The fix landed, the entry did not move,
and the implementer had to tell the lead.)*
The shape is the lesson: *each of these tests pins a correspondence between two NAMES, and the one
thing no name-correspondence can see is whether a name was bound to the right value in the first
place.*
Not fixed here beyond that: the wider posture change (adding limiters to unlimited routes) is a
Go-side change across the whole limiter surface, and folding it into a test-quality tier for the
judge menu would be scope creep into a security surface deserving its own change. **The counts are `ad6c63d9`'s inventory, not constants; the durable claims are
mount-vs-behaviour, the direct-function-call blindness, and that the only two assertions are
tautological with respect to the mount they construct.** Raised to the user for a follow-up issue.

**Follow-up (not in this PRD): bulk file-as-one-issue.** A `POST
.../recommendations/file` that files one aggregated GitLab issue linking N members
across a group (or several groups) needs its own decisions — per-item `repo_id`
selection (+ caller-owns-repo), an aggregated **human draft** through #68's
sanitizer, `forgeLimiter`, and a `RequireAuth`→`RequireUser` posture change (or a
deliberate browser-only stance). Tracked separately; v1 files per-recommendation
via the existing #68 flow.

**Dependency graph** (house convention):

| Phase | Milestones | Depends on | Touches |
|---|---|---|---|
| 1 (parallel) | **M1** grouped read · **M4** /runs badge · **M6** filed→Done (migration) | existing #46/#68/#94 | new judge-recs query/handler · `runs.go`+`RunListItem` · migration + `poller`/`forgesvc` |
| 2 | **M2** bulk disposition | M1 | new `handler/judge_bulk.go` + `handler.go` route |
| 3 (parallel) | **M3** Judge page+nav (+ strip removal) · **M5** notif retarget+inbox grouping · **M7** CLI | M1 + M2 (M5 needs M3's route) | `web/` · `judge_worker`+`Notifications.tsx` · `api/cmd/uzi/` |
| 4 | **M8** tests + docs | all | e2e/vitest/docs |

M4 and M6 are independent of the API core (own join / own poller pass) and run in
parallel with M1 from day one; M2 gates the mutating consumers. **M5 moved to
Phase 3** — it deep-links `/judge` and needs the `?run=` filter, both of which land
in M3; shipping it in Phase 1 would point every judge notification at a 404. The
`TriageSummary` **strip removal is folded into M3** (its aggregate's new home is the
Judge page header). Single repo, so no cross-repo phase.

## Success Criteria

- From the **Judge** menu a user sees every open recommendation **deduped by
  `(category, target)` across all their runs**, each with a "seen in N runs"
  count and an expander to the per-run occurrences.
- **One group *disposition* action settles every occurrence**: a group **Dismiss**
  / **Mark done** dispositions all open (`bucket==todo`) members across runs, in
  one call; **filing** is per-recommendation via the existing #68 browser draft
  (bulk file-as-one-issue is a follow-up).
- The **nav badge, the notification, and the page's To-triage tab show the same
  number** (`triage.todo` via the shared `BucketOf`); "seen in N runs" never
  appears as a competing count.
- The **judge notification is unchanged as an event** but deep-links to
  `/judge?run={id}` (web and Slack); the in-app inbox groups consecutive judge
  rows (Slack DMs keep their one-per-review cadence — no Slack digest).
- The `/runs` list shows a **one-grammar per-row judge badge** (`⚖ verdict · N`)
  and **no longer** carries the global strip.
- A **filed issue closing auto-moves its recommendation to Done exactly once**
  (edge-triggered), dropping it from To triage and, for `improve_uzi`, the
  self-improve backlog; a human **Undo sticks** (the next poll tick does not
  re-apply); a member the user **dismissed** is never overwritten; a reopen does
  not re-open.
- `uzi review backlog` + the disposition verbs drive the **same state** as the web
  from a uzc_ token; a uza_ read-only token can `backlog` but is refused (404) on a
  bulk disposition mutation.
- **No Anthropic token is spent** by any disposition/backlog action (proven by
  the M8 e2e leg).

## Risks

- **Payload growth on an all-time backlog.** Owner-scoped and ≤50 recs/review,
  but many runs → many groups. **Corrected 2026-07-20 (audit of M1 at `0874d3f6`):
  this originally read "bounded by default", and that was false.** The default
  `?bucket=todo` filter bounds only the **response body** — the query is always the
  caller's entire all-time row set, every row is materialized into occurrence DTOs,
  and grouping runs over all of them. That is not an oversight to fix by narrowing
  the query: `triage` is *deliberately* computed over the unfiltered set so it
  equals `/me/judge/stats` whatever the filter (Decision 1), and that design is
  right. So the real bound is an explicit **hard `LIMIT` plus a `truncated` flag**
  in the response, not the filter. Own-data amplification only (no cross-tenant
  exposure), on a route group with no rate limiter. Pagination remains the fast
  follow if a heavy user's To-triage exceeds one screenful of groups.
  **What `JudgeBacklogMaxRows = 2000` does and does not bound** (audit of
  `d701a388`, 2026-07-20 — state this precisely, because the loose version is
  already circulating): it bounds rows on the wire, the Go materialization, the
  grouping pass, and the response. It does **not** bound Postgres's own work. The
  `ORDER BY` spans two tables (`rv.created_at, rv.id, rr.created_at, rr.id`), so no
  single index supplies that ordering and the server must produce the caller's full
  join result and top-N sort it before `LIMIT` applies. `?bucket=all` on a heavy
  account still walks all of *that caller's* rows server-side. **`EXPLAIN (ANALYZE,
  BUFFERS)` was subsequently run against a seeded multi-tenant fixture (120 users,
  7,278 runs/reviews, 145,222 recommendations, caller owns 1,200) and one clause of
  this paragraph came back FALSE — corrected here rather than left standing.** The
  top-N-sort-above-the-join claim is confirmed (`Limit → Sort → Hash Left Join`,
  the sort sits above the entire join), and `run_reviews` is index-bounded as
  claimed (`Bitmap Index Scan on idx_run_reviews_user` → 60 rows). But **"never a
  full-table scan" is wrong**: the plan carries `Seq Scan on review_recommendations
  (rows=145222)` and `Seq Scan on runs (rows=7278)` — both read in full to return
  1,200 rows. `idx_review_recommendations_review` exists and the planner does not
  choose it, preferring hash joins, and the plan shape is identical at 24k and at
  145k rows, so this is not a small-data artifact. **The read therefore scales with
  the TOTAL size of `review_recommendations`/`runs` across all tenants, not with the
  caller's backlog.** This is inherited from #94's join spine, not introduced by
  this PRD — the companion `GET /me/judge/stats` shows the identical seq scan — but
  #98 makes the judge page run **two** such scans per request, and the nav badge
  polls one of them. Indexing it is a **#94-scoped** change needing its own
  measurement, deliberately not attempted here. So
  the claim "an anchored pull reads only the rows it returns" is true of the API and
  **false of the database** — the `EXISTS` is still evaluated per candidate row.
  Do not promote that sentence into `docs/judge.md`.
  Relatedly the request is **half-capped**: the wide read stops at 2000 rows, but
  the `triage` stats query has no `LIMIT` (one row per recommendation, all-time).
  Acceptable and non-blocking — it is 3 narrow columns rather than 15 wide ones
  carrying up to 4 KiB of `rationale_md` each, and it is the identical query
  `/me/judge/stats` already serves unbounded on the same `RequireUser` mount, so it
  adds no exposure that is not already shipped.
- **Truncation understates SURVIVING groups, not merely missing ones — M3 and M7
  must not render a cut page as authoritative** (found in the `d701a388` review,
  2026-07-20). The `LIMIT` cuts **rows before grouping**, and the cut is by review
  recency, so a group that survives the cut can still lose its older occurrences.
  That understates its `run_count` and `open_count` and **can change its rollup
  bucket**: a group whose only open occurrence sat among the cut rows rolls up
  `done`/`dismissed` instead of `todo` and is then filtered *out* of the default
  `?bucket=todo` view. `truncated: true` honestly flags the page as partial and at
  2000 rows this is a heavy-user edge case, so the behavior is acceptable — but the
  consumer contract is "when `truncated` is true, surviving groups' counts and
  rollup may be understated", NOT "only the oldest occurrences are missing".
- **Cross-user duplication for factory-wide categories.** `install_worker_tool`
  and `improve_uzi` affect everyone, but reviews are **owner-scoped** by #46
  design, so two users can independently see and file the same recommendation →
  duplicate GitLab issues. The menu stays owner-scoped in v1; cross-user
  visibility ("also recommended for 2 other users") or admin-scoped routing for
  factory-wide categories is **Future Work**. Called out so it is a known
  limitation, not a surprise.
- **Per-coordinate divergence inside a group.** Members can hold different
  dispositions; the group rollup is display-only and group actions default to
  `scope=open`, so nothing silently overwrites a member's prior decision. The
  occurrence expander is the per-run source of truth. Documented.
- **Filed→Done provenance + observability.** An auto-done (`set_via='issue_close'`,
  `set_by_user_id=NULL`) must be visibly distinct from a hand-marked done; the
  column + "done via #IID" label carry it. And because the sync reads the
  **PRD-labeled, enabled-repo** issue cache, a filed issue that loses its label or
  whose repo is disabled won't auto-Done — a documented silent no-op, not a bug
  (Decision 6).
- **Filed→Done correctness hinges on the edge marker.** Without `close_synced_at`
  the level-triggered pass would re-apply after Undo and could overwrite a
  `dismissed` verdict (fable's finding). The marker + `ON CONFLICT DO NOTHING`
  make it fire once and never clobber a human verdict — but this is the subtle
  part of M6 and must be the test that gates it.

## Resolved / Open Questions

- **Name is Judge** (not Reviews) — matches `RunKindJudge`, `JudgePanel`, the
  `uzi review` CLI, and the Settings opt-in; "Reviews" collides with
  MR/code-review. [user-decided 2026-07-20]
- **Worklist is by-target + dedup**, run as evidence — not by-run. Frequency, not
  self-reported confidence, ranks the backlog. A by-run *toggle* is **Future
  Work**, not v1. [user-decided 2026-07-20]
- **The triage state machine is already closed by PRD #94** — Done, and Dismiss ▾
  (Won't-do / Not-an-issue = false positive). The menu reuses those verbs via
  bulk fan-out; it introduces **no new disposition states**. (The concept mock's
  flat three-button row under-represented what already ships.)
- **The notification is kept, not removed** — the original "move notifications
  into the menu" idea is resolved as *retarget the ping's deep-link*, keeping the
  generic inbox and the "review landed" signal. [discussion-decided]
- **v1 scope includes** bulk multi-select **disposition** (dismiss / mark-done),
  **web-inbox grouping** of judge rows, and Filed→Done issue sync. **No Slack
  digest** — judge Slack DMs stay one-per-review. **Keyboard triage (j/k)** and a
  **target-file staleness marker** (distinct from #94's rationale-hash stale flag)
  are **Future Work**. [user-decided 2026-07-20]
- **RESOLVED — bulk "file N as one issue" is descoped to a follow-up.**
  [user-decided 2026-07-20] fable's PRD pass showed it is a mini-PRD, not a #68
  reuse (repo pick + aggregated human draft + `forgeLimiter` + a
  `RequireAuth`→`RequireUser` posture change). v1 ships bulk *disposition* +
  per-recommendation browser filing as written; bulk file-as-one-issue becomes a
  separate PRD. M2/M3/M7 therefore do **not** grow to carry those four concerns,
  and the CLI gains **no** `file` verb (#68's browser-only stance stands).
- **This PRD ships exactly one migration** (Decision 6: `set_via` +
  `close_synced_at`); everything else is a new query, new endpoints, and web. The
  read model (M1) is migration-free.
